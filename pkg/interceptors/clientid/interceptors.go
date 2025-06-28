package clientid

import (
	"context"
	"os"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const CGClientID = "cgclientid"
const CGRequestID = "cgrequestid"

func getClientID() string {
	e, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	return e
}

func AppendClientID(ctx context.Context) context.Context {
	if metadata.ValueFromIncomingContext(ctx, CGClientID) != nil && metadata.ValueFromIncomingContext(ctx, CGRequestID) != nil {
		// Return original context if it already has chainguard client id and request id.
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx,
		"clientid", getClientID(),
		"requestid", uuid.New().String(),
	)
}

func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		nc := AppendClientID(ctx)
		// Make the call
		return invoker(nc, method, req, reply, cc, opts...)
	}
}

func StreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		nc := AppendClientID(ctx)
		// Make the call
		return streamer(nc, desc, cc, method, opts...)
	}
}
