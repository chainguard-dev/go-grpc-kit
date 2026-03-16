/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package options

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestGetEnv(t *testing.T) {
	old := os.Getenv("GRPC_CLIENT_MAX_RETRY")
	defer os.Setenv("GRPC_CLIENT_MAX_RETRY", old)

	want := envStruct{
		EnableClientHandlingTimeHistogram:      true,
		EnableClientStreamReceiveTimeHistogram: true,
		EnableClientStreamSendTimeHistogram:    true,
		GrpcClientMaxRetry:                     42,
		GrpcClientDefaultTimeout:               30 * time.Second,
	}
	t.Run("can change env right before usage", func(t *testing.T) {
		os.Setenv("GRPC_CLIENT_MAX_RETRY", strconv.Itoa(int(want.GrpcClientMaxRetry)))
		if diff := cmp.Diff(want, state().env); diff != "" {
			t.Errorf("getEnv() -want,+got: %s", diff)
		}
	})
	t.Run("but cannot change after usage", func(t *testing.T) {
		os.Setenv("GRPC_CLIENT_MAX_RETRY", strconv.Itoa(int(want.GrpcClientMaxRetry+10)))
		if diff := cmp.Diff(want, state().env); diff != "" {
			t.Errorf("getEnv() -want,+got: %s", diff)
		}
	})
}

// slowServer sleeps before responding, allowing deadline tests.
type slowServer struct {
	healthpb.UnimplementedHealthServer
	delay time.Duration
}

func (s *slowServer) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	select {
	case <-time.After(s.delay):
		return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

func TestDefaultDeadlineInterceptor_AppliesTimeout(t *testing.T) {
	// Server takes 5s to respond. The default timeout is 30s, but we
	// override to 100ms via env var for this test. The call should fail
	// with DeadlineExceeded.
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, &slowServer{delay: 5 * time.Second})
	go srv.Serve(lis)
	defer srv.Stop()

	interceptor := defaultDeadlineInterceptor(100 * time.Millisecond)
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(interceptor),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	_, err = client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatal("expected DeadlineExceeded, got nil")
	}
	if got := status.Code(err); got != codes.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", got)
	}
}

func TestDefaultDeadlineInterceptor_RespectsExistingDeadline(t *testing.T) {
	// If the caller already set a deadline, the interceptor should not
	// override it. Use a 5s caller deadline with a 100ms interceptor timeout.
	// The server responds in 50ms — should succeed because the caller's
	// 5s deadline governs, not the interceptor's 100ms.
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, &slowServer{delay: 50 * time.Millisecond})
	go srv.Serve(lis)
	defer srv.Stop()

	interceptor := defaultDeadlineInterceptor(100 * time.Millisecond)
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(interceptor),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("expected success with caller deadline, got: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("got %v, want SERVING", resp.Status)
	}
}

func TestDefaultDeadlineInterceptor_DisabledWithZero(t *testing.T) {
	// With timeout=0, no deadline is applied. The call should succeed
	// even though the server takes a moment to respond.
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, &slowServer{delay: 50 * time.Millisecond})
	go srv.Serve(lis)
	defer srv.Stop()

	interceptor := defaultDeadlineInterceptor(0)
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(interceptor),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("expected success with disabled timeout, got: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("got %v, want SERVING", resp.Status)
	}
}
