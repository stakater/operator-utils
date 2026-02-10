package util

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
)

var readNamespace = func() ([]byte, error) {
	return os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
}

// GetOperatorNamespace returns the namespace the operator should be running in.
func GetOperatorNamespace() (string, error) {
	nsBytes, err := readNamespace()
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New("cannot find namespace of the operator")
		}
		return "", err
	}
	ns := strings.TrimSpace(string(nsBytes))
	return ns, nil
}

// TimeElapsed prints time it takes to execute a function
// Usage: defer TimeElapsed("function-name")()
func TimeElapsed(functionName string) func() {
	start := time.Now()
	return func() {
		fmt.Printf("%s took %v\n", functionName, time.Since(start))
	}
}

// StringP Returns a pointer of a string variable
func StringP(val string) *string {
	return &val
}

// PString Returns a string value from a pointer
func PString(val *string) string {
	if val == nil {
		return ""
	}
	return *val
}

// GetLabelSelector returns label selector
func GetLabelSelector(key string, value string) labels.Selector {
	// Key is empty
	if len(key) == 0 {
		return labels.SelectorFromSet(map[string]string{})
	}
	return labels.SelectorFromSet(labels.Set{key: value})
}
