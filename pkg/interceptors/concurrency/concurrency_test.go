/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package concurrency

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// slowServer holds each request for the configured delay.
type slowServer struct {
	healthpb.UnimplementedHealthServer
	delay   time.Duration
	serving atomic.Int32
}

func (s *slowServer) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	s.serving.Add(1)
	defer s.serving.Add(-1)
	select {
	case <-time.After(s.delay):
		return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
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

func TestLimiter_LimitsConcurrency(t *testing.T) {
	srv := &slowServer{delay: 200 * time.Millisecond}
	addr, stop := startServer(t, srv)
	defer stop()

	limiter := NewLimiter(2)
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(limiter.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := healthpb.NewHealthClient(conn)

	// Launch 5 concurrent calls with a limit of 2.
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.Check(context.Background(), &healthpb.HealthCheckRequest{})
		}()
	}
	// Give goroutines time to saturate the limiter, then verify the
	// server-side serving count never exceeds the limit. srv.serving
	// is incremented inside the server handler, so it reflects actual
	// concurrent RPCs reaching the server (not just semaphore occupancy).
	time.Sleep(50 * time.Millisecond)

	if got := srv.serving.Load(); got > 2 {
		t.Errorf("server serving count = %d, want <= 2", got)
	}
	if got := limiter.InFlight(); got > 2 {
		t.Errorf("InFlight() = %d, want <= 2", got)
	}

	wg.Wait()
}

func TestLimiter_RespectsContextCancellation(t *testing.T) {
	srv := &slowServer{delay: 5 * time.Second}
	addr, stop := startServer(t, srv)
	defer stop()

	// Limit to 1 concurrent call.
	limiter := NewLimiter(1)
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(limiter.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := healthpb.NewHealthClient(conn)

	// First call takes the only slot.
	go client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	time.Sleep(50 * time.Millisecond) // Let it acquire the slot.

	// Second call with a short context should fail waiting for the slot.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = client.Check(ctx, &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	code := status.Code(err)
	if code != codes.DeadlineExceeded && code != codes.Canceled {
		t.Errorf("expected DeadlineExceeded or Canceled, got %v", code)
	}
}

func TestLimiter_DisabledWithZero(t *testing.T) {
	srv := &slowServer{delay: 10 * time.Millisecond}
	addr, stop := startServer(t, srv)
	defer stop()

	limiter := NewLimiter(0)
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(limiter.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := healthpb.NewHealthClient(conn)

	// Should pass through with no limit.
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("got %v, want SERVING", resp.Status)
	}
	if got := limiter.InFlight(); got != 0 {
		t.Errorf("InFlight() = %d, want 0 (disabled)", got)
	}
	if got := limiter.Capacity(); got != 0 {
		t.Errorf("Capacity() = %d, want 0 (disabled)", got)
	}
}

func TestLimiter_StreamReleasesSlotOnRecvError(t *testing.T) {
	// The stream interceptor should hold a slot while the stream is alive
	// and release it when RecvMsg returns an error.
	srv := &slowServer{delay: 10 * time.Millisecond}
	addr, stop := startServer(t, srv)
	defer stop()

	limiter := NewLimiter(1)
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStreamInterceptor(limiter.StreamClientInterceptor()),
		grpc.WithUnaryInterceptor(limiter.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := healthpb.NewHealthClient(conn)

	// Start a Watch stream — this acquires the only slot.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	stream, err := client.Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Slot should be occupied.
	if got := limiter.InFlight(); got != 1 {
		t.Errorf("InFlight() after Watch = %d, want 1", got)
	}

	// Drain RecvMsg until error (context timeout or server close).
	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
	}

	// Slot should now be released.
	if got := limiter.InFlight(); got != 0 {
		t.Errorf("InFlight() after stream drain = %d, want 0", got)
	}

	// Verify the slot is actually usable — a unary call should succeed.
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check after stream release: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("got %v, want SERVING", resp.Status)
	}
}

func TestLimiter_StreamReleasesSlotOnContextCancel(t *testing.T) {
	// Verify that canceling the stream context releases the slot even
	// if RecvMsg is never drained — the safety net goroutine should fire.
	srv := &slowServer{delay: 10 * time.Millisecond}
	addr, stop := startServer(t, srv)
	defer stop()

	limiter := NewLimiter(1)
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStreamInterceptor(limiter.StreamClientInterceptor()),
		grpc.WithUnaryInterceptor(limiter.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := healthpb.NewHealthClient(conn)

	// Start a Watch stream — acquires the only slot.
	ctx, cancel := context.WithCancel(context.Background())
	_, err = client.Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if got := limiter.InFlight(); got != 1 {
		t.Errorf("InFlight() after Watch = %d, want 1", got)
	}

	// Cancel the context WITHOUT draining RecvMsg.
	cancel()

	// The safety net goroutine should release the slot shortly.
	deadline := time.After(2 * time.Second)
	for limiter.InFlight() != 0 {
		select {
		case <-deadline:
			t.Fatal("slot was not released within 2 seconds after context cancellation")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Verify the slot is reusable — a unary call should succeed.
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check after context cancel release: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("got %v, want SERVING", resp.Status)
	}
}

func TestLimiter_Capacity(t *testing.T) {
	l := NewLimiter(42)
	if got := l.Capacity(); got != 42 {
		t.Errorf("Capacity() = %d, want 42", got)
	}
}
