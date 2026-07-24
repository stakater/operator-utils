package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestLogHandlerStampsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(NewLogHandler(base))

	tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	sid, _ := trace.SpanIDFromHex("0123456789abcdef")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec["trace_id"] != tid.String() {
		t.Errorf("trace_id = %v, want %v", rec["trace_id"], tid.String())
	}
	if rec["span_id"] != sid.String() {
		t.Errorf("span_id = %v, want %v", rec["span_id"], sid.String())
	}
}

// Stamps must stay top-level even when the logger has open groups — a grouped
// logger must not nest trace_id under the group.
func TestLogHandlerStampsStayTopLevelUnderGroups(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(NewLogHandler(base)).WithGroup("req").With("k", "v")

	tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	sid, _ := trace.SpanIDFromHex("0123456789abcdef")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "hello", "inner", 1)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec["trace_id"] != tid.String() {
		t.Errorf("trace_id must be top-level, got top-level=%v (full: %s)", rec["trace_id"], buf.String())
	}
	group, ok := rec["req"].(map[string]any)
	if !ok {
		t.Fatalf("expected req group in output, got: %s", buf.String())
	}
	if group["k"] != "v" || group["inner"] != float64(1) {
		t.Errorf("user attrs must stay inside the group, got: %s", buf.String())
	}
	if _, leaked := group["trace_id"]; leaked {
		t.Errorf("trace_id leaked into the group: %s", buf.String())
	}
}

// Logger must be cached — not rebuilt on every call.
func TestLoggerIsCached(t *testing.T) {
	first, second := Logger(), Logger()
	if first != second {
		t.Error("Logger() must return the same cached instance")
	}
}
