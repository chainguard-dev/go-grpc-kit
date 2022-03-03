/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package options

import (
	"context"
	"crypto/tls"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"knative.dev/pkg/apis"
)

// ListenerForTest is to support bufnet in our testing.
var ListenerForTest interface {
	net.Listener

	Dial() (net.Conn, error)
}

func GRPCOptions(delegate apis.URL) (string, []grpc.DialOption) {
	switch delegate.Scheme {
	case "http":
		port := "80"
		// Explicit port from the user signifies we should override the scheme-based defaults.
		if delegate.URL().Port() != "" {
			port = delegate.URL().Port()
		}
		return net.JoinHostPort(delegate.URL().Hostname(), port), []grpc.DialOption{
			grpc.WithBlock(),
			grpc.WithInsecure(),
		}
	case "https":
		port := "443"
		// Explicit port from the user signifies we should override the scheme-based defaults.
		if delegate.URL().Port() != "" {
			port = delegate.URL().Port()
		}
		return net.JoinHostPort(delegate.URL().Hostname(), port), []grpc.DialOption{
			grpc.WithBlock(),
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
				MinVersion: tls.VersionTLS12,
			})),
		}

	case "bufnet": // This is to support testing, it will not pass webhook validation.
		return "bufnet", []grpc.DialOption{
			grpc.WithInsecure(),
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return ListenerForTest.Dial()
			}),
		}

	default:
		panic("unreachable for valid delegates.")
	}
}
