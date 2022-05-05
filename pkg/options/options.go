/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package options

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"knative.dev/pkg/apis"
)

// ListenerForTest is to support bufnet in our testing.
var ListenerForTest DialableListener

type DialableListener interface {
	net.Listener

	Dial() (net.Conn, error)
}

var listenersForTest sync.Map

// Register a test listener and get a provided scheme.
func RegisterListenerForTest(listener DialableListener) string {
	for {
		scheme := fmt.Sprintf("test%d", rand.Uint64())
		if _, conflicted := listenersForTest.LoadOrStore(scheme, listener); !conflicted {
			return scheme
		}
	}
}

// Unregister a test listener.
func UnregisterTestListener(scheme string) {
	listenersForTest.Delete(scheme)
}

func getTestListener(scheme string) (DialableListener, bool) {
	v, ok := listenersForTest.Load(scheme)
	if !ok {
		return nil, ok
	}
	return v.(DialableListener), true
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
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUnaryInterceptor(otelgrpc.UnaryClientInterceptor()),
			grpc.WithStreamInterceptor(otelgrpc.StreamClientInterceptor()),
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
			grpc.WithUnaryInterceptor(otelgrpc.UnaryClientInterceptor()),
			grpc.WithStreamInterceptor(otelgrpc.StreamClientInterceptor()),
		}

	case "bufnet": // This is to support testing, it will not pass webhook validation.
		return "bufnet", []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return ListenerForTest.Dial()
			}),
		}

	default:
		listener, ok := getTestListener(delegate.Scheme)
		if !ok {
			panic("unreachable for valid delegates.")
		}
		return delegate.Scheme, []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return listener.Dial()
			}),
		}
	}
}
