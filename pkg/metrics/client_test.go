/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/stats"
)

func TestClientStatsHandler(t *testing.T) {
	start := time.Unix(0, 0)
	for _, test := range []struct {
		name      string
		tagged    bool
		now       time.Time
		events    []stats.RPCStats
		wantCount uint64
		wantSum   float64
	}{
		{
			name:   "dispatch delay",
			tagged: true,
			now:    start.Add(250 * time.Millisecond),
			events: []stats.RPCStats{
				&stats.Begin{Client: true, BeginTime: start},
				&stats.OutHeader{Client: true},
			},
			wantCount: 1,
			wantSum:   0.25,
		},
		{
			name:   "clock moves backward",
			tagged: true,
			now:    start,
			events: []stats.RPCStats{
				&stats.Begin{Client: true, BeginTime: start.Add(time.Second)},
				&stats.OutHeader{Client: true},
			},
		},
		{
			name: "untagged context",
			now:  start.Add(250 * time.Millisecond),
			events: []stats.RPCStats{
				&stats.Begin{Client: true, BeginTime: start},
				&stats.OutHeader{Client: true},
			},
		},
		{
			name:   "header without begin",
			tagged: true,
			now:    start.Add(250 * time.Millisecond),
			events: []stats.RPCStats{
				&stats.OutHeader{Client: true},
			},
		},
		{
			name:   "ends before dispatch",
			tagged: true,
			now:    start.Add(250 * time.Millisecond),
			events: []stats.RPCStats{
				&stats.Begin{Client: true, BeginTime: start},
				&stats.End{Client: true},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			registerer := prometheus.NewRegistry()
			metrics := newClientTransportMetrics(registerer)
			handler := newClientStatsHandler(metrics, func() time.Time { return test.now })
			ctx := context.Background()
			if test.tagged {
				ctx = handler.TagRPC(ctx, &stats.RPCTagInfo{FullMethodName: "/test.Service/Method"})
			}
			for _, event := range test.events {
				handler.HandleRPC(ctx, event)
			}

			gotCount, gotSum := dispatchHistogram(t, registerer)
			if gotCount != test.wantCount || gotSum != test.wantSum {
				t.Fatalf("dispatch-delay histogram = count %d, sum %v; want count %d, sum %v", gotCount, gotSum, test.wantCount, test.wantSum)
			}
		})
	}
}

func TestSplitGRPCMethod(t *testing.T) {
	for _, test := range []struct {
		name        string
		fullMethod  string
		wantService string
		wantMethod  string
	}{
		{
			name:        "valid",
			fullMethod:  "/test.Service/Method",
			wantService: "test.Service",
			wantMethod:  "Method",
		},
		{
			name:        "missing separator",
			fullMethod:  "Method",
			wantService: "unknown",
			wantMethod:  "unknown",
		},
		{
			name:        "missing method",
			fullMethod:  "/test.Service/",
			wantService: "unknown",
			wantMethod:  "unknown",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, method := splitGRPCMethod(test.fullMethod)
			if service != test.wantService || method != test.wantMethod {
				t.Fatalf("splitGRPCMethod(%q) = %q, %q; want %q, %q", test.fullMethod, service, method, test.wantService, test.wantMethod)
			}
		})
	}
}

func dispatchHistogram(t *testing.T, registerer *prometheus.Registry) (uint64, float64) {
	t.Helper()
	families, err := registerer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "grpc_client_attempt_dispatch_delay_seconds" {
			continue
		}
		if got, want := len(family.GetMetric()), 1; got != want {
			t.Fatalf("dispatch-delay series = %d; want %d", got, want)
		}
		metric := family.GetMetric()[0]
		labels := map[string]string{}
		for _, label := range metric.GetLabel() {
			labels[label.GetName()] = label.GetValue()
		}
		if got, want := labels["grpc_service"], "test.Service"; got != want {
			t.Fatalf("grpc_service label = %q; want %q", got, want)
		}
		if got, want := labels["grpc_method"], "Method"; got != want {
			t.Fatalf("grpc_method label = %q; want %q", got, want)
		}
		got := metric.GetHistogram()
		return got.GetSampleCount(), got.GetSampleSum()
	}
	return 0, 0
}
