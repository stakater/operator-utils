package gintel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/stakater/operator-utils/telemetry-web/internal/adaptertest"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	adaptertest.Setup()
	os.Exit(m.Run())
}

func handlerFor(b adaptertest.Behavior) gin.HandlerFunc {
	switch b {
	case adaptertest.OK:
		return func(c *gin.Context) { c.String(http.StatusOK, "ok") }
	case adaptertest.Fail500:
		return func(c *gin.Context) { c.String(http.StatusInternalServerError, "boom") }
	case adaptertest.Fail400:
		return func(c *gin.Context) { c.String(http.StatusBadRequest, "bad") }
	case adaptertest.Panic:
		return func(c *gin.Context) { panic("kaboom") }
	case adaptertest.PanicAbort:
		return func(c *gin.Context) { panic(http.ErrAbortHandler) }
	case adaptertest.Stream:
		return func(c *gin.Context) {
			for i := range len(adaptertest.StreamBody) {
				_, _ = c.Writer.WriteString(adaptertest.StreamBody[i : i+1])
				c.Writer.Flush()
			}
		}
	default:
		// Silently skipping an unknown Behavior would leave the route
		// unregistered and the conformance case asserting on a 404.
		panic("unhandled adaptertest.Behavior")
	}
}

func buildGin(routes []adaptertest.Route, opts ...nethttp.Option) http.Handler {
	e := gin.New()
	h := Instrument(e, opts...)
	for _, r := range routes {
		e.Handle(adaptertest.MethodOf(r), r.Template, handlerFor(r.Behavior))
	}
	return h
}

// TestConformance runs the shared adapter contract: route-templated metrics,
// 500→failure, 4xx→success, http.route stamping, skip paths, panic and
// ErrAbortHandler semantics.
func TestConformance(t *testing.T) {
	adaptertest.Run(t, buildGin)
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Gin applies global middleware only to routes registered afterward, so a late
// Instrument silently instruments nothing. It must fail loudly instead.
func TestInstrumentAfterRoutesPanics(t *testing.T) {
	e := gin.New()
	e.GET("/gin/early", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	defer func() {
		if recover() == nil {
			t.Error("Instrument after route registration must panic")
		}
	}()
	Instrument(e)
}

// Instrument forwards nethttp options, so probe filtering is wireable through
// the adapter's one-call path.
func TestInstrumentForwardsSkipPaths(t *testing.T) {
	adaptertest.Reset()
	e := gin.New()
	h := Instrument(e, nethttp.WithSkipPaths("/gin/skipme"))
	e.GET("/gin/skipme", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	if rec := get(t, h, "/gin/skipme"); rec.Code != http.StatusOK {
		t.Fatalf("skipped path must still be served: status = %d", rec.Code)
	}
	if adaptertest.RouteOnDuration(adaptertest.Collect(t), "/gin/skipme") {
		t.Error("skipped path must not appear on the duration metric")
	}
	if adaptertest.RouteOnSpan("/gin/skipme") {
		t.Error("skipped path must not produce a span")
	}
}

// WithoutRecovery must mean no recovery at all, not "the framework one instead".
// Instrument previously resolved the option and then ignored it.
func TestWithoutRecoveryHonoredThroughInstrument(t *testing.T) {
	e := gin.New()
	h := Instrument(e, nethttp.WithoutRecovery())
	e.GET("/gin/escape", func(c *gin.Context) { panic("mine to handle") })

	before := adaptertest.PanicCount(adaptertest.Collect(t))
	escaped := func() (escaped bool) {
		defer func() { escaped = recover() != nil }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/gin/escape", nil))
		return false
	}()
	after := adaptertest.PanicCount(adaptertest.Collect(t))

	if !escaped {
		t.Error("WithoutRecovery must let the panic escape the adapter")
	}
	if delta := after - before; delta != 0 {
		t.Errorf("WithoutRecovery must record no panic, got delta %d", delta)
	}
}

// Middleware registered with engine.Use BEFORE Instrument runs upstream of
// gintel.Recovery — and Instrument's guard cannot catch it, since middleware
// creates no routes. Only nethttp.Handler's recovery is outside the engine,
// which is why Instrument keeps it.
func TestPreInstrumentMiddlewarePanicIsRecoveredAndCounted(t *testing.T) {
	e := gin.New()
	e.Use(func(*gin.Context) { panic("pre-instrument boom") })
	h := Instrument(e)
	e.GET("/gin/pre", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	before := adaptertest.PanicCount(adaptertest.Collect(t))
	rec := get(t, h, "/gin/pre")
	after := adaptertest.PanicCount(adaptertest.Collect(t))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if delta := after - before; delta != 1 {
		t.Errorf("panic counter delta = %d, want 1", delta)
	}
}
