package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/internal/ident"
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

// service.name comes from the scope set by Init, not from the context, and is
// stamped alongside the trace IDs.
func TestLogHandlerStampsServiceName(t *testing.T) {
	ident.SetServiceName("billing-api")

	var buf bytes.Buffer
	slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil))).
		InfoContext(context.Background(), "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec["service.name"] != "billing-api" {
		t.Errorf("service.name = %v, want %q", rec["service.name"], "billing-api")
	}
}

// Enabled must delegate so a level filter on the wrapped handler is honored.
func TestEnabledDelegatesToBase(t *testing.T) {
	base := slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewLogHandler(base)

	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Info must be disabled when the base handler is Warn-level")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Error must be enabled when the base handler is Warn-level")
	}
}

// The no-op forms must not allocate a new handler.
func TestWithAttrsNilAndWithGroupEmptyAreIdentity(t *testing.T) {
	h := NewLogHandler(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	if got := h.WithAttrs(nil); got != h {
		t.Error("WithAttrs(nil) must return the same handler")
	}
	if got := h.WithGroup(""); got != h {
		t.Error(`WithGroup("") must return the same handler`)
	}
}

// SetDefault redirects everything the library logs through, so a consuming
// operator gets one stream in its own format instead of a second one on stdout.
func TestSetDefaultRedirectsLibraryLogging(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })

	var buf bytes.Buffer
	custom := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	SetDefault(custom)

	if Logger() != custom {
		t.Fatal("Logger() must return the logger installed by SetDefault")
	}
	Logger().Info("through the injected logger")
	if buf.Len() == 0 {
		t.Error("the injected logger received nothing")
	}

	SetDefault(nil)
	if Logger() == custom {
		t.Error("SetDefault(nil) must restore the default logger")
	}
}

// Without an open group the handler takes the fast path; the stamps must still
// land, and user attrs must survive.
func TestFastPathKeepsAttrsAndStamps(t *testing.T) {
	ident.SetServiceName("fastpath-svc")

	var buf bytes.Buffer
	slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil))).
		With("user", "attr").
		InfoContext(context.Background(), "hello", "inline", 2)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k, want := range map[string]any{
		"service.name": "fastpath-svc",
		"user":         "attr",
		"inline":       float64(2),
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v (full: %s)", k, rec[k], want, buf.String())
		}
	}
}

// A nil base is a wiring mistake, and it used to surface as a nil dereference on
// the first record — arbitrarily far from the line that caused it.
func TestNewLogHandlerRejectsNilBase(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewLogHandler(nil) returned a handler that would nil-panic on first use")
		}
	}()
	NewLogHandler(nil)
}
