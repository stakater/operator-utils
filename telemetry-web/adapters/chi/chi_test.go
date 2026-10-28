package chitel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/stakater/operator-utils/telemetry-web/internal/adaptertest"
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
	h := Instrument(r)
	sub := chi.NewRouter()
	sub.Get("/users/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Mount("/api", sub)

	before := adaptertest.Collect(t)
	if rec := get(t, h, "/api/users/7"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	after := adaptertest.Collect(t)

	if delta := adaptertest.DurationCount(after, "/api/users/{id}") -
		adaptertest.DurationCount(before, "/api/users/{id}"); delta != 1 {
		t.Errorf("mounted pattern observation delta = %d, want 1", delta)
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
// the adapter's one-call path.
func TestInstrumentForwardsSkipPaths(t *testing.T) {
	adaptertest.Reset()
	r := chi.NewRouter()
	h := Instrument(r, nethttp.WithSkipPaths("/chi/skipme"))
	r.Get("/chi/skipme", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	if rec := get(t, h, "/chi/skipme"); rec.Code != http.StatusOK {
		t.Fatalf("skipped path must still be served: status = %d", rec.Code)
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

// rejectAll answers 401 before chi routes, as authentication middleware does.
func rejectAll(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
}

func okFunc(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

// assertRejected checks one 401 was recorded with route wantRoute, or with no
// route at all when wantRoute is "".
func assertRejected(t *testing.T, h http.Handler, path, wantRoute string) {
	t.Helper()
	count := func() uint64 {
		rm := adaptertest.Collect(t)
		if wantRoute == "" {
			return adaptertest.DurationStatusCountNoRoute(rm, http.StatusUnauthorized)
		}
		return adaptertest.DurationStatusCount(rm, wantRoute, http.StatusUnauthorized)
	}
	before := count()
	if rec := get(t, h, path); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if delta := count() - before; delta != 1 {
		t.Errorf("401 observations for route %q: delta = %d, want 1", wantRoute, delta)
	}
}

// Middleware that answers before chi routes leaves RoutePattern empty;
// RouteTag still names the route chi would have matched, through mounted
// routers too. A path that matches nothing keeps no route.
func TestRouteTagNamesRejectedRequests(t *testing.T) {
	r := chi.NewRouter()
	h := Instrument(r)
	r.Use(rejectAll)
	r.Get("/rej/items/{id}", okFunc)
	sub := chi.NewRouter()
	sub.Get("/users/{id}", okFunc)
	sub.Get("/", okFunc)
	r.Mount("/rej/api", sub)
	r.Get("/rej/slash/", okFunc)

	tests := []struct{ name, path, wantRoute string }{
		{"top-level route", "/rej/items/7", "/rej/items/{id}"},
		{"mounted router", "/rej/api/users/7", "/rej/api/users/{id}"},
		{"no route matches", "/rej/nope", ""},
		{"mount root with trailing slash", "/rej/api/", "/rej/api"},
		{"route registered with trailing slash", "/rej/slash/", "/rej/slash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assertRejected(t, h, tt.path, tt.wantRoute) })
	}
}

// The lookup uses the router chi records in the routing context, with the
// path that router routes on. A context made by hand needs Routes set.
// RouteTag inside a mounted router does no lookup: it keeps the mount's
// pattern, which chi recorded while routing into it, as before.
func TestRouteTagLookupNeedsTheRouter(t *testing.T) {
	handMade := func(setRoutes bool) http.Handler {
		r := chi.NewRouter()
		r.Use(RouteTag(), rejectAll)
		r.Get("/ctx/items/{id}", okFunc)
		return nethttp.Handler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			rctx := chi.NewRouteContext()
			if setRoutes {
				rctx.Routes = r
			}
			r.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
		}))
	}
	mounted := func() http.Handler {
		r := chi.NewRouter()
		r.Route("/ctx", func(p chi.Router) {
			p.Use(RouteTag(), rejectAll)
			p.Get("/items/{id}", okFunc)
		})
		return nethttp.Handler(r)
	}

	tests := []struct {
		name      string
		h         http.Handler
		wantRoute string
	}{
		{"hand-made context with Routes", handMade(true), "/ctx/items/{id}"},
		{"hand-made context without Routes", handMade(false), ""},
		{"RouteTag inside a mounted router", mounted(), "/ctx/*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assertRejected(t, tt.h, "/ctx/items/7", tt.wantRoute) })
	}
}

// A rejected request must name the same route as a served one, including for
// patterns chi trims a trailing "/" from.
func TestRejectedMatchesServedRoute(t *testing.T) {
	build := func(reject bool) http.Handler {
		r := chi.NewRouter()
		h := Instrument(r)
		if reject {
			r.Use(rejectAll)
		}
		sub := chi.NewRouter()
		sub.Get("/", okFunc)
		r.Mount("/rej/api", sub)
		r.Get("/rej/slash/", okFunc)
		return h
	}
	served, rejected := build(false), build(true)

	for _, tt := range []struct{ path, route string }{
		{"/rej/api/", "/rej/api"},
		{"/rej/slash/", "/rej/slash"},
	} {
		t.Run(tt.route, func(t *testing.T) {
			before := adaptertest.Collect(t)
			if rec := get(t, served, tt.path); rec.Code != http.StatusOK {
				t.Fatalf("served status = %d, want 200", rec.Code)
			}
			after := adaptertest.Collect(t)
			if delta := adaptertest.DurationStatusCount(after, tt.route, http.StatusOK) -
				adaptertest.DurationStatusCount(before, tt.route, http.StatusOK); delta != 1 {
				t.Errorf("served observations on %q = %d, want 1", tt.route, delta)
			}
			assertRejected(t, rejected, tt.path, tt.route)
		})
	}
}

// A method the route does not allow matches nothing, so the rejection
// carries no route.
func TestRejectedWrongMethodGetsNoRoute(t *testing.T) {
	r := chi.NewRouter()
	h := Instrument(r)
	r.Use(rejectAll)
	r.Get("/rej/wrong/{id}", okFunc)

	before := adaptertest.DurationStatusCountNoRoute(adaptertest.Collect(t), http.StatusUnauthorized)
	req := httptest.NewRequest(http.MethodPost, "/rej/wrong/7", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	after := adaptertest.DurationStatusCountNoRoute(adaptertest.Collect(t), http.StatusUnauthorized)

	if delta := after - before; delta != 1 {
		t.Errorf("unrouted 401 observations = %d, want 1", delta)
	}
}
