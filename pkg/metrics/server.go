/*
Copyright 2022 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"context"
	"log"
	"net/http"
	"net/http/pprof"
	"time"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"knative.dev/pkg/logging"
)

// Fractions >= 1 will always sample. Fractions < 0 are treated as zero. To
// respect the parent trace's `SampledFlag`, the `TraceIDRatioBased` sampler
// should be used as a delegate of a `Parent` sampler.
//
// Expected usage:
//
//	defer metrics.SetupTracer(ctx)()
func SetupTracer(ctx context.Context) func() {
	logger := logging.FromContext(ctx)

	traceExporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		logger.Panicf("SetupTracer() = %v", err)
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

func RegisterListenAndServe(server *grpc.Server, listenAddr string, enablePprof bool) {
	grpc_prometheus.Register(server)
	grpc_prometheus.EnableHandlingTimeHistogram(
		grpc_prometheus.WithHistogramBuckets(
			[]float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
		),
	)

	go func(addr string) {
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

		server := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 600 * time.Second,
		}

		if err := server.ListenAndServe(); err != nil {
			log.Fatalf("listen and server for http /metrics = %v", err)
		}
	}(listenAddr)
}
