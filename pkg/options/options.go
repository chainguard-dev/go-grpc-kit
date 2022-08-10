/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package options

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/url"
	"sync"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
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
		val, err := rand.Int(rand.Reader, big.NewInt(int64(math.MaxInt64)))
		if err != nil {
			panic(err)
		}
		scheme := fmt.Sprintf("test%d", val.Int64())
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

// These are defined as global variables, so that folks can expose them as flags
// in their entrypoints, if they choose.
var (
	RecvMsgSize = 100 * 1024 * 1024 // 100MB
	SendMsgSize = 100 * 1024 * 1024 // 100MB
)

func GRPCOptions(delegate url.URL) (string, []grpc.DialOption) {
	switch delegate.Scheme {
	case "http":
		port := "80"
		// Explicit port from the user signifies we should override the scheme-based defaults.
		if delegate.Port() != "" {
			port = delegate.Port()
		}
		return net.JoinHostPort(delegate.Hostname(), port), []grpc.DialOption{
			grpc.WithUnaryInterceptor(grpc_prometheus.UnaryClientInterceptor),
			grpc.WithBlock(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(RecvMsgSize),
				grpc.MaxCallSendMsgSize(SendMsgSize),
			),
		}
	case "https":
		port := "443"
		// Explicit port from the user signifies we should override the scheme-based defaults.
		if delegate.Port() != "" {
			port = delegate.Port()
		}
		return net.JoinHostPort(delegate.Hostname(), port), []grpc.DialOption{
			grpc.WithUnaryInterceptor(grpc_prometheus.UnaryClientInterceptor),
			grpc.WithBlock(),
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
				MinVersion: tls.VersionTLS12,
			})),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(RecvMsgSize),
				grpc.MaxCallSendMsgSize(SendMsgSize),
			),
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
