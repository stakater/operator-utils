package gintel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/stakater/operator-utils/telemetry-web/adaptertest"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	adaptertest.Setup()
	os.Exit(m.Run())
}

func buildGin(routes []adaptertest.Route) http.Handler {
	e := gin.New()
	h := Instrument(e)
	for _, r := range routes {
		switch r.Behavior {
		case adaptertest.OK:
			e.GET(r.Template, func(c *gin.Context) { c.String(http.StatusOK, "ok") })
		case adaptertest.Fail500:
			e.GET(r.Template, func(c *gin.Context) { c.String(http.StatusInternalServerError, "boom") })
		case adaptertest.Panic:
			e.GET(r.Template, func(c *gin.Context) { panic("kaboom") })
		case adaptertest.PanicAbort:
			e.GET(r.Template, func(c *gin.Context) { panic(http.ErrAbortHandler) })
		}
	}
	return h
}

// TestConformance runs the shared adapter contract: route-templated metrics,
// 500→failure, http.route stamping, panic and ErrAbortHandler semantics.
func TestConformance(t *testing.T) {
	adaptertest.Run(t, buildGin)
}

// Gin-specific: an error attached via c.Error marks the request failed even
// when the response status is 2xx.
func TestMetricsRecordsFailureOnGinErrors(t *testing.T) {
	e := gin.New()
	h := Instrument(e)
	e.GET("/gin/errs", func(c *gin.Context) {
		_ = c.Error(errors.New("handler recorded an error"))
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/gin/errs", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := adaptertest.EndpointOutcome(adaptertest.Collect(t), "/gin/errs", "failure"); got != 1 {
		t.Errorf("c.Error must mark the request failed: failure count = %d, want 1", got)
	}
}
