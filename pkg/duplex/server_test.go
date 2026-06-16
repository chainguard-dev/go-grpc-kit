/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package duplex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	pb "chainguard.dev/go-grpc-kit/pkg/duplex/internal/proto/helloworld"
	"chainguard.dev/go-grpc-kit/pkg/interceptors/clientid"
	"chainguard.dev/go-grpc-kit/pkg/metrics"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func TestMetrics(t *testing.T) {
	ctx := context.Background()

	// Reserve port for test server.
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Logf("server listening at %v", lis.Addr())

	// Get IP address from listener
	ip, err := net.ResolveTCPAddr(lis.Addr().Network(), lis.Addr().String())
	if err != nil {
		t.Fatalf("error resolving IP address: %v", err)
	}

	// Setup server — no explicit WithTransportCredentials dial option here;
	// LoopbackDialOptions now includes insecure credentials for the loopback.
	d := New(ip.Port,
		grpc.ChainStreamInterceptor(metrics.StreamServerInterceptor()),
		grpc.ChainUnaryInterceptor(metrics.UnaryServerInterceptor()),
	)
	impl := &server{}
	pb.RegisterGreeterServer(d.Server, impl)
	if err := d.RegisterHandler(ctx, pb.RegisterGreeterHandlerFromEndpoint); err != nil {
		t.Fatalf("error registering handler: %v", err)
	}

	// Start metrics endpoint
	// Reserve port for test server.
	mlis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Logf("server listening at %v", mlis.Addr())

	d.RegisterAndServeMetrics(mlis, false)

	// Start server
	go func() {
		if err := d.Serve(ctx, lis); err != nil {
			panic(fmt.Sprintf("failed to serve: %v", err))
		}
	}()

	// Setup GRPC client
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()
	client := pb.NewGreeterClient(conn)
	req := &pb.HelloRequest{
		Name: "world",
	}
	resp, err := client.SayHello(ctx, req)
	if err != nil {
		t.Fatalf("grpc request failed: %v", err)
	}
	t.Log("grpc response:", resp)

	// Setup HTTP client
	httpClient := &http.Client{}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal json: %v", err)
	}
	url := fmt.Sprintf("http://%s/v1/example/echo", lis.Addr().String())
	t.Log(url)
	httpResp, err := httpClient.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Fatalf("http request failed: %v", err)
	}
	// Fail if failed request
	if httpResp.StatusCode != 200 {
		t.Fatalf("non-zero HTTP response code: %s", httpResp.Status)
	}
	b, _ := io.ReadAll(httpResp.Body)
	t.Log("http response:", string(b))

	// Verify the loopback connection carries cgclientid via the clientid
	// interceptor that is now included in the duplex dial options.
	if impl.lastClientID == "" {
		t.Error("expected cgclientid to be set on HTTP→gRPC loopback request")
	} else {
		t.Logf("HTTP loopback cgclientid: %s", impl.lastClientID)
	}

	// Verify metrics endpoint is available and shows request count
	metricsURL := fmt.Sprintf("http://%s/metrics", mlis.Addr().String())

	// Give some time for metrics to be recorded
	time.Sleep(100 * time.Millisecond)

	// Check metrics endpoint
	metricsResp, err := httpClient.Get(metricsURL)
	if err != nil {
		t.Fatalf("Failed to fetch metrics: %v", err)
	}
	defer metricsResp.Body.Close()

	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("Metrics endpoint returned status %d, expected %d", metricsResp.StatusCode, http.StatusOK)
	}

	metricsBody, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("Failed to read metrics response: %v", err)
	}

	metricsContent := string(metricsBody)
	t.Logf("Metrics content preview:\n%s", metricsContent)

	// Check for gRPC request metrics
	// Common Prometheus metrics for gRPC include:
	// - grpc_server_handled_total
	// - grpc_server_started_total
	// - grpc_server_msg_received_total
	// - grpc_server_msg_sent_total
	expectedMetrics := []string{
		"grpc_server_handled_total",
		"grpc_server_started_total",
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(metricsContent, metric) {
			t.Errorf("Expected metric %q not found in metrics output", metric)
		} else {
			t.Logf("Found expected metric: %s", metric)
		}
	}

	// Look for our specific method in the metrics
	// The metric should include method="/helloworld.Greeter/SayHello"
	// Example
	// grpc_server_handled_total{grpc_code="OK",grpc_method="SayHello",grpc_service="helloworld.Greeter",grpc_type="unary"} 2
	sayHelloMetric := `grpc_method="SayHello",grpc_service="helloworld.Greeter"`
	if !strings.Contains(metricsContent, sayHelloMetric) {
		t.Errorf("Expected SayHello method metric not found in metrics output")
	} else {
		t.Logf("Found SayHello method in metrics")
	}

	// Verify that the request count is at least 1
	// Look for patterns like: grpc_server_handled_total{...} 1
	if strings.Contains(metricsContent, "grpc_server_handled_total") {
		// Parse the metrics to find the count
		lines := strings.Split(metricsContent, "\n")
		for _, line := range lines {
			if strings.Contains(line, "grpc_server_handled_total") &&
				strings.Contains(line, sayHelloMetric) &&
				!strings.HasPrefix(line, "#") {
				t.Logf("Found handled total metric line: %s", line)
				// Basic check that the line ends with a number > 0
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					if parts[len(parts)-1] != "0" {
						t.Logf("Request count is non-zero: %s", parts[len(parts)-1])
					}
				}
			}
		}
	}
}

// TestHTTPLoopbackWithoutExplicitDialOptions verifies that the HTTP→gRPC
// loopback works when the caller passes only grpc.ServerOption (no
// grpc.DialOption). This matches how most production services call New().
// Regression test for: LoopbackDialOptions must include transport credentials.
func TestHTTPLoopbackWithoutExplicitDialOptions(t *testing.T) {
	ctx := context.Background()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	ip, err := net.ResolveTCPAddr(lis.Addr().Network(), lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// Only grpc.ServerOption — no grpc.DialOption, no WithTransportCredentials.
	// This is the pattern used by ~49 production services.
	d := New(ip.Port,
		grpc.ChainUnaryInterceptor(metrics.UnaryServerInterceptor()),
	)
	impl := &server{}
	pb.RegisterGreeterServer(d.Server, impl)
	if err := d.RegisterHandler(ctx, pb.RegisterGreeterHandlerFromEndpoint); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}

	go func() {
		if err := d.Serve(ctx, lis); err != nil {
			panic(fmt.Sprintf("Serve: %v", err))
		}
	}()

	// HTTP request through the gateway → loopback → gRPC handler.
	body, _ := json.Marshal(&pb.HelloRequest{Name: "loopback-test"})
	url := fmt.Sprintf("http://%s/v1/example/echo", lis.Addr().String())
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		t.Fatalf("HTTP POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	// Verify the request actually reached the gRPC handler.
	if impl.lastClientID == "" {
		t.Error("expected cgclientid on loopback request, got empty")
	}
}

// TestShutdown verifies that Shutdown drains the duplex and unblocks the
// serving goroutine: a request succeeds before shutdown, Shutdown returns
// cleanly, and Serve then returns http.ErrServerClosed.
func TestShutdown(t *testing.T) {
	ctx := t.Context()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	d := New(0, grpc.ChainUnaryInterceptor(metrics.UnaryServerInterceptor()))
	pb.RegisterGreeterServer(d.Server, &server{})

	serveErr := make(chan error, 1)
	go func() { serveErr <- d.Serve(ctx, lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewGreeterClient(conn)
	if _, err := client.SayHello(ctx, &pb.HelloRequest{Name: "world"}); err != nil {
		t.Fatalf("grpc request failed before shutdown: %v", err)
	}

	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned unexpected error: %v", err)
	}
}

// TestShutdownBeforeServe verifies that shutting down before serving starts is
// safe and that a subsequent Serve returns immediately with http.ErrServerClosed
// rather than starting to accept connections.
func TestShutdownBeforeServe(t *testing.T) {
	ctx := t.Context()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	d := New(0)
	pb.RegisterGreeterServer(d.Server, &server{})

	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown before serve: %v", err)
	}

	if err := d.Serve(ctx, lis); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("expected http.ErrServerClosed, got %v", err)
	}
}

// TestShutdownDrainsInflight verifies that an in-flight request is allowed to
// finish: with a request blocked in the handler, Shutdown waits for it, the
// request completes successfully, and Shutdown then returns cleanly.
func TestShutdownDrainsInflight(t *testing.T) {
	ctx := t.Context()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &blockingServer{started: make(chan struct{}, 1), release: make(chan struct{})}
	d := New(0)
	pb.RegisterGreeterServer(d.Server, srv)

	serveErr := make(chan error, 1)
	go func() { serveErr <- d.Serve(ctx, lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewGreeterClient(conn)

	callErr := make(chan error, 1)
	go func() {
		_, err := client.SayHello(ctx, &pb.HelloRequest{Name: "world"})
		callErr <- err
	}()

	<-srv.started // request is now in the handler

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- d.Shutdown(ctx) }()

	close(srv.release) // let the in-flight handler finish

	if err := <-callErr; err != nil {
		t.Fatalf("in-flight request failed during graceful shutdown: %v", err)
	}
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned unexpected error: %v", err)
	}
}

// TestShutdownTimeoutStopsInflight verifies that when the drain exceeds the
// context deadline, Shutdown returns the context error and the gRPC server is
// stopped outright, cutting off the request that overstayed.
func TestShutdownTimeoutStopsInflight(t *testing.T) {
	ctx := t.Context()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &blockingServer{started: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(srv.release) })

	d := New(0)
	pb.RegisterGreeterServer(d.Server, srv)

	go func() { _ = d.Serve(ctx, lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewGreeterClient(conn)

	callErr := make(chan error, 1)
	go func() {
		_, err := client.SayHello(ctx, &pb.HelloRequest{Name: "world"})
		callErr <- err
	}()

	<-srv.started // request is now wedged in the handler

	shutdownCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := d.Shutdown(shutdownCtx); err == nil {
		t.Fatal("expected Shutdown to return an error when the drain exceeds its deadline")
	}

	if err := <-callErr; err == nil {
		t.Error("expected the in-flight request to fail once the server is stopped")
	}
}

// blockingServer holds each SayHello in the handler until release is closed,
// signaling started once a request has arrived. It lets a test drive the
// duplex into a state where a request is genuinely in flight during shutdown.
type blockingServer struct {
	pb.UnimplementedGreeterServer

	started chan struct{}
	release chan struct{}
}

func (s *blockingServer) SayHello(_ context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	s.started <- struct{}{}
	<-s.release
	return &pb.HelloReply{Message: "Hello " + in.GetName()}, nil
}

// server is used to implement helloworld.GreeterServer.
type server struct {
	pb.UnimplementedGreeterServer

	// lastClientID captures the cgclientid from the most recent request.
	lastClientID string
}

// SayHello implements helloworld.GreeterServer
func (s *server) SayHello(ctx context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	log.Printf("Received: %v (%v)", in.GetName(), md)
	if vals := md.Get(clientid.CGClientID); len(vals) > 0 {
		s.lastClientID = vals[0]
	} else {
		s.lastClientID = ""
	}
	return &pb.HelloReply{Message: "Hello " + in.GetName()}, nil
}
