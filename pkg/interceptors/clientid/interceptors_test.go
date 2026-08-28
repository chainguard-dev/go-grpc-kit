/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clientid

import (
	"context"
	"errors"
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

func TestResolveClientID(t *testing.T) {
	tests := []struct {
		name          string
		kService      string
		configuredID  string
		executable    string
		executableErr error
		want          string
	}{
		{
			name:         "K_SERVICE takes precedence",
			kService:     "oidc",
			configuredID: "chainctl",
			executable:   "/tmp/random/chainctl",
			want:         "oidc",
		},
		{
			name:         "CG_CLIENT_ID takes precedence over executable",
			configuredID: "explicit/client",
			executable:   "/tmp/random/chainctl",
			want:         "explicit/client",
		},
		{
			name:       "executable fallback is normalized",
			executable: "/tmp/random/chainctl",
			want:       "chainctl",
		},
		{
			name:          "executable lookup failure",
			executableErr: errors.New("lookup failed"),
			want:          "unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveClientID(test.kService, test.configuredID, test.executable, test.executableErr); got != test.want {
				t.Errorf("resolveClientID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "Unix path", id: "/tmp/random/chainctl", want: "chainctl"},
		{name: "Windows drive path", id: `C:\Temp\random\chainctl.exe`, want: "chainctl.exe"},
		{name: "Windows UNC path", id: `\\server\share\chainctl.exe`, want: "chainctl.exe"},
		{name: "relative path", id: "tmp/random/chainctl", want: "tmp/random/chainctl"},
		{name: "service ID", id: "prober/oidc", want: "prober/oidc"},
		{name: "empty", id: "", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Normalize(test.id); got != test.want {
				t.Errorf("Normalize(%q) = %q, want %q", test.id, got, test.want)
			}
		})
	}
}
