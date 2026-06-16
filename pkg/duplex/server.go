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
	"sync"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	"chainguard.dev/go-grpc-kit/pkg/interceptors/clientid"
	"chainguard.dev/go-grpc-kit/pkg/metrics"
	"chainguard.dev/go-grpc-kit/pkg/options"
)

// handler routes inbound requests to either the gRPC server or the gateway MUX
// based on the request content type, served over h2c so gRPC works on a
// cleartext port. Each request is counted while it runs, so Shutdown can wait
// for in-flight requests to finish: the gRPC requests are served over hijacked
// h2c connections that http.Server.Shutdown does not track, so counting them
// here is the only way to know when they have drained.
// See also, https://grpc-ecosystem.github.io/grpc-gateway/
// This is based on: https://github.com/philips/grpc-gateway-example/issues/22#issuecomment-490733965
func (d *Duplex) handler() http.Handler {
	return h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.inflight.add()
		defer d.inflight.done()

		if r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
			d.Server.ServeHTTP(w, r)
			return
		}

		d.MUX.ServeHTTP(w, r)
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

	httpServerOnce sync.Once
	httpServer     *http.Server

	// inflight counts requests currently being served, so Shutdown can wait for
	// them to finish.
	inflight inflightTracker
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
// Note: This call is blocking. It returns http.ErrServerClosed after Shutdown.
func (d *Duplex) ListenAndServe(_ context.Context) error {
	server := d.httpServerInstance()
	server.Addr = fmt.Sprintf("%s:%d", d.Host, d.Port)

	return server.ListenAndServe()
}

// Serve starts both the gRPC server and HTTP Gateway MUX on the given listener.
// Note: This call is blocking. It returns http.ErrServerClosed after Shutdown.
func (d *Duplex) Serve(_ context.Context, listener net.Listener) error {
	return d.httpServerInstance().Serve(listener)
}

// httpServerInstance returns the underlying http.Server, constructing it on
// first use.
func (d *Duplex) httpServerInstance() *http.Server {
	d.httpServerOnce.Do(func() {
		d.httpServer = &http.Server{
			Handler:           d.handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
	})

	return d.httpServer
}

// Shutdown gracefully stops the duplex. It stops accepting new connections and
// waits for in-flight requests to finish, bounded by ctx; if ctx is done before
// they drain it stops waiting and returns ctx.Err(). After Shutdown returns, the
// blocking ListenAndServe or Serve call returns http.ErrServerClosed.
//
// The wait is bounded only by ctx, mirroring http.Server.Shutdown: pass a
// context with a deadline to cap it, or a long-lived request will hold shutdown
// open indefinitely.
//
// gRPC is served over h2c, whose connections http.Server.Shutdown does not
// track once hijacked, so its return does not imply gRPC has drained; and
// grpc.Server.GracefulStop panics on a ServeHTTP-backed transport, so it cannot
// drain them either. Shutdown therefore stops the listeners via the HTTP
// server, waits on its own in-flight counter for requests of both kinds to
// finish, then stops the gRPC server with grpc.Server.Stop and closes the HTTP
// server. Those final steps run on every path: after a clean drain they release
// the now-idle server; if the wait ended on ctx they also close transports that
// are still open.
func (d *Duplex) Shutdown(ctx context.Context) error {
	server := d.httpServerInstance()

	// Stop accepting new connections and drain the gateway's tracked
	// connections. This does not wait for hijacked h2c (gRPC) connections, so
	// run it in the background while the in-flight counter is drained below. Its
	// error is dropped deliberately: the wait below reports the drain outcome,
	// and the Close below is the backstop.
	go func() { _ = server.Shutdown(ctx) }()

	err := d.inflight.wait(ctx)

	d.Server.Stop()
	_ = server.Close()

	return err
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
