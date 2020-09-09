package secrets

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LoadSecretData loads a given secret key and returns it's data as a string.
func LoadSecretData(apiReader client.Reader, secretName, namespace, dataKey string) (string, error) {
	secret := &corev1.Secret{}
	err := apiReader.Get(context.TODO(), types.NamespacedName{Name: secretName, Namespace: namespace}, secret)
	if err != nil {
		return "", err
	}

	retStr, ok := secret.Data[dataKey]
	if !ok {
		return "", fmt.Errorf("secret %s did not contain key %s", secretName, dataKey)
	}
	return string(retStr), nil
}

func LoadSecretDataUsingClient(c client.Client, secretName, namespace, dataKey string) (string, error) {
	secret := &corev1.Secret{}
	err := c.Get(context.TODO(), types.NamespacedName{Name: secretName, Namespace: namespace}, secret)
	if err != nil {
		return "", err
	}
	retStr, ok := secret.Data[dataKey]
	if !ok {
		return "", fmt.Errorf("secret %s did not contain key %s", secretName, dataKey)
	}
	return string(retStr), nil
}
