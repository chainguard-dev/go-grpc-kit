# gRPC

This repository contains helpers working with gRPC.

## gRPC Duplex

For cases where you would like to serve both the gRPC endpoint and a
[gRPC Gateway](https://grpc-ecosystem.github.io/grpc-gateway/), the
chainguard.dev/grpc/pkg/duplex package is a helpful wrapper.

First, define the REST endpoints in the `.proto` file. Then change to use the
duplex object to start and build up the gRPC server. Example:

```go
d := duplex.New(8080)

pb.Register<Type>Server(d.Server, impl.New<Type>Server())
if err := d.RegisterHandler(ctx, impl.Register<Type>ServiceHandlerFromEndpoint); err != nil {
    log.Panicf("Failed to register gateway endpoint: %v", err)
}

...

if err := d.ListenAndServe(ctx); err != nil {
    log.Panicf("ListenAndServe() = %v", err)
}
```

Run this and you should see a message like:

```text
Duplex gRPC/HTTP server starting at ::8080
```

### Options

You can pass `grpc.ServerOption` and `runtime.NewServeMux` into `duplex.New`.

For example, if you wanted to make a duplex server with a
[unary interceptor](https://pkg.go.dev/google.golang.org/grpc#UnaryInterceptor):

```go
d := duplex.New(port, grpc.UnaryInterceptor(myInterceptorFn))
```
