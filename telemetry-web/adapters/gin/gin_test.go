package gin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var reader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
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

func serve(engine *gin.Engine, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestMetricsRecordsRouteTemplateSuccess(t *testing.T) {
	e := gin.New()
	e.Use(Metrics())
	e.GET("/items/:id", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

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
	e := gin.New()
	e.Use(Metrics())
	e.GET("/fail/:id", func(c *gin.Context) { c.String(http.StatusInternalServerError, "boom") })

	serve(e, http.MethodGet, "/fail/7")

	rm := collect(t)
	if got := endpointOutcome(rm, "/fail/:id", "failure"); got != 1 {
		t.Errorf("failure count for /fail/:id = %d, want 1", got)
	}
}

func TestRecoveryRecordsPanicAnd500(t *testing.T) {
	before := panicCount(collect(t))

	e := gin.New()
	e.Use(Recovery())
	e.GET("/panic", func(c *gin.Context) { panic("kaboom") })

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

	e := gin.New()
	e.Use(Recovery())
	e.GET("/abort", func(c *gin.Context) { panic(http.ErrAbortHandler) })

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

func TestInstrumentServesAndRecords(t *testing.T) {
	e := gin.New()
	h := Instrument(e)
	e.GET("/smoke/:id", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

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
