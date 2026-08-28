/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clientid

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const CGClientID = "cgclientid"
const CGRequestID = "cgrequestid"

var cachedClientID = sync.OnceValue(func() string {
	kService := os.Getenv("K_SERVICE")
	configuredID := os.Getenv("CG_CLIENT_ID")
	if kService != "" || configuredID != "" {
		return resolveClientID(kService, configuredID, "", nil)
	}
	executable, err := os.Executable()
	return resolveClientID("", "", executable, err)
})

func resolveClientID(kService, configuredID, executable string, executableErr error) string {
	if kService != "" {
		return kService
	}
	if configuredID != "" {
		return configuredID
	}
	if executableErr != nil {
		return "unknown"
	}
	return Normalize(executable)
}

// Normalize returns a stable executable name for absolute client ID paths.
// Other client IDs are returned unchanged.
func Normalize(id string) string {
	if !isAbsolutePath(id) {
		return id
	}

	trimmed := strings.TrimRight(id, `/\`)
	if trimmed == "" {
		return id
	}
	if separator := strings.LastIndexAny(trimmed, `/\`); separator >= 0 {
		return trimmed[separator+1:]
	}
	return trimmed
}

func isAbsolutePath(value string) bool {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\\`) {
		return true
	}
	return len(value) >= 3 && isASCIILetter(value[0]) && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

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
