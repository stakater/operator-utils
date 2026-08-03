package echotel

import (
	"errors"
	"fmt"
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

// Echo-specific: a returned non-HTTP error becomes Echo's default 500 and must
// record outcome=failure. Echo runs its error handler after the middleware
// chain, so the returned error — not the not-yet-written status — is the signal.
func TestMetricsRecordsFailureOnReturnedError(t *testing.T) {
	e := echo.New()
	h := Instrument(e, nethttp.WithEndpointMetrics())
	e.GET("/echo/err", func(c echo.Context) error { return errors.New("boom") })

	before := adaptertest.Collect(t)
	if rec := get(t, h, "/echo/err"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (echo default error handler)", rec.Code)
	}
	after := adaptertest.Collect(t)

	if delta := adaptertest.EndpointOutcome(after, "/echo/err", "failure") -
		adaptertest.EndpointOutcome(before, "/echo/err", "failure"); delta != 1 {
		t.Errorf("failure count delta = %d, want 1", delta)
	}
}

// Echo-specific: a returned *echo.HTTPError follows its code — 5xx is a
// failure, 4xx is a client error and counts as success, matching gin and chi.
func TestMetricsClassifiesHTTPErrorsByCode(t *testing.T) {
	e := echo.New()
	h := Instrument(e, nethttp.WithEndpointMetrics())
	e.GET("/echo/unavailable", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "down")
	})
	e.GET("/echo/missing/:id", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusNotFound, "no such thing")
	})

	before := adaptertest.Collect(t)
	get(t, h, "/echo/unavailable")
	if rec := get(t, h, "/echo/missing/9"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	after := adaptertest.Collect(t)

	if delta := adaptertest.EndpointOutcome(after, "/echo/unavailable", "failure") -
		adaptertest.EndpointOutcome(before, "/echo/unavailable", "failure"); delta != 1 {
		t.Errorf("5xx HTTPError failure delta = %d, want 1", delta)
	}
	if delta := adaptertest.EndpointOutcome(after, "/echo/missing/:id", "success") -
		adaptertest.EndpointOutcome(before, "/echo/missing/:id", "success"); delta != 1 {
		t.Errorf("4xx HTTPError success delta = %d, want 1", delta)
	}
	if delta := adaptertest.EndpointOutcome(after, "/echo/missing/:id", "failure") -
		adaptertest.EndpointOutcome(before, "/echo/missing/:id", "failure"); delta != 0 {
		t.Errorf("4xx HTTPError must not be a failure, got delta %d", delta)
	}
}

// Endpoint metrics are opt-in: without WithEndpointMetrics no counter is
// emitted, only the otelhttp duration histogram.
func TestEndpointMetricsAreOptIn(t *testing.T) {
	e := echo.New()
	h := Instrument(e)
	e.GET("/echo/optin", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })

	before := adaptertest.EndpointTotal(adaptertest.Collect(t))
	get(t, h, "/echo/optin")
	after := adaptertest.EndpointTotal(adaptertest.Collect(t))

	if after != before {
		t.Errorf("endpoint.requests must be off by default, got delta %d", after-before)
	}
	if !adaptertest.RouteOnDuration(adaptertest.Collect(t), "/echo/optin") {
		t.Error("the duration histogram must still carry http.route")
	}
}

// Instrument forwards nethttp options, so probe filtering is wireable through
// the adapter's one-call path — and the exclusion must cover the per-endpoint
// counter, not just otelhttp's own metrics.
func TestInstrumentForwardsSkipPaths(t *testing.T) {
	adaptertest.Reset()
	e := echo.New()
	h := Instrument(e, nethttp.WithEndpointMetrics(), nethttp.WithSkipPaths("/echo/skipme"))
	e.GET("/echo/skipme", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })

	before := adaptertest.EndpointTotal(adaptertest.Collect(t))
	if rec := get(t, h, "/echo/skipme"); rec.Code != http.StatusOK {
		t.Fatalf("skipped path must still be served: status = %d", rec.Code)
	}
	after := adaptertest.EndpointTotal(adaptertest.Collect(t))

	if after != before {
		t.Errorf("skipped path must not record endpoint.requests, got delta %d", after-before)
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

// status must mirror Echo's DefaultHTTPErrorHandler, not approximate it: the
// outcome on endpoint.requests is only meaningful if it matches the status the
// client actually received.
func TestStatusMirrorsEchoErrorHandler(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		// Echo does not unwrap, so this answers 500. errors.As would find the 404
		// and record a server fault as a client error.
		{"wrapped HTTPError", fmt.Errorf("load user: %w", echo.NewHTTPError(http.StatusNotFound)), http.StatusInternalServerError},
		// Echo unwraps Internal one level and answers with the inner code.
		{"SetInternal", echo.NewHTTPError(500).SetInternal(echo.NewHTTPError(http.StatusNotFound)), http.StatusNotFound},
		{"plain HTTPError", echo.NewHTTPError(http.StatusNotFound), http.StatusNotFound},
		{"opaque error", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			h := Instrument(e, nethttp.WithEndpointMetrics())
			e.GET("/echo/status", func(c echo.Context) error { return tc.err })

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/echo/status", nil))
			if rec.Code != tc.want {
				t.Fatalf("Echo answered %d, want %d — the test's premise is wrong", rec.Code, tc.want)
			}
			// And the recorded outcome must follow that same status.
			wantOutcome := "success"
			if tc.want >= 500 {
				wantOutcome = "failure"
			}
			before := adaptertest.EndpointOutcome(adaptertest.Collect(t), "/echo/status", wantOutcome)
			rec2 := httptest.NewRecorder()
			h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/echo/status", nil))
			if delta := adaptertest.EndpointOutcome(adaptertest.Collect(t), "/echo/status", wantOutcome) - before; delta != 1 {
				t.Errorf("outcome=%s delta = %d, want 1 for a %d response", wantOutcome, delta, tc.want)
			}
		})
	}
}

// A handler that already wrote 200 and then returns an error must not be recorded
// as a failure: Echo's error handler bails out on Committed, so the client got 200.
func TestCommittedResponseKeepsItsStatus(t *testing.T) {
	e := echo.New()
	h := Instrument(e, nethttp.WithEndpointMetrics())
	e.GET("/echo/committed", func(c echo.Context) error {
		_ = c.JSON(http.StatusOK, map[string]string{"ok": "yes"})
		return errors.New("too late to matter")
	})

	before := adaptertest.EndpointOutcome(adaptertest.Collect(t), "/echo/committed", "failure")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/echo/committed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("client saw %d, want 200", rec.Code)
	}
	if delta := adaptertest.EndpointOutcome(adaptertest.Collect(t), "/echo/committed", "failure") - before; delta != 0 {
		t.Errorf("committed 200 recorded %d failures, want 0", delta)
	}
}
