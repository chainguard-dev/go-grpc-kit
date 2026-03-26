/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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
