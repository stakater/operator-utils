package util

import (
	"errors"
	"io/ioutil"
	"os"
	"strings"
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
