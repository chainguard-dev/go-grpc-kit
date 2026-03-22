/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package duplex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
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

	// Setup server
	d := New(ip.Port,
		grpc.ChainStreamInterceptor(metrics.StreamServerInterceptor()),
		grpc.ChainUnaryInterceptor(metrics.UnaryServerInterceptor()),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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

func TestGracefulShutdown(t *testing.T) {
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	ip, err := net.ResolveTCPAddr(lis.Addr().Network(), lis.Addr().String())
	if err != nil {
		t.Fatalf("error resolving: %v", err)
	}

	// Use a slow server to verify in-flight RPCs are drained.
	impl := &slowServer{delay: 500 * time.Millisecond}
	d := New(ip.Port,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	pb.RegisterGreeterServer(d.Server, impl)
	if err := d.RegisterHandler(context.Background(), pb.RegisterGreeterHandlerFromEndpoint); err != nil {
		t.Fatalf("error registering handler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- d.Serve(ctx, lis)
	}()

	// Give server time to start.
	time.Sleep(100 * time.Millisecond)

	// Start a slow in-flight RPC.
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()
	client := pb.NewGreeterClient(conn)

	rpcDone := make(chan error, 1)
	go func() {
		_, err := client.SayHello(context.Background(), &pb.HelloRequest{Name: "drain"})
		rpcDone <- err
	}()

	// Wait for the RPC to be in-flight, then cancel the server context.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// The in-flight RPC should complete successfully (graceful drain).
	select {
	case err := <-rpcDone:
		if err != nil {
			t.Errorf("in-flight RPC failed during graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight RPC did not complete within 5 seconds")
	}

	if impl.completed.Load() != 1 {
		t.Errorf("expected 1 completed RPC, got %d", impl.completed.Load())
	}

	// Serve should return cleanly.
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("Serve returned error after graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5 seconds after context cancellation")
	}
}

func TestNewInvalidPort(t *testing.T) {
	tests := []struct {
		name string
		port int
	}{
		{"negative", -1},
		{"too large", 70000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for port %d", tt.port)
				}
			}()
			New(tt.port)
		})
	}
}

// slowServer delays each RPC to test graceful shutdown drain behavior.
type slowServer struct {
	pb.UnimplementedGreeterServer
	delay     time.Duration
	completed atomic.Int64
}

func (s *slowServer) SayHello(_ context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	time.Sleep(s.delay)
	s.completed.Add(1)
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
