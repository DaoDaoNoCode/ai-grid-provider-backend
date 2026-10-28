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

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/acmsearch"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/devserve"
	"github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/hub"
	fakekeycloak "github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/servers"
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
	if _, err := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: "missing"}); connect.CodeOf(err) != connect.CodeNotFound {
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
	if _, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{Object: &aigridv1.ManagedCluster{Id: "missing"}}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("Update(missing) error = %v, want NotFound", err)
	}

	if _, err := s.Create(ctx, &aigridv1.ClustersCreateRequest{Object: &aigridv1.ManagedCluster{Id: "site-paris-1"}}); connect.CodeOf(err) != connect.CodeAlreadyExists {
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
	if _, err := denied.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-paris-1"}); connect.CodeOf(err) != connect.CodePermissionDenied {
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

// TestRealTenantsServerVersionOnFakeKeycloak runs the console's organization
// flow against the fake Keycloak: Create stamps version 1 and the creation
// time, a fixture organization (no attributes) reads as version 1, an update
// at the read version bumps it, a stale one is Aborted, removing every domain
// clears them, and a stale delete is Aborted.
func TestRealTenantsServerVersionOnFakeKeycloak(t *testing.T) {
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
	s, err := servers.NewTenantsServer().SetLogger(slog.New(slog.DiscardHandler)).SetOrganizationClient(keycloak.New(keycloak.Config{
		BaseURL: srv.URL, AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "admin", HTTPClient: srv.Client(),
	})).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	ctx := context.Background()

	list, err := s.List(ctx, &aigridv1.TenantsListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if meta := list.GetItems()[0].GetMetadata(); meta.GetVersion() != 1 || meta.GetCreationTimestamp() != nil {
		t.Fatalf("fixture tenant metadata = %v, want version 1 and no creation time", meta)
	}

	created, err := s.Create(ctx, &aigridv1.TenantsCreateRequest{Object: &aigridv1.Tenant{
		Metadata: &aigridv1.Metadata{Name: "scratch-org"},
		Spec:     &aigridv1.TenantSpec{Domains: []string{"scratch.example.com"}},
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id, meta := created.GetObject().GetId(), created.GetObject().GetMetadata()
	if meta.GetVersion() != 1 || meta.GetCreationTimestamp() == nil || time.Since(meta.GetCreationTimestamp().AsTime()) > time.Minute {
		t.Fatalf("Create() metadata = %v", meta)
	}
	got, err := s.Get(ctx, &aigridv1.TenantsGetRequest{Id: id})
	if err != nil || got.GetObject().GetMetadata().GetVersion() != 1 || !got.GetObject().GetMetadata().GetCreationTimestamp().AsTime().Equal(meta.GetCreationTimestamp().AsTime()) {
		t.Fatalf("Get() = %v, %v", got, err)
	}

	clear := &aigridv1.TenantsUpdateRequest{
		Object:     &aigridv1.Tenant{Id: id, Metadata: &aigridv1.Metadata{Version: 1}, Spec: &aigridv1.TenantSpec{}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.domains"}},
		Lock:       true,
	}
	updated, err := s.Update(ctx, clear)
	if err != nil {
		t.Fatalf("Update(clear domains) error = %v", err)
	}
	if updated.GetObject().GetMetadata().GetVersion() != 2 || len(updated.GetObject().GetSpec().GetDomains()) != 0 {
		t.Fatalf("Update(clear domains) = %v", updated.GetObject())
	}
	if !updated.GetObject().GetMetadata().GetCreationTimestamp().AsTime().Equal(meta.GetCreationTimestamp().AsTime()) {
		t.Fatalf("Update() changed the creation time: %v", updated.GetObject().GetMetadata())
	}
	if _, err := s.Update(ctx, clear); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("Update(stale version) error = %v, want Aborted", err)
	}
	if _, err := s.Delete(ctx, &aigridv1.TenantsDeleteRequest{Id: id, Version: 1}); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("Delete(stale version) error = %v, want Aborted", err)
	}
	if _, err := s.Delete(ctx, &aigridv1.TenantsDeleteRequest{Id: id, Version: 2}); err != nil {
		t.Fatalf("Delete(current version) error = %v", err)
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
	if connect.CodeOf(err) != connect.CodeAborted {
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
		if _, err := s.Update(ctx, req); connect.CodeOf(err) != connect.CodeAborted {
			t.Fatalf("Update(stale version, %s) error = %v, want Aborted", name, err)
		}
	}
	// At the current version the changed label is refused.
	if _, err := s.Update(ctx, &aigridv1.ClustersUpdateRequest{
		Object:     &aigridv1.ManagedCluster{Id: "site-oslo-1", Metadata: &aigridv1.Metadata{Labels: changed, Version: added.GetObject().GetMetadata().GetVersion()}},
		UpdateMask: mask,
	}); connect.CodeOf(err) != connect.CodeInvalidArgument {
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
	}); connect.CodeOf(err) != connect.CodeInvalidArgument {
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

// startCatalogServers starts the fake hub and builds the real Models and
// Subscriptions servers on it, as make dev-fake does: status from the hub's
// status Policies, commits to an in-memory repository, which is returned.
func startCatalogServers(t *testing.T) (*servers.ModelsServer, *servers.SubscriptionsServer, *gitops.MemoryWriter) {
	t.Helper()
	h, srv := startHub(t)
	cfg := &rest.Config{Host: srv.URL, BearerToken: h.ServiceAccountToken(), QPS: 1000, Burst: 1000}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic.NewForConfig() error = %v", err)
	}
	status := servers.NewCatalogStatusReader(client, "")
	writer := gitops.NewMemoryWriter(nil)
	models, err := servers.NewModelsServer().SetLogger(slog.New(slog.DiscardHandler)).
		SetWriter(writer).SetCatalogStatus(status).SetDynamicClient(client).Build()
	if err != nil {
		t.Fatalf("Models Build() error = %v", err)
	}
	subscriptions, err := servers.NewSubscriptionsServer().SetLogger(slog.New(slog.DiscardHandler)).
		SetWriter(writer).SetCatalogStatus(status).Build()
	if err != nil {
		t.Fatalf("Subscriptions Build() error = %v", err)
	}
	return models, subscriptions, writer
}

// The real Models and Subscriptions servers, committing to an in-memory
// repository, read status from the fake hub's status Policies
// (fixtures/default/policies.json), as in make dev-fake.
func TestRealCatalogStatusOnFakeHub(t *testing.T) {
	models, subscriptions, _ := startCatalogServers(t)
	ctx := context.Background()

	replicas := int32(1)
	created, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: &aigridv1.Model{
		Id: "granite-3b",
		Spec: &aigridv1.ModelSpec{
			Model:    &aigridv1.ModelSource{Uri: "hf://ibm-granite/granite-3b"},
			Replicas: &replicas,
			Placement: &aigridv1.ModelPlacement{LabelSelector: &aigridv1.ModelLabelSelector{
				MatchExpressions: []*aigridv1.ModelLabelSelectorRequirement{{Key: "name", Operator: "In", Values: []string{"site-paris-1", "site-madrid-1"}}},
			}},
		},
	}})
	if err != nil {
		t.Fatalf("Models Create() error = %v", err)
	}
	st := created.GetObject().GetStatus()
	if st.GetPhase() != aigridv1.ModelPhase_MODEL_PHASE_PENDING || st.GetMessage() != "Not ready on site-madrid-1; ready on site-ashburn-1, site-paris-1" {
		t.Fatalf("model status = %v, %q", st.GetPhase(), st.GetMessage())
	}
	if got := st.GetPlacement().GetLocations(); !slices.Equal(got, []string{"madrid", "paris"}) {
		t.Fatalf("model locations = %v", got)
	}

	sub, err := subscriptions.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: &aigridv1.Subscription{
		Id: "team-a",
		Spec: &aigridv1.SubscriptionSpec{
			Organization: &aigridv1.SubscriptionOrganization{Id: "org-a"},
			Owner:        &aigridv1.SubscriptionOwner{Groups: []string{"team-a"}},
			ModelRefs:    []*aigridv1.SubscriptionModelRef{{Name: "granite-3b", Unlimited: true}},
		},
	}})
	if err != nil {
		t.Fatalf("Subscriptions Create() error = %v", err)
	}
	if phase := sub.GetObject().GetStatus().GetPhase(); phase != "Active" {
		t.Fatalf("subscription phase = %q, want Active", phase)
	}
	if phase := sub.GetAuthPolicy().GetPhase(); phase != "Active" {
		t.Fatalf("auth policy %q phase = %q, want Active", sub.GetAuthPolicy().GetId(), phase)
	}
}

// The real Subscriptions server on the fake hub, as in make dev-fake, refuses
// a rate-limit window longer than 8784h on Create and Update and commits
// nothing for it, and accepts 8784h and 24h.
func TestRealSubscriptionsTokenWindowOnFakeHub(t *testing.T) {
	models, subscriptions, writer := startCatalogServers(t)
	ctx := context.Background()

	replicas := int32(1)
	if _, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: &aigridv1.Model{
		Id: "win-model",
		Spec: &aigridv1.ModelSpec{
			Model:    &aigridv1.ModelSource{Uri: "hf://facebook/opt-125m"},
			Replicas: &replicas,
			Placement: &aigridv1.ModelPlacement{LabelSelector: &aigridv1.ModelLabelSelector{
				MatchExpressions: []*aigridv1.ModelLabelSelectorRequirement{{Key: "name", Operator: "In", Values: []string{"site-paris-1"}}},
			}},
		},
	}}); err != nil {
		t.Fatalf("Models Create() error = %v", err)
	}
	subscription := func(id, window string) *aigridv1.Subscription {
		return &aigridv1.Subscription{
			Id: id,
			Spec: &aigridv1.SubscriptionSpec{
				Organization: &aigridv1.SubscriptionOrganization{Id: "org-a"},
				Owner:        &aigridv1.SubscriptionOwner{Groups: []string{"team-a"}},
				ModelRefs: []*aigridv1.SubscriptionModelRef{{
					Name:            "win-model",
					TokenRateLimits: []*aigridv1.TokenRateLimit{{Limit: 100, Window: window}},
				}},
			},
		}
	}

	for _, window := range []string{"9999h", "8785h"} {
		commits := len(writer.Messages())
		_, err := subscriptions.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: subscription("win-refused", window)})
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("Create(window %q) error = %v, want InvalidArgument", window, err)
		}
		if n := len(writer.Messages()); n != commits {
			t.Fatalf("Create(window %q) commits = %d, want %d", window, n, commits)
		}
	}
	for _, window := range []string{"8784h", "24h"} {
		if _, err := subscriptions.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: subscription("win-"+window, window)}); err != nil {
			t.Fatalf("Create(window %q) error = %v", window, err)
		}
	}

	commits := len(writer.Messages())
	_, err := subscriptions.Update(ctx, &aigridv1.SubscriptionsUpdateRequest{
		Object:     subscription("win-24h", "9999h"),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.model_refs"}},
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Update(window 9999h) error = %v, want InvalidArgument", err)
	}
	if n := len(writer.Messages()); n != commits {
		t.Fatalf("Update(window 9999h) commits = %d, want %d", n, commits)
	}
	got, err := subscriptions.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "win-24h"})
	if err != nil {
		t.Fatalf("Get(win-24h) error = %v", err)
	}
	if w := got.GetObject().GetSpec().GetModelRefs()[0].GetTokenRateLimits()[0].GetWindow(); w != "24h" {
		t.Fatalf("win-24h window = %q after a refused Update, want 24h", w)
	}
}

// The backend's own ACM Search client against the fake Search: List and Get
// carry each cluster's GPUs from fixtures/default/searchnodes.json, and a
// token Search rejects leaves them out without failing the call.
func TestRealClustersServerGPUsFromFakeSearch(t *testing.T) {
	h, srv := startHub(t)
	cfg := &rest.Config{Host: srv.URL, BearerToken: h.ServiceAccountToken(), QPS: 1000, Burst: 1000}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	withToken := func(token string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+token)
			return http.DefaultTransport.RoundTrip(r)
		})}
	}
	server := func(token string) *servers.ClustersServer {
		inv := servers.NewGPUInventory(acmsearch.New(srv.URL+hub.SearchPath, withToken(token)), 0, slog.New(slog.DiscardHandler))
		s, err := servers.NewClustersServer().SetLogger(slog.New(slog.DiscardHandler)).SetDynamicClient(client).SetGPUInventory(inv).Build()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	ctx := context.Background()

	s := server(h.ServiceAccountToken())
	got, err := s.Get(ctx, &aigridv1.ClustersGetRequest{Id: "site-paris-1"})
	if err != nil {
		t.Fatal(err)
	}
	gpus := got.GetObject().GetStatus().GetGpus()
	capacity := got.GetObject().GetStatus().GetCapacity()["nvidia.com/gpu"]
	if gpus.GetTotal() != 16 || capacity != "16" || len(gpus.GetConfigurations()) != 1 ||
		gpus.GetConfigurations()[0].GetProduct() != "NVIDIA-H200" || gpus.GetConfigurations()[0].GetNodeCount() != 2 {
		t.Fatalf("site-paris-1 gpus = %v (capacity %q)", gpus, capacity)
	}
	list, err := s.List(ctx, &aigridv1.ClustersListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	strategies := map[string]string{}
	for _, c := range list.GetItems() {
		if g := c.GetStatus().GetGpus(); g != nil {
			strategies[c.GetId()] = g.GetConfigurations()[0].GetSharing().GetStrategy()
		}
	}
	want := map[string]string{"site-paris-1": "none", "site-madrid-1": "time-slicing", "site-ashburn-1": "mig", "site-frankfurt-1": "none", "edge-lyon-2": "none"}
	if !maps.Equal(strategies, want) {
		t.Fatalf("clusters with GPUs = %v, want %v", strategies, want)
	}

	// Search rejects the token: the clusters still list, without GPUs.
	rejected := server("")
	list, err = rejected.List(ctx, &aigridv1.ClustersListRequest{})
	if err != nil {
		t.Fatalf("List() must not fail on Search: %v", err)
	}
	for _, c := range list.GetItems() {
		if c.GetStatus().GetGpus() != nil {
			t.Fatalf("%s has gpus without Search: %v", c.GetId(), c.GetStatus().GetGpus())
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
