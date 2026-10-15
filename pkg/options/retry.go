/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package options

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// RetryPolicy is a gRPC retry policy: how many tries a call gets, the
// jittered exponential wait between them, and which codes earn another.
// MaxTokens and TokenRatio, when MaxTokens is set, throttle retries across
// the connection: each failure spends a token, each success returns
// TokenRatio of one, and retries pause while fewer than half are left.
type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Multiplier     float64
	Codes          []codes.Code
	MaxTokens      int
	TokenRatio     float64
}

// DefaultRetryPolicy retries UNAVAILABLE, the code a server answers when the
// same request may land on another try, up to gRPC's cap of 5 tries inside
// the call's deadline. Its waits of about 1, 2, 4 and 8 s (gRPC jitters each
// by ±20%) outlast an instance replacement or a cold start. Retries pause
// while most calls on the connection are failing (gRPC's documented
// throttling example), so an outage is not met with five times the load.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    5,
		InitialBackoff: time.Second,
		MaxBackoff:     10 * time.Second,
		Multiplier:     2,
		Codes:          []codes.Code{codes.Unavailable},
		MaxTokens:      10,
		TokenRatio:     0.1,
	}
}

// IdempotentRetryDialOptions returns the dial options that retry, under p,
// every method of svcs its proto marks NO_SIDE_EFFECTS or IDEMPOTENT. They set
// the connection's default service config, which a resolver-supplied config,
// such as an xDS target's, replaces. A caller with its own default config, a
// load-balancing policy for one, builds a combined one from
// IdempotentRetryServiceConfig and passes it after these options, which keeps
// the larger replay buffer. gRPC replays only a request it buffered, 256 KiB
// by default, so the options raise that to SendMsgSize for the unary calls
// they retry, leaving streams and unmarked methods at the default.
// DefaultRetryPolicy suits most services. Leave GRPC_CLIENT_MAX_RETRY unset
// on such a connection: its interceptor retries every method, on top of this
// policy.
func IdempotentRetryDialOptions(p RetryPolicy, svcs ...protoreflect.ServiceDescriptor) ([]grpc.DialOption, error) {
	cfg, err := IdempotentRetryServiceConfig(p, svcs...)
	if err != nil {
		return nil, err
	}
	retried := make(map[string]struct{})
	for _, m := range idempotentMethods(svcs) {
		retried["/"+string(m.Parent().FullName())+"/"+string(m.Name())] = struct{}{}
	}
	return []grpc.DialOption{
		grpc.WithDefaultServiceConfig(cfg),
		grpc.WithChainUnaryInterceptor(retryBufferInterceptor(retried)),
	}, nil
}

// retryBufferInterceptor raises the replay buffer to SendMsgSize for the
// methods in retried, so a large request to one stays retryable. gRPC buffers
// every call until it commits, retried or not, so raising it for the whole
// connection would let any stream hold that much. A buffer size the caller
// passes on the call still wins.
func retryBufferInterceptor(retried map[string]struct{}) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := retried[method]; ok {
			opts = append([]grpc.CallOption{grpc.MaxRetryRPCBufferSize(SendMsgSize)}, opts...)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// IdempotentRetryServiceConfig returns the gRPC service config that retries,
// under p, every method of svcs its proto marks NO_SIDE_EFFECTS or IDEMPOTENT
// (the idempotency_level method option). Marking a method is the opt-in: an
// unmarked one is never sent twice, and services with no marked methods
// yield an empty config. IdempotentRetryDialOptions installs it as the
// connection's default. A caller with a service config of its own merges this
// one into it and passes the result after IdempotentRetryDialOptions.
func IdempotentRetryServiceConfig(p RetryPolicy, svcs ...protoreflect.ServiceDescriptor) (string, error) {
	type name struct {
		Service string `json:"service"`
		Method  string `json:"method"`
	}
	type policy struct {
		MaxAttempts          int          `json:"maxAttempts"`
		InitialBackoff       string       `json:"initialBackoff"`
		MaxBackoff           string       `json:"maxBackoff"`
		BackoffMultiplier    float64      `json:"backoffMultiplier"`
		RetryableStatusCodes []codes.Code `json:"retryableStatusCodes"`
	}
	type methodConfig struct {
		Name        []name `json:"name"`
		RetryPolicy policy `json:"retryPolicy"`
	}
	type throttling struct {
		MaxTokens  int     `json:"maxTokens"`
		TokenRatio float64 `json:"tokenRatio"`
	}

	methods := idempotentMethods(svcs)
	if len(methods) == 0 {
		return "{}", nil
	}
	names := make([]name, 0, len(methods))
	for _, m := range methods {
		names = append(names, name{Service: string(m.Parent().FullName()), Method: string(m.Name())})
	}
	var cfg struct {
		MethodConfig    []methodConfig `json:"methodConfig"`
		RetryThrottling *throttling    `json:"retryThrottling,omitempty"`
	}
	cfg.MethodConfig = []methodConfig{{
		Name: names,
		RetryPolicy: policy{
			MaxAttempts:          p.MaxAttempts,
			InitialBackoff:       protoDuration(p.InitialBackoff),
			MaxBackoff:           protoDuration(p.MaxBackoff),
			BackoffMultiplier:    p.Multiplier,
			RetryableStatusCodes: p.Codes,
		},
	}}
	if p.MaxTokens > 0 {
		cfg.RetryThrottling = &throttling{MaxTokens: p.MaxTokens, TokenRatio: p.TokenRatio}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal retry service config: %w", err)
	}
	return string(b), nil
}

// idempotentMethods lists the methods of svcs their proto marks
// NO_SIDE_EFFECTS or IDEMPOTENT.
func idempotentMethods(svcs []protoreflect.ServiceDescriptor) []protoreflect.MethodDescriptor {
	var out []protoreflect.MethodDescriptor
	for _, svc := range svcs {
		methods := svc.Methods()
		for i := range methods.Len() {
			m := methods.Get(i)
			opts, _ := m.Options().(*descriptorpb.MethodOptions)
			switch opts.GetIdempotencyLevel() {
			case descriptorpb.MethodOptions_NO_SIDE_EFFECTS, descriptorpb.MethodOptions_IDEMPOTENT:
				out = append(out, m)
			}
		}
	}
	return out
}

// protoDuration formats d as service-config JSON: decimal seconds plus "s".
func protoDuration(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}
