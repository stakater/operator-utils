package chitel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/stakater/operator-utils/telemetry-web/adaptertest"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

func TestMain(m *testing.M) {
	adaptertest.Setup()
	os.Exit(m.Run())
}

var paramRe = regexp.MustCompile(`:(\w+)`)

// chiTemplate rewrites the suite's canonical ":param" syntax to chi's "{param}".
func chiTemplate(t string) string {
	return paramRe.ReplaceAllString(t, `{$1}`)
}

func handlerFor(b adaptertest.Behavior) http.HandlerFunc {
	switch b {
	case adaptertest.Fail500:
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	case adaptertest.Fail400:
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) }
	case adaptertest.Panic:
		return func(w http.ResponseWriter, r *http.Request) { panic("kaboom") }
	case adaptertest.PanicAbort:
		return func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }
	case adaptertest.Stream:
		return func(w http.ResponseWriter, _ *http.Request) {
			for i := range len(adaptertest.StreamBody) {
				_, _ = w.Write([]byte(adaptertest.StreamBody[i : i+1]))
				w.(http.Flusher).Flush()
			}
		}
	case adaptertest.OK:
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	default:
		// A default that quietly behaved like OK would let a new Behavior added to
		// the suite pass here while testing nothing.
		panic("unhandled adaptertest.Behavior")
	}
}

func buildChi(routes []adaptertest.Route, opts ...nethttp.Option) http.Handler {
	r := chi.NewRouter()
	h := Instrument(r, opts...)
	for _, rt := range routes {
		r.Method(adaptertest.MethodOf(rt), rt.Template, handlerFor(rt.Behavior))
	}
	return h
}

// TestConformance runs the shared adapter contract with chi's {param} syntax.
func TestConformance(t *testing.T) {
	adaptertest.Run(t, buildChi, adaptertest.WithTemplateRewrite(chiTemplate))
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// chi-specific: mounted subrouters record the joined pattern (/api/users/{id}
// on chi >= v5.2) as-is — truthful and cardinality-bounded.
func TestMountedSubrouterPatternRecordedAsIs(t *testing.T) {
	r := chi.NewRouter()
	h := Instrument(r, nethttp.WithEndpointMetrics())
	sub := chi.NewRouter()
	sub.Get("/users/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Mount("/api", sub)

	before := adaptertest.Collect(t)
	if rec := get(t, h, "/api/users/7"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	after := adaptertest.Collect(t)

	if delta := adaptertest.EndpointOutcome(after, "/api/users/{id}", "success") -
		adaptertest.EndpointOutcome(before, "/api/users/{id}", "success"); delta != 1 {
		t.Errorf("mounted pattern success delta = %d, want 1", delta)
	}
}

// chi fills RoutePattern in before the handler runs, so a deferred stamp
// survives a panic unwinding past it — the route is not lost on panicked
// requests, matching gin and echo.
func TestRouteStampedOnPanickedRequest(t *testing.T) {
	adaptertest.Reset()
	r := chi.NewRouter()
	h := Instrument(r)
	r.Get("/chi/boom/{id}", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	if rec := get(t, h, "/chi/boom/7"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !adaptertest.RouteOnSpan("/chi/boom/{id}") {
		t.Error("panicked request must still carry http.route on its span")
	}
}

// Endpoint metrics are opt-in: without WithEndpointMetrics no counter is
// emitted, only the otelhttp duration histogram.
func TestEndpointMetricsAreOptIn(t *testing.T) {
	r := chi.NewRouter()
	h := Instrument(r)
	r.Get("/chi/optin", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	before := adaptertest.EndpointTotal(adaptertest.Collect(t))
	get(t, h, "/chi/optin")
	after := adaptertest.EndpointTotal(adaptertest.Collect(t))

	if after != before {
		t.Errorf("endpoint.requests must be off by default, got delta %d", after-before)
	}
	if !adaptertest.RouteOnDuration(adaptertest.Collect(t), "/chi/optin") {
		t.Error("the duration histogram must still carry http.route")
	}
}

// A panic must be counted exactly once. Instrument deliberately does not
// install chitel.Recovery, because nethttp.Handler already recovers and
// chitel.Recovery IS nethttp.Recovery — installing both double-counts.
func TestPanicCountedOnce(t *testing.T) {
	r := chi.NewRouter()
	h := Instrument(r)
	r.Get("/chi/once", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	before := adaptertest.PanicCount(adaptertest.Collect(t))
	get(t, h, "/chi/once")
	after := adaptertest.PanicCount(adaptertest.Collect(t))

	if delta := after - before; delta != 1 {
		t.Errorf("panic counter delta = %d, want exactly 1", delta)
	}
}

// Instrument forwards nethttp options, so probe filtering is wireable through
// the adapter's one-call path — and the exclusion must cover the per-endpoint
// counter, not just otelhttp's own metrics.
func TestInstrumentForwardsSkipPaths(t *testing.T) {
	adaptertest.Reset()
	r := chi.NewRouter()
	h := Instrument(r, nethttp.WithEndpointMetrics(), nethttp.WithSkipPaths("/chi/skipme"))
	r.Get("/chi/skipme", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	before := adaptertest.EndpointTotal(adaptertest.Collect(t))
	if rec := get(t, h, "/chi/skipme"); rec.Code != http.StatusOK {
		t.Fatalf("skipped path must still be served: status = %d", rec.Code)
	}
	after := adaptertest.EndpointTotal(adaptertest.Collect(t))

	if after != before {
		t.Errorf("skipped path must not record endpoint.requests, got delta %d", after-before)
	}
	if adaptertest.RouteOnDuration(adaptertest.Collect(t), "/chi/skipme") {
		t.Error("skipped path must not appear on the duration metric")
	}
	if adaptertest.RouteOnSpan("/chi/skipme") {
		t.Error("skipped path must not produce a span")
	}
}

// chi installs no recovery of its own, so WithoutRecovery leaves the router with
// none at all and the panic reaches net/http.
func TestWithoutRecoveryHonoredThroughInstrument(t *testing.T) {
	r := chi.NewRouter()
	h := Instrument(r, nethttp.WithoutRecovery())
	r.Get("/chi/escape", func(http.ResponseWriter, *http.Request) { panic("mine to handle") })

	before := adaptertest.PanicCount(adaptertest.Collect(t))
	escaped := func() (escaped bool) {
		defer func() { escaped = recover() != nil }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/chi/escape", nil))
		return false
	}()
	after := adaptertest.PanicCount(adaptertest.Collect(t))

	if !escaped {
		t.Error("WithoutRecovery must let the panic escape")
	}
	if delta := after - before; delta != 0 {
		t.Errorf("WithoutRecovery must record no panic, got delta %d", delta)
	}
}

// Recovery is exported for hand-built chains but Instrument does not install
// it, so nothing else here exercises the behavior its doc promises. Paired with
// nethttp.WithoutRecovery this is the supported way to own recovery yourself.
func TestExportedRecoveryHandlesPanics(t *testing.T) {
	r := chi.NewRouter()
	r.Use(Recovery())
	h := Instrument(r, nethttp.WithoutRecovery())
	r.Get("/chi/own", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	before := adaptertest.PanicCount(adaptertest.Collect(t))
	if rec := get(t, h, "/chi/own"); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if delta := adaptertest.PanicCount(adaptertest.Collect(t)) - before; delta != 1 {
		t.Errorf("panic counter delta = %d, want 1", delta)
	}
}

// ...and it re-raises ErrAbortHandler untouched rather than counting it.
func TestExportedRecoveryReRaisesErrAbortHandler(t *testing.T) {
	r := chi.NewRouter()
	r.Use(Recovery())
	h := Instrument(r, nethttp.WithoutRecovery())
	r.Get("/chi/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })

	before := adaptertest.PanicCount(adaptertest.Collect(t))
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity
			t.Errorf("ErrAbortHandler must propagate, got %v", rec)
		}
		if delta := adaptertest.PanicCount(adaptertest.Collect(t)) - before; delta != 0 {
			t.Errorf("ErrAbortHandler must not be counted, got delta %d", delta)
		}
	}()
	get(t, h, "/chi/abort")
}
