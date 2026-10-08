/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

var liveSeriesRuns atomic.Int64

func TestRegisterAndServeCreatesOnlyLiveSeries(t *testing.T) {
	server := grpc.NewServer()
	healthpb.RegisterHealthServer(server, health.NewServer())

	families := []string{
		"grpc_server_started_total",
		"grpc_server_handled_total",
		"grpc_server_msg_received_total",
		"grpc_server_msg_sent_total",
		"grpc_server_handling_seconds",
	}
	// The collector is process-global, so counts are relative to earlier runs
	// and each run uses its own client ID.
	counts := func() map[string]int {
		got := make(map[string]int, len(families))
		for _, family := range families {
			got[family] = testutil.CollectAndCount(state().serverMetrics, family)
		}
		return got
	}
	initial := counts()

	// The /metrics goroutine exits the process if its listener closes, so the
	// listener stays open until the test binary exits.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}
	RegisterAndServe(server, listener, false)

	registered := counts()
	for _, family := range families {
		if got := registered[family] - initial[family]; got != 0 {
			t.Errorf("%s series added by RegisterAndServe: got %d, want 0", family, got)
		}
	}

	id := fmt.Sprintf("issuer-%d", liveSeriesRuns.Add(1))
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(clientid.CGClientID, id))
	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}
	handler := func(context.Context, any) (any, error) { return &healthpb.HealthCheckResponse{}, nil }
	if _, err := UnaryServerInterceptor()(ctx, &healthpb.HealthCheckRequest{}, info, handler); err != nil {
		t.Fatalf("interceptor() = %v", err)
	}

	served := counts()
	for _, family := range families {
		if got := served[family] - registered[family]; got != 1 {
			t.Errorf("%s series added by one RPC: got %d, want 1", family, got)
		}
	}

	want := map[string]string{
		"cgclientid":   id,
		"grpc_code":    "OK",
		"grpc_method":  "Check",
		"grpc_service": "grpc.health.v1.Health",
		"grpc_type":    "unary",
	}
	if got := handledSeries(t, id); !maps.Equal(got, want) {
		t.Errorf("grpc_server_handled_total labels: got %v, want %v", got, want)
	}
}

// handledSeries returns the labels of the single grpc_server_handled_total
// series for the client ID, failing unless its value is 1.
func handledSeries(t *testing.T, id string) map[string]string {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(state().serverMetrics); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() = %v", err)
	}
	var found map[string]string
	for _, mf := range mfs {
		if mf.GetName() != "grpc_server_handled_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["cgclientid"] != id {
				continue
			}
			if found != nil {
				t.Fatalf("more than one grpc_server_handled_total series for %q", id)
			}
			if got := m.GetCounter().GetValue(); got != 1 {
				t.Errorf("grpc_server_handled_total value: got %v, want 1", got)
			}
			found = labels
		}
	}
	return found
}
