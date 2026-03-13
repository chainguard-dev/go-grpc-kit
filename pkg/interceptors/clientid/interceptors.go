/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clientid

import (
	"context"
	"os"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const CGClientID = "cgclientid"
const CGRequestID = "cgrequestid"

var cachedClientID = sync.OnceValue(func() string {
	// Prefer K_SERVICE (Cloud Run service name) for descriptive labels.
	if svc := os.Getenv("K_SERVICE"); svc != "" {
		return svc
	}
	// Prefer CG_CLIENT_ID for explicit override.
	if id := os.Getenv("CG_CLIENT_ID"); id != "" {
		return id
	}
	e, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	return e
})

func appendClientID(ctx context.Context) context.Context {
	// Always set this service's identity on outgoing calls so the
	// downstream server knows its immediate caller.
	return metadata.AppendToOutgoingContext(ctx,
		CGClientID, cachedClientID(),
		CGRequestID, uuid.New().String(),
	)
}

func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		nc := appendClientID(ctx)
		// Make the call
		return invoker(nc, method, req, reply, cc, opts...)
	}
}

func StreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		nc := appendClientID(ctx)
		// Make the call
		return streamer(nc, desc, cc, method, opts...)
	}
}
