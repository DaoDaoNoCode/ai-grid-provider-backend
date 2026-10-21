package fakeinfra_test

// Real code, fake infrastructure: the backend's own servers.NewClustersServer
// and servers.NewTenantsServer, with its own dynamic client and its own
// keycloak.New client, against the fakes seeded from fixtures/default.

import (
	"context"
	"crypto/tls"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/devserve"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/hub"
	fakekeycloak "github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/servers"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var fixturesDir = filepath.Join("..", "..", "fixtures", "default")

func startHub(t *testing.T) (*hub.Hub, *httptest.Server) {
	t.Helper()
	fx, err := hub.LoadFixtures(fixturesDir)
	if err != nil {
		t.Fatalf("hub.LoadFixtures() error = %v", err)
	}
	h, err := hub.New(fx, hub.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("hub.New() error = %v", err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return h, srv
}

func clustersServer(t *testing.T, cfg *rest.Config) *servers.ClustersServer {
	t.Helper()
	cfg.QPS, cfg.Burst = 1000, 1000
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic.NewForConfig() error = %v", err)
	}
	s, err := servers.NewClustersServer().SetLogger(slog.New(slog.DiscardHandler)).SetDynamicClient(client).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return s
}

func TestRealClustersServerOnFakeHub(t *testing.T) {
	h, srv := startHub(t)
	s := clustersServer(t, &rest.Config{Host: srv.URL, BearerToken: h.ServiceAccountToken()})
	ctx := context.Background()

	list, err := s.List(ctx, &aigridv1.ClustersListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var ids []string
	for _, c := range list.GetItems() {
		ids = append(ids, c.GetId())
	}
	want := []string{"edge-lyon-2", "local-cluster", "site-ashburn-1", "site-berlin-1", "site-dublin-1", "site-frankfurt-1", "site-lisbon-1", "site-madrid-1", "site-oslo-1", "site-paris-1"}
	if list.GetTotal() != 10 || !slices.Equal(ids, want) {
		t.Fatalf("List() = %d %v", list.GetTotal(), ids)
	}

	got, err := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-paris-1"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	paris := got.GetObject()
	if !paris.GetStatus().GetAvailable() || paris.GetStatus().GetAvailableCondition() != "True" || !paris.GetSpec().GetHubAcceptsClient() ||
		paris.GetMetadata().GetLabels()["location"] != "paris" || paris.GetMetadata().GetAnnotations()["ai-grid/site-name"] != "Paris DC1" {
		t.Fatalf("Get(site-paris-1) = %v", paris)
	}
	madrid, _ := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-madrid-1"})
	if madrid.GetObject().GetStatus().GetAvailableCondition() != "Unknown" {
		t.Fatalf("site-madrid-1 status = %v", madrid.GetObject().GetStatus())
	}
	lyon, _ := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: "edge-lyon-2"})
	if lyon.GetObject().GetMetadata().GetDeletionTimestamp() == nil {
		t.Fatal("edge-lyon-2 has no deletion timestamp")
	}
	if _, err := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get(missing) error = %v, want NotFound", err)
	}

	updated, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object:     &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: map[string]string{"ai-grid/enabled": "true"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	labels := updated.GetObject().GetMetadata().GetLabels()
	if labels["ai-grid/enabled"] != "true" || labels["name"] != "site-oslo-1" || labels["cluster.open-cluster-management.io/clusterset"] != "default" {
		t.Fatalf("Update() labels = %v", labels)
	}
	again, _ := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-oslo-1"})
	if again.GetObject().GetMetadata().GetLabels()["ai-grid/enabled"] != "true" {
		t.Fatal("update not stored")
	}
	if _, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{Object: &aigridv1.ManagedCluster{Id: "missing"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("Update(missing) error = %v, want NotFound", err)
	}

	if _, err := s.Create(ctx, &aigridv1.ClustersCreateRequest{Object: &aigridv1.ManagedCluster{Id: "site-paris-1"}}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("Create(existing) error = %v, want AlreadyExists", err)
	}
	created, err := s.Create(ctx, &aigridv1.ClustersCreateRequest{Object: &aigridv1.ManagedCluster{Id: "site-rome-1", Spec: &aigridv1.ManagedClusterSpec{HubAcceptsClient: true}}})
	if err != nil || created.GetObject().GetMetadata().GetCreationTimestamp() == nil {
		t.Fatalf("Create() = %v, %v", created, err)
	}
	// The hub's controllers keep a deleted cluster detaching for a while.
	if _, err := s.Delete(ctx, &aigridv1.ClustersDeleteRequest{Id: "site-rome-1"}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	rome, err := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-rome-1"})
	if err != nil || rome.GetObject().GetMetadata().GetDeletionTimestamp() == nil {
		t.Fatalf("Get(deleted) = %v, %v", rome, err)
	}

	// A token the hub rejects reaches the backend as PermissionDenied.
	denied := clustersServer(t, &rest.Config{Host: srv.URL, BearerToken: "expired-token"})
	if _, err := denied.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-paris-1"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Get(expired token) error = %v, want PermissionDenied", err)
	}
}

// TestRealClustersServerThroughKubeconfig goes the way cmd/ai-grid-provider-backend
// goes: clientcmd.BuildConfigFromFlags on the kubeconfig ai-grid-fake-infra
// writes, against the hub on its TLS-and-plain listener.
func TestRealClustersServerThroughKubeconfig(t *testing.T) {
	fx, err := hub.LoadFixtures(fixturesDir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.New(fx, hub.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	cert, caPEM, err := devserve.SelfSignedCert("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := devserve.NewDualListener(inner, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	srv := &http.Server{Handler: h.Handler()}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if _, err := devserve.WriteKubeconfig(path, "https://"+l.Addr().String(), caPEM, h.ServiceAccountToken()); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("BuildConfigFromFlags() error = %v", err)
	}
	list, err := clustersServer(t, cfg).List(context.Background(), &aigridv1.ClustersListRequest{})
	if err != nil || list.GetTotal() != 10 {
		t.Fatalf("List() through kubeconfig = %v, %v", list, err)
	}
}

func TestRealTenantsServerOnFakeKeycloak(t *testing.T) {
	orgs, err := fakekeycloak.LoadFixtures(fixturesDir)
	if err != nil {
		t.Fatalf("keycloak.LoadFixtures() error = %v", err)
	}
	kc, err := fakekeycloak.New(fakekeycloak.Config{AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "admin", Logger: slog.New(slog.DiscardHandler)}, orgs)
	if err != nil {
		t.Fatalf("keycloak fake New() error = %v", err)
	}
	srv := httptest.NewServer(kc.Handler())
	t.Cleanup(srv.Close)

	tenants := func(password string) *servers.TenantsServer {
		s, err := servers.NewTenantsServer().SetLogger(slog.New(slog.DiscardHandler)).SetOrganizationLister(keycloak.New(keycloak.Config{
			BaseURL: srv.URL, AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: password, HTTPClient: srv.Client(),
		})).Build()
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		return s
	}
	list, err := tenants("admin").List(context.Background(), &aigridv1.TenantsListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var names []string
	for _, tenant := range list.GetItems() {
		names = append(names, tenant.GetMetadata().GetName())
	}
	want := []string{"acme-robotics", "globex-research", "initech-labs", "umbrella-biotech", "vandelay-industries", "wayne-aerospace"}
	if list.GetTotal() != 6 || !slices.Equal(names, want) {
		t.Fatalf("List() = %d %v", list.GetTotal(), names)
	}
	acme := list.GetItems()[0]
	if acme.GetId() != "3f1c9a72-5b6e-4d2a-8c01-2d3e4f5a6b7c" || !slices.Equal(acme.GetSpec().GetDomains(), []string{"acme-robotics.example.com", "acme.example.org"}) {
		t.Fatalf("acme-robotics = %v", acme)
	}
	if _, err := tenants("wrong").List(context.Background(), &aigridv1.TenantsListRequest{}); err == nil {
		t.Fatal("List() with a wrong password succeeded")
	}
}

// TestRealClustersServerUpdateVersion covers the console's Add to and Remove
// from AI Grid writes: metadata.version is the hub resourceVersion, a stale
// one is Aborted, and the request's labels are the whole set of console keys.
func TestRealClustersServerUpdateVersion(t *testing.T) {
	h, srv := startHub(t)
	s := clustersServer(t, &rest.Config{Host: srv.URL, BearerToken: h.ServiceAccountToken()})
	ctx := context.Background()
	mask := &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}}

	got, err := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-oslo-1"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	read := got.GetObject().GetMetadata()
	if read.GetVersion() == 0 {
		t.Fatal("Get() version = 0, want the hub resourceVersion")
	}
	add := maps.Clone(read.GetLabels())
	add["ai-grid/enabled"] = "true"
	added, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object:     &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: add, Version: read.GetVersion()}},
		UpdateMask: mask,
	})
	if err != nil {
		t.Fatalf("Update(add) error = %v", err)
	}
	if added.GetObject().GetMetadata().GetLabels()["ai-grid/enabled"] != "true" || added.GetObject().GetMetadata().GetVersion() == read.GetVersion() {
		t.Fatalf("Update(add) = %v", added.GetObject().GetMetadata())
	}

	// The read version is stale now.
	_, err = s.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object:     &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: read.GetLabels(), Version: read.GetVersion()}},
		UpdateMask: mask,
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("Update(stale version) error = %v, want Aborted", err)
	}
	// A stale version is Aborted before the labels are checked, so a label ACM
	// changed since the read gives Aborted, not InvalidArgument; and so is a
	// request that selects no field.
	changed := maps.Clone(add)
	changed["vendor"] = "Other"
	for name, req := range map[string]*aigridv1.ClustersUpdateRequest{
		"changed ACM label": {Object: &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: changed, Version: read.GetVersion()}}, UpdateMask: mask},
		"no field":          {Object: &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Version: read.GetVersion()}}},
	} {
		if _, err := s.Update(ctx, req); status.Code(err) != codes.Aborted {
			t.Fatalf("Update(stale version, %s) error = %v, want Aborted", name, err)
		}
	}
	// At the current version the changed label is refused.
	if _, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object:     &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: changed, Version: added.GetObject().GetMetadata().GetVersion()}},
		UpdateMask: mask,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Update(current version, changed ACM label) error = %v, want InvalidArgument", err)
	}

	// Remove from AI Grid: the console sends the labels without ai-grid/enabled
	// and without the ACM labels it doesn't own.
	remove := map[string]string{}
	for k, v := range added.GetObject().GetMetadata().GetLabels() {
		if strings.HasPrefix(k, "ai-grid/") || k == "location" {
			remove[k] = v
		}
	}
	delete(remove, "ai-grid/enabled")
	removed, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object:     &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: remove, Version: added.GetObject().GetMetadata().GetVersion()}},
		UpdateMask: mask,
	})
	if err != nil {
		t.Fatalf("Update(remove) error = %v", err)
	}
	labels := removed.GetObject().GetMetadata().GetLabels()
	if _, ok := labels["ai-grid/enabled"]; ok || labels["name"] != "site-oslo-1" || labels["location"] != read.GetLabels()["location"] {
		t.Fatalf("Update(remove) labels = %v", labels)
	}

	// lock without a version is refused.
	if _, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object: &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: remove}}, UpdateMask: mask, Lock: true,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Update(lock, no version) error = %v, want InvalidArgument", err)
	}
}

// TestRealClustersServerProvisioning reads status.provisioning from the
// ai-grid-ready Policy: fixtures keep their ai-grid/provisioned state, a
// cluster added to the grid is Pending, NonCompliant while it installs, then
// Compliant, and removing it clears the state.
func TestRealClustersServerProvisioning(t *testing.T) {
	fx, err := hub.LoadFixtures(fixturesDir)
	if err != nil {
		t.Fatalf("hub.LoadFixtures() error = %v", err)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h, err := hub.New(fx, hub.WithLogger(slog.New(slog.DiscardHandler)), hub.WithClock(clock))
	if err != nil {
		t.Fatalf("hub.New() error = %v", err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	s := clustersServer(t, &rest.Config{Host: srv.URL, BearerToken: h.ServiceAccountToken()})
	ctx := context.Background()

	state := func(id string) string {
		t.Helper()
		got, err := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: id})
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		return got.GetObject().GetStatus().GetProvisioning()
	}
	list, err := s.List(ctx, &aigridv1.ClustersListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	got := map[string]string{}
	for _, c := range list.GetItems() {
		got[c.GetId()] = c.GetStatus().GetProvisioning()
	}
	if got["site-paris-1"] != "Compliant" || got["site-ashburn-1"] != "NonCompliant" || got["site-oslo-1"] != "" || got["local-cluster"] != "" {
		t.Fatalf("List() provisioning = %v", got)
	}

	mask := &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels"}}
	if _, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object:     &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: map[string]string{"ai-grid/enabled": "true", "location": "oslo"}}},
		UpdateMask: mask,
	}); err != nil {
		t.Fatalf("Update(add) error = %v", err)
	}
	if p := state("site-oslo-1"); p != "Pending" {
		t.Fatalf("after add, provisioning = %q, want Pending", p)
	}
	mu.Lock()
	now = now.Add(hub.DefaultEvaluateAfter)
	mu.Unlock()
	if p := state("site-oslo-1"); p != "NonCompliant" {
		t.Fatalf("after %v, provisioning = %q, want NonCompliant", hub.DefaultEvaluateAfter, p)
	}
	mu.Lock()
	now = now.Add(hub.DefaultProvisionAfter - hub.DefaultEvaluateAfter)
	mu.Unlock()
	if p := state("site-oslo-1"); p != "Compliant" {
		t.Fatalf("after %v, provisioning = %q, want Compliant", hub.DefaultProvisionAfter, p)
	}

	if _, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object:     &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: map[string]string{"location": "oslo"}}},
		UpdateMask: mask,
	}); err != nil {
		t.Fatalf("Update(remove) error = %v", err)
	}
	if p := state("site-oslo-1"); p != "" {
		t.Fatalf("after remove, provisioning = %q, want empty", p)
	}
}
