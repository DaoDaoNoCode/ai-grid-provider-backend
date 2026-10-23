package servers

import (
	"regexp"
	"strings"
	"time"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	defaultSubscriptionNamespace = "models-as-a-service"
	defaultModelNamespace        = "llm"
	authPolicyNameSuffix         = "-policy"
	// maxSubscriptionIDLen leaves room for authPolicyNameSuffix within DNS-1123 (63).
	maxSubscriptionIDLen = 63 - len(authPolicyNameSuffix)

	managedByLabelKey    = "app.kubernetes.io/managed-by"
	managedByLabelValue  = "ai-grid-provider-backend"
	subscriptionLabelKey = "ai-grid/subscription"

	annotationDisplayName = "openshift.io/display-name"
	annotationDescription = "openshift.io/description"
)

var (
	maasSubscriptionsResource = schema.GroupVersionResource{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maassubscriptions",
	}
	maasAuthPoliciesResource = schema.GroupVersionResource{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maasauthpolicies",
	}
	maasModelRefsResource = schema.GroupVersionResource{
		Group: "maas.opendatahub.io", Version: "v1alpha1", Resource: "maasmodelrefs",
	}

	subscriptionNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tokenWindowRE      = regexp.MustCompile(`^[1-9]\d{0,3}(s|m|h)$`)
)

func subscriptionID(object *aigridv1.Subscription) string {
	if object == nil {
		return ""
	}
	if id := object.GetId(); id != "" {
		return id
	}
	if object.GetMetadata() != nil {
		return object.GetMetadata().GetName()
	}
	return ""
}

func authPolicyNameForSubscription(id string) string {
	return id + authPolicyNameSuffix
}

func modelRefKey(name, namespace string) string {
	if namespace == "" {
		namespace = defaultModelNamespace
	}
	return namespace + "/" + name
}

func subscriptionFromUnstructured(item unstructured.Unstructured) *aigridv1.Subscription {
	name := item.GetName()
	meta := &aigridv1.Metadata{
		Name:        name,
		Labels:      item.GetLabels(),
		Annotations: item.GetAnnotations(),
		Version:     resourceVersionToVersion(item.GetResourceVersion()),
	}
	if t := item.GetCreationTimestamp(); !t.IsZero() {
		meta.CreationTimestamp = timestamppb.New(t.Time)
	}
	if ts := item.GetDeletionTimestamp(); ts != nil && !ts.IsZero() {
		meta.DeletionTimestamp = timestamppb.New(ts.Time)
	}

	spec := &aigridv1.SubscriptionSpec{
		Priority: int32(nestedInt64(item.Object, "spec", "priority")),
		Owner:    ownerFromUnstructured(item.Object),
	}
	if refs, found, err := unstructured.NestedSlice(item.Object, "spec", "modelRefs"); err == nil && found {
		for _, raw := range refs {
			ref, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			spec.ModelRefs = append(spec.ModelRefs, modelRefFromMap(ref))
		}
	}
	if orgID, found, err := unstructured.NestedString(item.Object, "spec", "organization", "id"); err == nil && found && orgID != "" {
		spec.Organization = &aigridv1.SubscriptionOrganization{Id: orgID}
	}
	if tm := tokenMetadataFromUnstructured(item.Object); tm != nil {
		spec.TokenMetadata = tm
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(item.Object, "spec", "inferencePriority"); found {
		v := int32(nestedInt64(item.Object, "spec", "inferencePriority"))
		spec.InferencePriority = &v
	}
	if g, found, err := unstructured.NestedSlice(item.Object, "spec", "guardrails"); err == nil && found {
		spec.Guardrails = guardrailsFromSlice(g)
	}

	status := &aigridv1.SubscriptionStatus{}
	if phase, found, err := unstructured.NestedString(item.Object, "status", "phase"); err == nil && found {
		status.Phase = phase
	}
	if conds, found, err := unstructured.NestedSlice(item.Object, "status", "conditions"); err == nil && found {
		status.Conditions = conditionsFromSlice(conds)
	}
	if refs, found, err := unstructured.NestedSlice(item.Object, "status", "modelRefStatuses"); err == nil && found {
		for _, raw := range refs {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			status.ModelRefStatuses = append(status.ModelRefStatuses, resourceRefStatusFromMap(m))
		}
	}
	if refs, found, err := unstructured.NestedSlice(item.Object, "status", "tokenRateLimitStatuses"); err == nil && found {
		for _, raw := range refs {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			status.TokenRateLimitStatuses = append(status.TokenRateLimitStatuses, &aigridv1.TokenRateLimitStatus{
				Ref:   resourceRefStatusFromMap(m),
				Model: stringField(m, "model"),
			})
		}
	}

	return &aigridv1.Subscription{
		Id:       name,
		Metadata: meta,
		Spec:     spec,
		Status:   status,
	}
}

func ownerFromUnstructured(obj map[string]interface{}) *aigridv1.SubscriptionOwner {
	owner := &aigridv1.SubscriptionOwner{}
	groups, _, _ := unstructured.NestedSlice(obj, "spec", "owner", "groups")
	for _, raw := range groups {
		switch g := raw.(type) {
		case map[string]interface{}:
			if name, _ := g["name"].(string); name != "" {
				owner.Groups = append(owner.Groups, name)
			}
		case string:
			if g != "" {
				owner.Groups = append(owner.Groups, g)
			}
		}
	}
	users, _, _ := unstructured.NestedStringSlice(obj, "spec", "owner", "users")
	owner.Users = append(owner.Users, users...)
	return owner
}

func modelRefFromMap(ref map[string]interface{}) *aigridv1.SubscriptionModelRef {
	out := &aigridv1.SubscriptionModelRef{
		Name:      stringField(ref, "name"),
		Namespace: stringField(ref, "namespace"),
		Unlimited: nestedBool(ref, "unlimited"),
	}
	limits, _, _ := unstructured.NestedSlice(ref, "tokenRateLimits")
	for _, raw := range limits {
		lim, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		out.TokenRateLimits = append(out.TokenRateLimits, &aigridv1.TokenRateLimit{
			Limit:  nestedInt64(lim, "limit"),
			Window: stringField(lim, "window"),
		})
	}
	if perToken, found, err := unstructured.NestedString(ref, "billingRate", "perToken"); err == nil && found && perToken != "" {
		out.BillingRate = &aigridv1.BillingRate{PerToken: perToken}
	}
	if g, found, err := unstructured.NestedSlice(ref, "guardrails"); err == nil && found {
		out.Guardrails = guardrailsFromSlice(g)
	}
	return out
}

func subscriptionToUnstructured(object *aigridv1.Subscription, namespace string) *unstructured.Unstructured {
	id := subscriptionID(object)
	meta := map[string]interface{}{
		"name":      id,
		"namespace": namespace,
	}
	if object.GetMetadata() != nil {
		if labels := object.GetMetadata().GetLabels(); len(labels) > 0 {
			meta["labels"] = stringMapToInterfaceMap(labels)
		}
		if annotations := object.GetMetadata().GetAnnotations(); len(annotations) > 0 {
			meta["annotations"] = stringMapToInterfaceMap(annotations)
		}
	}

	spec := subscriptionSpecMap(object.GetSpec())
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSSubscription",
		"metadata":   meta,
		"spec":       spec,
	}}
}

func subscriptionSpecMap(spec *aigridv1.SubscriptionSpec) map[string]interface{} {
	out := map[string]interface{}{
		"owner":     ownerToUnstructured(nil),
		"modelRefs": []interface{}{},
		"priority":  int64(0),
	}
	if spec == nil {
		return out
	}
	out["owner"] = ownerToUnstructured(spec.GetOwner())
	out["modelRefs"] = modelRefsToUnstructured(spec.GetModelRefs())
	out["priority"] = int64(spec.GetPriority())
	if org := spec.GetOrganization(); org != nil && org.GetId() != "" {
		out["organization"] = map[string]interface{}{"id": org.GetId()}
	}
	if tm := tokenMetadataToUnstructured(spec.GetTokenMetadata()); tm != nil {
		out["tokenMetadata"] = tm
	}
	if spec.InferencePriority != nil {
		out["inferencePriority"] = int64(spec.GetInferencePriority())
	}
	if g := guardrailsToUnstructured(spec.GetGuardrails()); len(g) > 0 {
		out["guardrails"] = g
	}
	return out
}

func ownerToUnstructured(owner *aigridv1.SubscriptionOwner) map[string]interface{} {
	out := map[string]interface{}{
		"groups": []interface{}{},
		"users":  []interface{}{},
	}
	if owner == nil {
		return out
	}
	groups := make([]interface{}, 0, len(owner.GetGroups()))
	for _, name := range owner.GetGroups() {
		groups = append(groups, map[string]interface{}{"name": name})
	}
	users := make([]interface{}, 0, len(owner.GetUsers()))
	for _, u := range owner.GetUsers() {
		users = append(users, u)
	}
	out["groups"] = groups
	out["users"] = users
	return out
}

func modelRefsToUnstructured(refs []*aigridv1.SubscriptionModelRef) []interface{} {
	out := make([]interface{}, 0, len(refs))
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		ns := ref.GetNamespace()
		if ns == "" {
			ns = defaultModelNamespace
		}
		entry := map[string]interface{}{
			"name":      ref.GetName(),
			"namespace": ns,
		}
		if ref.GetUnlimited() {
			entry["unlimited"] = true
		} else if limits := ref.GetTokenRateLimits(); len(limits) > 0 {
			lims := make([]interface{}, 0, len(limits))
			for _, lim := range limits {
				if lim == nil {
					continue
				}
				lims = append(lims, map[string]interface{}{
					"limit":  lim.GetLimit(),
					"window": lim.GetWindow(),
				})
			}
			entry["tokenRateLimits"] = lims
		}
		if br := ref.GetBillingRate(); br != nil && br.GetPerToken() != "" {
			entry["billingRate"] = map[string]interface{}{"perToken": br.GetPerToken()}
		}
		if g := guardrailsToUnstructured(ref.GetGuardrails()); len(g) > 0 {
			entry["guardrails"] = g
		}
		out = append(out, entry)
	}
	return out
}

// authPolicyFromSubscription builds a managed MaaSAuthPolicy for the subscription.
func authPolicyFromSubscription(object *aigridv1.Subscription, namespace string) *unstructured.Unstructured {
	id := subscriptionID(object)
	policyName := authPolicyNameForSubscription(id)

	labels := map[string]interface{}{
		managedByLabelKey:    managedByLabelValue,
		subscriptionLabelKey: id,
	}
	meta := map[string]interface{}{
		"name":      policyName,
		"namespace": namespace,
		"labels":    labels,
	}
	if object.GetMetadata() != nil {
		if anns := object.GetMetadata().GetAnnotations(); len(anns) > 0 {
			copied := map[string]interface{}{}
			for _, key := range []string{annotationDisplayName, annotationDescription} {
				if v, ok := anns[key]; ok {
					copied[key] = v
				}
			}
			if len(copied) > 0 {
				meta["annotations"] = copied
			}
		}
	}

	modelRefs := make([]interface{}, 0, len(object.GetSpec().GetModelRefs()))
	for _, ref := range object.GetSpec().GetModelRefs() {
		if ref == nil {
			continue
		}
		ns := ref.GetNamespace()
		if ns == "" {
			ns = defaultModelNamespace
		}
		modelRefs = append(modelRefs, map[string]interface{}{
			"name":      ref.GetName(),
			"namespace": ns,
		})
	}

	subjects := map[string]interface{}{
		"groups": []interface{}{},
		"users":  []interface{}{},
	}
	if owner := object.GetSpec().GetOwner(); owner != nil {
		groups := make([]interface{}, 0, len(owner.GetGroups()))
		for _, name := range owner.GetGroups() {
			groups = append(groups, map[string]interface{}{"name": name})
		}
		users := make([]interface{}, 0, len(owner.GetUsers()))
		for _, u := range owner.GetUsers() {
			users = append(users, u)
		}
		subjects["groups"] = groups
		subjects["users"] = users
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSAuthPolicy",
		"metadata":   meta,
		"spec": map[string]interface{}{
			"modelRefs": modelRefs,
			"subjects":  subjects,
		},
	}}
}

func authPolicyFromUnstructured(item unstructured.Unstructured) *aigridv1.AuthPolicy {
	name := item.GetName()
	meta := &aigridv1.Metadata{
		Name:        name,
		Labels:      item.GetLabels(),
		Annotations: item.GetAnnotations(),
		Version:     resourceVersionToVersion(item.GetResourceVersion()),
	}
	if t := item.GetCreationTimestamp(); !t.IsZero() {
		meta.CreationTimestamp = timestamppb.New(t.Time)
	}

	pol := &aigridv1.AuthPolicy{
		Id:       name,
		Metadata: meta,
	}
	groups, _, _ := unstructured.NestedSlice(item.Object, "spec", "subjects", "groups")
	for _, raw := range groups {
		if g, ok := raw.(map[string]interface{}); ok {
			if n, _ := g["name"].(string); n != "" {
				pol.SubjectGroups = append(pol.SubjectGroups, n)
			}
		}
	}
	users, _, _ := unstructured.NestedStringSlice(item.Object, "spec", "subjects", "users")
	pol.SubjectUsers = append(pol.SubjectUsers, users...)
	refs, _, _ := unstructured.NestedSlice(item.Object, "spec", "modelRefs")
	for _, raw := range refs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		pol.ModelRefs = append(pol.ModelRefs, &aigridv1.AuthPolicyModelRef{
			Name:      stringField(m, "name"),
			Namespace: stringField(m, "namespace"),
		})
	}
	if phase, found, err := unstructured.NestedString(item.Object, "status", "phase"); err == nil && found {
		pol.Phase = phase
	}
	if conds, found, err := unstructured.NestedSlice(item.Object, "status", "conditions"); err == nil && found {
		pol.Conditions = conditionsFromSlice(conds)
	}
	return pol
}

func modelRegistrationFromUnstructured(item unstructured.Unstructured) *aigridv1.ModelRegistration {
	reg := &aigridv1.ModelRegistration{
		Name:      item.GetName(),
		Namespace: item.GetNamespace(),
	}
	if phase, found, err := unstructured.NestedString(item.Object, "status", "phase"); err == nil && found {
		reg.Phase = phase
	}
	if endpoint, found, err := unstructured.NestedString(item.Object, "status", "endpoint"); err == nil && found {
		reg.Endpoint = endpoint
	}
	if conds, found, err := unstructured.NestedSlice(item.Object, "status", "conditions"); err == nil && found {
		reg.Conditions = conditionsFromSlice(conds)
	}
	return reg
}

func enrichModelRefsFromAnnotations(sub *aigridv1.Subscription, modelRefs map[string]*unstructured.Unstructured) {
	if sub == nil || sub.GetSpec() == nil {
		return
	}
	for _, ref := range sub.GetSpec().GetModelRefs() {
		if ref == nil {
			continue
		}
		obj := modelRefs[modelRefKey(ref.GetName(), ref.GetNamespace())]
		if obj == nil {
			continue
		}
		anns := obj.GetAnnotations()
		ref.DisplayName = anns[annotationDisplayName]
		ref.Description = anns[annotationDescription]
	}
}

func authPolicyCoversModels(pol *unstructured.Unstructured, modelKeys map[string]struct{}) bool {
	refs, _, _ := unstructured.NestedSlice(pol.Object, "spec", "modelRefs")
	for _, raw := range refs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		key := modelRefKey(stringField(m, "name"), stringField(m, "namespace"))
		if _, hit := modelKeys[key]; hit {
			return true
		}
	}
	return false
}

func authPolicySpecPatch(object *aigridv1.Subscription) map[string]interface{} {
	policy := authPolicyFromSubscription(object, "")
	spec, _, _ := unstructured.NestedMap(policy.Object, "spec")
	return map[string]interface{}{"spec": spec}
}

func isManagedAuthPolicy(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	labels := obj.GetLabels()
	return labels[managedByLabelKey] == managedByLabelValue
}

func subscriptionSpecPatch(object *aigridv1.Subscription) map[string]interface{} {
	return map[string]interface{}{"spec": subscriptionSpecMap(object.GetSpec())}
}

func tokenMetadataFromUnstructured(obj map[string]interface{}) *aigridv1.TokenMetadata {
	tm, found, err := unstructured.NestedMap(obj, "spec", "tokenMetadata")
	if err != nil || !found || len(tm) == 0 {
		return nil
	}
	out := &aigridv1.TokenMetadata{
		OrganizationId: stringField(tm, "organizationId"),
		CostCenter:     stringField(tm, "costCenter"),
	}
	if labels, _, _ := unstructured.NestedStringMap(tm, "labels"); len(labels) > 0 {
		out.Labels = labels
	}
	if out.OrganizationId == "" && out.CostCenter == "" && len(out.Labels) == 0 {
		return nil
	}
	return out
}

func tokenMetadataToUnstructured(tm *aigridv1.TokenMetadata) map[string]interface{} {
	if tm == nil {
		return nil
	}
	out := map[string]interface{}{}
	if tm.GetOrganizationId() != "" {
		out["organizationId"] = tm.GetOrganizationId()
	}
	if tm.GetCostCenter() != "" {
		out["costCenter"] = tm.GetCostCenter()
	}
	if len(tm.GetLabels()) > 0 {
		out["labels"] = stringMapToInterfaceMap(tm.GetLabels())
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func guardrailsFromSlice(raw []interface{}) []*aigridv1.GuardrailAttachment {
	out := make([]*aigridv1.GuardrailAttachment, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		g := &aigridv1.GuardrailAttachment{}
		if name, found, err := unstructured.NestedString(m, "ref", "name"); err == nil && found {
			g.RefName = name
		}
		checks, _, _ := unstructured.NestedStringSlice(m, "checks")
		g.Checks = append(g.Checks, checks...)
		if g.RefName != "" {
			out = append(out, g)
		}
	}
	return out
}

func guardrailsToUnstructured(items []*aigridv1.GuardrailAttachment) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, g := range items {
		if g == nil || g.GetRefName() == "" {
			continue
		}
		entry := map[string]interface{}{
			"ref": map[string]interface{}{"name": g.GetRefName()},
		}
		if len(g.GetChecks()) > 0 {
			checks := make([]interface{}, 0, len(g.GetChecks()))
			for _, c := range g.GetChecks() {
				checks = append(checks, c)
			}
			entry["checks"] = checks
		}
		out = append(out, entry)
	}
	return out
}

func conditionsFromSlice(raw []interface{}) []*aigridv1.Condition {
	out := make([]*aigridv1.Condition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		c := &aigridv1.Condition{
			Type:               stringField(m, "type"),
			Status:             conditionStatusFromString(stringField(m, "status")),
			ObservedGeneration: nestedInt64(m, "observedGeneration"),
		}
		if reason := stringField(m, "reason"); reason != "" {
			c.Reason = &reason
		}
		if message := stringField(m, "message"); message != "" {
			c.Message = &message
		}
		if ts := stringField(m, "lastTransitionTime"); ts != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				c.LastTransitionTime = timestamppb.New(t)
			} else if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				c.LastTransitionTime = timestamppb.New(t)
			}
		}
		out = append(out, c)
	}
	return out
}

func conditionStatusFromString(s string) aigridv1.ConditionStatus {
	switch strings.ToLower(s) {
	case "true":
		return aigridv1.ConditionStatus_CONDITION_STATUS_TRUE
	case "false":
		return aigridv1.ConditionStatus_CONDITION_STATUS_FALSE
	default:
		return aigridv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
	}
}

func resourceRefStatusFromMap(m map[string]interface{}) *aigridv1.ResourceRefStatus {
	return &aigridv1.ResourceRefStatus{
		Name:      stringField(m, "name"),
		Namespace: stringField(m, "namespace"),
		Ready:     nestedBool(m, "ready"),
		Reason:    stringField(m, "reason"),
		Message:   stringField(m, "message"),
	}
}

func nestedInt64(obj map[string]interface{}, fields ...string) int64 {
	v, found, err := unstructured.NestedInt64(obj, fields...)
	if err != nil || !found {
		f, found, err := unstructured.NestedFloat64(obj, fields...)
		if err != nil || !found {
			return 0
		}
		return int64(f)
	}
	return v
}

func nestedBool(obj map[string]interface{}, fields ...string) bool {
	v, found, err := unstructured.NestedBool(obj, fields...)
	if err != nil || !found {
		return false
	}
	return v
}

func stringField(obj map[string]interface{}, key string) string {
	v, _, _ := unstructured.NestedString(obj, key)
	return v
}
