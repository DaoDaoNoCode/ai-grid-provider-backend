package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// writeNamespacedUpdate is the shared decode-lock-validate-commit path for
// namespaced hub resources (Models NS CRs and MaaS CRs). Must be called under h.mu.
func (h *Hub) writeNamespacedUpdate(
	w http.ResponseWriter,
	resource string,
	groupKind schema.GroupKind,
	apiVersion string,
	cur, next *unstructured.Unstructured,
	namespace, name string,
	requireNamespaceMatch bool,
) {
	if next.GetName() != name {
		writeStatus(w, apierrors.NewBadRequest(fmt.Sprintf("the name of the object (%s) does not match the name on the URL (%s)", next.GetName(), name)))
		return
	}
	if requireNamespaceMatch && next.GetNamespace() != "" && next.GetNamespace() != namespace {
		writeStatus(w, apierrors.NewBadRequest(fmt.Sprintf("the namespace of the object (%s) does not match the namespace on the URL (%s)", next.GetNamespace(), namespace)))
		return
	}
	if next.GetResourceVersion() == "" {
		writeStatus(w, apierrors.NewInvalid(groupKind, name, field.ErrorList{
			field.Invalid(field.NewPath("metadata", "resourceVersion"), uint64(0), "must be specified for an update"),
		}))
		return
	}
	if next.GetResourceVersion() != cur.GetResourceVersion() {
		writeStatus(w, apierrors.NewConflict(
			schema.GroupResource{Group: groupKind.Group, Resource: resource}, name, errors.New(msgObjectModified)))
		return
	}
	next.SetUID(cur.GetUID())
	next.SetCreationTimestamp(cur.GetCreationTimestamp())
	next.SetNamespace(namespace)
	next.SetAPIVersion(apiVersion)
	next.SetKind(groupKind.Kind)
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
		writeStatus(w, apierrors.NewInvalid(groupKind, name, errs))
		return
	}
	stored := h.commitMAAS(resource, cur, next)
	writeJSON(w, http.StatusOK, stored.DeepCopy().Object)
}

// decodeTypedObject unmarshals a typed unstructured object and fills name/namespace defaults.
func decodeTypedObject(body []byte, apiVersion, kind, namespace, name string) (*unstructured.Unstructured, *apierrors.StatusError) {
	var typeMeta metav1.TypeMeta
	if err := json.Unmarshal(body, &typeMeta); err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	switch {
	case typeMeta.Kind == "":
		return nil, apierrors.NewBadRequest(fmt.Sprintf("Object 'Kind' is missing in '%s'", body))
	case typeMeta.APIVersion != apiVersion:
		return nil, apierrors.NewBadRequest(fmt.Sprintf("the API version in the data (%s) does not match the expected API version (%s)", typeMeta.APIVersion, apiVersion))
	case typeMeta.Kind != kind:
		return nil, apierrors.NewBadRequest(fmt.Sprintf("the kind in the data (%s) does not match the expected kind (%s)", typeMeta.Kind, kind))
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
