package echotel

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/stakater/operator-utils/telemetry-web/internal/adaptertest"
	"github.com/stakater/operator-utils/telemetry-web/nethttp"
)

func TestMain(m *testing.M) {
	adaptertest.Setup()
	os.Exit(m.Run())
}

func handlerFor(b adaptertest.Behavior) echo.HandlerFunc {
	switch b {
	case adaptertest.OK:
		return func(c echo.Context) error { return c.String(http.StatusOK, "ok") }
	case adaptertest.Fail500:
		return func(c echo.Context) error { return c.String(http.StatusInternalServerError, "boom") }
	case adaptertest.Fail400:
		return func(c echo.Context) error { return c.String(http.StatusBadRequest, "bad") }
	case adaptertest.Panic:
		return func(c echo.Context) error { panic("kaboom") }
	case adaptertest.PanicAbort:
		return func(c echo.Context) error { panic(http.ErrAbortHandler) }
	case adaptertest.Stream:
		return func(c echo.Context) error {
			for i := range len(adaptertest.StreamBody) {
				if _, err := c.Response().Write([]byte(adaptertest.StreamBody[i : i+1])); err != nil {
					return err
				}
				c.Response().Flush()
			}
			return nil
		}
	default:
		// Silently skipping an unknown Behavior would leave the route
		// unregistered and the conformance case asserting on a 404.
		panic("unhandled adaptertest.Behavior")
	}
}

func buildEcho(routes []adaptertest.Route, opts ...nethttp.Option) http.Handler {
	e := echo.New()
	h := Instrument(e, opts...)
	for _, r := range routes {
		e.Add(adaptertest.MethodOf(r), r.Template, handlerFor(r.Behavior))
	}
	return h
}

// TestConformance runs the shared adapter contract: route-templated metrics,
// 500→failure, 4xx→success, http.route stamping, skip paths, panic and
// ErrAbortHandler semantics.
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

// Instrument forwards nethttp options, so probe filtering is wireable through
// the adapter's one-call path.
func TestInstrumentForwardsSkipPaths(t *testing.T) {
	adaptertest.Reset()
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

// WithoutRecovery must mean no recovery at all, not "the framework one instead".
func TestWithoutRecoveryHonoredThroughInstrument(t *testing.T) {
	e := echo.New()
	h := Instrument(e, nethttp.WithoutRecovery())
	e.GET("/echo/escape", func(c echo.Context) error { panic("mine to handle") })

	before := adaptertest.PanicCount(adaptertest.Collect(t))
	escaped := func() (escaped bool) {
		defer func() { escaped = recover() != nil }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/echo/escape", nil))
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

// Echo runs e.Pre middleware before the entire Use chain, so it is upstream of
// echotel.Recovery. Only nethttp.Handler's recovery is outside the engine, which
// is why Instrument keeps it: without that layer a panic here escapes to
// net/http with no metric, no span error, and no response.
func TestPrePanicIsRecoveredAndCounted(t *testing.T) {
	e := echo.New()
	e.Pre(func(echo.HandlerFunc) echo.HandlerFunc {
		return func(echo.Context) error { panic("pre-chain boom") }
	})
	h := Instrument(e)
	e.GET("/echo/pre", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })

	before := adaptertest.PanicCount(adaptertest.Collect(t))
	rec := get(t, h, "/echo/pre")
	after := adaptertest.PanicCount(adaptertest.Collect(t))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if delta := after - before; delta != 1 {
		t.Errorf("panic counter delta = %d, want 1", delta)
	}
}

// Echo runs its error handler AFTER the middleware chain, so the status a request
// answers with is written outside the handler. The recorded status has to be the
// one the client saw, across every shape Echo resolves differently — otherwise
// nothing in the chain (Recovery's httpsnoop wrapping, otelhttp's own writer)
// can be trusted to observe a late WriteHeader.
func TestDurationMetricRecordsClientVisibleStatus(t *testing.T) {
	cases := []struct {
		name    string
		handler echo.HandlerFunc
		want    int
	}{
		// Echo does not unwrap, so a wrapped HTTPError answers 500, not 404.
		{"wrapped HTTPError", func(echo.Context) error {
			return fmt.Errorf("load user: %w", echo.NewHTTPError(http.StatusNotFound))
		}, http.StatusInternalServerError},
		// Echo unwraps Internal one level and answers with the inner code.
		{"SetInternal", func(echo.Context) error {
			return echo.NewHTTPError(500).SetInternal(echo.NewHTTPError(http.StatusNotFound))
		}, http.StatusNotFound},
		{"plain HTTPError", func(echo.Context) error {
			return echo.NewHTTPError(http.StatusNotFound)
		}, http.StatusNotFound},
		{"opaque error", func(echo.Context) error {
			return errors.New("boom")
		}, http.StatusInternalServerError},
		// Already committed: Echo's error handler bails out, so the client keeps 200
		// even though a non-nil error came back.
		{"committed then error", func(c echo.Context) error {
			_ = c.JSON(http.StatusOK, map[string]string{"ok": "yes"})
			return errors.New("too late to matter")
		}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			h := Instrument(e)
			e.GET("/echo/status", tc.handler)

			before := adaptertest.Collect(t)
			rec := get(t, h, "/echo/status")
			if rec.Code != tc.want {
				t.Fatalf("Echo answered %d, want %d — the test's premise is wrong", rec.Code, tc.want)
			}
			after := adaptertest.Collect(t)

			got := adaptertest.DurationStatusCount(after, "/echo/status", tc.want) -
				adaptertest.DurationStatusCount(before, "/echo/status", tc.want)
			if got != 1 {
				t.Errorf("observations at status=%d = %d, want 1", tc.want, got)
			}
		})
	}
}
