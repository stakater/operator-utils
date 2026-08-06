// Package nethttp provides net/http integration for the telemetry library:
// an inbound Handler (spans + server metrics + panic recovery) and an outbound
// Transport that propagates trace context.
package nethttp

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// Option configures Handler.
type Option func(*config)

type config struct {
	skipPaths []string
	noRecover bool
}

// Settings is the resolved option set. Adapters call Resolve to learn which
// middleware their Instrument should install.
type Settings struct {
	// Recovery reports whether the built-in recovery middleware is installed.
	Recovery bool
}

// Resolve applies opts and reports the result, so an adapter and the Handler
// it builds agree on one configuration.
func Resolve(opts ...Option) Settings {
	cfg := config{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return Settings{Recovery: !cfg.noRecover}
}

// DefaultSkipPaths are the usual noise endpoints — k8s health probes and the
// Prometheus scrape path. Handler does NOT skip them on its own; opt in with
//
//	nethttp.Handler(mux, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))
//
// or append your own: WithSkipPaths(append(nethttp.DefaultSkipPaths, "/ping")...).
var DefaultSkipPaths = []string{"/healthz", "/readyz", "/livez", "/metrics"}

// WithSkipPaths excludes the given exact request paths from instrumentation: no
// span and no http.server.* metrics. The request is still served and recovery
// still applies. Repeated calls accumulate; by default nothing is skipped.
//
// The filter runs before trace context is extracted, so a skipped path also
// drops any inbound traceparent and will not continue a caller's trace. Fine for
// probes, wrong for paths that participate in traces.
func WithSkipPaths(paths ...string) Option {
	return func(c *config) { c.skipPaths = append(c.skipPaths, paths...) }
}

// WithoutRecovery installs no recovery at all — not Handler's, and not the
// framework middleware an adapter's Instrument would add. Only pass it when an
// outer layer recovers and calls endpoint.RecordPanic itself.
//
// A panic then escapes otelhttp, whose only deferred work is span.End(): the span
// ends Unset with no response attributes and the request contributes no
// http.server.* metrics at all, so a service panicking on every request looks
// like it is serving zero traffic. Same on the ErrAbortHandler re-raise path.
func WithoutRecovery() Option {
	return func(c *config) { c.noRecover = true }
}

// skipKey marks a request that WithSkipPaths excluded, so middleware running
// inside the router (which otelhttp's own filter cannot reach) can bail out too.
type skipKey struct{}

// skipped reports whether this request's path was excluded via WithSkipPaths.
func skipped(ctx context.Context) bool {
	v, _ := ctx.Value(skipKey{}).(bool)
	return v
}

// Handler is the composed inbound chain: otelhttp (spans + metrics) -> recovery
// -> next. Frameworks that don't expose http.Handler chaining should instead call
// StampRoute and endpoint.Recovered from their own middleware. Every path is
// instrumented unless excluded via WithSkipPaths.
func Handler(next http.Handler, opts ...Option) http.Handler {
	// The config directly, not via Resolve: Handler is the one caller that needs
	// the skip paths themselves, which Settings does not carry.
	cfg := config{}
	for _, opt := range opts {
		opt(&cfg)
	}

	inner := next
	if !cfg.noRecover {
		inner = Recovery(next)
	}

	instrumented := otelhttp.NewHandler(inner, "server",
		otelhttp.WithFilter(func(r *http.Request) bool { return !skipped(r.Context()) }),
		otelhttp.WithSpanNameFormatter(spanName),
	)

	if len(cfg.skipPaths) == 0 {
		return instrumented
	}
	skip := make(map[string]bool, len(cfg.skipPaths))
	for _, p := range cfg.skipPaths {
		skip[p] = true
	}
	// Marking outside otelhttp is what lets both its filter and the adapters'
	// middleware see the same decision.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip[r.URL.Path] {
			r = r.WithContext(context.WithValue(r.Context(), skipKey{}, true))
		}
		instrumented.ServeHTTP(w, r)
	})
}

// trackWrites returns w wrapped so responded reports whether anything has been
// committed to the client yet.
//
// httpsnoop rather than an embedded http.ResponseWriter: it re-exposes exactly
// the optional interfaces w already implements, where embedding would promote
// only Header/Write/WriteHeader and make streaming handlers panic.
func trackWrites(w http.ResponseWriter) (wrapped http.ResponseWriter, responded func() bool) {
	var wrote bool
	wrapped = httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) { wrote = true; next(code) }
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(b []byte) (int, error) { wrote = true; return next(b) }
		},
		// A hijacked connection is no longer ours to write a status to.
		Hijack: func(next httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) { wrote = true; return next() }
		},
		// Flush and ReadFrom also commit an implicit 200, so without these an SSE
		// or io.Copy handler leaves wrote false and a later panic makes Recovery
		// call WriteHeader(500) pointlessly.
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() { wrote = true; next() }
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(src io.Reader) (int64, error) { wrote = true; return next(src) }
		},
	})
	return wrapped, func() bool { return wrote }
}

// Recovery recovers panics, records telemetry via endpoint.RecordPanic, and
// writes 500 if nothing has been written yet. http.ErrAbortHandler is
// re-raised. Must sit inside the otelhttp handler so the span exists in ctx and
// the response is measured. The writer handed to next keeps whatever optional
// interfaces the original had, so SSE and hijacking keep working.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked, responded := trackWrites(w)
		defer func() {
			if p := recover(); p != nil {
				ctx := r.Context()
				if !endpoint.Recovered(ctx, p, routeAttrs(ctx)...) {
					panic(p)
				}
				// A handler that panicked mid-stream already sent its status;
				// a second write would only be log noise.
				if !responded() {
					tracked.WriteHeader(http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(tracked, r)
	})
}

// Transport wraps a RoundTripper so outbound requests inject trace context.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

// HTTPClient returns a client whose Transport already propagates trace context.
func HTTPClient() *http.Client { return &http.Client{Transport: Transport(nil)} }

// WrapClient adds propagation to an existing client, in place, and returns it so
// it can be used inline. Calling it twice on the same client would nest one
// otelhttp transport inside another and inject the headers twice, so a client
// that is already wrapped is left alone.
//
// A nil client yields a new propagating one rather than panicking, matching
// Transport(nil), so a caller threading an optional client through does not have
// to nil-check first.
func WrapClient(c *http.Client) *http.Client {
	if c == nil {
		return HTTPClient()
	}
	if _, ok := c.Transport.(*otelhttp.Transport); !ok {
		c.Transport = Transport(c.Transport)
	}
	return c
}

// routeAttrs recovers http.route from the otelhttp labeler, where StampRoute put
// it. Recovery runs outside the router, so at recover time the labeler is the
// only place the matched template can still be read from.
func routeAttrs(ctx context.Context) []attribute.KeyValue {
	labeler, ok := otelhttp.LabelerFromContext(ctx)
	if !ok {
		return nil
	}
	for _, kv := range labeler.Get() {
		if kv.Key == semconv.HTTPRouteKey {
			return []attribute.KeyValue{kv}
		}
	}
	return nil
}

// warnNoLabeler fires at most once per process: a missing labeler is a wiring
// mistake, not a per-request event, and no re-Init can change it.
var warnNoLabeler sync.Once

// stdMethods are the methods semconv keeps verbatim in a span name. Anything else
// becomes "HTTP", so an attacker-supplied method cannot create a span name per
// request. The set is frozen by the HTTP spec, not by otelhttp.
var stdMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodConnect: true, http.MethodOptions: true, http.MethodTrace: true,
}

// spanName names the server span, preferring the route StampRoute put on the
// labeler over the one net/http derived from its own mux pattern.
//
// Without this, otelhttp re-sets the span name after the handler returns whenever
// r.Pattern != "" (handler.go:187 in contrib v0.69), which silently overwrites
// StampRoute's rename with the coarser outer pattern. Mounting an instrumented
// router under a mux, say outer.Handle("/api/", nethttp.Handler(router)), would
// otherwise leave the span named "GET /api/" while http.route on the very same
// span reads "/api/users/{id}".
//
// otelhttp calls this twice: once at span creation, before it puts the labeler in
// ctx, so the labeler is empty and the r.Pattern branch is what runs; and once
// after the handler, where the labeler has the adapter's template. The fallback
// therefore has to reproduce otelhttp's default (semconv HTTPServer.SpanName),
// which is unexported.
func spanName(_ string, r *http.Request) string {
	method := strings.ToUpper(r.Method)
	if !stdMethods[method] {
		method = "HTTP"
	}
	route := labelerRoute(r.Context())
	if route == "" {
		route = patternRoute(r.Pattern)
	}
	if route == "" {
		return method
	}
	return method + " " + route
}

// labelerRoute returns the http.route StampRoute recorded, or "" before it has run.
func labelerRoute(ctx context.Context) string {
	if attrs := routeAttrs(ctx); len(attrs) > 0 {
		return attrs[0].Value.AsString()
	}
	return ""
}

// patternRoute reduces a net/http mux pattern to its path, mirroring otelhttp's
// internal httpRoute verbatim. A pattern may carry a method and a host, as in
// "GET example.com/x", and neither belongs in a route; taking everything from the
// first slash drops both.
func patternRoute(pattern string) string {
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		return pattern[i:]
	}
	return ""
}

// StampRoute puts http.route on the active server span and on the otelhttp
// metric attributes (via the request Labeler), and renames the span to the
// semconv "{method} {route}" form. An empty method leaves the name alone. The
// adapters call it from RouteTag; call it yourself when wiring a framework by
// hand.
//
// The request must be inside Handler. Without the otelhttp labeler in ctx the
// span attribute still lands but the metric one is dropped, and a warning is
// logged once.
//
// The rename here is not the only thing naming the span: Handler also installs
// spanName, which re-derives the name from the labeler after the handler returns.
// Both are needed. otelhttp only re-derives when r.Pattern != "", so this call is
// what names the span for a router served at the root, and spanName is what keeps
// a mounted router from being renamed to its outer mux pattern.
//
// A path excluded via WithSkipPaths returns immediately: it has neither a labeler
// nor a recording span, but the adapters' RouteTag still runs for it, so without
// this the recommended WithSkipPaths(DefaultSkipPaths...) setup would warn on the
// first health probe.
func StampRoute(ctx context.Context, method, route string) {
	if skipped(ctx) {
		return
	}
	attr := semconv.HTTPRoute(route)
	labeler, ok := otelhttp.LabelerFromContext(ctx)
	if !ok {
		warnNoLabeler.Do(func() {
			logging.Logger().Warn("nethttp.StampRoute: no otelhttp labeler in context; " +
				"http.route will not reach http.server.* metrics. Serve through nethttp.Handler.")
		})
	}
	labeler.Add(attr)
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attr)
	if method != "" {
		span.SetName(method + " " + route)
	}
}
