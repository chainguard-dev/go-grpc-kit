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

// dialWithDeadlineInterceptor starts a gRPC server with the given delay and
// returns a health client wired through the deadline interceptor.
func dialWithDeadlineInterceptor(t *testing.T, serverDelay, interceptorTimeout time.Duration) healthpb.HealthClient {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, &slowServer{delay: serverDelay})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(defaultDeadlineInterceptor(interceptorTimeout)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return healthpb.NewHealthClient(conn)
}

func TestDefaultDeadlineInterceptor_AppliesTimeout(t *testing.T) {
	// Server takes 5s to respond, interceptor timeout is 100ms.
	// No caller deadline → interceptor applies 100ms → DeadlineExceeded.
	client := dialWithDeadlineInterceptor(t, 5*time.Second, 100*time.Millisecond)

	_, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatal("expected DeadlineExceeded, got nil")
	}
	if got := status.Code(err); got != codes.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", got)
	}
}

func TestDefaultDeadlineInterceptor_RespectsExistingDeadline(t *testing.T) {
	// Server responds in 200ms, interceptor timeout is 100ms, caller
	// deadline is 5s. If the interceptor incorrectly overwrote the
	// caller's deadline, the 200ms response would exceed the 100ms
	// interceptor timeout and fail. Since the caller's deadline governs,
	// the call succeeds.
	client := dialWithDeadlineInterceptor(t, 200*time.Millisecond, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("expected success with caller deadline, got: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("got %v, want SERVING", resp.Status)
	}
}

func TestDefaultDeadlineInterceptor_DisabledWithZero(t *testing.T) {
	// timeout=0 disables the interceptor. Call succeeds despite no deadline.
	client := dialWithDeadlineInterceptor(t, 50*time.Millisecond, 0)

	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("expected success with disabled timeout, got: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("got %v, want SERVING", resp.Status)
	}
}
