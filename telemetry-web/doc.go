// Package telemetry provides two-call OpenTelemetry setup for Go services:
// traces + metrics + trace-correlated logs, exported over OTLP/gRPC. Call Init
// once in main; the returned shutdown flushes providers.
//
// Setup (net/http):
//
//	shutdown, err := telemetry.Init(ctx, telemetry.Config{ServiceName: "svc"})
//	defer func() { // bounded flush — an unreachable collector must not hang exit
//	    sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	    defer cancel()
//	    _ = shutdown(sctx)
//	}()
//	http.ListenAndServe(":8080", nethttp.Handler(mux))
//
// The library is framework-agnostic. Capabilities live in sub-packages:
//
//	logging.Logger() / logging.NewLogHandler  trace-correlated slog
//	logging.SetDefault(l)                     redirect what the library logs
//	endpoint.Instrument(ctx, name)(&err)      count + time a named operation
//	endpoint.Record(ctx, name, failed)        count only, outcome already known
//	endpoint.RecordPanic(ctx, recovered)      panic -> span + log + counter
//	nethttp.Handler / nethttp.Recovery        inbound spans/metrics/recovery
//	nethttp.StampRoute(ctx, method, route)    http.route on the span + metrics
//	nethttp.HTTPClient / nethttp.Transport    outbound trace propagation
//
// The adapters under adapters/{gin,echo,chi} wire these in one call.
package telemetry
