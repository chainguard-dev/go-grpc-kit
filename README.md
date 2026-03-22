# gRPC Kit

Utility library for gRPC in Go — provides duplex serving (gRPC + grpc-gateway),
Prometheus metrics, OpenTelemetry tracing, client identity propagation, and
pre-configured dial options.

## Packages

### `pkg/duplex` — Duplex gRPC + REST Gateway Server

Serves both gRPC and [gRPC Gateway](https://grpc-ecosystem.github.io/grpc-gateway/)
on the same port. Supports graceful shutdown via context cancellation.

```go
d := duplex.New(8080,
    grpc.ChainUnaryInterceptor(metrics.UnaryServerInterceptor()),
)

pb.RegisterTypeServer(d.Server, impl.NewTypeServer())
if err := d.RegisterHandler(ctx, pb.RegisterTypeHandlerFromEndpoint); err != nil {
    log.Panicf("Failed to register gateway endpoint: %v", err)
}

if err := d.ListenAndServe(ctx); err != nil {
    log.Panicf("ListenAndServe() = %v", err)
}
```

`duplex.New` accepts `grpc.ServerOption`, `runtime.ServeMuxOption`, and
`grpc.DialOption` (for the internal loopback connection).

### `pkg/options` — gRPC Client Dial Options

Pre-configured gRPC dial options for production use:

- **`GRPCOptions(url)`** — Returns target address and dial options for a URL.
  Handles `http`, `https`, `bufnet`, and test listener schemes.
- **`GRPCDialOptions()`** — Standard dial options with OTEL tracing,
  Prometheus client metrics, client identity propagation, and retry support.
- **`LoopbackDialOptions()`** — Minimal dial options for grpc-gateway
  loopback connections, omitting metrics/tracing to avoid double-counting.
- **`ClientOptions()`** — Wraps `GRPCDialOptions()` as `google.golang.org/api/option.ClientOption`.

Configuration via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `ENABLE_CLIENT_HANDLING_TIME_HISTOGRAM` | `true` | Enable client handling time histogram |
| `ENABLE_CLIENT_STREAM_RECEIVE_TIME_HISTOGRAM` | `true` | Enable client stream receive histogram |
| `ENABLE_CLIENT_STREAM_SEND_TIME_HISTOGRAM` | `true` | Enable client stream send histogram |
| `GRPC_CLIENT_MAX_RETRY` | `0` | Max retries (0 disables) |

### `pkg/metrics` — Prometheus Metrics & OpenTelemetry Tracing

- **`UnaryServerInterceptor()`** / **`StreamServerInterceptor()`** — gRPC
  server interceptors that record Prometheus metrics with `cgclientid` labels.
- **`SetupTracer(ctx)`** — Initializes OpenTelemetry tracing with OTLP gRPC
  exporter. Returns a shutdown function.
- **`RegisterListenAndServe(server, addr, enablePprof)`** — Starts a metrics
  HTTP server in the background serving `/metrics` and optionally `/debug/pprof/`.

### `pkg/trace` — Cloud Run Traceparent Preservation

Cloud Run may replace the `traceparent` header, losing span context. This
package provides stats handlers to work around this:

- **`PreserveTraceParentHandler`** — Client-side handler that copies the
  outgoing `traceparent` to `original-traceparent`.
- **`RestoreTraceParentHandler`** — Server-side handler that restores
  `traceparent` from `original-traceparent` if Cloud Run replaced it.

Prometheus counters for observability:
`grpc_traceparent_preserved_total`, `grpc_traceparent_restore_attempted_total`,
`grpc_traceparent_restored_total`.

### `pkg/interceptors/clientid` — Client Identity Propagation

Automatically propagates caller identity via gRPC metadata:

- **`UnaryClientInterceptor()`** / **`StreamClientInterceptor()`** — Adds
  `cgclientid` (service identity) and `cgrequestid` (unique per-call UUID)
  to outgoing metadata.

Client ID resolution: `K_SERVICE` env → `CG_CLIENT_ID` env → executable path.
