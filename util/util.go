package util

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"
)

var readNamespace = func() ([]byte, error) {
	return ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
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