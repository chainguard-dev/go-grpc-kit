/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"chainguard.dev/go-grpc-kit/pkg/interceptors/clientid"
	"github.com/chainguard-dev/clog"
)

type initStuff struct {
	serverMetrics *grpc_prometheus.ServerMetrics
}

var (
	state = sync.OnceValue(func() initStuff {
		init := initStuff{}

		init.serverMetrics = grpc_prometheus.NewServerMetrics(
			grpc_prometheus.WithServerHandlingTimeHistogram(
				grpc_prometheus.WithHistogramBuckets(
					[]float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1200, 2400, 3666},
				),
			),
			// Register cgclientid as a custom label so WithLabelsFromContext
			// values are included in all server metrics.
			grpc_prometheus.WithContextLabels(clientid.CGClientID),
		)
		prometheus.MustRegister(init.serverMetrics)

		return init
	})
)

// Fractions >= 1 will always sample. Fractions < 0 are treated as zero. To
// respect the parent trace's `SampledFlag`, the `TraceIDRatioBased` sampler
// should be used as a delegate of a `Parent` sampler.
//
// Expected usage:
//
//	defer metrics.SetupTracer(ctx)()
func SetupTracer(ctx context.Context) func() {
	logger := clog.FromContext(ctx)

	traceExporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		logger.Errorf("SetupTracer() = %v", err)
		panic(err)
	}
	bsp := trace.NewBatchSpanProcessor(traceExporter)
	res := resource.Default()

	tp := trace.NewTracerProvider(
		trace.WithResource(res),
		trace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tp)

	prp := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	otel.SetTextMapPropagator(prp)

	return func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			logger.Infof("Error shutting down tracer provider: %v", err)
		}
	}
}

func labelsFromContext(ctx context.Context) prometheus.Labels {
	cid := "unknown"
	if clientids := metadata.ValueFromIncomingContext(ctx, clientid.CGClientID); len(clientids) > 0 {
		cid = clientids[0]
	}
	return prometheus.Labels{clientid.CGClientID: cid}
}

func getServer(enablePprof bool) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	if enablePprof {
		// pprof handles — restricted to loopback IPs only to prevent
		// remote access to profiling data (goroutine stacks, heap, etc.).
		mux.Handle("/debug/pprof/", localhostOnly(http.HandlerFunc(pprof.Index)))
		mux.Handle("/debug/pprof/cmdline", localhostOnly(http.HandlerFunc(pprof.Cmdline)))
		mux.Handle("/debug/pprof/profile", localhostOnly(http.HandlerFunc(pprof.Profile)))
		mux.Handle("/debug/pprof/symbol", localhostOnly(http.HandlerFunc(pprof.Symbol)))
		mux.Handle("/debug/pprof/trace", localhostOnly(http.HandlerFunc(pprof.Trace)))
		mux.Handle("/debug/pprof/allocs", localhostOnly(pprof.Handler("allocs")))
		mux.Handle("/debug/pprof/block", localhostOnly(pprof.Handler("block")))
		mux.Handle("/debug/pprof/goroutine", localhostOnly(pprof.Handler("goroutine")))
		mux.Handle("/debug/pprof/heap", localhostOnly(pprof.Handler("heap")))
		mux.Handle("/debug/pprof/mutex", localhostOnly(pprof.Handler("mutex")))
		mux.Handle("/debug/pprof/threadcreate", localhostOnly(pprof.Handler("threadcreate")))

		log.Println("registering handle for /debug/pprof (localhost-only)")
	}

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// localhostOnly wraps an http.Handler to reject requests from non-loopback IPs.
func localhostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Used ONLY for testing
func RegisterAndServe(server *grpc.Server, listener net.Listener, enablePprof bool) {
	state().serverMetrics.InitializeMetrics(server)

	go func() {
		s := getServer(enablePprof)

		if err := s.Serve(listener); err != nil {
			log.Fatalf("serve for http /metrics = %v", err)
		}
	}()
}

func RegisterListenAndServe(server *grpc.Server, listenAddr string, enablePprof bool) {
	state().serverMetrics.InitializeMetrics(server)

	go func() {
		s := getServer(enablePprof)
		s.Addr = listenAddr

		if err := s.ListenAndServe(); err != nil {
			log.Fatalf("listen and serve for http /metrics = %v", err)
		}
	}()
}

// UnaryServerInterceptor returns a gRPC unary server interceptor that records
// Prometheus metrics with cgclientid labels from request metadata.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return state().serverMetrics.UnaryServerInterceptor(
		grpc_prometheus.WithLabelsFromContext(labelsFromContext),
	)
}

// StreamServerInterceptor returns a gRPC stream server interceptor that records
// Prometheus metrics with cgclientid labels from request metadata.
func StreamServerInterceptor() grpc.StreamServerInterceptor {
	return state().serverMetrics.StreamServerInterceptor(
		grpc_prometheus.WithLabelsFromContext(labelsFromContext),
	)
}
