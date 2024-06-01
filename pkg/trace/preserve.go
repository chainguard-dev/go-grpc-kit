/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package trace

import (
	"context"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
)

const (
	OriginalTraceParentHeader string = "original-traceparent"
	TraceParentHeader         string = "traceparent"
)

var (
	// PreserveTraceParentHandler is a client stats.Handler that preserves the original
	// traceparent header in the outgoing context, with a different header name.
	//
	// This is useful when the next hop in the request chain (like Cloud Run) may lose span
	// information, and become an unreliable span. In those cases, we just use the original
	// traceparent header to associate child spans directly with the outgoing span here.
	PreserveTraceParentHandler stats.Handler = &preserveTraceParentHandler{}
)

type preserveTraceParentHandler struct{}

// TagRPC implements stats.Handler interface.
func (*preserveTraceParentHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.MD{}
	}
	if tp := md.Get(TraceParentHeader); len(tp) > 0 {
		md.Set(OriginalTraceParentHeader, tp...)
	}
	return metadata.NewOutgoingContext(ctx, md)
}

// HandleRPC implments stats.Handler interface.
func (*preserveTraceParentHandler) HandleRPC(context.Context, stats.RPCStats) {
	// Do nothing
}

// TagConn implements stats.Handler interface.
func (*preserveTraceParentHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn processes the Conn stats.
func (*preserveTraceParentHandler) HandleConn(context.Context, stats.ConnStats) {
	// Do nothing
}
