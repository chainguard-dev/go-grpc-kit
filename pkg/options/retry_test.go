/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package options

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// decodedRetryConfig is the part of a retry service config the tests read
// back.
type decodedRetryConfig struct {
	MethodConfig []struct {
		Name []struct {
			Service string `json:"service"`
			Method  string `json:"method"`
		} `json:"name"`
		RetryPolicy struct {
			MaxAttempts          int          `json:"maxAttempts"`
			InitialBackoff       string       `json:"initialBackoff"`
			MaxBackoff           string       `json:"maxBackoff"`
			BackoffMultiplier    float64      `json:"backoffMultiplier"`
			RetryableStatusCodes []codes.Code `json:"retryableStatusCodes"`
		} `json:"retryPolicy"`
	} `json:"methodConfig"`
	RetryThrottling *struct {
		MaxTokens  int     `json:"maxTokens"`
		TokenRatio float64 `json:"tokenRatio"`
	} `json:"retryThrottling"`
}

// decodedPolicy flattens a decoded config's retry policy and throttling
// into one comparable value; it is zero when the config retries nothing.
type decodedPolicy struct {
	MaxAttempts    int
	InitialBackoff string
	MaxBackoff     string
	Multiplier     float64
	Codes          []codes.Code
	MaxTokens      int
	TokenRatio     float64
}

func (c decodedRetryConfig) policy() decodedPolicy {
	var p decodedPolicy
	if len(c.MethodConfig) > 0 {
		rp := c.MethodConfig[0].RetryPolicy
		p = decodedPolicy{MaxAttempts: rp.MaxAttempts, InitialBackoff: rp.InitialBackoff, MaxBackoff: rp.MaxBackoff, Multiplier: rp.BackoffMultiplier, Codes: rp.RetryableStatusCodes}
	}
	if th := c.RetryThrottling; th != nil {
		p.MaxTokens, p.TokenRatio = th.MaxTokens, th.TokenRatio
	}
	return p
}

// retriedMethods lists the "service/method" names a decoded config retries.
func (c decodedRetryConfig) retriedMethods() []string {
	var out []string
	for _, mc := range c.MethodConfig {
		for _, n := range mc.Name {
			out = append(out, n.Service+"/"+n.Method)
		}
	}
	return out
}

// TestIdempotentRetryServiceConfig pins the selection and the policy for any
// set of services: a method its proto marks NO_SIDE_EFFECTS or IDEMPOTENT is
// retried under the policy, one marked IDEMPOTENCY_UNKNOWN or not marked at
// all is not, and a set with nothing marked yields an empty config, without
// throttling. DefaultRetryPolicy is pinned the same way, since changing it
// changes every caller that takes it. gRPC must accept the config either way,
// or a Dial carrying it would fail.
func TestIdempotentRetryServiceConfig(t *testing.T) {
	method := func(name string, opts *descriptorpb.MethodOptions) *descriptorpb.MethodDescriptorProto {
		return &descriptorpb.MethodDescriptorProto{Name: new(name), InputType: new(".test.v1.Msg"), OutputType: new(".test.v1.Msg"), Options: opts}
	}
	level := func(l descriptorpb.MethodOptions_IdempotencyLevel) *descriptorpb.MethodOptions {
		return &descriptorpb.MethodOptions{IdempotencyLevel: l.Enum()}
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        new("retry_test.proto"),
		Package:     new("test.v1"),
		Syntax:      new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: new("Msg")}},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{Name: new("Store"), Method: []*descriptorpb.MethodDescriptorProto{
				method("Get", level(descriptorpb.MethodOptions_NO_SIDE_EFFECTS)),
				method("Put", level(descriptorpb.MethodOptions_IDEMPOTENT)),
				method("Post", level(descriptorpb.MethodOptions_IDEMPOTENCY_UNKNOWN)),
				method("Patch", nil),
			}},
			{Name: new("Index"), Method: []*descriptorpb.MethodDescriptorProto{
				method("List", level(descriptorpb.MethodOptions_NO_SIDE_EFFECTS)),
			}},
			{Name: new("Writer"), Method: []*descriptorpb.MethodDescriptorProto{
				method("Write", nil),
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("building the test descriptor: %v", err)
	}
	svcs := file.Services()
	custom := RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: 250 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
		Multiplier:     1.5,
		Codes:          []codes.Code{codes.Unavailable, codes.ResourceExhausted},
		MaxTokens:      10,
		TokenRatio:     0.1,
	}

	tests := []struct {
		name       string
		policy     RetryPolicy
		svcs       []protoreflect.ServiceDescriptor
		want       []string
		wantPolicy decodedPolicy
	}{{
		name:       "marked methods across services",
		policy:     custom,
		svcs:       []protoreflect.ServiceDescriptor{svcs.ByName("Store"), svcs.ByName("Index"), svcs.ByName("Writer")},
		want:       []string{"test.v1.Store/Get", "test.v1.Store/Put", "test.v1.Index/List"},
		wantPolicy: decodedPolicy{MaxAttempts: 3, InitialBackoff: "0.25s", MaxBackoff: "2s", Multiplier: 1.5, Codes: []codes.Code{codes.Unavailable, codes.ResourceExhausted}, MaxTokens: 10, TokenRatio: 0.1},
	}, {
		name:   "nothing marked",
		policy: custom,
		svcs:   []protoreflect.ServiceDescriptor{svcs.ByName("Writer")},
	}, {
		name:       "the default policy",
		policy:     DefaultRetryPolicy(),
		svcs:       []protoreflect.ServiceDescriptor{svcs.ByName("Index")},
		want:       []string{"test.v1.Index/List"},
		wantPolicy: decodedPolicy{MaxAttempts: 5, InitialBackoff: "1s", MaxBackoff: "10s", Multiplier: 2, Codes: []codes.Code{codes.Unavailable}, MaxTokens: 10, TokenRatio: 0.1},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := IdempotentRetryServiceConfig(tc.policy, tc.svcs...)
			if err != nil {
				t.Fatalf("IdempotentRetryServiceConfig: %v", err)
			}
			conn, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultServiceConfig(raw))
			if err != nil {
				t.Fatalf("gRPC refused the config %s: %v", raw, err)
			}
			_ = conn.Close()

			var cfg decodedRetryConfig
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			if diff := cmp.Diff(tc.want, cfg.retriedMethods()); diff != "" {
				t.Errorf("retried methods (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantPolicy, cfg.policy()); diff != "" {
				t.Errorf("retry policy (-want +got):\n%s", diff)
			}
		})
	}
}

// flakyHealth answers UNAVAILABLE to its first Check and SERVING after,
// counting the calls it took.
type flakyHealth struct {
	healthpb.UnimplementedHealthServer
	checks atomic.Int32
}

func (f *flakyHealth) Check(context.Context, *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if f.checks.Add(1) == 1 {
		return nil, status.Error(codes.Unavailable, "instance replaced")
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

// TestIdempotentRetryDialOptions pins that the dial options alone make a
// marked method retry: a Check that fails UNAVAILABLE once lands on its
// second try. The request is past gRPC's 256 KiB default replay buffer, so
// the call is retried only if the options raise it too.
func TestIdempotentRetryDialOptions(t *testing.T) {
	// Health's real descriptor marks nothing; this one marks Check under the
	// same names, which is all a service config matches on.
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        new("health_retry_test.proto"),
		Package:     new("grpc.health.v1"),
		Syntax:      new("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: new("Msg")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: new("Health"), Method: []*descriptorpb.MethodDescriptorProto{{
			Name: new("Check"), InputType: new(".grpc.health.v1.Msg"), OutputType: new(".grpc.health.v1.Msg"),
			Options: &descriptorpb.MethodOptions{IdempotencyLevel: descriptorpb.MethodOptions_NO_SIDE_EFFECTS.Enum()},
		}}}},
	}, nil)
	if err != nil {
		t.Fatalf("building the test descriptor: %v", err)
	}
	retries, err := IdempotentRetryDialOptions(RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     10 * time.Millisecond,
		Multiplier:     1,
		Codes:          []codes.Code{codes.Unavailable},
	}, file.Services().ByName("Health"))
	if err != nil {
		t.Fatalf("IdempotentRetryDialOptions: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &flakyHealth{}
	gs := grpc.NewServer()
	healthpb.RegisterHealthServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), append(retries, grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: strings.Repeat("x", 300<<10)})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("Check after one UNAVAILABLE: got %v, %v; want SERVING", resp.GetStatus(), err)
	}
	if got, want := srv.checks.Load(), int32(2); got != want {
		t.Errorf("Check calls: got = %d, want = %d", got, want)
	}
}

// TestRetryBufferInterceptor pins that the replay buffer is raised only for
// the methods the policy retries: gRPC buffers every call until it commits, so
// raising it for an unmarked method would only hold memory. A buffer size the
// caller passes on the call comes last and so still wins.
func TestRetryBufferInterceptor(t *testing.T) {
	const retriedMethod, otherMethod = "/test.v1.Store/Get", "/test.v1.Store/Post"
	callerBuffer := grpc.MaxRetryRPCBufferSize(1 << 20)
	interceptor := retryBufferInterceptor(map[string]struct{}{retriedMethod: {}})

	tests := []struct {
		name   string
		method string
		opts   []grpc.CallOption
		want   []grpc.CallOption
	}{
		{"retried method", retriedMethod, nil, []grpc.CallOption{grpc.MaxRetryRPCBufferSize(SendMsgSize)}},
		{"retried method, caller's own buffer", retriedMethod, []grpc.CallOption{callerBuffer}, []grpc.CallOption{grpc.MaxRetryRPCBufferSize(SendMsgSize), callerBuffer}},
		{"other method", otherMethod, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []grpc.CallOption
			invoker := func(_ context.Context, _ string, _, _ any, _ *grpc.ClientConn, opts ...grpc.CallOption) error {
				got = opts
				return nil
			}
			if err := interceptor(t.Context(), tc.method, nil, nil, nil, invoker, tc.opts...); err != nil {
				t.Fatalf("interceptor: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("call options (-want +got):\n%s", diff)
			}
		})
	}
}
