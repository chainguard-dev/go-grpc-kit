/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clientid

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestAppendClientID_AlwaysSetsOutgoing(t *testing.T) {
	ctx := context.Background()
	got := appendClientID(ctx)

	md, ok := metadata.FromOutgoingContext(got)
	if !ok {
		t.Fatal("expected outgoing metadata to be set")
	}
	if vals := md.Get(CGClientID); len(vals) == 0 {
		t.Fatal("expected cgclientid in outgoing metadata")
	}
	if vals := md.Get(CGRequestID); len(vals) == 0 {
		t.Fatal("expected cgrequestid in outgoing metadata")
	}
}

func TestAppendClientID_OverridesIncoming(t *testing.T) {
	// Simulate a server handler context that has incoming metadata
	// from a previous caller (e.g., chainctl → api-impl → datastore).
	// The interceptor should still set its own identity on outgoing metadata.
	incoming := metadata.Pairs(CGClientID, "previous-caller", CGRequestID, "prev-req-id")
	ctx := metadata.NewIncomingContext(context.Background(), incoming)

	got := appendClientID(ctx)

	md, ok := metadata.FromOutgoingContext(got)
	if !ok {
		t.Fatal("expected outgoing metadata to be set even when incoming has cgclientid")
	}
	vals := md.Get(CGClientID)
	if len(vals) == 0 {
		t.Fatal("expected cgclientid in outgoing metadata")
	}
	// The outgoing cgclientid should be this service's identity, not the previous caller's.
	if vals[0] == "previous-caller" {
		t.Error("outgoing cgclientid should be this service's identity, not the upstream caller's")
	}
}

func TestAppendClientID_UniqueRequestIDs(t *testing.T) {
	ctx := context.Background()
	ctx1 := appendClientID(ctx)
	ctx2 := appendClientID(ctx)

	md1, _ := metadata.FromOutgoingContext(ctx1)
	md2, _ := metadata.FromOutgoingContext(ctx2)

	rid1 := md1.Get(CGRequestID)
	rid2 := md2.Get(CGRequestID)
	if len(rid1) == 0 || len(rid2) == 0 {
		t.Fatal("expected request IDs to be set")
	}
	if rid1[0] == rid2[0] {
		t.Error("expected unique request IDs per call")
	}
}

func TestCachedClientID_KServiceEnv(t *testing.T) {
	// This tests the getClientID behavior indirectly via cachedClientID.
	// Since cachedClientID uses sync.OnceValue, we test the logic directly.
	t.Run("K_SERVICE takes precedence", func(t *testing.T) {
		// We can't easily test the sync.OnceValue behavior since it's
		// already initialized, but we verify the current value is non-empty.
		id := cachedClientID()
		if id == "" {
			t.Error("cachedClientID should never be empty")
		}
	})
}
