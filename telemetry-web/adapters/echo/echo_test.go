package echotel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/stakater/operator-utils/telemetry-web/adaptertest"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

func TestMain(m *testing.M) {
	adaptertest.Setup()
	os.Exit(m.Run())
}

func buildEcho(routes []adaptertest.Route) http.Handler {
	e := echo.New()
	h := Instrument(e)
	for _, r := range routes {
		switch r.Behavior {
		case adaptertest.OK:
			e.GET(r.Template, func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
		case adaptertest.Fail500:
			e.GET(r.Template, func(c echo.Context) error { return c.String(http.StatusInternalServerError, "boom") })
		case adaptertest.Panic:
			e.GET(r.Template, func(c echo.Context) error { panic("kaboom") })
		case adaptertest.PanicAbort:
			e.GET(r.Template, func(c echo.Context) error { panic(http.ErrAbortHandler) })
		}
	}
	return h
}

// TestConformance runs the shared adapter contract: route-templated metrics,
// 500→failure, http.route stamping, panic and ErrAbortHandler semantics.
func TestConformance(t *testing.T) {
	adaptertest.Run(t, buildEcho)
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Echo-specific: a returned non-HTTP error becomes Echo's default 500 and must
// record outcome=failure. Echo runs its error handler after the middleware
// chain, so the returned error — not the status — is the signal here.
func TestMetricsRecordsFailureOnReturnedError(t *testing.T) {
	e := echo.New()
	h := Instrument(e)
	e.GET("/echo/err", func(c echo.Context) error { return errors.New("boom") })

	if rec := get(t, h, "/echo/err"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (echo default error handler)", rec.Code)
	}
	if got := adaptertest.EndpointOutcome(adaptertest.Collect(t), "/echo/err", "failure"); got != 1 {
		t.Errorf("failure count = %d, want 1", got)
	}
}

// Echo-specific: a returned *echo.HTTPError follows its code — 5xx is a
// failure, 4xx is a client error and counts as success.
func TestMetricsClassifiesHTTPErrorsByCode(t *testing.T) {
	e := echo.New()
	h := Instrument(e)
	e.GET("/echo/unavailable", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "down")
	})
	e.GET("/echo/missing/:id", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusNotFound, "no such thing")
	})

	get(t, h, "/echo/unavailable")
	if rec := get(t, h, "/echo/missing/9"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	rm := adaptertest.Collect(t)
	if got := adaptertest.EndpointOutcome(rm, "/echo/unavailable", "failure"); got != 1 {
		t.Errorf("5xx HTTPError failure count = %d, want 1", got)
	}
	if got := adaptertest.EndpointOutcome(rm, "/echo/missing/:id", "success"); got != 1 {
		t.Errorf("4xx HTTPError success count = %d, want 1", got)
	}
	if got := adaptertest.EndpointOutcome(rm, "/echo/missing/:id", "failure"); got != 0 {
		t.Errorf("4xx HTTPError must not be a failure, got %d", got)
	}
}

// Instrument forwards nethttp options, so probe filtering is wireable through
// the adapter's one-call path.
func TestInstrumentForwardsSkipPaths(t *testing.T) {
	e := echo.New()
	h := Instrument(e, nethttp.WithSkipPaths("/echo/skipme"))
	e.GET("/echo/skipme", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })

	if rec := get(t, h, "/echo/skipme"); rec.Code != http.StatusOK {
		t.Fatalf("skipped path must still be served: status = %d", rec.Code)
	}
	if adaptertest.RouteOnDuration(adaptertest.Collect(t), "/echo/skipme") {
		t.Error("skipped path must not appear on the duration metric")
	}
	if adaptertest.RouteOnSpan("/echo/skipme") {
		t.Error("skipped path must not produce a span")
	}
}
