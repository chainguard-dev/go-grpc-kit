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
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/stats"
)

func TestClientStatsHandler(t *testing.T) {
	registerer := prometheus.NewRegistry()
	metrics := newClientTransportMetrics(registerer)
	start := time.Unix(0, 0)
	handler := newClientStatsHandler(metrics, func() time.Time {
		return start.Add(250 * time.Millisecond)
	})
	labels := []string{"test.Service", "Method"}

	ctx := handler.TagRPC(context.Background(), &stats.RPCTagInfo{
		FullMethodName: "/test.Service/Method",
	})
	handler.HandleRPC(ctx, &stats.Begin{Client: true, BeginTime: start})
	if got, want := testutil.ToFloat64(metrics.activeAttempts.WithLabelValues(append(labels, clientAttemptPhasePending)...)), float64(1); got != want {
		t.Fatalf("pending attempts after Begin = %v, want %v", got, want)
	}

	handler.HandleRPC(ctx, &stats.OutHeader{Client: true})
	if got, want := testutil.ToFloat64(metrics.activeAttempts.WithLabelValues(append(labels, clientAttemptPhasePending)...)), float64(0); got != want {
		t.Fatalf("pending attempts after OutHeader = %v, want %v", got, want)
	}
	if got, want := testutil.ToFloat64(metrics.activeAttempts.WithLabelValues(append(labels, clientAttemptPhaseInFlight)...)), float64(1); got != want {
		t.Fatalf("in-flight attempts after OutHeader = %v, want %v", got, want)
	}
	handler.HandleRPC(ctx, &stats.End{Client: true})
	if got, want := testutil.ToFloat64(metrics.activeAttempts.WithLabelValues(append(labels, clientAttemptPhaseInFlight)...)), float64(0); got != want {
		t.Fatalf("in-flight attempts after End = %v, want %v", got, want)
	}

	families, err := registerer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	found := false
	for _, family := range families {
		if family.GetName() != "grpc_client_attempt_dispatch_delay_seconds" {
			continue
		}
		found = true
		got := family.GetMetric()[0].GetHistogram()
		if got.GetSampleCount() != 1 || got.GetSampleSum() != 0.25 {
			t.Fatalf("dispatch-delay histogram = count %d, sum %v; want count 1, sum 0.25", got.GetSampleCount(), got.GetSampleSum())
		}
		break
	}
	if !found {
		t.Fatal("dispatch-delay histogram was not collected")
	}

	failedCtx := handler.TagRPC(context.Background(), &stats.RPCTagInfo{
		FullMethodName: "/test.Service/Method",
	})
	handler.HandleRPC(failedCtx, &stats.Begin{Client: true, BeginTime: start})
	handler.HandleRPC(failedCtx, &stats.End{Client: true})
	if got, want := testutil.ToFloat64(metrics.activeAttempts.WithLabelValues(append(labels, clientAttemptPhasePending)...)), float64(0); got != want {
		t.Fatalf("pending attempts after End without OutHeader = %v, want %v", got, want)
	}
}
