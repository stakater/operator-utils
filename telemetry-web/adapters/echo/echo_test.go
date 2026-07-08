package echo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var reader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	reader = sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	os.Exit(m.Run())
}

func collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

// endpointOutcome returns the cumulative count for {endpoint,outcome}.
func endpointOutcome(rm metricdata.ResourceMetrics, route, outcome string) int64 {
	for _, sm := range rm.ScopeMetrics {
		for _, mtr := range sm.Metrics {
			if mtr.Name != "http.endpoint.requests" {
				continue
			}
			sum, ok := mtr.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				ep, _ := dp.Attributes.Value(attribute.Key("endpoint"))
				oc, _ := dp.Attributes.Value(attribute.Key("outcome"))
				if ep.AsString() == route && oc.AsString() == outcome {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// endpointTotal returns the cumulative count across all endpoints/outcomes.
func endpointTotal(rm metricdata.ResourceMetrics) int64 {
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, mtr := range sm.Metrics {
			if mtr.Name != "http.endpoint.requests" {
				continue
			}
			if sum, ok := mtr.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}

func panicCount(rm metricdata.ResourceMetrics) int64 {
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, mtr := range sm.Metrics {
			if mtr.Name != "http.server.panics" {
				continue
			}
			if sum, ok := mtr.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}

func serve(e *echo.Echo, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestMetricsRecordsRouteTemplateSuccess(t *testing.T) {
	e := echo.New()
	e.Use(Metrics())
	e.GET("/items/:id", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })

	serve(e, http.MethodGet, "/items/42")

	rm := collect(t)
	if got := endpointOutcome(rm, "/items/:id", "success"); got != 1 {
		t.Errorf("success count for /items/:id = %d, want 1", got)
	}
	if got := endpointOutcome(rm, "/items/42", "success"); got != 0 {
		t.Errorf("raw path should not be recorded, got %d", got)
	}
}

func TestMetricsRecordsFailureOn500(t *testing.T) {
	e := echo.New()
	e.Use(Metrics())
	e.GET("/fail/:id", func(c echo.Context) error { return c.String(http.StatusInternalServerError, "boom") })

	serve(e, http.MethodGet, "/fail/7")

	rm := collect(t)
	if got := endpointOutcome(rm, "/fail/:id", "failure"); got != 1 {
		t.Errorf("failure count for /fail/:id = %d, want 1", got)
	}
}

func TestMetricsRecordsFailureOnReturnedError(t *testing.T) {
	e := echo.New()
	e.Use(Metrics())
	e.GET("/err", func(c echo.Context) error { return errors.New("boom") })

	rec := serve(e, http.MethodGet, "/err")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (echo default error handler)", rec.Code)
	}

	rm := collect(t)
	if got := endpointOutcome(rm, "/err", "failure"); got != 1 {
		t.Errorf("failure count for /err = %d, want 1", got)
	}
}

func TestMetricsRecordsFailureOn5xxHTTPError(t *testing.T) {
	e := echo.New()
	e.Use(Metrics())
	e.GET("/unavailable", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "down")
	})

	serve(e, http.MethodGet, "/unavailable")

	rm := collect(t)
	if got := endpointOutcome(rm, "/unavailable", "failure"); got != 1 {
		t.Errorf("failure count for /unavailable = %d, want 1", got)
	}
}

func TestMetricsRecordsSuccessOn4xxHTTPError(t *testing.T) {
	e := echo.New()
	e.Use(Metrics())
	e.GET("/missing/:id", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusNotFound, "no such thing")
	})

	rec := serve(e, http.MethodGet, "/missing/9")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	rm := collect(t)
	if got := endpointOutcome(rm, "/missing/:id", "success"); got != 1 {
		t.Errorf("client errors are not failures: success count = %d, want 1", got)
	}
	if got := endpointOutcome(rm, "/missing/:id", "failure"); got != 0 {
		t.Errorf("client errors are not failures: failure count = %d, want 0", got)
	}
}

func TestMetricsSkipsUnmatchedRoutes(t *testing.T) {
	e := echo.New()
	e.Use(Metrics())
	e.GET("/known", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })

	before := endpointTotal(collect(t))
	rec := serve(e, http.MethodGet, "/definitely/not/registered")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	after := endpointTotal(collect(t))
	if after != before {
		t.Errorf("unmatched route must not be recorded: delta = %d, want 0", after-before)
	}
}

func TestRecoveryRecordsPanicAnd500(t *testing.T) {
	before := panicCount(collect(t))

	e := echo.New()
	e.Use(Recovery())
	e.GET("/panic", func(c echo.Context) error { panic("kaboom") })

	rec := serve(e, http.MethodGet, "/panic")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	after := panicCount(collect(t))
	if after-before != 1 {
		t.Errorf("panic counter delta = %d, want 1", after-before)
	}
}

func TestRecoveryReRaisesErrAbortHandler(t *testing.T) {
	before := panicCount(collect(t))

	e := echo.New()
	e.Use(Recovery())
	e.GET("/abort", func(c echo.Context) error { panic(http.ErrAbortHandler) })

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Errorf("expected ErrAbortHandler to propagate, got %v", rec)
		}
		if after := panicCount(collect(t)); after != before {
			t.Errorf("ErrAbortHandler must not be counted: delta %d", after-before)
		}
	}()
	serve(e, http.MethodGet, "/abort") // expected to panic(ErrAbortHandler)
}

func TestPanicDoesNotRecordEndpointMetric(t *testing.T) {
	e := echo.New()
	e.Use(Recovery(), Metrics())
	e.GET("/panic/:id", func(c echo.Context) error { panic("kaboom") })

	before := endpointTotal(collect(t))
	serve(e, http.MethodGet, "/panic/1")
	after := endpointTotal(collect(t))
	if after != before {
		t.Errorf("panicked request must not record a per-endpoint data point: delta = %d", after-before)
	}
}

func TestInstrumentServesAndRecords(t *testing.T) {
	e := echo.New()
	h := Instrument(e)
	e.GET("/smoke/:id", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/smoke/1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := endpointOutcome(collect(t), "/smoke/:id", "success"); got != 1 {
		t.Errorf("Instrument did not record per-endpoint metric: got %d", got)
	}
}
