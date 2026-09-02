/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package metrics provides Prometheus metrics for gRPC transport behavior.
package metrics

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/stats"
)

type clientTransportMetrics struct {
	dispatchDelay *prometheus.HistogramVec
}

var clientMetrics = sync.OnceValue(func() *clientTransportMetrics {
	return newClientTransportMetrics(prometheus.DefaultRegisterer)
})

func newClientTransportMetrics(registerer prometheus.Registerer) *clientTransportMetrics {
	factory := promauto.With(registerer)

	return &clientTransportMetrics{
		dispatchDelay: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "grpc_client_attempt_dispatch_delay_seconds",
			Help:    "Time from starting a gRPC client attempt until it is dispatched to a transport.",
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5, 1, 2.5, 5, 10, 25, 60},
		}, []string{"grpc_service", "grpc_method"}),
	}
}

// ClientStatsHandler returns a stats handler that records how long client
// attempts wait for a transport. Install it with grpc.WithStatsHandler.
//
// Each attempt, including retries, is measured separately. Attempts that end
// before dispatch do not contribute to the dispatch delay histogram.
func ClientStatsHandler() stats.Handler {
	return newClientStatsHandler(clientMetrics(), time.Now)
}

type clientRPCState struct {
	beginTime time.Time
	service   string
	method    string
}

type clientStatsHandler struct {
	metrics *clientTransportMetrics
	now     func() time.Time
}

func newClientStatsHandler(metrics *clientTransportMetrics, now func() time.Time) stats.Handler {
	return &clientStatsHandler{
		metrics: metrics,
		now:     now,
	}
}

type clientRPCStateKey struct{}

func (*clientStatsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	service, method := splitGRPCMethod(info.FullMethodName)
	return context.WithValue(ctx, clientRPCStateKey{}, &clientRPCState{
		service: service,
		method:  method,
	})
}

func (h *clientStatsHandler) HandleRPC(ctx context.Context, event stats.RPCStats) {
	state, ok := ctx.Value(clientRPCStateKey{}).(*clientRPCState)
	if !ok {
		return
	}

	switch event := event.(type) {
	case *stats.Begin:
		state.beginTime = event.BeginTime
	case *stats.OutHeader:
		if state.beginTime.IsZero() {
			return
		}
		duration := h.now().Sub(state.beginTime)
		if duration < 0 {
			return
		}
		h.metrics.dispatchDelay.WithLabelValues(state.service, state.method).Observe(duration.Seconds())
	}
}

func (*clientStatsHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (*clientStatsHandler) HandleConn(context.Context, stats.ConnStats) {}

func splitGRPCMethod(fullMethod string) (string, string) {
	trimmed := strings.TrimPrefix(fullMethod, "/")
	service, method, ok := strings.Cut(trimmed, "/")
	if !ok || service == "" || method == "" {
		return "unknown", "unknown"
	}
	return service, method
}
