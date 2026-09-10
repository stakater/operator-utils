package naming

import "testing"

func TestValidateMetricName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "reconcile_total", false},
		{"valid single char", "x", false},
		{"valid with digits", "errors_5xx_total", false},
		{"empty", "", true},
		{"starts with digit", "5_errors", true},
		{"starts with underscore", "_errors", true},
		{"uppercase", "Errors", true},
		{"dash", "errors-total", true},
		{"whitespace", "errors total", true},
		{"reserved otel_", "otel_metric", true},
		{"reserved go_", "go_alloc", true},
		{"reserved process_", "process_cpu", true},
		{"reserved controller_runtime_", "controller_runtime_x", true},
		{"reserved workqueue_", "workqueue_depth", true},
		{"reserved rest_client_", "rest_client_requests", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMetricName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateMetricName(%q) error = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateAttributeKey(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "namespace", false},
		{"valid with digits", "shard_3", false},
		{"empty", "", true},
		{"uppercase", "Namespace", true},
		{"dash", "name-space", true},
		{"whitespace", "name space", true},
		{"starts with digit", "3_shard", true},
		{"starts with underscore", "_shard", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAttributeKey(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateAttributeKey(%q) error = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}
