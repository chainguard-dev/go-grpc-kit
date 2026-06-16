/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package options

import (
	"context"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
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

func TestGRPCOptions_HTTP(t *testing.T) {
	u, _ := url.Parse("http://example.com")
	addr, opts := GRPCOptions(*u)
	if addr != "example.com:80" {
		t.Errorf("expected example.com:80, got %s", addr)
	}
	if len(opts) == 0 {
		t.Error("expected non-empty dial options")
	}
}

func TestGRPCOptions_HTTPWithPort(t *testing.T) {
	u, _ := url.Parse("http://example.com:9090")
	addr, opts := GRPCOptions(*u)
	if addr != "example.com:9090" {
		t.Errorf("expected example.com:9090, got %s", addr)
	}
	if len(opts) == 0 {
		t.Error("expected non-empty dial options")
	}
}

func TestGRPCOptions_HTTPS(t *testing.T) {
	u, _ := url.Parse("https://example.com")
	addr, opts := GRPCOptions(*u)
	if addr != "example.com:443" {
		t.Errorf("expected example.com:443, got %s", addr)
	}
	if len(opts) == 0 {
		t.Error("expected non-empty dial options")
	}
}

func TestGRPCOptions_HTTPSWithPort(t *testing.T) {
	u, _ := url.Parse("https://example.com:8443")
	addr, opts := GRPCOptions(*u)
	if addr != "example.com:8443" {
		t.Errorf("expected example.com:8443, got %s", addr)
	}
	if len(opts) == 0 {
		t.Error("expected non-empty dial options")
	}
}

func TestGRPCOptions_Unix(t *testing.T) {
	// The gRPC unix resolver requires an empty authority and reads the socket
	// from the path (or opaque) component, so exercise each of the forms it
	// accepts and confirm the returned target round-trips to an empty host.
	cases := []struct {
		name   string
		target string
	}{
		{"absolute with authority", "unix:///tmp/example.sock"},
		{"absolute without authority", "unix:/tmp/example.sock"},
		{"relative opaque", "unix:example.sock"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.target)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.target, err)
			}

			addr, opts := GRPCOptions(*u)
			if addr != tc.target {
				t.Errorf("addr = %q, want %q", addr, tc.target)
			}
			if len(opts) == 0 {
				t.Error("expected non-empty dial options")
			}

			parsed, err := url.Parse(addr)
			if err != nil {
				t.Fatalf("re-parse %q: %v", addr, err)
			}
			if parsed.Host != "" {
				t.Errorf("target host = %q, want empty so the unix resolver accepts it", parsed.Host)
			}
		})
	}
}

func TestGRPCOptions_TestListener(t *testing.T) {
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	dl := &testDialableListener{Listener: lis}
	scheme := RegisterListenerForTest(dl)
	defer UnregisterTestListener(scheme)

	u, _ := url.Parse(scheme + "://test")
	addr, opts := GRPCOptions(*u)
	if addr != scheme {
		t.Errorf("expected %s, got %s", scheme, addr)
	}
	if len(opts) == 0 {
		t.Error("expected non-empty dial options")
	}
}

func TestGRPCOptions_UnknownSchemePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for unknown scheme")
		}
	}()
	u, _ := url.Parse("unknown://example.com")
	GRPCOptions(*u)
}

func TestGRPCDialOptions_ReturnsNonEmpty(t *testing.T) {
	opts := GRPCDialOptions()
	if len(opts) == 0 {
		t.Error("expected non-empty dial options")
	}
}

func TestLoopbackDialOptions_ReturnsNonEmpty(t *testing.T) {
	opts := LoopbackDialOptions()
	if len(opts) == 0 {
		t.Error("expected non-empty dial options")
	}
}

func TestClientOptions_ReturnsNonEmpty(t *testing.T) {
	opts := ClientOptions()
	if len(opts) == 0 {
		t.Error("expected non-empty client options")
	}
}

func TestKeepaliveDialOption(t *testing.T) {
	if KeepaliveDialOption() == nil {
		t.Error("expected a non-nil keepalive dial option")
	}
}

func TestDialReady(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	srv := grpc.NewServer()
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	u := url.URL{Scheme: "http", Host: lis.Addr().String()}

	conn, err := DialReady(context.Background(), u, time.Second)
	if err != nil {
		t.Fatalf("DialReady: %v", err)
	}
	defer conn.Close()

	if state := conn.GetState(); state != connectivity.Ready {
		t.Errorf("expected connection to be %s, got %s", connectivity.Ready, state)
	}
}

func TestDialReady_UnreachableFails(t *testing.T) {
	// Reserve a port, then close it so the dial is refused.
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()

	u := url.URL{Scheme: "http", Host: addr}

	conn, err := DialReady(context.Background(), u, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("expected DialReady to fail for an unreachable target")
	}
}

type testDialableListener struct {
	net.Listener
}

func (l *testDialableListener) Dial() (net.Conn, error) {
	return net.Dial(l.Listener.Addr().Network(), l.Listener.Addr().String())
}
