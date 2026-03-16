/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package trace

import "github.com/prometheus/client_golang/prometheus"

var (
	traceparentPreserved = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "grpc_traceparent_preserved_total",
		Help: "Number of outgoing RPCs where the original traceparent was preserved.",
	})
	traceparentRestoreAttempted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "grpc_traceparent_restore_attempted_total",
		Help: "Number of incoming RPCs where an original-traceparent header was found.",
	})
	traceparentRestored = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "grpc_traceparent_restored_total",
		Help: "Number of incoming RPCs where the traceparent was actually replaced (Cloud Run lost the original).",
	})
)

func init() {
	prometheus.MustRegister(traceparentPreserved, traceparentRestoreAttempted, traceparentRestored)
}
