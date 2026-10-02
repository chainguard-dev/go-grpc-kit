/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"

	"chainguard.dev/go-grpc-kit/pkg/interceptors/clientid"
)

func TestLabelsFromContext(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "missing", want: "unknown"},
		{name: "Unix executable path", id: "/tmp/random/chainctl", want: "chainctl"},
		{name: "Windows executable path", id: `C:\Temp\random\chainctl.exe`, want: "chainctl.exe"},
		{name: "service ID", id: "issuer", want: "issuer"},
		{name: "relative service ID", id: "prober/issuer", want: "prober/issuer"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.id != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(clientid.CGClientID, test.id))
			}
			if got := labelsFromContext(ctx)[clientid.CGClientID]; got != test.want {
				t.Errorf("labelsFromContext() = %q, want %q", got, test.want)
			}
		})
	}
}

// brokenCollector is a prometheus.Collector that always returns an error
// during collection, simulating the OTel prometheus exporter behavior
// that produces metrics with "no value, sum, or explicit bounds".
type brokenCollector struct{}

func (brokenCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("broken_metric", "always fails", nil, nil)
}

func (brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(
		prometheus.NewDesc("broken_metric", "always fails", nil, nil),
		fmt.Errorf("no value, sum, or explicit bounds"),
	)
}

func TestMetricsEndpointContinuesOnCollectorError(t *testing.T) {
	// Register a collector that always errors, simulating the OTel
	// prometheus exporter behavior that caused HTTP 500s.
	prometheus.MustRegister(brokenCollector{})
	t.Cleanup(func() {
		prometheus.Unregister(brokenCollector{})
	})

	// Verify the broken collector WOULD cause HTTP 500 with the default
	// handler (no ContinueOnError). This is the regression baseline.
	defaultHandler := httptest.NewServer(promhttp.Handler())
	t.Cleanup(defaultHandler.Close)

	resp, err := http.Get(defaultHandler.URL)
	if err != nil {
		t.Fatalf("GET default handler: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("default handler: got %d, want %d — broken collector did not trigger 500",
			resp.StatusCode, http.StatusInternalServerError)
	}

	// Verify getServer() returns 200 despite the same broken collector,
	// proving ContinueOnError is working.
	s := getServer(false)
	ts := httptest.NewServer(s.Handler)
	t.Cleanup(ts.Close)

	resp, err = http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestMetricsEndpointServesHealthyMetrics(t *testing.T) {
	s := getServer(false)
	ts := httptest.NewServer(s.Handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestGetServerWithPprof(t *testing.T) {
	s := getServer(true)
	ts := httptest.NewServer(s.Handler)
	t.Cleanup(ts.Close)

	for _, path := range []string{
		"/metrics",
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/symbol",
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status: got %d, want %d", path, resp.StatusCode, http.StatusOK)
		}
	}
}

func TestRegisterAndServeCreatesOnlyLiveSeries(t *testing.T) {
	server := grpc.NewServer()
	healthpb.RegisterHealthServer(server, health.NewServer())

	// The /metrics goroutine exits the process if its listener closes, so the
	// listener stays open until the test binary exits.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}
	RegisterAndServe(server, listener, false)

	families := []string{
		"grpc_server_started_total",
		"grpc_server_handled_total",
		"grpc_server_msg_received_total",
		"grpc_server_msg_sent_total",
		"grpc_server_handling_seconds",
	}
	if got := testutil.CollectAndCount(state().serverMetrics, families...); got != 0 {
		t.Errorf("series before any RPC: got %d, want 0", got)
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(clientid.CGClientID, "issuer"))
	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}
	handler := func(context.Context, any) (any, error) { return &healthpb.HealthCheckResponse{}, nil }
	if _, err := UnaryServerInterceptor()(ctx, &healthpb.HealthCheckRequest{}, info, handler); err != nil {
		t.Fatalf("interceptor() = %v", err)
	}

	for _, family := range families {
		if got := testutil.CollectAndCount(state().serverMetrics, family); got != 1 {
			t.Errorf("%s series after one RPC: got %d, want 1", family, got)
		}
	}

	want := `
# HELP grpc_server_handled_total Total number of RPCs completed on the server, regardless of success or failure.
# TYPE grpc_server_handled_total counter
grpc_server_handled_total{cgclientid="issuer",grpc_code="OK",grpc_method="Check",grpc_service="grpc.health.v1.Health",grpc_type="unary"} 1
`
	if err := testutil.CollectAndCompare(state().serverMetrics, strings.NewReader(want), "grpc_server_handled_total"); err != nil {
		t.Error(err)
	}
}
