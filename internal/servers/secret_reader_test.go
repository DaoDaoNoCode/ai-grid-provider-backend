package servers

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKubernetesSecretReader(t *testing.T) {
	t.Parallel()
	const ns = "ai-grid-idp-secrets"

	t.Run("reads client-secret bytes", func(t *testing.T) {
		t.Parallel()
		client := fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "acme-idp", Namespace: ns},
			Data:       map[string][]byte{idpClientSecretKey: []byte(" s3cret ")},
		})
		reader := NewKubernetesSecretReader(client, ns)
		got, err := reader.Read(context.Background(), "acme-idp")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if string(got) != " s3cret " {
			t.Fatalf("got %q, want exact bytes including spaces", got)
		}
		actions := client.Actions()
		if len(actions) != 1 || actions[0].GetNamespace() != ns || actions[0].GetVerb() != "get" {
			t.Fatalf("actions = %#v", actions)
		}
	})

	t.Run("missing secret", func(t *testing.T) {
		t.Parallel()
		reader := NewKubernetesSecretReader(fake.NewSimpleClientset(), ns)
		_, err := reader.Read(context.Background(), "missing")
		if !IsSecretNotFound(err) {
			t.Fatalf("err = %v, want NotFound", err)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		t.Parallel()
		client := fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "acme-idp", Namespace: ns},
			Data:       map[string][]byte{"other": []byte("x")},
		})
		_, err := NewKubernetesSecretReader(client, ns).Read(context.Background(), "acme-idp")
		if err == nil || !strings.Contains(err.Error(), idpClientSecretKey) {
			t.Fatalf("err = %v, want key name", err)
		}
	})

	t.Run("empty value", func(t *testing.T) {
		t.Parallel()
		client := fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "acme-idp", Namespace: ns},
			Data:       map[string][]byte{idpClientSecretKey: {}},
		})
		_, err := NewKubernetesSecretReader(client, ns).Read(context.Background(), "acme-idp")
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("err = %v, want empty", err)
		}
	})
}
