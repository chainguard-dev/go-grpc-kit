/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package duplex

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// DefaultMaxRequestBodyBytes is the default cap on the size of a request
	// body served by the gateway MUX. It fits a JSON-encoded request for a
	// 10 MiB gRPC message, and 16 MiB request bodies served on the MUX.
	DefaultMaxRequestBodyBytes int64 = 16 << 20

	// DefaultRequestBodyReadTimeout is the default time allowed to read a
	// request body served by the gateway MUX, measured from when the request
	// reaches the Duplex handler. It lets a 10 MiB body arrive over a link of
	// about 1.5 Mbit/s.
	DefaultRequestBodyReadTimeout = 60 * time.Second
)

// Option configures the Duplex itself, as opposed to the gRPC server, the
// gateway MUX, or the loopback connection. Pass it to New alongside the other
// option types.
type Option func(*Duplex)

// WithMaxRequestBodyBytes caps the size of a request body served by the
// gateway MUX, including handlers registered with MUX.HandlePath and the form
// body the MUX parses itself before any handler or middleware runs. A request
// that declares a larger Content-Length is refused with 413 before the MUX
// sees it; one that streams more than n bytes fails its next body read, and a
// resulting error reply is sent as 413. A non-positive n removes the cap.
// Native gRPC requests are not affected; bound those with
// grpc.MaxRecvMsgSize. The default is DefaultMaxRequestBodyBytes.
func WithMaxRequestBodyBytes(n int64) Option {
	return func(d *Duplex) {
		d.maxRequestBodyBytes = n
	}
}

// WithRequestBodyReadTimeout bounds the time allowed to read a request body
// served by the gateway MUX, including handlers registered with
// MUX.HandlePath and the form body the MUX parses itself. A body not read to
// its end within timeout fails its next read, and a resulting error reply is
// sent as 408. The bound covers only the body read; once the body has been
// read to its end the handler may run for as long as it needs, and a request
// with no body is not bounded at all. A non-positive timeout removes the
// bound. Native gRPC requests are not affected, so long-lived gRPC streams
// are not cut off. The default is DefaultRequestBodyReadTimeout.
func WithRequestBodyReadTimeout(timeout time.Duration) Option {
	return func(d *Duplex) {
		d.requestBodyReadTimeout = timeout
	}
}

// serveGateway serves r on the gateway MUX with its body bounded in size by
// d.maxRequestBodyBytes and in read time by d.requestBodyReadTimeout.
//
// The gateway reads the request body before it makes the gRPC call, and so
// before any gRPC interceptor, such as authentication or rate limiting, runs;
// grpc-gateway v2.26 and later drain the body even for methods that take none,
// such as GET. Without a bound, a large or slow body holds a request open
// unauthenticated. Bounding here, in front of the MUX, also covers the form
// body the MUX parses before any runtime.WithMiddlewares middleware runs.
func (d *Duplex) serveGateway(w http.ResponseWriter, r *http.Request) {
	// A bodyless request is left alone. Over HTTP/1 net/http is already
	// reading the connection in the background to detect a client hangup,
	// and a deadline would fail that read and cancel the request context of
	// a handler still running.
	unbounded := d.maxRequestBodyBytes <= 0 && d.requestBodyReadTimeout <= 0
	if unbounded || r.Body == nil || r.Body == http.NoBody {
		d.MUX.ServeHTTP(w, r)
		return
	}

	body := &boundedBody{ReadCloser: r.Body}
	bw := &boundedBodyWriter{ResponseWriter: w, body: body}

	if d.maxRequestBodyBytes > 0 {
		if r.ContentLength > d.maxRequestBodyBytes {
			// Reply through the MUX's error handler so gateway clients get
			// the error body they expect; the writer holds the status at
			// 413 whatever status that handler picks.
			body.failStatus.Store(http.StatusRequestEntityTooLarge)
			_, marshaler := runtime.MarshalerForRequest(d.MUX, r)
			runtime.HTTPError(r.Context(), d.MUX, marshaler, bw, r, &runtime.HTTPStatusError{
				HTTPStatus: http.StatusRequestEntityTooLarge,
				Err:        status.Error(codes.ResourceExhausted, "request body too large"),
			})
			return
		}
		body.ReadCloser = http.MaxBytesReader(w, r.Body, d.maxRequestBodyBytes)
	}

	if d.requestBodyReadTimeout > 0 {
		// ErrNotSupported means the ResponseWriter cannot carry a deadline;
		// serve with the size cap alone rather than refuse the request. Over
		// HTTP/1 net/http clears the deadline itself once the body has been
		// read to its end; over HTTP/2 it only ever fails reads of this
		// request's body.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d.requestBodyReadTimeout))
	}

	r.Body = body
	d.MUX.ServeHTTP(bw, r)
}

// boundedBody is a request body that records why a read failed.
//
// Reads may happen on a different goroutine from the response writes, so the
// failure is recorded atomically.
type boundedBody struct {
	io.ReadCloser

	// failStatus is the HTTP status that names a failed read: 413 for a body
	// over the cap, 408 for one that missed its read deadline, or 0.
	failStatus atomic.Int32
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	switch {
	case err == nil, errors.Is(err, io.EOF):
	case errors.As(err, new(*http.MaxBytesError)):
		b.failStatus.CompareAndSwap(0, http.StatusRequestEntityTooLarge)
	case errors.Is(err, os.ErrDeadlineExceeded):
		b.failStatus.CompareAndSwap(0, http.StatusRequestTimeout)
	}
	return n, err
}

// boundedBodyWriter reports a failed body read in the response status. The
// gateway turns a failed body read into a generic error status, such as 400
// for a request message or form that did not decode, or 499 for a call
// canceled when the connection failed; this replaces it with the status that
// names the cause. A successful status is never replaced.
type boundedBodyWriter struct {
	http.ResponseWriter

	body *boundedBody
}

func (w *boundedBodyWriter) WriteHeader(code int) {
	if code >= http.StatusBadRequest {
		if s := w.body.failStatus.Load(); s != 0 {
			code = int(s)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher, which the gateway requires of the
// ResponseWriter to serve server-streaming methods.
func (w *boundedBodyWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter.
func (w *boundedBodyWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
