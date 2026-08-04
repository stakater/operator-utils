package nethttp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Recovery is exercised indirectly by the adapter conformance suite, but that
// runs in the adapter modules. These cover the core middleware directly.

func TestRecoveryWrites500AndSwallowsPanic(t *testing.T) {
	h := Recovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestRecoveryReRaisesErrAbortHandler(t *testing.T) {
	h := Recovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity
			t.Errorf("ErrAbortHandler must propagate, got %v", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort", nil))
}

// A handler that panics after writing its response must not have a second
// header written over it — that is net/http's "superfluous WriteHeader" noise.
func TestRecoveryDoesNotOverwriteAWrittenStatus(t *testing.T) {
	h := Recovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("partial"))
		panic("late failure")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/late", nil))

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want the already-written 202", rec.Code)
	}
}

// A panic before anything is written still yields 500 even though the handler
// wrote a body-less response.
func TestRecoveryWrites500WhenOnlyBodyPending(t *testing.T) {
	h := Recovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("nothing written yet")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// WithoutRecovery lets an outer layer own panic handling, so the panic must
// escape Handler untouched.
func TestWithoutRecoveryLetsPanicEscape(t *testing.T) {
	h := Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("mine to handle")
	}), WithoutRecovery())

	defer func() {
		if recover() == nil {
			t.Error("WithoutRecovery must let the panic escape")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}

// StampRoute is documented to require nethttp.Handler. Outside it there is no
// otelhttp labeler, and it must degrade rather than panic: the span attribute
// still lands.
func TestStampRouteWithoutLabelerStillSetsSpanAttribute(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := tp.Tracer("test").Start(context.Background(), "server")

	StampRoute(ctx, http.MethodGet, "/users/:id") // no labeler in ctx
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 span, got %d", len(ended))
	}
	if got := ended[0].Name(); got != "GET /users/:id" {
		t.Errorf("span name = %q, want %q", got, "GET /users/:id")
	}
	var found bool
	for _, kv := range ended[0].Attributes() {
		if kv.Key == "http.route" && kv.Value.AsString() == "/users/:id" {
			found = true
		}
	}
	if !found {
		t.Error("http.route must still reach the span without a labeler")
	}
}

// The skip decision must reach middleware running inside the router, which
// otelhttp's own filter cannot.
func TestSkippedReflectsSkipPaths(t *testing.T) {
	var skippedSeen, normalSeen bool
	probe := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			skippedSeen = skipped(r.Context())
		} else {
			normalSeen = skipped(r.Context())
		}
	})
	h := Handler(probe, WithSkipPaths("/healthz"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api", nil))

	if !skippedSeen {
		t.Error("skipped must be true for an excluded path")
	}
	if normalSeen {
		t.Error("skipped must be false for a normal path")
	}
}

// Repeated WithSkipPaths calls accumulate rather than the last one winning.
// Asserted through Handler, since Settings does not carry the paths.
func TestWithSkipPathsAccumulates(t *testing.T) {
	seen := map[string]bool{}
	probe := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = skipped(r.Context())
	})
	h := Handler(probe, WithSkipPaths("/a"), WithSkipPaths("/b", "/c"))

	for _, p := range []string{"/a", "/b", "/c", "/kept"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	for _, p := range []string{"/a", "/b", "/c"} {
		if !seen[p] {
			t.Errorf("%s not skipped: a later WithSkipPaths overwrote an earlier one", p)
		}
	}
	if seen["/kept"] {
		t.Error("/kept must not be skipped")
	}
}

func TestResolveDefaults(t *testing.T) {
	s := Resolve()
	if !s.Recovery {
		t.Error("recovery must be on by default")
	}
}

// WrapClient adds propagation in place, and Transport(nil) falls back to the
// default transport rather than a nil round tripper.
func TestWrapClientAndTransportFallback(t *testing.T) {
	if Transport(nil) == nil {
		t.Error("Transport(nil) must return a usable RoundTripper")
	}

	c := &http.Client{}
	if got := WrapClient(c); got != c {
		t.Error("WrapClient must wrap in place and return the same client")
	}
	if c.Transport == nil {
		t.Error("WrapClient must install a propagating transport")
	}

	if HTTPClient().Transport == nil {
		t.Error("HTTPClient must come with a transport")
	}
}

// WrapClient(nil) must behave like Transport(nil) and yield something usable,
// rather than nil-dereferencing on c.Transport. Two adjacent functions in the same
// public API taking nil differently is the kind of asymmetry a caller threading an
// optional client through discovers in production.
func TestWrapClientNil(t *testing.T) {
	got := WrapClient(nil)
	if got == nil {
		t.Fatal("WrapClient(nil) must return a usable client")
	}
	if got.Transport == nil {
		t.Error("WrapClient(nil) must return a propagating client")
	}
}

// Wrapping twice would nest one otelhttp transport in another and inject the
// propagation headers twice. Easy to reach: a helper that wraps defensively plus a
// caller that already used HTTPClient().
func TestWrapClientIsIdempotent(t *testing.T) {
	c := &http.Client{}
	WrapClient(c)
	first := c.Transport

	WrapClient(c)
	if c.Transport != first {
		t.Error("a second WrapClient re-wrapped the transport, double-injecting trace headers")
	}

	// An already-instrumented client from HTTPClient must be left alone too.
	hc := HTTPClient()
	before := hc.Transport
	if WrapClient(hc).Transport != before {
		t.Error("WrapClient re-wrapped an HTTPClient transport")
	}
}

// optionalIfaces reports which optional ResponseWriter interfaces reach the
// handler. Uses a real server: httptest.ResponseRecorder is a Flusher but never
// a Hijacker, which would hide exactly the regression this guards.
func optionalIfaces(t *testing.T, wrap func(http.Handler) http.Handler) (flusher, hijacker bool) {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flusher = w.(http.Flusher)
		_, hijacker = w.(http.Hijacker)
	})
	var h http.Handler = inner
	if wrap != nil {
		h = wrap(inner)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	return flusher, hijacker
}

// Recovery must not mask the optional interfaces. Embedding the
// http.ResponseWriter interface promotes only Header/Write/WriteHeader, which
// makes gin's c.Writer.Flush() and echo's Response.Flush() panic on a type
// assertion, and breaks WebSocket upgrades.
func TestRecoveryPreservesFlusherAndHijacker(t *testing.T) {
	if f, hj := optionalIfaces(t, nil); !f || !hj {
		t.Fatalf("baseline lost interfaces: Flusher=%v Hijacker=%v", f, hj)
	}
	if f, hj := optionalIfaces(t, Recovery); !f || !hj {
		t.Errorf("Recovery masked interfaces: Flusher=%v Hijacker=%v, want both true", f, hj)
	}
}

// Same guarantee through the full composed chain, which is what every adapter
// and consumer actually serves.
func TestHandlerPreservesFlusherAndHijacker(t *testing.T) {
	wrap := func(n http.Handler) http.Handler { return Handler(n) }
	if f, hj := optionalIfaces(t, wrap); !f || !hj {
		t.Errorf("Handler masked interfaces: Flusher=%v Hijacker=%v, want both true", f, hj)
	}
}

// A flushing handler must actually stream, not just satisfy the assertion.
func TestHandlerSupportsStreaming(t *testing.T) {
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 3 {
			_, _ = w.Write([]byte{byte('a' + i)})
			w.(http.Flusher).Flush()
		}
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "abc" {
		t.Errorf("streamed body = %q, want %q", body, "abc")
	}
}

// A handler that hijacks the connection and then panics must not have a 500
// written into the hijacked stream — the connection is no longer ours to answer
// on. The Hijack hook is what marks the response as already committed.
func TestRecoveryDoesNotWriteAfterHijack(t *testing.T) {
	h := Recovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n\r\nhijacked")
		_ = buf.Flush()
		_ = conn.Close()
		panic("after hijack")
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "101 Switching Protocols") {
		t.Errorf("hijacked response missing: %q", got)
	}
	if strings.Contains(got, "500") {
		t.Errorf("Recovery wrote a 500 into the hijacked connection: %q", got)
	}
}

// Flush and ReadFrom commit an implicit 200 in net/http, and httpsnoop passes
// unhooked methods straight through, so trackWrites has to hook them too. Without
// that, a panic after io.Copy or after flushing headers makes Recovery write a
// pointless 500 over a response the client already received in full.
func TestTrackWritesSeesFlushAndReadFrom(t *testing.T) {
	body := strings.Repeat("x", 4096)
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantLen int
	}{
		{"io.Copy selects ReadFrom", func(w http.ResponseWriter, _ *http.Request) {
			if _, err := io.Copy(w, strings.NewReader(body)); err != nil {
				t.Error(err)
			}
			panic("late boom")
		}, len(body)},
		{"Flush commits the header", func(w http.ResponseWriter, _ *http.Request) {
			w.(http.Flusher).Flush()
			panic("sse boom")
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var responded func() bool
			probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var tracked http.ResponseWriter
				tracked, responded = trackWrites(w)
				defer func() { _ = recover() }()
				tc.handler(tracked, r)
			})
			srv := httptest.NewServer(probe)
			defer srv.Close()

			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK || len(got) != tc.wantLen {
				t.Fatalf("client saw %d with %d bytes, want 200 with %d", resp.StatusCode, len(got), tc.wantLen)
			}
			if !responded() {
				t.Error("responded() = false after the response was committed, so Recovery would write a spurious 500")
			}
		})
	}
}
