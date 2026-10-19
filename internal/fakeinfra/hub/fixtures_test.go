package hub

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var shippedFixtures = []string{"default", "poc"}

func loadShipped(t *testing.T, name string) *Fixtures {
	t.Helper()
	fx, err := LoadFixtures(filepath.Join("..", "..", "..", "fixtures", name))
	if err != nil {
		t.Fatalf("LoadFixtures(%s) error = %v", name, err)
	}
	return fx
}

func TestShippedFixturesLoad(t *testing.T) {
	for _, name := range shippedFixtures {
		fx := loadShipped(t, name)
		if len(fx.Clusters) == 0 {
			t.Fatalf("%s: no clusters", name)
		}
		if fx.Access.ServiceAccount.Token != DefaultServiceAccountToken {
			t.Fatalf("%s: service account token = %q", name, fx.Access.ServiceAccount.Token)
		}
		if _, err := New(fx); err != nil {
			t.Fatalf("%s: New() error = %v", name, err)
		}
	}
}

// TestShippedClustersAreSettled checks that every fixture cluster is what the
// hub controllers would leave behind: running them after a client write
// changes nothing, so a label update writes only the client's change.
func TestShippedClustersAreSettled(t *testing.T) {
	for _, name := range shippedFixtures {
		for _, c := range loadShipped(t, name).Clusters {
			next := c.DeepCopy()
			reconcile(next, c.DeepCopy(), time.Now())
			if !jsonEqual(&c, next) {
				a, _ := json.MarshalIndent(c, "", " ")
				b, _ := json.MarshalIndent(next, "", " ")
				t.Errorf("%s: reconcile changes %s\nbefore %s\nafter %s", name, c.Name, a, b)
			}
		}
	}
}

func TestShippedScenarios(t *testing.T) {
	fx := loadShipped(t, "default")
	fx.Access.ServiceAccount.Token = testToken
	th := startHub(t, fx)
	ctx := context.Background()
	client := th.client(t, testToken)

	if _, joined := conditionsOf(t, getCluster(t, th, "site-berlin-1"))[CondJoined]; joined {
		t.Fatal("site-berlin-1 joined at start")
	}
	th.clock.Advance(20 * time.Second)
	if c := conditionsOf(t, getCluster(t, th, "site-berlin-1"))[CondJoined]; c["status"] != "True" {
		t.Fatalf("site-berlin-1 at 20s: %v", c)
	}
	if _, err := client.Get(ctx, "edge-lyon-2", metav1.GetOptions{}); err != nil {
		t.Fatalf("edge-lyon-2 before 30s: %v", err)
	}
	th.clock.Advance(10 * time.Second)
	if _, err := client.Get(ctx, "edge-lyon-2", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("edge-lyon-2 after 30s = %v, want NotFound", err)
	}
	th.clock.Advance(270 * time.Second)
	if c := conditionsOf(t, getCluster(t, th, "site-madrid-1"))[CondAvailable]; c["status"] != "True" {
		t.Fatalf("site-madrid-1 at 300s: %v", c)
	}
}
