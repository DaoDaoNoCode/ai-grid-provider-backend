package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var gvr = schema.GroupVersionResource{Group: Group, Version: Version, Resource: Resource}

const testToken = "test-sa-token"

// fakeClock is a settable clock for the scenario timeline.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// readyCluster is a joined, available cluster as the hub controllers leave
// it, so that reconcile changes nothing on it.
func readyCluster(name, rv string) ManagedCluster {
	created := metav1.NewTime(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	lease := int32(60)
	cond := func(typ, reason, msg string) metav1.Condition {
		return metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: reason, Message: msg, LastTransitionTime: created}
	}
	return ManagedCluster{
		TypeMeta: metav1.TypeMeta{APIVersion: APIVersion, Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: types.UID("00000000-0000-4000-8000-" + strings.Repeat("0", 12-len(rv)) + rv),
			ResourceVersion: rv, Generation: 3, CreationTimestamp: created,
			Labels: map[string]string{
				LabelName: name, LabelClusterSet: ClusterSetDefault,
				LabelAddonPrefix + "work-manager": AddonAvailable,
			},
			Annotations: map[string]string{AnnotationCreatedVia: AnnotationCreatedViaOther},
			Finalizers:  []string{FinalizerImportCleanup, FinalizerClusterInfo, FinalizerResourceCleanup, FinalizerClusterRole, FinalizerManifestWork, FinalizerClusterAPIResource},
		},
		Spec: ManagedClusterSpec{HubAcceptsClient: true, LeaseDurationSeconds: &lease},
		Status: &ManagedClusterStatus{Conditions: []metav1.Condition{
			cond(CondImportSucceeded, ReasonImported, MessageImported),
			cond(CondHubAccepted, ReasonHubAccepted, MessageHubAccepted),
			cond(CondJoined, ReasonJoined, MessageJoined),
			cond(CondAvailable, ReasonAvailable, MessageAvailable),
			cond(CondClockSynced, ReasonClockSynced, MessageClockSynced),
		}, Version: ManagedClusterVersion{Kubernetes: "v1.35.6"}},
	}
}

func testFixtures() *Fixtures {
	return &Fixtures{
		Clusters: []ManagedCluster{readyCluster("site-b", "120"), readyCluster("site-a", "100")},
		Access: Access{
			DefaultAllowed: true, DeniedTokens: []string{"viewer-token"}, RejectedTokens: []string{"expired-token"},
			ServiceAccount: ServiceAccount{Token: testToken, Username: DefaultServiceAccountUsername},
		},
	}
}

type testHub struct {
	hub   *Hub
	srv   *httptest.Server
	clock *fakeClock
}

func startHub(t *testing.T, fx *Fixtures) *testHub {
	t.Helper()
	clock := newFakeClock()
	h, err := New(fx, WithClock(clock.Now), WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return &testHub{hub: h, srv: srv, clock: clock}
}

func (th *testHub) client(t *testing.T, token string) dynamic.ResourceInterface {
	t.Helper()
	c, err := dynamic.NewForConfig(&rest.Config{Host: th.srv.URL, BearerToken: token, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatalf("dynamic.NewForConfig() error = %v", err)
	}
	return c.Resource(gvr)
}

// do sends a raw request and returns the status code and decoded JSON body.
func (th *testHub) do(t *testing.T, method, path, token, contentType, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, th.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("%s %s Content-Type = %q, want application/json", method, path, got)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s body %s: %v", method, path, raw, err)
	}
	return resp.StatusCode, out
}

func assertStatus(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if !bytes.Equal(g, w) {
		t.Fatalf("Status body\n got %s\nwant %s", g, w)
	}
}

func TestListIsSortedWithListMeta(t *testing.T) {
	th := startHub(t, testFixtures())
	list, err := th.client(t, testToken).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list.GetAPIVersion() != APIVersion || list.GetKind() != ListKind {
		t.Fatalf("list type = %s %s", list.GetAPIVersion(), list.GetKind())
	}
	if rv := list.GetResourceVersion(); rv != "120" {
		t.Fatalf("list resourceVersion = %q, want the highest seeded one, 120", rv)
	}
	var names []string
	for _, item := range list.Items {
		names = append(names, item.GetName())
		if item.GetKind() != Kind || item.GetAPIVersion() != APIVersion {
			t.Fatalf("item %s type = %s %s", item.GetName(), item.GetAPIVersion(), item.GetKind())
		}
	}
	if strings.Join(names, ",") != "site-a,site-b" {
		t.Fatalf("names = %v, want sorted", names)
	}

	selected, err := th.client(t, testToken).List(context.Background(), metav1.ListOptions{LabelSelector: LabelName + "=site-b"})
	if err != nil || len(selected.Items) != 1 || selected.Items[0].GetName() != "site-b" {
		t.Fatalf("List(labelSelector) = %v, %v", selected, err)
	}
}

func TestGetAndNotFound(t *testing.T) {
	th := startHub(t, testFixtures())
	obj, err := th.client(t, testToken).Get(context.Background(), "site-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if obj.GetResourceVersion() != "100" || obj.GetUID() == "" {
		t.Fatalf("Get() = rv %q uid %q", obj.GetResourceVersion(), obj.GetUID())
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if len(conds) != 5 {
		t.Fatalf("conditions = %v", conds)
	}

	_, err = th.client(t, testToken).Get(context.Background(), "missing", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Get(missing) error = %v, want NotFound", err)
	}
	code, body := th.do(t, http.MethodGet, managedClustersPath+"/missing", testToken, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("code = %d", code)
	}
	assertStatus(t, body, map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure",
		"message": `managedclusters.cluster.open-cluster-management.io "missing" not found`,
		"reason":  "NotFound",
		"details": map[string]any{"name": "missing", "group": Group, "kind": Resource},
		"code":    404,
	})
}

func TestAuthentication(t *testing.T) {
	th := startHub(t, testFixtures())
	want := map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure",
		"message": "Unauthorized", "reason": "Unauthorized", "code": 401,
	}
	for _, token := range []string{"", "expired-token"} {
		code, body := th.do(t, http.MethodGet, managedClustersPath, token, "", "")
		if code != http.StatusUnauthorized {
			t.Fatalf("token %q: code = %d", token, code)
		}
		assertStatus(t, body, want)
	}
	if _, err := th.client(t, "expired-token").List(context.Background(), metav1.ListOptions{}); !apierrors.IsUnauthorized(err) {
		t.Fatalf("List(expired) error = %v, want Unauthorized", err)
	}
	// Any other token stands for a user; the unknown path is a 404.
	code, body := th.do(t, http.MethodGet, "/apis/example.com/v1/widgets", "some-user-token", "", "")
	if code != http.StatusNotFound || body["message"] != "the server could not find the requested resource" {
		t.Fatalf("unknown path = %d %v", code, body)
	}
}

func TestForbiddenForDeniedToken(t *testing.T) {
	th := startHub(t, testFixtures())
	_, err := th.client(t, "viewer-token").Get(context.Background(), "site-a", metav1.GetOptions{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Get(viewer) error = %v, want Forbidden", err)
	}
	want := `managedclusters.cluster.open-cluster-management.io "site-a" is forbidden: User "dev-user" cannot get resource "managedclusters" in API group "cluster.open-cluster-management.io" at the cluster scope`
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	// The ServiceAccount stays allowed when nobody else is.
	fx := testFixtures()
	fx.Access.DefaultAllowed = false
	th = startHub(t, fx)
	if _, err := th.client(t, testToken).List(context.Background(), metav1.ListOptions{}); err != nil {
		t.Fatalf("List(sa) error = %v", err)
	}
	if _, err := th.client(t, "someone").List(context.Background(), metav1.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("List(someone) error = %v, want Forbidden", err)
	}
}

func newObject(name string, hubAccepts bool, labels map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if labels != nil {
		meta["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": APIVersion, "kind": Kind, "metadata": meta,
		"spec":   map[string]any{"hubAcceptsClient": hubAccepts},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Bogus", "status": "True"}}},
	}}
}

func TestCreateThenControllersReact(t *testing.T) {
	th := startHub(t, testFixtures())
	client := th.client(t, testToken)
	created, err := client.Create(context.Background(), newObject("site-new", true, map[string]any{"ai-grid/enabled": "true"}), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// The create response is the object as the API server stored it: CRD
	// default, webhook label, system fields, no status.
	if created.GetResourceVersion() != "121" || created.GetGeneration() != 1 || created.GetUID() == "" || created.GetCreationTimestamp().Time.IsZero() {
		t.Fatalf("created meta = rv %q gen %d uid %q ts %v", created.GetResourceVersion(), created.GetGeneration(), created.GetUID(), created.GetCreationTimestamp())
	}
	if lease, _, _ := unstructured.NestedInt64(created.Object, "spec", "leaseDurationSeconds"); lease != 60 {
		t.Fatalf("leaseDurationSeconds = %d, want the CRD default 60", lease)
	}
	if got := created.GetLabels(); got[LabelClusterSet] != ClusterSetDefault || got["ai-grid/enabled"] != "true" {
		t.Fatalf("labels = %v", got)
	}
	if _, found := created.Object["status"]; found {
		t.Fatalf("status = %v, want none on create", created.Object["status"])
	}

	got, err := client.Get(context.Background(), "site-new", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.GetResourceVersion() != "122" {
		t.Fatalf("rv after controllers = %q, want 122", got.GetResourceVersion())
	}
	if got.GetGeneration() != 2 {
		t.Fatalf("generation = %d, want 2 (taint controller changed spec)", got.GetGeneration())
	}
	if got.GetLabels()[LabelName] != "site-new" || got.GetAnnotations()[AnnotationCreatedVia] != AnnotationCreatedViaOther {
		t.Fatalf("labels %v annotations %v", got.GetLabels(), got.GetAnnotations())
	}
	if len(got.GetFinalizers()) != 5 {
		t.Fatalf("finalizers = %v", got.GetFinalizers())
	}
	conds := conditionsOf(t, got)
	if c := conds[CondHubAccepted]; c["status"] != "True" || c["reason"] != ReasonHubAccepted {
		t.Fatalf("HubAccepted = %v", c)
	}
	if c := conds[CondImportSucceeded]; c["status"] != "False" || c["reason"] != ReasonWaitForImporting || c["message"] != MessageWaitForImporting {
		t.Fatalf("ImportSucceeded = %v", c)
	}
	if _, bogus := conds["Bogus"]; bogus {
		t.Fatal("client-supplied status was stored")
	}
	taints, _, _ := unstructured.NestedSlice(got.Object, "spec", "taints")
	if len(taints) != 1 || taints[0].(map[string]any)["key"] != TaintUnreachable || taints[0].(map[string]any)["timeAdded"] == nil {
		t.Fatalf("taints = %v", taints)
	}
}

func conditionsOf(t *testing.T, obj *unstructured.Unstructured) map[string]map[string]any {
	t.Helper()
	raw, _, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("conditions: %v", err)
	}
	out := map[string]map[string]any{}
	for _, c := range raw {
		m := c.(map[string]any)
		out[m["type"].(string)] = m
	}
	return out
}

func TestCreateErrors(t *testing.T) {
	th := startHub(t, testFixtures())
	client := th.client(t, testToken)
	_, err := client.Create(context.Background(), newObject("site-a", true, nil), metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) || err.Error() != `managedclusters.cluster.open-cluster-management.io "site-a" already exists` {
		t.Fatalf("Create(existing) error = %v", err)
	}
	_, err = client.Create(context.Background(), newObject("Bad_Name", true, nil), metav1.CreateOptions{})
	if !apierrors.IsInvalid(err) {
		t.Fatalf("Create(Bad_Name) error = %v, want Invalid", err)
	}
	want := `ManagedCluster.cluster.open-cluster-management.io "Bad_Name" is invalid: metadata.name: Invalid value: "Bad_Name": a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')`
	if err.Error() != want {
		t.Fatalf("message\n got %s\nwant %s", err.Error(), want)
	}
	code, body := th.do(t, http.MethodPost, managedClustersPath, testToken, "application/json",
		`{"apiVersion":"cluster.open-cluster-management.io/v1","kind":"ManagedCluster","metadata":{"name":"x"},"spec":{"hubAcceptsClient":"yes"}}`)
	if code != http.StatusUnprocessableEntity || body["message"] != `ManagedCluster.cluster.open-cluster-management.io "x" is invalid: spec.hubAcceptsClient: Invalid value: "string": spec.hubAcceptsClient in body must be of type boolean: "string"` {
		t.Fatalf("type error = %d %v", code, body)
	}
}

func TestMergePatch(t *testing.T) {
	th := startHub(t, testFixtures())
	client := th.client(t, testToken)
	ctx := context.Background()

	patched, err := client.Patch(ctx, "site-a", types.MergePatchType, []byte(`{"metadata":{"labels":{"ai-grid/enabled":"true"}},"status":{"conditions":[]}}`), metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("Patch(labels) error = %v", err)
	}
	if patched.GetLabels()["ai-grid/enabled"] != "true" || patched.GetLabels()[LabelName] != "site-a" {
		t.Fatalf("labels = %v", patched.GetLabels())
	}
	if patched.GetResourceVersion() != "121" || patched.GetGeneration() != 3 {
		t.Fatalf("rv %q gen %d, want 121 and an unchanged generation", patched.GetResourceVersion(), patched.GetGeneration())
	}
	if len(conditionsOf(t, patched)) != 5 {
		t.Fatal("status was writable through the main resource")
	}

	// The same patch again changes nothing: no write, same resourceVersion.
	again, err := client.Patch(ctx, "site-a", types.MergePatchType, []byte(`{"metadata":{"labels":{"ai-grid/enabled":"true"}}}`), metav1.PatchOptions{})
	if err != nil || again.GetResourceVersion() != "121" {
		t.Fatalf("no-op Patch = rv %q, %v; want 121", again.GetResourceVersion(), err)
	}

	// A spec change bumps generation; the registration controller then
	// writes the denial.
	denied, err := client.Patch(ctx, "site-a", types.MergePatchType, []byte(`{"spec":{"hubAcceptsClient":false}}`), metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("Patch(spec) error = %v", err)
	}
	if denied.GetGeneration() != 4 || denied.GetResourceVersion() != "122" {
		t.Fatalf("gen %d rv %q, want 4 and 122", denied.GetGeneration(), denied.GetResourceVersion())
	}
	got, _ := client.Get(ctx, "site-a", metav1.GetOptions{})
	if c := conditionsOf(t, got)[CondHubAccepted]; c["status"] != "False" || c["reason"] != ReasonHubDenied || got.GetResourceVersion() != "123" {
		t.Fatalf("after controllers HubAccepted = %v rv %q", c, got.GetResourceVersion())
	}

	// JSON patch is served too.
	jp, err := client.Patch(ctx, "site-b", types.JSONPatchType, []byte(`[{"op":"add","path":"/metadata/annotations/ai-grid~1site-name","value":"B"}]`), metav1.PatchOptions{})
	if err != nil || jp.GetAnnotations()["ai-grid/site-name"] != "B" {
		t.Fatalf("JSON patch = %v, %v", jp.GetAnnotations(), err)
	}
	// Strategic merge patch is not, for a custom resource.
	_, err = client.Patch(ctx, "site-b", types.StrategicMergePatchType, []byte(`{}`), metav1.PatchOptions{})
	if !apierrors.IsUnsupportedMediaType(err) {
		t.Fatalf("strategic patch error = %v, want UnsupportedMediaType", err)
	}
	_, err = client.Patch(ctx, "missing", types.MergePatchType, []byte(`{}`), metav1.PatchOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Patch(missing) error = %v", err)
	}
}

func TestPatchConflict(t *testing.T) {
	th := startHub(t, testFixtures())
	_, err := th.client(t, testToken).Patch(context.Background(), "site-a", types.MergePatchType,
		[]byte(`{"metadata":{"resourceVersion":"99","labels":{"x":"y"}}}`), metav1.PatchOptions{})
	if !apierrors.IsConflict(err) {
		t.Fatalf("Patch(stale) error = %v, want Conflict", err)
	}
	want := `Operation cannot be fulfilled on managedclusters.cluster.open-cluster-management.io "site-a": the object has been modified; please apply your changes to the latest version and try again`
	if err.Error() != want {
		t.Fatalf("message = %q", err.Error())
	}
	// The current resourceVersion is accepted.
	if _, err := th.client(t, testToken).Patch(context.Background(), "site-a", types.MergePatchType,
		[]byte(`{"metadata":{"resourceVersion":"100","labels":{"x":"y"}}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("Patch(current rv) error = %v", err)
	}
}

func TestUpdate(t *testing.T) {
	th := startHub(t, testFixtures())
	client := th.client(t, testToken)
	ctx := context.Background()
	obj, _ := client.Get(ctx, "site-b", metav1.GetOptions{})
	obj.SetLabels(map[string]string{"only": "this"})
	updated, err := client.Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	// The webhook puts the clusterset label back before the write.
	if updated.GetLabels()[LabelClusterSet] != ClusterSetDefault || updated.GetLabels()["only"] != "this" {
		t.Fatalf("labels = %v", updated.GetLabels())
	}
	obj.SetResourceVersion("")
	if _, err := client.Update(ctx, obj, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) ||
		!strings.Contains(err.Error(), "metadata.resourceVersion: Invalid value: 0: must be specified for an update") {
		t.Fatalf("Update(no rv) error = %v", err)
	}
	obj.SetResourceVersion("1")
	if _, err := client.Update(ctx, obj, metav1.UpdateOptions{}); !apierrors.IsConflict(err) {
		t.Fatalf("Update(stale) error = %v", err)
	}
}

func TestDeleteWithFinalizersDetaches(t *testing.T) {
	th := startHub(t, testFixtures())
	client := th.client(t, testToken)
	ctx := context.Background()

	code, body := th.do(t, http.MethodDelete, managedClustersPath+"/site-a", testToken, "", "")
	if code != http.StatusOK || body["kind"] != Kind {
		t.Fatalf("DELETE = %d %v, want 200 with the object", code, body)
	}
	meta := body["metadata"].(map[string]any)
	if meta["deletionTimestamp"] == nil || meta["deletionGracePeriodSeconds"] != float64(0) || meta["generation"] != float64(4) {
		t.Fatalf("metadata = %v", meta)
	}
	got, err := client.Get(ctx, "site-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(detaching) error = %v", err)
	}
	conds := conditionsOf(t, got)
	if c := conds[CondImportSucceeded]; c["reason"] != ReasonDetaching || c["message"] != MessageDetaching {
		t.Fatalf("ImportSucceeded = %v", c)
	}
	if c := conds[CondDeleting]; c["status"] != "False" || c["message"] != "The resource managedclusteraddons is remaning, the remaining count is 1, the finalizer pending count is 0" {
		t.Fatalf("Deleting = %v", c)
	}
	// Deleting again changes nothing.
	if err := client.Delete(ctx, "site-a", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete(again) error = %v", err)
	}
	th.clock.Advance(DefaultDetachAfter - time.Second)
	if _, err := client.Get(ctx, "site-a", metav1.GetOptions{}); err != nil {
		t.Fatalf("Get before detach finished: %v", err)
	}
	th.clock.Advance(time.Second)
	if _, err := client.Get(ctx, "site-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Get after detach = %v, want NotFound", err)
	}
}

func TestDeleteWithoutFinalizersAndPreconditions(t *testing.T) {
	fx := testFixtures()
	fx.Clusters[0].Finalizers = nil
	th := startHub(t, fx)
	client := th.client(t, testToken)
	stale := "1"
	err := client.Delete(context.Background(), "site-b", metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &stale}})
	if !apierrors.IsConflict(err) || !strings.HasSuffix(err.Error(), "Precondition failed: ResourceVersion in precondition: 1, ResourceVersion in object meta: 120") {
		t.Fatalf("Delete(stale precondition) error = %v", err)
	}
	code, body := th.do(t, http.MethodDelete, managedClustersPath+"/site-b", testToken, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE = %d", code)
	}
	assertStatus(t, body, map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Success",
		"details": map[string]any{"name": "site-b", "group": Group, "kind": Resource, "uid": "00000000-0000-4000-8000-000000000120"},
	})
	if err := client.Delete(context.Background(), "site-b", metav1.DeleteOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Delete(gone) error = %v", err)
	}
}

func TestUnsupportedVerbs(t *testing.T) {
	th := startHub(t, testFixtures())
	if _, err := th.client(t, testToken).Watch(context.Background(), metav1.ListOptions{}); !apierrors.IsMethodNotSupported(err) {
		t.Fatalf("Watch() error = %v", err)
	}
	if err := th.client(t, testToken).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{}); !apierrors.IsMethodNotSupported(err) {
		t.Fatalf("DeleteCollection() error = %v", err)
	}
}

func TestSelfSubjectAccessReview(t *testing.T) {
	th := startHub(t, testFixtures())
	review := `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"verb":"patch","resource":"managedclusters","group":"cluster.open-cluster-management.io"}}}`
	code, body := th.do(t, http.MethodPost, ssarPath, "dev-token", "application/json", review)
	if code != http.StatusCreated || body["status"].(map[string]any)["allowed"] != true {
		t.Fatalf("allowed review = %d %v", code, body)
	}
	if reason := body["status"].(map[string]any)["reason"]; reason != `RBAC: allowed by ClusterRoleBinding "ai-grid-provider-admins" of ClusterRole "ai-grid-provider-admin" to User "dev-user"` {
		t.Fatalf("reason = %v", reason)
	}
	code, body = th.do(t, http.MethodPost, ssarPath, "viewer-token", "application/json", review)
	if code != http.StatusCreated || body["status"].(map[string]any)["allowed"] != false {
		t.Fatalf("denied review = %d %v", code, body)
	}
	code, body = th.do(t, http.MethodPost, ssarPath, "expired-token", "application/json", review)
	if code != http.StatusUnauthorized || body["reason"] != "Unauthorized" {
		t.Fatalf("rejected review = %d %v", code, body)
	}
}

func TestConcurrentWrites(t *testing.T) {
	th := startHub(t, testFixtures())
	client := th.client(t, testToken)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := []string{"site-a", "site-b"}[i%2]
			patch := []byte(`{"metadata":{"labels":{"n` + strconv.Itoa(i) + `":"v"}}}`)
			if _, err := client.Patch(context.Background(), name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
				t.Errorf("Patch() error = %v", err)
			}
			if _, err := client.List(context.Background(), metav1.ListOptions{}); err != nil {
				t.Errorf("List() error = %v", err)
			}
		}()
	}
	wg.Wait()
	list, _ := client.List(context.Background(), metav1.ListOptions{})
	if rv := list.GetResourceVersion(); rv != "140" {
		t.Fatalf("resourceVersion after 20 writes = %q, want 140", rv)
	}
}

// TestDetachKeepsForeignFinalizers: when the hub controllers finish they
// remove only their own finalizers; an object that still carries another
// one stays terminating until that is removed too, as on the API server.
func TestDetachKeepsForeignFinalizers(t *testing.T) {
	fx := testFixtures()
	lyon := detachingCluster("edge-lyon-2", "130")
	lyon.Finalizers = append(lyon.Finalizers, "example.com/keep")
	fx.Clusters = append(fx.Clusters, lyon)
	fx.Scenarios = []ScenarioEvent{{Cluster: "edge-lyon-2", At: Duration(5 * time.Second), Event: EventDetached}}
	th := startHub(t, fx)
	client := th.client(t, testToken)
	ctx := context.Background()

	if _, err := client.Patch(ctx, "site-a", types.MergePatchType, []byte(`{"metadata":{"finalizers":["`+FinalizerImportCleanup+`","`+FinalizerClusterInfo+`","`+FinalizerResourceCleanup+`","`+FinalizerClusterRole+`","`+FinalizerManifestWork+`","`+FinalizerClusterAPIResource+`","example.com/keep"]}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("Patch(finalizers) error = %v", err)
	}
	if err := client.Delete(ctx, "site-a", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	before := getCluster(t, th, "site-a").GetResourceVersion()
	th.clock.Advance(DefaultDetachAfter)
	for _, name := range []string{"site-a", "edge-lyon-2"} {
		obj, err := client.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("%s gone although a foreign finalizer remains: %v", name, err)
		}
		if got := obj.GetFinalizers(); len(got) != 1 || got[0] != "example.com/keep" || obj.GetDeletionTimestamp() == nil {
			t.Fatalf("%s finalizers %v deletionTimestamp %v", name, got, obj.GetDeletionTimestamp())
		}
	}
	if after := getCluster(t, th, "site-a").GetResourceVersion(); after == before {
		t.Fatal("finalizer removal was not a write")
	}
	// Removing the last finalizer deletes the object.
	if _, err := client.Patch(ctx, "site-a", types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("Patch(remove finalizer) error = %v", err)
	}
	if _, err := client.Get(ctx, "site-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Get after last finalizer = %v, want NotFound", err)
	}
}

// TestAccessReviewAndRBACAgree: SelfSubjectAccessReview and the
// managedclusters authorizer give every token the same answer.
func TestAccessReviewAndRBACAgree(t *testing.T) {
	fx := testFixtures()
	fx.Access.DefaultAllowed = false
	th := startHub(t, fx)
	review := `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"verb":"list","resource":"managedclusters","group":"cluster.open-cluster-management.io"}}}`
	for token, want := range map[string]bool{testToken: true, "someone": false, "viewer-token": false} {
		code, body := th.do(t, http.MethodPost, ssarPath, token, "application/json", review)
		if code != http.StatusCreated || body["status"].(map[string]any)["allowed"] != want {
			t.Errorf("%s: review = %d %v, want allowed %v", token, code, body, want)
		}
		_, err := th.client(t, token).List(context.Background(), metav1.ListOptions{})
		if want && err != nil || !want && !apierrors.IsForbidden(err) {
			t.Errorf("%s: List() error = %v, want allowed %v", token, err, want)
		}
	}
	_, body := th.do(t, http.MethodPost, ssarPath, testToken, "application/json", review)
	if reason := body["status"].(map[string]any)["reason"]; !strings.Contains(reason.(string), `to User "`+DefaultServiceAccountUsername+`"`) {
		t.Errorf("service account review reason = %v", reason)
	}
}

// TestPatchErrorCodes follows k8s.io/apiserver pkg/endpoints/handlers/patch.go
// applyJSPatch: a JSON patch that does not decode is 400, one that decodes
// but does not apply is 422; a merge patch that is not JSON is 400.
func TestPatchErrorCodes(t *testing.T) {
	th := startHub(t, testFixtures())
	client := th.client(t, testToken)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		pt    types.PatchType
		patch string
		code  int
		check func(error) bool
	}{
		{"json patch that does not decode", types.JSONPatchType, `[{"op":`, 400, apierrors.IsBadRequest},
		{"json patch whose test fails", types.JSONPatchType, `[{"op":"test","path":"/spec/hubAcceptsClient","value":false}]`, 422, apierrors.IsInvalid},
		{"json patch on a missing path", types.JSONPatchType, `[{"op":"remove","path":"/metadata/labels/nope"}]`, 422, apierrors.IsInvalid},
		{"merge patch that is not JSON", types.MergePatchType, `{`, 400, apierrors.IsBadRequest},
	} {
		_, err := client.Patch(ctx, "site-a", tc.pt, []byte(tc.patch), metav1.PatchOptions{})
		var status apierrors.APIStatus
		if !tc.check(err) || !errorsAs(err, &status) || int(status.Status().Code) != tc.code {
			t.Errorf("%s: error = %v, want code %d", tc.name, err, tc.code)
		}
	}
	if obj := getCluster(t, th, "site-a"); obj.GetResourceVersion() != "100" {
		t.Fatalf("a failed patch wrote: rv %s", obj.GetResourceVersion())
	}
}

func errorsAs(err error, target *apierrors.APIStatus) bool {
	s, ok := err.(apierrors.APIStatus)
	if ok {
		*target = s
	}
	return ok
}
