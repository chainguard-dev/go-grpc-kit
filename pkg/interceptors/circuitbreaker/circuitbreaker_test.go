/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package circuitbreaker

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// flakyServer fails with the given code for the first N calls, then succeeds.
type flakyServer struct {
	healthpb.UnimplementedHealthServer
	failCode  codes.Code
	failCount int32
	calls     atomic.Int32
}

func (s *flakyServer) Check(_ context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	n := s.calls.Add(1)
	if n <= s.failCount {
		return nil, status.Error(s.failCode, "error")
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func startServer(t *testing.T, srv healthpb.HealthServer) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	healthpb.RegisterHealthServer(s, srv)
	go s.Serve(lis)
	return lis.Addr().String(), s.Stop
}

func dial(t *testing.T, addr string, cb *gobreaker.CircuitBreaker[any]) healthpb.HealthClient {
	t.Helper()
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(UnaryClientInterceptor(cb)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return healthpb.NewHealthClient(conn)
}

func TestCircuitBreaker_TripsAfterConsecutiveFailures(t *testing.T) {
	// Server always fails with Internal.
	addr, stop := startServer(t, &flakyServer{failCode: codes.Internal, failCount: 100})
	defer stop()

	settings := DefaultSettings("test")
	settings.ReadyToTrip = func(counts gobreaker.Counts) bool {
		return counts.ConsecutiveFailures > 2 // Trip after 2 for faster test
	}
	cb := gobreaker.NewCircuitBreaker[any](settings)
	client := dial(t, addr, cb)

	// First 3 calls hit the server and get Internal errors.
	for i := range 3 {
		_, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err == nil {
			t.Fatalf("call %d: expected error, got nil", i)
		}
		if got := status.Code(err); got != codes.Internal {
			t.Fatalf("call %d: expected Internal, got %v", i, got)
		}
	}

	// Circuit should now be open. Next call should fail fast with Unavailable.
	_, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatal("expected circuit open error, got nil")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("expected Unavailable (circuit open), got %v", got)
	}
}

func TestCircuitBreaker_ClientErrorsDoNotTrip(t *testing.T) {
	// Server always fails with NotFound (a client-side error).
	addr, stop := startServer(t, &flakyServer{failCode: codes.NotFound, failCount: 100})
	defer stop()

	settings := DefaultSettings("test")
	settings.ReadyToTrip = func(counts gobreaker.Counts) bool {
		return counts.ConsecutiveFailures > 2
	}
	cb := gobreaker.NewCircuitBreaker[any](settings)
	client := dial(t, addr, cb)

	// NotFound is classified as successful (client error), so the circuit
	// should NOT trip even after many calls.
	for i := range 10 {
		_, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err == nil {
			t.Fatalf("call %d: expected NotFound error, got nil", i)
		}
		if got := status.Code(err); got != codes.NotFound {
			t.Fatalf("call %d: expected NotFound, got %v", i, got)
		}
	}

	// Circuit should still be closed.
	if cb.State() != gobreaker.StateClosed {
		t.Errorf("expected circuit closed, got %v", cb.State())
	}
}

func TestCircuitBreaker_RecoversThroughHalfOpen(t *testing.T) {
	// Server fails first 3 calls, then succeeds.
	srv := &flakyServer{failCode: codes.Unavailable, failCount: 3}
	addr, stop := startServer(t, srv)
	defer stop()

	settings := DefaultSettings("test")
	settings.ReadyToTrip = func(counts gobreaker.Counts) bool {
		return counts.ConsecutiveFailures > 2
	}
	settings.Timeout = 1 * time.Millisecond // Transition to half-open almost immediately
	settings.MaxRequests = 1
	cb := gobreaker.NewCircuitBreaker[any](settings)
	client := dial(t, addr, cb)

	// Trip the breaker with 3 failures.
	for range 3 {
		client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	}
	if cb.State() != gobreaker.StateOpen {
		t.Fatalf("expected open, got %v", cb.State())
	}

	// Wait for the timeout to elapse so the breaker transitions to half-open.
	time.Sleep(5 * time.Millisecond)

	// The next call is a probe — server now succeeds (failCount=3, we're past it).
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("half-open probe should succeed: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("got %v, want SERVING", resp.Status)
	}

	// Circuit should now be closed again.
	if cb.State() != gobreaker.StateClosed {
		t.Errorf("expected closed after recovery, got %v", cb.State())
	}
}

func TestStreamClientInterceptor_TripsAndFailsFast(t *testing.T) {
	// Server always fails with Internal.
	addr, stop := startServer(t, &flakyServer{failCode: codes.Internal, failCount: 100})
	defer stop()

	settings := DefaultSettings("stream-test")
	settings.ReadyToTrip = func(counts gobreaker.Counts) bool {
		return counts.ConsecutiveFailures >= 2
	}
	cb := gobreaker.NewCircuitBreaker[any](settings)

	// Dial with both unary and stream interceptors.
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(UnaryClientInterceptor(cb)),
		grpc.WithStreamInterceptor(StreamClientInterceptor(cb)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	client := healthpb.NewHealthClient(conn)

	// Trip the breaker via unary calls (2 failures).
	for range 2 {
		_, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err == nil {
			t.Fatal("expected error")
		}
	}
	if cb.State() != gobreaker.StateOpen {
		t.Fatalf("expected open, got %v", cb.State())
	}

	// Stream call should fail fast with Unavailable (circuit is open).
	stream, err := client.Watch(context.Background(), &healthpb.HealthCheckRequest{})
	if err == nil && stream != nil {
		// Some gRPC versions defer the error to Recv.
		_, err = stream.Recv()
	}
	if err == nil {
		t.Fatal("expected circuit open error on stream, got nil")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("expected Unavailable (circuit open) on stream, got %v", got)
	}
}

func TestDefaultSettings_IsSuccessful(t *testing.T) {
	settings := DefaultSettings("test")

	tests := []struct {
		code codes.Code
		want bool
	}{
		{codes.OK, true},
		{codes.InvalidArgument, true},
		{codes.NotFound, true},
		{codes.AlreadyExists, true},
		{codes.PermissionDenied, true},
		{codes.Unauthenticated, true},
		{codes.FailedPrecondition, true},
		{codes.OutOfRange, true},
		{codes.Unimplemented, true},
		{codes.Canceled, true},
		{codes.Internal, false},
		{codes.Unavailable, false},
		{codes.DeadlineExceeded, false},
		{codes.ResourceExhausted, false},
		{codes.Unknown, false},
		{codes.Aborted, false},
		{codes.DataLoss, false},
	}

	for _, tt := range tests {
		var err error
		if tt.code != codes.OK {
			err = status.Error(tt.code, "test")
		}
		if got := settings.IsSuccessful(err); got != tt.want {
			t.Errorf("IsSuccessful(%v) = %v, want %v", tt.code, got, tt.want)
		}
	}
}
