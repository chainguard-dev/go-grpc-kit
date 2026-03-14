/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package trace

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
)

func TestPreserveIncrementsMetric(t *testing.T) {
	before := testutil.ToFloat64(traceparentPreserved)

	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		TraceParentHeader, "00-abc123-def456-01",
	))
	PreserveTraceParentHandler.TagRPC(ctx, &stats.RPCTagInfo{})

	after := testutil.ToFloat64(traceparentPreserved)
	if after-before != 1 {
		t.Errorf("expected preserved counter to increment by 1, got %v", after-before)
	}
}

func TestPreserveNoMetricWithoutTraceparent(t *testing.T) {
	before := testutil.ToFloat64(traceparentPreserved)

	ctx := metadata.NewOutgoingContext(context.Background(), metadata.MD{})
	PreserveTraceParentHandler.TagRPC(ctx, &stats.RPCTagInfo{})

	after := testutil.ToFloat64(traceparentPreserved)
	if after != before {
		t.Errorf("expected preserved counter unchanged, got delta %v", after-before)
	}
}

func TestRestoreMetrics_TraceparentChanged(t *testing.T) {
	attemptBefore := testutil.ToFloat64(traceparentRestoreAttempted)
	restoredBefore := testutil.ToFloat64(traceparentRestored)

	// Simulate Cloud Run replacing the traceparent with a different one.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		OriginalTraceParentHeader, "00-original-span-01",
		TraceParentHeader, "00-cloudrun-replaced-01",
	))
	newCtx := RestoreTraceParentHandler.TagRPC(ctx, &stats.RPCTagInfo{})

	attemptAfter := testutil.ToFloat64(traceparentRestoreAttempted)
	restoredAfter := testutil.ToFloat64(traceparentRestored)

	if attemptAfter-attemptBefore != 1 {
		t.Errorf("expected attempt counter +1, got %v", attemptAfter-attemptBefore)
	}
	if restoredAfter-restoredBefore != 1 {
		t.Errorf("expected restored counter +1, got %v", restoredAfter-restoredBefore)
	}

	// Verify traceparent was actually restored.
	md, _ := metadata.FromIncomingContext(newCtx)
	if tp := md.Get(TraceParentHeader); len(tp) == 0 || tp[0] != "00-original-span-01" {
		t.Errorf("expected traceparent restored, got %v", tp)
	}
}

func TestRestoreMetrics_TraceparentUnchanged(t *testing.T) {
	restoredBefore := testutil.ToFloat64(traceparentRestored)

	// Simulate Cloud Run preserving the traceparent correctly.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		OriginalTraceParentHeader, "00-same-span-01",
		TraceParentHeader, "00-same-span-01",
	))
	RestoreTraceParentHandler.TagRPC(ctx, &stats.RPCTagInfo{})

	restoredAfter := testutil.ToFloat64(traceparentRestored)
	if restoredAfter != restoredBefore {
		t.Errorf("expected restored counter unchanged when traceparent matches, got delta %v", restoredAfter-restoredBefore)
	}
}

func TestRestoreMetrics_NoOriginalHeader(t *testing.T) {
	attemptBefore := testutil.ToFloat64(traceparentRestoreAttempted)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		TraceParentHeader, "00-some-span-01",
	))
	RestoreTraceParentHandler.TagRPC(ctx, &stats.RPCTagInfo{})

	attemptAfter := testutil.ToFloat64(traceparentRestoreAttempted)
	if attemptAfter != attemptBefore {
		t.Errorf("expected attempt counter unchanged without original header, got delta %v", attemptAfter-attemptBefore)
	}
}
