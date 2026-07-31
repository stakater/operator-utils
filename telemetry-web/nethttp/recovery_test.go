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

// Skipped reports the WithSkipPaths decision to middleware running inside the
// router, which otelhttp's own filter cannot reach.
func TestSkippedReflectsSkipPaths(t *testing.T) {
	var skippedSeen, normalSeen bool
	probe := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			skippedSeen = Skipped(r.Context())
		} else {
			normalSeen = Skipped(r.Context())
		}
	})
	h := Handler(probe, WithSkipPaths("/healthz"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api", nil))

	if !skippedSeen {
		t.Error("Skipped must be true for an excluded path")
	}
	if normalSeen {
		t.Error("Skipped must be false for a normal path")
	}
}

// Repeated WithSkipPaths calls accumulate rather than the last one winning.
func TestWithSkipPathsAccumulates(t *testing.T) {
	s := Resolve(WithSkipPaths("/a"), WithSkipPaths("/b", "/c"))
	if len(s.SkipPaths) != 3 {
		t.Fatalf("SkipPaths = %v, want all three accumulated", s.SkipPaths)
	}
}

func TestResolveDefaults(t *testing.T) {
	s := Resolve()
	if !s.Recovery {
		t.Error("recovery must be on by default")
	}
	if s.EndpointMetrics {
		t.Error("endpoint metrics must be off by default")
	}
	if len(s.SkipPaths) != 0 {
		t.Error("nothing must be skipped by default")
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
