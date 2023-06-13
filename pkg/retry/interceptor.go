/*
Copyright 2023 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package retry

import (
	"context"

	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	"google.golang.org/grpc"
)

func UnaryClientInterceptor(policy Policy) grpc.UnaryClientInterceptor {
	return func(parentCtx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		for _, predicate := range policy {
			if predicate.Match(method) {
				r := grpc_retry.UnaryClientInterceptor(predicate.Options...)
				return r(parentCtx, method, req, reply, cc, invoker, opts...)
			}
		}
		return invoker(parentCtx, method, req, reply, cc, opts...)
	}
}

func StreamClientInterceptor(policy Policy) grpc.StreamClientInterceptor {
	return func(parentCtx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		for _, predicate := range policy {
			if predicate.Match(method) {
				r := grpc_retry.StreamClientInterceptor(predicate.Options...)
				return r(parentCtx, desc, cc, method, streamer, opts...)
			}
		}
		return streamer(parentCtx, desc, cc, method, opts...)
	}
}
