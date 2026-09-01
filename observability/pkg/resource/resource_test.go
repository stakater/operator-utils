package resource

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func assertAttr(t *testing.T, attrs []attribute.KeyValue, key, want string) {
	t.Helper()
	for _, a := range attrs {
		if string(a.Key) == key {
			if a.Value.AsString() != want {
				t.Errorf("attr %q = %q, want %q", key, a.Value.AsString(), want)
			}
			return
		}
	}
	t.Errorf("attr %q not set", key)
}

func TestBuild_UsesEnvPodInfo(t *testing.T) {
	t.Setenv("POD_NAME", "my-op-abc123")
	t.Setenv("POD_NAMESPACE", "operators")

	res, err := Build(context.Background(), "my-op", "1.2.3")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	attrs := res.Attributes()
	assertAttr(t, attrs, string(semconv.ServiceNameKey), "my-op")
	assertAttr(t, attrs, string(semconv.ServiceVersionKey), "1.2.3")
	assertAttr(t, attrs, string(semconv.ServiceInstanceIDKey), "my-op-abc123")
	assertAttr(t, attrs, string(semconv.K8SPodNameKey), "my-op-abc123")
	assertAttr(t, attrs, string(semconv.K8SNamespaceNameKey), "operators")
}

func TestBuild_FallsBackToHostname(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")

	res, err := Build(context.Background(), "my-op", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	attrs := res.Attributes()

	// service.instance.id must be present (hostname fallback)
	found := false
	for _, a := range attrs {
		if string(a.Key) == string(semconv.ServiceInstanceIDKey) {
			if a.Value.AsString() == "" {
				t.Fatalf("service.instance.id is empty")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("service.instance.id attribute not set")
	}

	// k8s.namespace.name must NOT be present when POD_NAMESPACE is unset
	for _, a := range attrs {
		if string(a.Key) == string(semconv.K8SNamespaceNameKey) {
			t.Fatalf("k8s.namespace.name should be omitted when POD_NAMESPACE unset, got %q", a.Value.AsString())
		}
	}
}
