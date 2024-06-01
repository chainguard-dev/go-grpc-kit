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

var (
	// RestoreTraceParentHandler is a server stats.Handler that restores the traceparent
	// stored by the PreserveTraceParentHandler.
	RestoreTraceParentHandler stats.Handler = &restoreTraceParentHandler{}
)

type restoreTraceParentHandler struct{}

// TagRPC implements stats.Handler.
func (r *restoreTraceParentHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		md = metadata.MD{}
	}
	if tp := md.Get(OriginalTraceParentHeader); len(tp) > 0 {
		md.Set(TraceParentHeader, tp...)
	}
	return metadata.NewIncomingContext(ctx, md)
}

// HandleRPC implements stats.Handler.
func (r *restoreTraceParentHandler) HandleRPC(_ context.Context, _ stats.RPCStats) {
	// Do nothing.
}

// TagConn implements stats.Handler.
func (r *restoreTraceParentHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	// Do nothing.
	return ctx
}

// HandleConn implements stats.Handler.
func (r *restoreTraceParentHandler) HandleConn(_ context.Context, _ stats.ConnStats) {
	// Do nothing.
}
