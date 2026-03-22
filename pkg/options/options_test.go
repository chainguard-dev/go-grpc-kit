/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package options

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
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

type testDialableListener struct {
	net.Listener
}

func (l *testDialableListener) Dial() (net.Conn, error) {
	return net.Dial(l.Listener.Addr().Network(), l.Listener.Addr().String())
}
