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
	case adaptertest.Panic:
		return func(w http.ResponseWriter, r *http.Request) { panic("kaboom") }
	case adaptertest.PanicAbort:
		return func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }
	default:
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	}
}

func buildChi(routes []adaptertest.Route) http.Handler {
	r := chi.NewRouter()
	h := Instrument(r)
	for _, rt := range routes {
		r.Get(rt.Template, handlerFor(rt.Behavior))
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

	if rec := get(t, h, "/api/users/7"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := adaptertest.EndpointOutcome(adaptertest.Collect(t), "/api/users/{id}", "success"); got != 1 {
		t.Errorf("mounted pattern success count = %d, want 1", got)
	}
}

// Instrument forwards nethttp options, so probe filtering is wireable through
// the adapter's one-call path.
func TestInstrumentForwardsSkipPaths(t *testing.T) {
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
