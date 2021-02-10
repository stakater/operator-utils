package labels

import (
	"k8s.io/apimachinery/pkg/labels"
)

// AddLabel inserts/updates a label in an existing map of labels
func AddLabel(labels map[string]string, key string, value string) map[string]string {
	// If key is empty, no changes are required
	if key == "" {
		return labels
	}

	// Initialize an empty map if it's nill
	if labels == nil {
		labels = make(map[string]string)
	}

	// Add label
	labels[key] = value
	return labels
}

// GetLabelSelector returns label selector
func GetLabelSelector(key string, value string) labels.Selector {
	// Key is empty
	if len(key) == 0 {
		return labels.SelectorFromSet(map[string]string{})
	}
	return labels.SelectorFromSet(labels.Set{key: value})
}
