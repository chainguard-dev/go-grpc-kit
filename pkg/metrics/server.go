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
	clientids := metadata.ValueFromIncomingContext(ctx, clientid.CGClientID)
	if clientids != nil {
		cid = clientids[0]
	}
	return prometheus.Labels{clientid.CGClientID: cid}
}

func getServer(enablePprof bool) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	if enablePprof {
		// pprof handles
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
		mux.Handle("/debug/pprof/block", pprof.Handler("block"))
		mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
		mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
		mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))

		log.Println("registering handle for /debug/pprof")
	}

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 600 * time.Second,
	}
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

func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return state().serverMetrics.UnaryServerInterceptor(
		grpc_prometheus.WithLabelsFromContext(labelsFromContext),
	)
}

func StreamServerInterceptor() grpc.StreamServerInterceptor {
	return state().serverMetrics.StreamServerInterceptor(
		grpc_prometheus.WithLabelsFromContext(labelsFromContext),
	)
}
