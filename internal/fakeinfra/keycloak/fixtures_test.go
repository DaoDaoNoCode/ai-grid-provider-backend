package keycloak

import (
	"path/filepath"
	"testing"
)

func TestShippedFixturesLoad(t *testing.T) {
	for name, want := range map[string]int{"default": 6, "poc": 3} {
		orgs, err := LoadFixtures(filepath.Join("..", "..", "..", "fixtures", name))
		if err != nil {
			t.Fatalf("LoadFixtures(%s) error = %v", name, err)
		}
		if len(orgs) != want {
			t.Fatalf("%s: %d organizations, want %d", name, len(orgs), want)
		}
		if _, err := New(Config{AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "admin"}, orgs); err != nil {
			t.Fatalf("%s: New() error = %v", name, err)
		}
	}
}
