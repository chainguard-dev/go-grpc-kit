/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package circuitbreaker provides a gRPC client interceptor that wraps calls
// with a circuit breaker. When a downstream service returns too many errors,
// the circuit opens and subsequent calls fail fast with codes.Unavailable
// instead of adding load to the failing service.
package circuitbreaker

import (
	"context"
	"errors"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/sony/gobreaker/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DefaultSettings returns circuit breaker settings tuned for gRPC
// service-to-service calls on Cloud Run.
//
//   - Opens after 5 consecutive failures
//   - Half-open after 15s (sends a probe request)
//   - Allows 10 probe requests in half-open state
//   - Resets failure count every 30s if no trip
func DefaultSettings(name string) gobreaker.Settings {
	return gobreaker.Settings{
		Name:        name,
		MaxRequests: 10,
		Interval:    30 * time.Second,
		Timeout:     15 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 5
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			// OnStateChange is called outside any request context, so use
			// context.Background() to get the process-level logger.
			clog.InfoContextf(context.Background(), "circuit breaker %s: %s -> %s", name, from, to)
		},
		IsSuccessful: func(err error) bool {
			if err == nil {
				return true
			}
			// Treat client-side errors as successes (the server didn't fail).
			// Canceled is included because cancellation is typically initiated
			// by the client (context timeout or user abort), not a server failure.
			code := status.Code(err)
			switch code {
			case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists,
				codes.PermissionDenied, codes.Unauthenticated, codes.FailedPrecondition,
				codes.OutOfRange, codes.Unimplemented, codes.Canceled:
				return true
			default:
				return false
			}
		},
	}
}

// UnaryClientInterceptor returns a gRPC unary client interceptor that
// wraps each call with the provided circuit breaker. When the circuit is
// open, calls fail immediately with codes.Unavailable.
func UnaryClientInterceptor(cb *gobreaker.CircuitBreaker[any]) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		_, err := cb.Execute(func() (any, error) {
			err := invoker(ctx, method, req, reply, cc, opts...)
			return nil, err
		})
		if err != nil {
			// Map gobreaker's sentinel errors to gRPC status codes.
			if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
				return status.Errorf(codes.Unavailable,
					"circuit breaker %s is open: %v", cb.Name(), err)
			}
		}
		return err
	}
}

// StreamClientInterceptor returns a gRPC stream client interceptor that
// checks the circuit breaker before establishing a stream. When the circuit
// is open, the stream fails immediately with codes.Unavailable.
//
// Note: only stream establishment is tracked by the circuit breaker.
// Errors on Send/Recv after the stream is established are not tracked,
// so a downstream that accepts connections but fails on every message
// will not trip the breaker.
func StreamClientInterceptor(cb *gobreaker.CircuitBreaker[any]) grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		result, err := cb.Execute(func() (any, error) {
			stream, err := streamer(ctx, desc, cc, method, opts...)
			return stream, err
		})
		if err != nil {
			if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
				return nil, status.Errorf(codes.Unavailable,
					"circuit breaker %s is open: %v", cb.Name(), err)
			}
			return nil, err
		}
		stream, _ := result.(grpc.ClientStream)
		return stream, nil
	}
}
