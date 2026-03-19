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
	"sync/atomic"
	"testing"

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

// flakyHealthServer returns Unavailable for the first N calls, then OK.
type flakyHealthServer struct {
	healthpb.UnimplementedHealthServer
	failCount int32
	calls     atomic.Int32
}

func (s *flakyHealthServer) Check(_ context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	n := s.calls.Add(1)
	if n <= s.failCount {
		return nil, status.Error(codes.Unavailable, "temporarily unavailable")
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func TestGRPCDialOptions_RetriesUnavailable(t *testing.T) {
	// Start a gRPC server that fails the first call with Unavailable,
	// then succeeds. With retries enabled, the client should succeed.
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	flaky := &flakyHealthServer{failCount: 1}
	healthpb.RegisterHealthServer(srv, flaky)
	go srv.Serve(lis)
	defer srv.Stop()

	// Dial with GRPCDialOptions (which include the retry interceptor).
	opts := GRPCDialOptions()
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	conn, err := grpc.NewClient(lis.Addr().String(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("expected retry to succeed after transient Unavailable, got: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("got status %v, want SERVING", resp.Status)
	}
	if got := flaky.calls.Load(); got != 2 {
		t.Errorf("expected 2 calls (1 fail + 1 success), got %d", got)
	}
}

func TestGRPCDialOptions_DoesNotRetryInternal(t *testing.T) {
	// Start a gRPC server that always fails with Internal.
	// With the default config, Internal should NOT be retried.
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	internal := &internalErrorServer{}
	healthpb.RegisterHealthServer(srv, internal)
	go srv.Serve(lis)
	defer srv.Stop()

	opts := GRPCDialOptions()
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	conn, err := grpc.NewClient(lis.Addr().String(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	_, err = client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := status.Code(err); got != codes.Internal {
		t.Errorf("expected codes.Internal, got %v", got)
	}
	if got := internal.calls.Load(); got != 1 {
		t.Errorf("expected 1 call (no retries for Internal), got %d", got)
	}
}

// internalErrorServer always returns Internal.
type internalErrorServer struct {
	healthpb.UnimplementedHealthServer
	calls atomic.Int32
}

func (s *internalErrorServer) Check(_ context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	s.calls.Add(1)
	return nil, status.Error(codes.Internal, "internal error")
}
