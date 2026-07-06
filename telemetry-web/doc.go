// Package telemetry provides two-call OpenTelemetry setup for Go services:
// traces + metrics + trace-correlated logs, exported over OTLP/gRPC. Call Init
// once in main; the returned shutdown flushes providers.
//
// Setup (net/http):
//
//	shutdown, err := telemetry.Init(ctx, telemetry.Config{ServiceName: "svc"})
//	defer shutdown(context.Background())
//	http.ListenAndServe(":8080", nethttp.Handler(mux))
//
// The library is framework-agnostic. Capabilities live in sub-packages:
//
//	logging.Logger()/logging.NewLogHandler   trace-correlated slog
//	endpoint.Instrument(ctx, name)(&err)     per-endpoint metrics (hand use)
//	endpoint.Record(ctx, name, failed)       per-endpoint metrics (adapters)
//	endpoint.RecordPanic(ctx, recovered)     panic -> span + log + counter
//	nethttp.Handler / nethttp.Recovery       inbound spans/metrics/recovery
//	nethttp.HTTPClient / nethttp.Transport   outbound trace propagation
//
// Framework adapters (e.g. telemetry/adapters/gin) wire these in one call.
package telemetry
