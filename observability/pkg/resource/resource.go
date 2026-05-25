// Package resource builds the OTel Resource used by the observability
// module. The result carries service.name, service.version, k8s.pod.name,
// k8s.namespace.name, and service.instance.id derived from the operator
// metadata and the pod environment (POD_NAME, POD_NAMESPACE).
//
// The Build function takes primitives rather than a full Config so this
// package can be used standalone by callers that wire their own
// MeterProvider while still adopting the module's resource conventions.
package resource

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/attribute"
	otelresource "go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Build constructs an OTel Resource from primitive operator metadata and
// the current pod environment. POD_NAME falls back to os.Hostname.
// POD_NAMESPACE is omitted entirely when unset (rather than recorded as
// the empty string).
func Build(ctx context.Context, operatorName, version string) (*otelresource.Resource, error) {
	instanceID := os.Getenv("POD_NAME")
	if instanceID == "" {
		h, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("resolve hostname for service.instance.id: %w", err)
		}
		instanceID = h
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceName(operatorName),
		semconv.ServiceVersion(version),
		semconv.ServiceInstanceID(instanceID),
		semconv.K8SPodName(instanceID),
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		attrs = append(attrs, semconv.K8SNamespaceName(ns))
	}

	return otelresource.New(ctx,
		otelresource.WithSchemaURL(semconv.SchemaURL),
		otelresource.WithAttributes(attrs...),
	)
}
