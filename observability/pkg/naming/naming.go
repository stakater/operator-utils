// Package naming validates metric names and attribute keys against the
// module's strict snake_case + no-reserved-prefix rules. The validators
// are exported so operator authors can fail loudly at startup rather
// than at first registration.
package naming

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	metricNameRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	attributeKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

var reservedPrefixes = []string{
	"otel_",
	"go_",
	"process_",
	"controller_runtime_",
	"workqueue_",
	"rest_client_",
}

// ValidateMetricName reports whether name is a legal custom metric name.
// Names must match ^[a-z][a-z0-9_]*$ and must not start with any reserved
// prefix used by controller-runtime, the Go runtime, or OTel itself.
func ValidateMetricName(name string) error {
	if name == "" {
		return fmt.Errorf("metric name must not be empty")
	}
	if !metricNameRe.MatchString(name) {
		return fmt.Errorf("metric name %q must match %s", name, metricNameRe.String())
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("metric name %q uses reserved prefix %q", name, p)
		}
	}
	return nil
}

// ValidateAttributeKey reports whether key is a legal attribute key.
// Keys must match ^[a-z][a-z0-9_]*$ (snake_case, no whitespace).
func ValidateAttributeKey(key string) error {
	if key == "" {
		return fmt.Errorf("attribute key must not be empty")
	}
	if !attributeKeyRe.MatchString(key) {
		return fmt.Errorf("attribute key %q must match %s", key, attributeKeyRe.String())
	}
	return nil
}
