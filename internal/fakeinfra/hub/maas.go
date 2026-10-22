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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// MaaS CRDs the Subscriptions server talks to
// (maas.opendatahub.io/v1alpha1). Served as namespaced unstructured objects
// so make dev-fake can exercise Create/Update/Delete and AuthPolicy pairing
// without a real MaaS controller.
const (
	maasAPIGroup   = "maas.opendatahub.io"
	maasAPIVersion = "maas.opendatahub.io/v1alpha1"
	maasVersion    = "v1alpha1"
	maasBasePath   = "/apis/" + maasAPIGroup + "/" + maasVersion + "/namespaces"

	maasSubscriptionsResource = "maassubscriptions"
	maasAuthPoliciesResource  = "maasauthpolicies"
	maasModelRefsResource     = "maasmodelrefs"

	maasSubscriptionKind = "MaaSSubscription"
	maasAuthPolicyKind   = "MaaSAuthPolicy"
	maasModelRefKind     = "MaaSModelRef"
)

var maasResources = map[string]schema.GroupKind{
	maasSubscriptionsResource: {Group: maasAPIGroup, Kind: maasSubscriptionKind},
	maasAuthPoliciesResource:  {Group: maasAPIGroup, Kind: maasAuthPolicyKind},
	maasModelRefsResource:     {Group: maasAPIGroup, Kind: maasModelRefKind},
}

func maasObjectKey(namespace, name string) string {
	return namespace + "/" + name
}

func (h *Hub) registerMAAS(mux *http.ServeMux) {
	for resource := range maasResources {
		res := resource
		base := maasBasePath + "/{namespace}/" + res
		mux.HandleFunc("GET "+base, h.maasList(res))
		mux.HandleFunc("POST "+base, h.maasCreate(res))
		mux.HandleFunc("DELETE "+base, h.maasUnsupported(res, "deletecollection"))
		mux.HandleFunc("GET "+base+"/{name}", h.maasGet(res))
		mux.HandleFunc("PUT "+base+"/{name}", h.maasUpdate(res))
		mux.HandleFunc("PATCH "+base+"/{name}", h.maasPatch(res))
		mux.HandleFunc("DELETE "+base+"/{name}", h.maasDelete(res))
	}
}

func (h *Hub) authorizeMAAS(w http.ResponseWriter, r *http.Request, resource, verb, name string) bool {
	user, ok := h.authenticate(w, r)
	if !ok {
		return false
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !h.allowed(token) {
		gr := schema.GroupResource{Group: maasAPIGroup, Resource: resource}
		writeStatus(w, apierrors.NewForbidden(gr, name,
			verbatim(fmt.Sprintf("User %q cannot %s resource %q in API group %q in the namespace %q",
				user, verb, resource, maasAPIGroup, r.PathValue("namespace")))))
		return false
	}
	return true
}

func (h *Hub) maasUnsupported(resource, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.authorizeMAAS(w, r, resource, action, "") {
			writeStatus(w, apierrors.NewMethodNotSupported(
				schema.GroupResource{Group: maasAPIGroup, Resource: resource}, action))
		}
	}
}

func (h *Hub) maasList(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace := r.PathValue("namespace")
		verb := "list"
		if isTrueParam(r.URL.Query().Get("watch")) {
			verb = "watch"
		}
		if !h.authorizeMAAS(w, r, resource, verb, "") {
			return
		}
		if verb == "watch" {
			writeStatus(w, apierrors.NewMethodNotSupported(
				schema.GroupResource{Group: maasAPIGroup, Resource: resource}, "watch"))
			return
		}
		gk := maasResources[resource]
		h.mu.Lock()
		items := make([]map[string]any, 0)
		for key, obj := range h.maas[resource] {
			ns, _, ok := strings.Cut(key, "/")
			if !ok || ns != namespace {
				continue
			}
			items = append(items, obj.DeepCopy().Object)
		}
		rv := h.rv
		h.mu.Unlock()
		sort.Slice(items, func(i, j int) bool {
			ni, _, _ := unstructured.NestedString(items[i], "metadata", "name")
			nj, _, _ := unstructured.NestedString(items[j], "metadata", "name")
			return ni < nj
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"apiVersion": maasAPIVersion,
			"kind":       gk.Kind + "List",
			"metadata":   metav1.ListMeta{ResourceVersion: strconv.FormatInt(rv, 10)},
			"items":      items,
		})
	}
}

func (h *Hub) maasGet(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace, name := r.PathValue("namespace"), r.PathValue("name")
		if !h.authorizeMAAS(w, r, resource, "get", name) {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		obj, ok := h.maas[resource][maasObjectKey(namespace, name)]
		if !ok {
			writeStatus(w, apierrors.NewNotFound(
				schema.GroupResource{Group: maasAPIGroup, Resource: resource}, name))
			return
		}
		writeJSON(w, http.StatusOK, obj.DeepCopy().Object)
	}
}

func (h *Hub) maasCreate(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace := r.PathValue("namespace")
		if !h.authorizeMAAS(w, r, resource, "create", "") {
			return
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		obj, statusErr := decodeMAASObject(body, resource, namespace, "")
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
				schema.GroupResource{Group: maasAPIGroup, Resource: resource}, name))
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
		obj.SetAPIVersion(maasAPIVersion)
		obj.SetKind(maasResources[resource].Kind)
		meta := objectMetaFromUnstructured(obj)
		if errs := apivalidation.ValidateObjectMeta(&meta, true, apivalidation.NameIsDNSSubdomain, field.NewPath("metadata")); len(errs) > 0 {
			writeStatus(w, apierrors.NewInvalid(maasResources[resource], name, errs))
			return
		}
		stored := h.commitMAAS(resource, nil, obj)
		h.logger.Debug("maas object created", "resource", resource, "namespace", namespace, "name", name, "resourceVersion", stored.GetResourceVersion())
		writeJSON(w, http.StatusCreated, stored.DeepCopy().Object)
	}
}

func (h *Hub) maasUpdate(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace, name := r.PathValue("namespace"), r.PathValue("name")
		if !h.authorizeMAAS(w, r, resource, "update", name) {
			return
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		next, statusErr := decodeMAASObject(body, resource, namespace, name)
		if statusErr != nil {
			writeStatus(w, statusErr)
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		cur, ok := h.maas[resource][maasObjectKey(namespace, name)]
		if !ok {
			writeStatus(w, apierrors.NewNotFound(
				schema.GroupResource{Group: maasAPIGroup, Resource: resource}, name))
			return
		}
		h.writeMAASUpdate(w, resource, cur, next, namespace, name)
	}
}

func (h *Hub) maasPatch(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace, name := r.PathValue("namespace"), r.PathValue("name")
		if !h.authorizeMAAS(w, r, resource, "patch", name) {
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
				schema.GroupResource{Group: maasAPIGroup, Resource: resource}, name))
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
		next, statusErr := decodeMAASObject(patched, resource, namespace, name)
		if statusErr != nil {
			writeStatus(w, statusErr)
			return
		}
		h.writeMAASUpdate(w, resource, cur, next, namespace, name)
	}
}

func (h *Hub) maasDelete(resource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace, name := r.PathValue("namespace"), r.PathValue("name")
		if !h.authorizeMAAS(w, r, resource, "delete", name) {
			return
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		var opts deleteOptions
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &opts); err != nil {
				writeStatus(w, apierrors.NewBadRequest(err.Error()))
				return
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		key := maasObjectKey(namespace, name)
		cur, ok := h.maas[resource][key]
		if !ok {
			writeStatus(w, apierrors.NewNotFound(
				schema.GroupResource{Group: maasAPIGroup, Resource: resource}, name))
			return
		}
		if p := opts.Preconditions; p != nil {
			if p.UID != nil && *p.UID != cur.GetUID() {
				writeStatus(w, apierrors.NewConflict(
					schema.GroupResource{Group: maasAPIGroup, Resource: resource}, name,
					verbatim(fmt.Sprintf("Precondition failed: UID in precondition: %v, UID in object meta: %v", *p.UID, cur.GetUID()))))
				return
			}
			if p.ResourceVersion != nil && *p.ResourceVersion != cur.GetResourceVersion() {
				writeStatus(w, apierrors.NewConflict(
					schema.GroupResource{Group: maasAPIGroup, Resource: resource}, name,
					verbatim(fmt.Sprintf("Precondition failed: ResourceVersion in precondition: %v, ResourceVersion in object meta: %v", *p.ResourceVersion, cur.GetResourceVersion()))))
				return
			}
		}
		uid := cur.GetUID()
		delete(h.maas[resource], key)
		h.logger.Debug("maas object deleted", "resource", resource, "namespace", namespace, "name", name)
		writeJSON(w, http.StatusOK, &metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusSuccess,
			Details:  &metav1.StatusDetails{Name: name, Group: maasAPIGroup, Kind: resource, UID: uid},
		})
	}
}

// writeMAASUpdate is the update path for a namespaced MaaS CR. Must be called under h.mu.
func (h *Hub) writeMAASUpdate(w http.ResponseWriter, resource string, cur, next *unstructured.Unstructured, namespace, name string) {
	if next.GetName() != name {
		writeStatus(w, apierrors.NewBadRequest(fmt.Sprintf("the name of the object (%s) does not match the name on the URL (%s)", next.GetName(), name)))
		return
	}
	if next.GetNamespace() != "" && next.GetNamespace() != namespace {
		writeStatus(w, apierrors.NewBadRequest(fmt.Sprintf("the namespace of the object (%s) does not match the namespace on the URL (%s)", next.GetNamespace(), namespace)))
		return
	}
	if next.GetResourceVersion() == "" {
		writeStatus(w, apierrors.NewInvalid(maasResources[resource], name, field.ErrorList{
			field.Invalid(field.NewPath("metadata", "resourceVersion"), uint64(0), "must be specified for an update"),
		}))
		return
	}
	if next.GetResourceVersion() != cur.GetResourceVersion() {
		writeStatus(w, apierrors.NewConflict(
			schema.GroupResource{Group: maasAPIGroup, Resource: resource}, name, errors.New(msgObjectModified)))
		return
	}
	next.SetUID(cur.GetUID())
	next.SetCreationTimestamp(cur.GetCreationTimestamp())
	next.SetNamespace(namespace)
	next.SetAPIVersion(maasAPIVersion)
	next.SetKind(maasResources[resource].Kind)
	if status, found, _ := unstructured.NestedFieldCopy(cur.Object, "status"); found {
		_ = unstructured.SetNestedField(next.Object, status, "status")
	} else {
		unstructured.RemoveNestedField(next.Object, "status")
	}
	nextMeta := objectMetaFromUnstructured(next)
	curMeta := objectMetaFromUnstructured(cur)
	errs := apivalidation.ValidateObjectMetaUpdate(&nextMeta, &curMeta, field.NewPath("metadata"))
	errs = append(errs, apivalidation.ValidateObjectMeta(&nextMeta, true, apivalidation.NameIsDNSSubdomain, field.NewPath("metadata"))...)
	if len(errs) > 0 {
		writeStatus(w, apierrors.NewInvalid(maasResources[resource], name, errs))
		return
	}
	stored := h.commitMAAS(resource, cur, next)
	h.logger.Debug("maas object updated", "resource", resource, "namespace", namespace, "name", name, "resourceVersion", stored.GetResourceVersion())
	writeJSON(w, http.StatusOK, stored.DeepCopy().Object)
}

// commitMAAS stores next. Must be called under h.mu.
func (h *Hub) commitMAAS(resource string, cur, next *unstructured.Unstructured) *unstructured.Unstructured {
	if cur != nil {
		next.SetResourceVersion(cur.GetResourceVersion())
		curSpec, _, _ := unstructured.NestedFieldNoCopy(cur.Object, "spec")
		nextSpec, _, _ := unstructured.NestedFieldNoCopy(next.Object, "spec")
		if !jsonEqual(curSpec, nextSpec) {
			next.SetGeneration(cur.GetGeneration() + 1)
		} else {
			next.SetGeneration(cur.GetGeneration())
		}
		if jsonEqual(cur.Object, next.Object) {
			return cur
		}
	} else if next.GetGeneration() < 1 {
		next.SetGeneration(1)
	}
	next.SetResourceVersion(h.nextResourceVersion())
	stored := next.DeepCopy()
	if h.maas[resource] == nil {
		h.maas[resource] = map[string]*unstructured.Unstructured{}
	}
	h.maas[resource][maasObjectKey(stored.GetNamespace(), stored.GetName())] = stored
	return stored
}

func decodeMAASObject(body []byte, resource, namespace, name string) (*unstructured.Unstructured, *apierrors.StatusError) {
	gk := maasResources[resource]
	var typeMeta metav1.TypeMeta
	if err := json.Unmarshal(body, &typeMeta); err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	switch {
	case typeMeta.Kind == "":
		return nil, apierrors.NewBadRequest(fmt.Sprintf("Object 'Kind' is missing in '%s'", body))
	case typeMeta.APIVersion != maasAPIVersion:
		return nil, apierrors.NewBadRequest(fmt.Sprintf("the API version in the data (%s) does not match the expected API version (%s)", typeMeta.APIVersion, maasAPIVersion))
	case typeMeta.Kind != gk.Kind:
		return nil, apierrors.NewBadRequest(fmt.Sprintf("the kind in the data (%s) does not match the expected kind (%s)", typeMeta.Kind, gk.Kind))
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(body); err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	if name != "" && obj.GetName() == "" {
		obj.SetName(name)
	}
	if obj.GetNamespace() == "" {
		obj.SetNamespace(namespace)
	}
	return obj, nil
}

func seedMAASFixtures(h *Hub, fx *Fixtures) error {
	h.maas = map[string]map[string]*unstructured.Unstructured{
		maasSubscriptionsResource: {},
		maasAuthPoliciesResource:  {},
		maasModelRefsResource:     {},
	}
	type seed struct {
		resource string
		items    []unstructured.Unstructured
	}
	for _, s := range []seed{
		{maasSubscriptionsResource, fx.MaaSSubscriptions},
		{maasAuthPoliciesResource, fx.MaaSAuthPolicies},
		{maasModelRefsResource, fx.MaaSModelRefs},
	} {
		for i := range s.items {
			obj := s.items[i].DeepCopy()
			obj.SetAPIVersion(maasAPIVersion)
			obj.SetKind(maasResources[s.resource].Kind)
			if obj.GetNamespace() == "" {
				return fmt.Errorf("fixture %s %q: namespace is required", s.resource, obj.GetName())
			}
			meta := objectMetaFromUnstructured(obj)
			if errs := apivalidation.ValidateObjectMeta(&meta, true, apivalidation.NameIsDNSSubdomain, field.NewPath("metadata")); len(errs) > 0 {
				return fmt.Errorf("fixture %s %q: %w", s.resource, obj.GetName(), errs.ToAggregate())
			}
			key := maasObjectKey(obj.GetNamespace(), obj.GetName())
			if _, dup := h.maas[s.resource][key]; dup {
				return fmt.Errorf("fixture %s %q listed twice", s.resource, key)
			}
			rv, err := strconv.ParseInt(obj.GetResourceVersion(), 10, 64)
			if err != nil || rv < 1 {
				return fmt.Errorf("fixture %s %q: resourceVersion %q is not a positive integer", s.resource, key, obj.GetResourceVersion())
			}
			h.rv = max(h.rv, rv)
			created := obj.GetCreationTimestamp()
			if obj.GetUID() == "" || created.Time.IsZero() || obj.GetGeneration() < 1 {
				return fmt.Errorf("fixture %s %q: uid, creationTimestamp and generation are required", s.resource, key)
			}
			h.maas[s.resource][key] = obj
		}
	}
	return nil
}

func objectMetaFromUnstructured(obj *unstructured.Unstructured) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:                       obj.GetName(),
		GenerateName:               obj.GetGenerateName(),
		Namespace:                  obj.GetNamespace(),
		UID:                        obj.GetUID(),
		ResourceVersion:            obj.GetResourceVersion(),
		Generation:                 obj.GetGeneration(),
		CreationTimestamp:          obj.GetCreationTimestamp(),
		DeletionTimestamp:          obj.GetDeletionTimestamp(),
		DeletionGracePeriodSeconds: obj.GetDeletionGracePeriodSeconds(),
		Labels:                     obj.GetLabels(),
		Annotations:                obj.GetAnnotations(),
		OwnerReferences:            obj.GetOwnerReferences(),
		Finalizers:                 obj.GetFinalizers(),
		ManagedFields:              obj.GetManagedFields(),
	}
}
