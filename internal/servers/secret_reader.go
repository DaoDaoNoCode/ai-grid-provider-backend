package servers

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

const idpClientSecretKey = "client-secret"

// errNoClientSecret is the cause when a Secret exists but has no usable client-secret key.
var errNoClientSecret = errors.New("no client secret")

// SecretReader reads IdP client-secret values by Secret name.
type SecretReader interface {
	Read(ctx context.Context, name string) ([]byte, error)
}

// KubernetesSecretReader reads the client-secret key from Secrets in one namespace.
type KubernetesSecretReader struct {
	Secrets   corev1client.SecretInterface
	Namespace string
}

// NewKubernetesSecretReader builds a reader for Secrets in namespace.
func NewKubernetesSecretReader(client kubernetes.Interface, namespace string) *KubernetesSecretReader {
	return &KubernetesSecretReader{
		Secrets:   client.CoreV1().Secrets(namespace),
		Namespace: namespace,
	}
}

// Read returns the exact bytes of the client-secret key. It does not trim.
func (r *KubernetesSecretReader) Read(ctx context.Context, name string) ([]byte, error) {
	if r == nil || r.Secrets == nil {
		return nil, fmt.Errorf("secret reader is not configured")
	}
	secret, err := r.Secrets.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return clientSecretFromSecret(secret)
}

func clientSecretFromSecret(secret *corev1.Secret) ([]byte, error) {
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("secret %q is missing key %q: %w", secretName(secret), idpClientSecretKey, errNoClientSecret)
	}
	value, ok := secret.Data[idpClientSecretKey]
	if !ok {
		return nil, fmt.Errorf("secret %q is missing key %q: %w", secret.Name, idpClientSecretKey, errNoClientSecret)
	}
	if len(value) == 0 {
		return nil, fmt.Errorf("secret %q key %q is empty: %w", secret.Name, idpClientSecretKey, errNoClientSecret)
	}
	return value, nil
}

func secretName(secret *corev1.Secret) string {
	if secret == nil {
		return ""
	}
	return secret.Name
}

// IsSecretNotFound reports whether err is a missing Secret.
func IsSecretNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

// IsSecretWithoutClientSecret reports whether err is a Secret without a client-secret key,
// or with an empty one.
func IsSecretWithoutClientSecret(err error) bool {
	return errors.Is(err, errNoClientSecret)
}
