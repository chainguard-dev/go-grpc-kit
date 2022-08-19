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

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
)

// Fractions >= 1 will always sample. Fractions < 0 are treated as zero. To
// respect the parent trace's `SampledFlag`, the `TraceIDRatioBased` sampler
// should be used as a delegate of a `Parent` sampler.
func SetupTracer(ctx context.Context) (*trace.TracerProvider, error) {
	traceExporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, nil
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

	return tp, nil
}

func RegisterListenAndServe(server *grpc.Server, listenAddr string, enablePprof bool) {
	grpc_prometheus.Register(server)
	grpc_prometheus.EnableHandlingTimeHistogram()

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

		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatalf("listen and server for http /metrics = %v", err)
		}
	}(listenAddr)
}
