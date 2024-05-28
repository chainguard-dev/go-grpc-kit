/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package duplex

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	"chainguard.dev/go-grpc-kit/pkg/metrics"
)

// grpcHandlerFunc routes inbound requests to either the passed gRPC server or
// the http handler based on the request content type.
// See also, https://grpc-ecosystem.github.io/grpc-gateway/
// This is based on: https://github.com/philips/grpc-gateway-example/issues/22#issuecomment-490733965
func grpcHandlerFunc(grpcServer *grpc.Server, httpHandler http.Handler) http.Handler {
	return h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			httpHandler.ServeHTTP(w, r)
		}
	}), &http2.Server{})
}

// Duplex is a wrapper for the gRPC server, gRPC HTTP Gateway MUX and options.
type Duplex struct {
	Server      *grpc.Server
	MUX         *runtime.ServeMux
	Loopback    string
	Host        string
	Port        int
	DialOptions []grpc.DialOption
}

type RegisterHandlerFromEndpointFn func(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error

// New creates a Duplex gRPC server / gRPC HTTP Gateway. New takes in options
// for `grpc.NewServer`, typed `grpc.ServerOption`, and `runtime.NewServeMux`,
// typed `runtime.ServeMuxOption`. Unknown opts will cause a panic.
func New(port int, opts ...interface{}) *Duplex {
	// Split out the options into their types.
	var (
		gOpts []grpc.ServerOption
		dOpts []grpc.DialOption
		mOpts []runtime.ServeMuxOption
	)
	for _, o := range opts {
		switch opt := o.(type) {
		case grpc.ServerOption:
			gOpts = append(gOpts, opt)
		case runtime.ServeMuxOption:
			mOpts = append(mOpts, opt)
		case grpc.DialOption:
			dOpts = append(dOpts, opt)
		default:
			panic(fmt.Errorf("unknown type: %T", o))
		}
	}

	// Create the Duplex Server.
	d := &Duplex{
		Server: grpc.NewServer(gOpts...),
		MUX:    runtime.NewServeMux(mOpts...),
		// The REST gateway translates the json to grpc and then dispatches to
		// the appropriate method on this address, so we loopback to ourselves.
		Loopback:    fmt.Sprintf("localhost:%d", port),
		Port:        port,
		DialOptions: dOpts,
	}
	return d
}

// RegisterHandler is a helper registration handler to call the passed in
// `RegisterHandlerFromEndpointFn` with the correct options after `d.Server`
// has been registered with the implementation. Use like:
// ```go
//
//	pb.Register<Type>Server(d.Server, impl.New<TypeImpl>())
//	if err := d.RegisterHandler(ctx, pb.Register<Type>HandlerFromEndpoint); err != nil {
//		log.Panicf("Failed to register gateway endpoint: %v", err)
//	}
//
// ```
func (d *Duplex) RegisterHandler(ctx context.Context, fn RegisterHandlerFromEndpointFn) error {
	return fn(ctx, d.MUX, d.Loopback, d.DialOptions)
}

// ListenAndServe starts both the gRPC server and HTTP Gateway MUX.
// Note: This call is blocking.
func (d *Duplex) ListenAndServe(_ context.Context) error {
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", d.Host, d.Port),
		Handler:           grpcHandlerFunc(d.Server, d.MUX),
		ReadHeaderTimeout: 600 * time.Second,
	}

	return server.ListenAndServe()
}

// ListenAndServe starts both the gRPC server and HTTP Gateway MUX on the given listener.
// Note: This call is blocking.
// #nosec G114 -- used only for testing tls.
// nolint:gosec
func (d *Duplex) Serve(_ context.Context, listener net.Listener) error {
	return http.Serve(listener, grpcHandlerFunc(d.Server, d.MUX))
}

// RegisterListenAndServe initializes Prometheus metrics and starts a HTTP
// /metrics endpoint for exporting Prometheus metrics in the background.
// Call this *after* all services have been registered.
func (d *Duplex) RegisterListenAndServeMetrics(port int, enablePprof bool) {
	metrics.RegisterListenAndServe(d.Server, fmt.Sprintf("%s:%d", d.Host, port), enablePprof)
}
