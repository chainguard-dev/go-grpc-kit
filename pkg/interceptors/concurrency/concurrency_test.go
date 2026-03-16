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
	var maxObserved atomic.Int32
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Sample in-flight count during the call.
			inflight := int32(limiter.InFlight())
			for {
				cur := maxObserved.Load()
				if inflight <= cur {
					break
				}
				if maxObserved.CompareAndSwap(cur, inflight) {
					break
				}
			}
			client.Check(context.Background(), &healthpb.HealthCheckRequest{})
		}()
	}
	// Give goroutines time to saturate the limiter.
	time.Sleep(50 * time.Millisecond)

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

func TestLimiter_Capacity(t *testing.T) {
	l := NewLimiter(42)
	if got := l.Capacity(); got != 42 {
		t.Errorf("Capacity() = %d, want 42", got)
	}
}
