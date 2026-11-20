/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package duplex

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "chainguard.dev/go-grpc-kit/pkg/duplex/internal/proto/helloworld"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	testBodyLimit   = 1 << 10
	testBodyTimeout = 300 * time.Millisecond
)

// testBounds are the Duplex options most tests serve with.
var testBounds = []any{WithMaxRequestBodyBytes(testBodyLimit), WithRequestBodyReadTimeout(testBodyTimeout)}

// greeter echoes the request name. Unlike server it keeps no state, so it is
// safe under concurrent requests.
type greeter struct {
	pb.UnimplementedGreeterServer
}

func (greeter) SayHello(_ context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	return &pb.HelloReply{Message: "Hello " + in.GetName()}, nil
}

// echoServiceDesc describes a bidirectional streaming gRPC service that echoes
// every message it receives, used to show gRPC streams are not bounded.
var echoServiceDesc = grpc.ServiceDesc{
	ServiceName: "duplextest.Echo",
	HandlerType: (*any)(nil),
	Streams: []grpc.StreamDesc{{
		StreamName:    "Echo",
		ServerStreams: true,
		ClientStreams: true,
		Handler: func(_ any, stream grpc.ServerStream) error {
			for {
				msg := new(wrapperspb.BytesValue)
				if err := stream.RecvMsg(msg); errors.Is(err, io.EOF) {
					return nil
				} else if err != nil {
					return err
				}
				if err := stream.SendMsg(msg); err != nil {
					return err
				}
			}
		},
	}},
}

// holdHandler reads the whole body, then holds the request for three body
// read timeouts before replying 200. It replies 500 if the request context is
// canceled while held, which is what a body read deadline left armed on an
// HTTP/1 connection would do.
func holdHandler(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if _, err := io.ReadAll(r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case <-time.After(3 * testBodyTimeout):
		w.WriteHeader(http.StatusOK)
	case <-r.Context().Done():
		http.Error(w, "request context canceled while held", http.StatusInternalServerError)
	}
}

// drainForwardHandler is shaped like the request handlers grpc-gateway v2.26
// and later generate for a method without a body, such as a GET: it drains
// the request body, ignoring any error, then forwards to SayHello with the
// name query parameter. It reports the drained byte count in X-Drained.
func drainForwardHandler(mux *runtime.ServeMux, client pb.GreeterClient) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
		n, _ := io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Drained", strconv.FormatInt(n, 10))
		reply, err := client.SayHello(r.Context(), &pb.HelloRequest{Name: r.URL.Query().Get("name")})
		if err != nil {
			_, marshaler := runtime.MarshalerForRequest(mux, r)
			runtime.HTTPError(r.Context(), mux, marshaler, w, r, err)
			return
		}
		fmt.Fprint(w, reply.GetMessage())
	}
}

// formHandler replies with the name form value. It is registered for GET
// only, so a form-encoded POST reaches it through the MUX's path length
// fallback or X-HTTP-Method-Override, both of which parse the form body in
// the MUX before any handler or middleware runs.
func formHandler(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	fmt.Fprint(w, r.FormValue("name"))
}

// testDuplex is a Duplex served on a loopback port.
type testDuplex struct {
	addr string
	// rpcs counts the unary gRPC calls that reached the server interceptors.
	rpcs *atomic.Int32
}

// startDuplex serves a duplex with the given options on a loopback port.
func startDuplex(t *testing.T, opts ...any) testDuplex {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port

	rpcs := new(atomic.Int32)
	count := grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		rpcs.Add(1)
		return h(ctx, req)
	})

	d := New(port, append([]any{count}, opts...)...)
	pb.RegisterGreeterServer(d.Server, greeter{})
	d.Server.RegisterService(&echoServiceDesc, struct{}{})
	if err := d.RegisterHandler(t.Context(), pb.RegisterGreeterHandlerFromEndpoint); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}

	conn, err := grpc.NewClient(d.Loopback, d.DialOptions...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	for _, h := range []struct {
		method, path string
		fn           runtime.HandlerFunc
	}{
		{http.MethodGet, "/v1/hold", holdHandler},
		{http.MethodPost, "/v1/hold", holdHandler},
		{http.MethodGet, "/v1/drain", drainForwardHandler(d.MUX, pb.NewGreeterClient(conn))},
		{http.MethodGet, "/v1/form", formHandler},
	} {
		if err := d.MUX.HandlePath(h.method, h.path, h.fn); err != nil {
			t.Fatalf("HandlePath(%s %s): %v", h.method, h.path, err)
		}
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- d.Serve(context.Background(), lis) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: got = %v, want = %v", err, http.ErrServerClosed)
		}
	})

	return testDuplex{addr: lis.Addr().String(), rpcs: rpcs}
}

// httpClient returns a client that speaks only HTTP/1 or only cleartext
// HTTP/2 (h2c), with protoMajor 1 or 2 respectively.
func httpClient(t *testing.T, protoMajor int) *http.Client {
	t.Helper()

	tr := &http.Transport{Protocols: new(http.Protocols)}
	switch protoMajor {
	case 1:
		tr.Protocols.SetHTTP1(true)
	case 2:
		tr.Protocols.SetUnencryptedHTTP2(true)
	default:
		t.Fatalf("unsupported protocol major version %d", protoMajor)
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// unsized hides the length of a reader from the HTTP client, so the body is
// sent chunked over HTTP/1 and without a Content-Length over HTTP/2.
type unsized struct{ io.Reader }

// jsonName returns a HelloRequest JSON body whose name is n bytes long.
func jsonName(n int) []byte {
	return []byte(`{"name":"` + strings.Repeat("a", n) + `"}`)
}

// formName returns a form body whose name value is n bytes long.
func formName(n int) []byte {
	return []byte(url.Values{"name": {strings.Repeat("a", n)}}.Encode())
}

// sized returns a body func for a body with a declared length.
func sized(b []byte) func() (io.Reader, func()) {
	return func() (io.Reader, func()) { return bytes.NewReader(b), nil }
}

// streamed returns a body func for a body sent without a declared length.
func streamed(b []byte) func() (io.Reader, func()) {
	return func() (io.Reader, func()) { return unsized{bytes.NewReader(b)}, nil }
}

// stalled returns a body func for a body that sends prefix, then stalls until
// the response has arrived.
func stalled(prefix string) func() (io.Reader, func()) {
	return func() (io.Reader, func()) {
		pr, pw := io.Pipe()
		go func() { _, _ = pw.Write([]byte(prefix)) }()
		return pr, func() { pw.Close() }
	}
}

// bodyCase is one gateway request and the response it should get.
type bodyCase struct {
	name string
	// opts are the Duplex options; nil means testBounds.
	opts   []any
	method string
	path   string
	// contentType defaults to application/json.
	contentType string
	header      map[string]string
	// body returns the request body, or nil for none, and a func, when
	// non-nil, to call once the response has arrived.
	body       func() (io.Reader, func())
	wantStatus int
	// wantStatusH2, when set, is the status wanted over HTTP/2 instead.
	wantStatusH2 int
	// wantNoRPC asserts no gRPC call reached the server interceptors.
	wantNoRPC bool
	// check, when set, inspects the response.
	check func(t *testing.T, resp *http.Response, body []byte)
}

// run sends tc over HTTP/1 and over h2c, each to its own Duplex.
func (tc bodyCase) run(t *testing.T) {
	for _, protoMajor := range []int{1, 2} {
		t.Run(fmt.Sprintf("HTTP%d/%s", protoMajor, tc.name), func(t *testing.T) {
			t.Parallel()

			opts := tc.opts
			if opts == nil {
				opts = testBounds
			}
			d := startDuplex(t, opts...)

			var body io.Reader
			var done func()
			if tc.body != nil {
				body, done = tc.body()
			}
			req, err := http.NewRequestWithContext(t.Context(), tc.method, "http://"+d.addr+tc.path, body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Content-Type", cmp.Or(tc.contentType, "application/json"))
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}

			resp, err := httpClient(t, protoMajor).Do(req)
			if done != nil {
				done()
			}
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading response: %v", err)
			}

			if resp.ProtoMajor != protoMajor {
				t.Errorf("protocol: got = %s, want = HTTP/%d", resp.Proto, protoMajor)
			}
			want := tc.wantStatus
			if protoMajor == 2 && tc.wantStatusH2 != 0 {
				want = tc.wantStatusH2
			}
			if resp.StatusCode != want {
				t.Errorf("status: got = %d, want = %d; body: %s", resp.StatusCode, want, got)
			}
			if n := d.rpcs.Load(); tc.wantNoRPC && n != 0 {
				t.Errorf("gRPC calls reached: got = %d, want = 0", n)
			}
			if tc.check != nil {
				tc.check(t, resp, got)
			}
		})
	}
}

// checkDrainedAtMost fails unless the X-Drained header is at most n.
func checkDrainedAtMost(n int) func(*testing.T, *http.Response, []byte) {
	return func(t *testing.T, resp *http.Response, _ []byte) {
		got, err := strconv.Atoi(resp.Header.Get("X-Drained"))
		if err != nil {
			t.Fatalf("parsing X-Drained %q: %v", resp.Header.Get("X-Drained"), err)
		}
		if got > n {
			t.Errorf("drained bytes: got = %d, want <= %d", got, n)
		}
	}
}

func TestGatewayBodyBounds(t *testing.T) {
	t.Parallel()

	for _, tc := range []bodyCase{{
		name:       "post under the limit",
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       sized(jsonName(16)),
		wantStatus: http.StatusOK,
	}, {
		name:       "declared length over the limit",
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       sized(jsonName(2 * testBodyLimit)),
		wantStatus: http.StatusRequestEntityTooLarge,
		wantNoRPC:  true,
		check: func(t *testing.T, resp *http.Response, body []byte) {
			if got, want := resp.Header.Get("Content-Type"), "application/json"; got != want {
				t.Errorf("Content-Type: got = %q, want = %q", got, want)
			}
			// The gateway's default error handler writes the status as
			// JSON, with code 8 for ResourceExhausted.
			if !bytes.Contains(body, []byte(`"code":8`)) {
				t.Errorf("body: got = %s, want a ResourceExhausted status", body)
			}
		},
	}, {
		name:       "streamed body over the limit",
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       streamed(jsonName(2 * testBodyLimit)),
		wantStatus: http.StatusRequestEntityTooLarge,
	}, {
		name:       "slow body cut off at the deadline",
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       stalled(`{"name":"`),
		wantStatus: http.StatusRequestTimeout,
	}, {
		name:       "get without a body",
		method:     http.MethodGet,
		path:       "/v1/drain?name=x",
		wantStatus: http.StatusOK,
		check: func(t *testing.T, _ *http.Response, body []byte) {
			if got, want := string(body), "Hello x"; got != want {
				t.Errorf("body: got = %q, want = %q", got, want)
			}
		},
	}, {
		name:       "get without a body held past the timeout",
		method:     http.MethodGet,
		path:       "/v1/hold",
		wantStatus: http.StatusOK,
	}, {
		name:       "post held past the timeout after its body is read",
		method:     http.MethodPost,
		path:       "/v1/hold",
		body:       sized(jsonName(16)),
		wantStatus: http.StatusOK,
	}, {
		name:       "drain-then-forward get declaring an oversize body",
		method:     http.MethodGet,
		path:       "/v1/drain?name=x",
		body:       sized(make([]byte, 2*testBodyLimit)),
		wantStatus: http.StatusRequestEntityTooLarge,
		wantNoRPC:  true,
	}, {
		name:       "drain-then-forward get drain stops at the limit",
		method:     http.MethodGet,
		path:       "/v1/drain?name=x",
		body:       streamed(make([]byte, 64*testBodyLimit)),
		wantStatus: http.StatusOK,
		check:      checkDrainedAtMost(testBodyLimit),
	}, {
		// Over HTTP/1 the failed read cancels the request context, so the
		// forwarded call fails and its 499 is reported as 408. Over HTTP/2
		// only the body read fails; the drain gives up at the deadline and
		// the call goes ahead.
		name:         "drain-then-forward get with a stalled body",
		method:       http.MethodGet,
		path:         "/v1/drain?name=x",
		body:         stalled("x"),
		wantStatus:   http.StatusRequestTimeout,
		wantStatusH2: http.StatusOK,
	}, {
		name:        "form fallback under the limit",
		method:      http.MethodPost,
		path:        "/v1/form",
		contentType: "application/x-www-form-urlencoded",
		body:        sized(formName(16)),
		wantStatus:  http.StatusOK,
		check: func(t *testing.T, _ *http.Response, body []byte) {
			if got, want := string(body), strings.Repeat("a", 16); got != want {
				t.Errorf("body: got = %q, want = %q", got, want)
			}
		},
	}, {
		name:        "form fallback declaring an oversize body",
		method:      http.MethodPost,
		path:        "/v1/form",
		contentType: "application/x-www-form-urlencoded",
		body:        sized(formName(2 * testBodyLimit)),
		wantStatus:  http.StatusRequestEntityTooLarge,
	}, {
		name:        "form fallback streaming an oversize body",
		method:      http.MethodPost,
		path:        "/v1/form",
		contentType: "application/x-www-form-urlencoded",
		body:        streamed(formName(2 * testBodyLimit)),
		wantStatus:  http.StatusRequestEntityTooLarge,
	}, {
		name:        "form fallback with a stalled body",
		method:      http.MethodPost,
		path:        "/v1/form",
		contentType: "application/x-www-form-urlencoded",
		body:        stalled("name="),
		wantStatus:  http.StatusRequestTimeout,
	}, {
		name:        "method override streaming an oversize body",
		method:      http.MethodPost,
		path:        "/v1/form",
		contentType: "application/x-www-form-urlencoded",
		header:      map[string]string{"X-HTTP-Method-Override": http.MethodGet},
		body:        streamed(formName(2 * testBodyLimit)),
		wantStatus:  http.StatusRequestEntityTooLarge,
	}, {
		name:        "method override with a stalled body",
		method:      http.MethodPost,
		path:        "/v1/form",
		contentType: "application/x-www-form-urlencoded",
		header:      map[string]string{"X-HTTP-Method-Override": http.MethodGet},
		body:        stalled("name="),
		wantStatus:  http.StatusRequestTimeout,
	}, {
		name:       "non-positive limit removes the cap",
		opts:       []any{WithMaxRequestBodyBytes(0), WithRequestBodyReadTimeout(testBodyTimeout)},
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       sized(jsonName(2 * testBodyLimit)),
		wantStatus: http.StatusOK,
	}, {
		name:   "non-positive timeout removes the deadline",
		opts:   []any{WithMaxRequestBodyBytes(testBodyLimit), WithRequestBodyReadTimeout(-1)},
		method: http.MethodPost,
		path:   "/v1/example/echo",
		body: func() (io.Reader, func()) {
			pr, pw := io.Pipe()
			go func() {
				// Send the message slower than testBodyTimeout.
				_, _ = pw.Write([]byte(`{"name":"`))
				time.Sleep(2 * testBodyTimeout)
				_, _ = pw.Write([]byte(`x"}`))
				pw.Close()
			}()
			return pr, nil
		},
		wantStatus: http.StatusOK,
	}, {
		name:       "both disabled",
		opts:       []any{WithMaxRequestBodyBytes(0), WithRequestBodyReadTimeout(0)},
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       streamed(jsonName(2 * testBodyLimit)),
		wantStatus: http.StatusOK,
	}} {
		tc.run(t)
	}
}

// consumerBodyLimit is a gateway middleware shaped like mono's
// cgrpc.GatewayBodyLimit, which consumers apply through
// runtime.WithMiddlewares, so it runs inside the Duplex bound.
func consumerBodyLimit(maxBytes int64, readTimeout time.Duration) runtime.ServeMuxOption {
	return runtime.WithMiddlewares(func(next runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			if r.Body == nil || r.Body == http.NoBody {
				next(w, r, pathParams)
				return
			}
			if r.ContentLength > maxBytes {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(readTimeout))
			body := &boundedBody{ReadCloser: http.MaxBytesReader(w, r.Body, maxBytes)}
			r.Body = body
			next(&boundedBodyWriter{ResponseWriter: w, body: body}, r, pathParams)
		}
	})
}

// TestGatewayBodyBoundsWithConsumerMiddleware shows the Duplex bound and a
// consumer's own gateway body middleware compose: whichever bound a request
// breaks, the reply carries the status naming it.
func TestGatewayBodyBoundsWithConsumerMiddleware(t *testing.T) {
	t.Parallel()

	for _, tc := range []bodyCase{{
		name:       "under both limits",
		opts:       append([]any{consumerBodyLimit(testBodyLimit, testBodyTimeout)}, testBounds...),
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       sized(jsonName(16)),
		wantStatus: http.StatusOK,
	}, {
		name:       "streamed over the consumer's smaller limit",
		opts:       append([]any{consumerBodyLimit(testBodyLimit/2, testBodyTimeout)}, testBounds...),
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       streamed(jsonName(3 * testBodyLimit / 4)),
		wantStatus: http.StatusRequestEntityTooLarge,
	}, {
		name:       "declared over the consumer's smaller limit",
		opts:       append([]any{consumerBodyLimit(testBodyLimit/2, testBodyTimeout)}, testBounds...),
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       sized(jsonName(3 * testBodyLimit / 4)),
		wantStatus: http.StatusRequestEntityTooLarge,
		wantNoRPC:  true,
	}, {
		name:       "streamed over the duplex's smaller limit",
		opts:       append([]any{consumerBodyLimit(2*testBodyLimit, testBodyTimeout)}, testBounds...),
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       streamed(jsonName(3 * testBodyLimit / 2)),
		wantStatus: http.StatusRequestEntityTooLarge,
	}, {
		name:       "equal limits",
		opts:       append([]any{consumerBodyLimit(testBodyLimit, testBodyTimeout)}, testBounds...),
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       streamed(jsonName(2 * testBodyLimit)),
		wantStatus: http.StatusRequestEntityTooLarge,
	}, {
		name:       "stalled under the consumer's shorter timeout",
		opts:       append([]any{consumerBodyLimit(testBodyLimit, testBodyTimeout/2)}, testBounds...),
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       stalled(`{"name":"`),
		wantStatus: http.StatusRequestTimeout,
	}, {
		name:       "stalled under the duplex's shorter timeout",
		opts:       []any{consumerBodyLimit(testBodyLimit, 2*testBodyTimeout), WithRequestBodyReadTimeout(testBodyTimeout / 2)},
		method:     http.MethodPost,
		path:       "/v1/example/echo",
		body:       stalled(`{"name":"`),
		wantStatus: http.StatusRequestTimeout,
	}} {
		tc.run(t)
	}
}

// TestGRPCUnaffectedByBodyBounds shows the gateway body bounds do not apply to
// gRPC: a message larger than the body limit is accepted, and a bidirectional
// stream opened before a gateway request misses its body deadline is still
// usable after it, and after idling past the body read timeout.
func TestGRPCUnaffectedByBodyBounds(t *testing.T) {
	d := startDuplex(t, testBounds...)

	conn, err := grpc.NewClient(d.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	name := strings.Repeat("a", 64*testBodyLimit)
	reply, err := pb.NewGreeterClient(conn).SayHello(t.Context(), &pb.HelloRequest{Name: name})
	if err != nil {
		t.Fatalf("SayHello with a message over the body limit: %v", err)
	}
	if got, want := reply.GetMessage(), "Hello "+name; got != want {
		t.Errorf("SayHello reply: got %d bytes, want %d bytes", len(got), len(want))
	}

	stream, err := conn.NewStream(t.Context(), &echoServiceDesc.Streams[0], "/duplextest.Echo/Echo")
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	payload := make([]byte, 2*testBodyLimit)
	echo := func(i int) {
		t.Helper()
		if err := stream.SendMsg(wrapperspb.Bytes(payload)); err != nil {
			t.Fatalf("SendMsg %d: %v", i, err)
		}
		got := new(wrapperspb.BytesValue)
		if err := stream.RecvMsg(got); err != nil {
			t.Fatalf("RecvMsg %d: %v", i, err)
		}
		if len(got.GetValue()) != len(payload) {
			t.Errorf("echo %d: got %d bytes, want %d bytes", i, len(got.GetValue()), len(payload))
		}
	}
	echo(0)

	// A gateway request on each protocol misses its body deadline while the
	// stream is open.
	for _, protoMajor := range []int{1, 2} {
		pr, pw := io.Pipe()
		go func() { _, _ = pw.Write([]byte(`{"name":"`)) }()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+d.addr+"/v1/example/echo", pr)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient(t, protoMajor).Do(req)
		pw.Close()
		if err != nil {
			t.Fatalf("HTTP/%d Do: %v", protoMajor, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestTimeout {
			t.Errorf("HTTP/%d stalled gateway request status: got = %d, want = %d", protoMajor, resp.StatusCode, http.StatusRequestTimeout)
		}
	}
	echo(1)

	// Idle on the open stream for longer than the body read timeout; a read
	// deadline on it would fire here.
	time.Sleep(2 * testBodyTimeout)
	echo(2)

	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := stream.RecvMsg(new(wrapperspb.BytesValue)); !errors.Is(err, io.EOF) {
		t.Errorf("RecvMsg after CloseSend: got = %v, want = %v", err, io.EOF)
	}
}

func TestBodyBoundOptions(t *testing.T) {
	tests := []struct {
		name        string
		opts        []any
		wantBytes   int64
		wantTimeout time.Duration
	}{{
		name:        "defaults",
		wantBytes:   16 << 20,
		wantTimeout: 60 * time.Second,
	}, {
		name:        "overrides",
		opts:        []any{WithMaxRequestBodyBytes(123), WithRequestBodyReadTimeout(time.Minute)},
		wantBytes:   123,
		wantTimeout: time.Minute,
	}, {
		name:        "disabled",
		opts:        []any{WithMaxRequestBodyBytes(0), WithRequestBodyReadTimeout(0)},
		wantBytes:   0,
		wantTimeout: 0,
	}, {
		name:        "last option wins",
		opts:        []any{WithMaxRequestBodyBytes(1), WithMaxRequestBodyBytes(2)},
		wantBytes:   2,
		wantTimeout: DefaultRequestBodyReadTimeout,
	}, {
		name:        "mixed with other option types",
		opts:        []any{grpc.MaxRecvMsgSize(1), WithRequestBodyReadTimeout(time.Second), runtime.WithDisablePathLengthFallback()},
		wantBytes:   DefaultMaxRequestBodyBytes,
		wantTimeout: time.Second,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := New(0, tc.opts...)
			if d.maxRequestBodyBytes != tc.wantBytes {
				t.Errorf("maxRequestBodyBytes: got = %d, want = %d", d.maxRequestBodyBytes, tc.wantBytes)
			}
			if d.requestBodyReadTimeout != tc.wantTimeout {
				t.Errorf("requestBodyReadTimeout: got = %v, want = %v", d.requestBodyReadTimeout, tc.wantTimeout)
			}
		})
	}
}
