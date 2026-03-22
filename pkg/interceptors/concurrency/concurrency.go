/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package concurrency provides a gRPC client interceptor that limits the
// number of concurrent in-flight RPCs per connection. When the limit is
// reached, new calls block until a slot opens or the context is canceled.
// This prevents a single client instance from consuming a disproportionate
// share of downstream capacity during traffic spikes.
package concurrency

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// Limiter controls the maximum number of concurrent in-flight RPCs.
type Limiter struct {
	sem chan struct{}
}

// NewLimiter creates a concurrency limiter with the given maximum.
// A maxConcurrent of 0 disables limiting (all calls pass through).
func NewLimiter(maxConcurrent int) *Limiter {
	if maxConcurrent <= 0 {
		return &Limiter{}
	}
	return &Limiter{sem: make(chan struct{}, maxConcurrent)}
}

// UnaryClientInterceptor returns a gRPC unary client interceptor that
// blocks until a concurrency slot is available or the context is canceled.
func (l *Limiter) UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		if l.sem == nil {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		select {
		case l.sem <- struct{}{}:
			defer func() { <-l.sem }()
			return invoker(ctx, method, req, reply, cc, opts...)
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// StreamClientInterceptor returns a gRPC stream client interceptor that
// acquires a concurrency slot before establishing the stream. The slot is
// released when RecvMsg returns an error (including io.EOF). Callers must
// drain RecvMsg to completion to avoid leaking slots.
func (l *Limiter) StreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		if l.sem == nil {
			return streamer(ctx, desc, cc, method, opts...)
		}
		select {
		case l.sem <- struct{}{}:
			stream, err := streamer(ctx, desc, cc, method, opts...)
			if err != nil {
				<-l.sem
				return nil, err
			}
			return newReleasingStream(stream, l.sem), nil
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
}

// InFlight returns the current number of in-flight RPCs.
// Returns 0 if the limiter is disabled.
//
// Note: the value is approximate under contention since len() on a buffered
// channel is not synchronized with concurrent send/receive operations.
func (l *Limiter) InFlight() int {
	if l.sem == nil {
		return 0
	}
	return len(l.sem)
}

// releasingStream wraps a ClientStream and releases the semaphore slot
// when the stream terminates. The slot is released on the first of:
//   - RecvMsg returns an error (including io.EOF)
//   - The stream context is canceled (safety net for abandoned streams)
type releasingStream struct {
	grpc.ClientStream
	sem     chan struct{}
	release sync.Once
}

func newReleasingStream(stream grpc.ClientStream, sem chan struct{}) *releasingStream {
	rs := &releasingStream{ClientStream: stream, sem: sem}
	// Safety net: release the slot if the stream context is canceled
	// without RecvMsg being drained. This prevents permanent slot leaks
	// from abandoned streams.
	go func() {
		<-stream.Context().Done()
		rs.release.Do(func() { <-rs.sem })
	}()
	return rs
}

func (s *releasingStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		s.release.Do(func() { <-s.sem })
	}
	return err
}

// Capacity returns the maximum number of concurrent RPCs allowed.
// Returns 0 if the limiter is disabled.
func (l *Limiter) Capacity() int {
	if l.sem == nil {
		return 0
	}
	return cap(l.sem)
}
