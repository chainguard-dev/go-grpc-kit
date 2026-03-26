/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package duplex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	"chainguard.dev/go-grpc-kit/pkg/interceptors/clientid"
	"chainguard.dev/go-grpc-kit/pkg/metrics"
	"chainguard.dev/go-grpc-kit/pkg/options"
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

// allowedHeaders are HTTP headers that should be forwarded as gRPC metadata
// by the grpc-gateway when converting REST requests to gRPC calls.
var allowedHeaders = map[string]bool{
	clientid.CGClientID:  true,
	clientid.CGRequestID: true,
}

// incomingHeaderMatcher forwards known custom headers (like cgclientid) from
// HTTP requests to gRPC metadata, in addition to the default set.
func incomingHeaderMatcher(key string) (string, bool) {
	if allowedHeaders[strings.ToLower(key)] {
		return strings.ToLower(key), true
	}
	return runtime.DefaultHeaderMatcher(key)
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
	if port < 0 || port > 65535 {
		panic(fmt.Sprintf("invalid port: %d", port))
	}
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

	// Include the clientid interceptor on the loopback connection so that
	// REST-originated requests carry cgclientid metadata. We use
	// LoopbackDialOptions (not GRPCDialOptions) to avoid double-counting
	// client metrics and creating noisy self-referential OTEL traces.
	dOpts = append(options.LoopbackDialOptions(), dOpts...)

	// Always forward cgclientid from HTTP headers to gRPC metadata.
	mOpts = append(mOpts, runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher))

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
// When the context is canceled, it gracefully stops the gRPC server and
// shuts down the HTTP server, allowing in-flight requests to complete.
// Note: This call is blocking.
func (d *Duplex) ListenAndServe(ctx context.Context) error {
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", d.Host, d.Port),
		Handler:           grpcHandlerFunc(d.Server, d.MUX),
		ReadHeaderTimeout: 10 * time.Second,
	}

	return d.serveAndWait(ctx, server, func() error {
		return server.ListenAndServe()
	})
}

// Serve starts both the gRPC server and HTTP Gateway MUX on the given listener.
// When the context is canceled, it gracefully stops the gRPC server and
// shuts down the HTTP server, allowing in-flight requests to complete.
// Note: This call is blocking.
func (d *Duplex) Serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{
		Handler:           grpcHandlerFunc(d.Server, d.MUX),
		ReadHeaderTimeout: 10 * time.Second,
	}

	return d.serveAndWait(ctx, server, func() error {
		return server.Serve(listener)
	})
}

const shutdownTimeout = 30 * time.Second

// serveAndWait runs serveFn in a goroutine and waits for either it to return
// or for the context to be canceled, triggering graceful shutdown.
//
// The duplex server runs gRPC via grpcHandlerFunc on an http.Server, so
// shutdown is driven by http.Server.Shutdown which gracefully drains both
// REST and gRPC-over-HTTP connections. grpc.Server.GracefulStop is NOT
// called because the serverHandlerTransport used in this mode does not
// support Drain().
func (d *Duplex) serveAndWait(ctx context.Context, server *http.Server, serveFn func() error) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- serveFn()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Gracefully shut down the HTTP server, allowing in-flight
		// requests (both REST and gRPC-over-HTTP) to complete.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}

		// Drain the serve error — http.ErrServerClosed is expected.
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// RegisterListenAndServe initializes Prometheus metrics and starts a HTTP
// /metrics endpoint for exporting Prometheus metrics in the background.
// Call this *after* all services have been registered.
func (d *Duplex) RegisterListenAndServeMetrics(port int, enablePprof bool) {
	metrics.RegisterListenAndServe(d.Server, fmt.Sprintf("%s:%d", d.Host, port), enablePprof)
}

// RegisterAndServe initializes Prometheus metrics and starts a HTTP
// /metrics endpoint for exporting Prometheus metrics in the background.
// Call this *after* all services have been registered.
// Used ONLY for testing
func (d *Duplex) RegisterAndServeMetrics(listener net.Listener, enablePprof bool) {
	metrics.RegisterAndServe(d.Server, listener, enablePprof)
}
