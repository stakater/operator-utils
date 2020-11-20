package labels

// AddLabel inserts/updates a label in an existing map of labels
func AddLabel(labels map[string]string, key string, value string) map[string]string {
	if key == "" {
		return labels
	}

	if labels == nil {
		labels = make(map[string]string)
	}

	labels[key] = value
	return labels
}
