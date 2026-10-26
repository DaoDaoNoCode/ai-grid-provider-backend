package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Model / ACM CRDs the Models server talks to. Objects live in the same
// namespaced store as MaaS CRs (h.maas).
const (
	llmInferenceAPIGroup   = "serving.kserve.io"
	llmInferenceAPIVersion = "serving.kserve.io/v1alpha2"
	llmInferenceVersion    = "v1alpha2"
	llmInferenceResource   = "llminferenceservices"
	llmInferenceKind       = "LLMInferenceService"

	placementAPIGroup          = "cluster.open-cluster-management.io"
	placementAPIVersion        = "cluster.open-cluster-management.io/v1beta1"
	placementVersion           = "v1beta1"
	placementsResource         = "placements"
	placementKind              = "Placement"
	placementDecisionsResource = "placementdecisions"
	placementDecisionKind      = "PlacementDecision"

	mwrsAPIGroup   = "work.open-cluster-management.io"
	mwrsAPIVersion = "work.open-cluster-management.io/v1alpha1"
	mwrsVersion    = "v1alpha1"
	mwrsResource   = "manifestworkreplicasets"
	mwrsKind       = "ManifestWorkReplicaSet"

	manifestWorkAPIVersion = "work.open-cluster-management.io/v1"
	manifestWorkVersion    = "v1"
	manifestWorksResource  = "manifestworks"
	manifestWorkKind       = "ManifestWork"
	// manifestWorksPath lists ManifestWorks across every cluster namespace,
	// as the backend does to find a model's works.
	manifestWorksPath = "/apis/" + mwrsAPIGroup + "/" + manifestWorkVersion + "/" + manifestWorksResource

	// LabelMWRSOwner is the label the ManifestWorkReplicaSet controller puts
	// on the works it generates, "<mwrs namespace>.<mwrs name>" (the legacy
	// owner label, open-cluster-management-io/api work/v1alpha1
	// ManifestWorkReplicaSetControllerNameLabelKey; the console's
	// mock/research/acm.md 4 names it the one MCE 2.17.3 certainly writes).
	LabelMWRSOwner = "work.open-cluster-management.io/manifestworkreplicaset"
)

type nsAPI struct {
	group, version, apiVersion, kind string
}

var modelResources = map[string]nsAPI{
	llmInferenceResource: {
		group: llmInferenceAPIGroup, version: llmInferenceVersion,
		apiVersion: llmInferenceAPIVersion, kind: llmInferenceKind,
	},
	placementsResource: {
		group: placementAPIGroup, version: placementVersion,
		apiVersion: placementAPIVersion, kind: placementKind,
	},
	placementDecisionsResource: {
		group: placementAPIGroup, version: placementVersion,
		apiVersion: placementAPIVersion, kind: placementDecisionKind,
	},
	mwrsResource: {
		group: mwrsAPIGroup, version: mwrsVersion,
		apiVersion: mwrsAPIVersion, kind: mwrsKind,
	},
	manifestWorksResource: {
		group: mwrsAPIGroup, version: manifestWorkVersion,
		apiVersion: manifestWorkAPIVersion, kind: manifestWorkKind,
	},
}

func (h *Hub) registerModels(mux *http.ServeMux) {
	for resource, api := range modelResources {
		res, api := resource, api
		base := "/apis/" + api.group + "/" + api.version + "/namespaces/{namespace}/" + res
		mux.HandleFunc("GET "+base, h.nsList(res, api))
		mux.HandleFunc("POST "+base, h.nsCreate(res, api))
		mux.HandleFunc("DELETE "+base, h.nsUnsupported(res, api, "deletecollection"))
		mux.HandleFunc("GET "+base+"/{name}", h.nsGet(res, api))
		mux.HandleFunc("PUT "+base+"/{name}", h.nsUpdate(res, api))
		mux.HandleFunc("PATCH "+base+"/{name}", h.nsPatch(res, api))
		mux.HandleFunc("DELETE "+base+"/{name}", h.nsDelete(res, api))
	}
	// A namespaced resource is also listed across all namespaces at its
	// collection path without /namespaces/{namespace}.
	mux.HandleFunc("GET "+manifestWorksPath, h.nsList(manifestWorksResource, modelResources[manifestWorksResource]))
}

func (h *Hub) authorizeNS(w http.ResponseWriter, r *http.Request, api nsAPI, resource, verb, name string) bool {
	user, ok := h.authenticate(w, r)
	if !ok {
		return false
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !h.allowed(token) {
		gr := schema.GroupResource{Group: api.group, Resource: resource}
		scope := fmt.Sprintf("in the namespace %q", r.PathValue("namespace"))
		if r.PathValue("namespace") == "" {
			scope = "at the cluster scope"
		}
		writeStatus(w, apierrors.NewForbidden(gr, name,
			verbatim(fmt.Sprintf("User %q cannot %s resource %q in API group %q %s",
				user, verb, resource, api.group, scope))))
		return false
	}
	return true
}

func (h *Hub) nsUnsupported(resource string, api nsAPI, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.authorizeNS(w, r, api, resource, action, "") {
			writeStatus(w, apierrors.NewMethodNotSupported(
				schema.GroupResource{Group: api.group, Resource: resource}, action))
		}
	}
}

func (h *Hub) nsList(resource string, api nsAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace := r.PathValue("namespace")
		verb := "list"
		if isTrueParam(r.URL.Query().Get("watch")) {
			verb = "watch"
		}
		if !h.authorizeNS(w, r, api, resource, verb, "") {
			return
		}
		if verb == "watch" {
			writeStatus(w, apierrors.NewMethodNotSupported(
				schema.GroupResource{Group: api.group, Resource: resource}, "watch"))
			return
		}
		selector := labels.Everything()
		if s := r.URL.Query().Get("labelSelector"); s != "" {
			parsed, err := labels.Parse(s)
			if err != nil {
				writeStatus(w, apierrors.NewBadRequest(err.Error()))
				return
			}
			selector = parsed
		}
		h.mu.Lock()
		items := make([]map[string]any, 0)
		for key, obj := range h.maas[resource] {
			ns, _, ok := strings.Cut(key, "/")
			if !ok || (namespace != "" && ns != namespace) || !selector.Matches(labels.Set(obj.GetLabels())) {
				continue
			}
			items = append(items, obj.DeepCopy().Object)
		}
		rv := h.rv
		h.mu.Unlock()
		// The API server lists in storage key order: namespace, then name.
		sort.Slice(items, func(i, j int) bool {
			si, _, _ := unstructured.NestedString(items[i], "metadata", "namespace")
			sj, _, _ := unstructured.NestedString(items[j], "metadata", "namespace")
			if si != sj {
				return si < sj
			}
			ni, _, _ := unstructured.NestedString(items[i], "metadata", "name")
			nj, _, _ := unstructured.NestedString(items[j], "metadata", "name")
			return ni < nj
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"apiVersion": api.apiVersion,
			"kind":       api.kind + "List",
			"metadata":   metav1.ListMeta{ResourceVersion: strconv.FormatInt(rv, 10)},
			"items":      items,
		})
	}
}

func (h *Hub) nsGet(resource string, api nsAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace, name := r.PathValue("namespace"), r.PathValue("name")
		if !h.authorizeNS(w, r, api, resource, "get", name) {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		obj, ok := h.maas[resource][maasObjectKey(namespace, name)]
		if !ok {
			writeStatus(w, apierrors.NewNotFound(
				schema.GroupResource{Group: api.group, Resource: resource}, name))
			return
		}
		writeJSON(w, http.StatusOK, obj.DeepCopy().Object)
	}
}

func (h *Hub) nsCreate(resource string, api nsAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace := r.PathValue("namespace")
		if !h.authorizeNS(w, r, api, resource, "create", "") {
			return
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		obj, statusErr := decodeNSObject(body, api, namespace, "")
		if statusErr != nil {
			writeStatus(w, statusErr)
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		name := obj.GetName()
		key := maasObjectKey(namespace, name)
		if _, exists := h.maas[resource][key]; exists {
			writeStatus(w, apierrors.NewAlreadyExists(
				schema.GroupResource{Group: api.group, Resource: resource}, name))
			return
		}
		if obj.GetResourceVersion() != "" {
			writeStatus(w, apierrors.NewInternalError(errors.New(msgRVOnCreate)))
			return
		}
		unstructured.RemoveNestedField(obj.Object, "status")
		obj.SetUID(types.UID(newUID()))
		obj.SetCreationTimestamp(metav1.NewTime(h.now()))
		obj.SetGeneration(1)
		obj.SetNamespace(namespace)
		obj.SetAPIVersion(api.apiVersion)
		obj.SetKind(api.kind)
		meta := objectMetaFromUnstructured(obj)
		if errs := apivalidation.ValidateObjectMeta(&meta, true, apivalidation.NameIsDNSSubdomain, field.NewPath("metadata")); len(errs) > 0 {
			writeStatus(w, apierrors.NewInvalid(schema.GroupKind{Group: api.group, Kind: api.kind}, name, errs))
			return
		}
		stored := h.commitMAAS(resource, nil, obj)
		writeJSON(w, http.StatusCreated, stored.DeepCopy().Object)
	}
}

func (h *Hub) nsUpdate(resource string, api nsAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace, name := r.PathValue("namespace"), r.PathValue("name")
		if !h.authorizeNS(w, r, api, resource, "update", name) {
			return
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		next, statusErr := decodeNSObject(body, api, namespace, name)
		if statusErr != nil {
			writeStatus(w, statusErr)
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		cur, ok := h.maas[resource][maasObjectKey(namespace, name)]
		if !ok {
			writeStatus(w, apierrors.NewNotFound(
				schema.GroupResource{Group: api.group, Resource: resource}, name))
			return
		}
		h.writeNSUpdate(w, resource, api, cur, next, namespace, name)
	}
}

func (h *Hub) nsPatch(resource string, api nsAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace, name := r.PathValue("namespace"), r.PathValue("name")
		if !h.authorizeNS(w, r, api, resource, "patch", name) {
			return
		}
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "application/merge-patch+json" && mediaType != "application/json-patch+json" {
			writeStatus(w, &apierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure, Code: http.StatusUnsupportedMediaType,
				Reason: metav1.StatusReasonUnsupportedMediaType, Message: msgPatchMedia,
			}})
			return
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		cur, ok := h.maas[resource][maasObjectKey(namespace, name)]
		if !ok {
			writeStatus(w, apierrors.NewNotFound(
				schema.GroupResource{Group: api.group, Resource: resource}, name))
			return
		}
		original, err := json.Marshal(cur.Object)
		if err != nil {
			writeStatus(w, apierrors.NewInternalError(err))
			return
		}
		patched, statusErr := applyPatch(mediaType, original, body)
		if statusErr != nil {
			writeStatus(w, statusErr)
			return
		}
		next, statusErr := decodeNSObject(patched, api, namespace, name)
		if statusErr != nil {
			writeStatus(w, statusErr)
			return
		}
		h.writeNSUpdate(w, resource, api, cur, next, namespace, name)
	}
}

func (h *Hub) nsDelete(resource string, api nsAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace, name := r.PathValue("namespace"), r.PathValue("name")
		if !h.authorizeNS(w, r, api, resource, "delete", name) {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		key := maasObjectKey(namespace, name)
		cur, ok := h.maas[resource][key]
		if !ok {
			writeStatus(w, apierrors.NewNotFound(
				schema.GroupResource{Group: api.group, Resource: resource}, name))
			return
		}
		uid := cur.GetUID()
		delete(h.maas[resource], key)
		if resource == mwrsResource {
			h.deleteOwnedWorks(namespace, name)
		}
		writeJSON(w, http.StatusOK, &metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusSuccess,
			Details:  &metav1.StatusDetails{Name: name, Group: api.group, Kind: resource, UID: uid},
		})
	}
}

// deleteOwnedWorks is the ManifestWorkReplicaSet controller's finalizer: it
// deletes every work the MWRS generated before letting the MWRS go
// (ocm pkg/work/hub/controllers/manifestworkreplicasetcontroller/manifestworkreplicaset_finalize_reconcile.go,
// finalizeManifestWorkReplicaSet). The fake does it in the same write, so a
// model created again under the same id does not find the old works. Must
// be called under h.mu.
func (h *Hub) deleteOwnedWorks(namespace, name string) {
	owner := namespace + "." + name
	for key, obj := range h.maas[manifestWorksResource] {
		if obj.GetLabels()[LabelMWRSOwner] == owner {
			delete(h.maas[manifestWorksResource], key)
		}
	}
}

func (h *Hub) writeNSUpdate(w http.ResponseWriter, resource string, api nsAPI, cur, next *unstructured.Unstructured, namespace, name string) {
	h.writeNamespacedUpdate(w, resource, schema.GroupKind{Group: api.group, Kind: api.kind}, api.apiVersion, cur, next, namespace, name, false)
}

func decodeNSObject(body []byte, api nsAPI, namespace, name string) (*unstructured.Unstructured, *apierrors.StatusError) {
	return decodeTypedObject(body, api.apiVersion, api.kind, namespace, name)
}
