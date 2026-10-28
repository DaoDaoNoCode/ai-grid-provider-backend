package servers

import (
	"regexp"
	"strconv"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	authPolicyNameSuffix = "-policy"
	// maxSubscriptionIDLen leaves room for authPolicyNameSuffix within DNS-1123 (63).
	maxSubscriptionIDLen = 63 - len(authPolicyNameSuffix)

	subscriptionLabelKey = "ai-grid/subscription"

	annotationDisplayName = "openshift.io/display-name"
	annotationDescription = "openshift.io/description"

	// maxTokenWindowSeconds is the longest rate-limit window MaaS accepts, 366
	// days (8784h): maxWindowSeconds and validateTokenRateLimit in
	// opendatahub-io/models-as-a-service at 4d71db2,
	// maas-controller/pkg/controller/maas/maassubscription_controller.go,
	// lines 119-122 and 134-170. The CRD checks only tokenWindowRE
	// (maas-controller/api/maas/v1alpha1/maassubscription_types.go, line 169),
	// so a longer window is admitted and Argo CD applies it; the controller
	// then logs an error and leaves the subscription out of the model's
	// TokenRateLimitPolicy (lines 736-741).
	maxTokenWindowSeconds = 366 * 24 * 3600
	maxTokenWindow        = "8784h"
)

var (
	subscriptionNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tokenWindowRE      = regexp.MustCompile(`^[1-9]\d{0,3}(s|m|h)$`)
)

// tokenWindowSeconds returns the length of a rate-limit window in seconds, or
// false when it doesn't match tokenWindowRE.
func tokenWindowSeconds(window string) (int64, bool) {
	m := tokenWindowRE.FindStringSubmatch(window)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(window[:len(window)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	switch m[1] {
	case "m":
		n *= 60
	case "h":
		n *= 3600
	}
	return n, true
}

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
		// renderSubscriptionDoc gives a ref without a namespace the
		// subscription's namespace before it gets here.
		ns := ref.GetNamespace()
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
		// renderSubscriptionDoc gives a ref without a namespace the
		// subscription's namespace before it gets here.
		ns := ref.GetNamespace()
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

func resourceRefStatusFromMap(m map[string]interface{}) *aigridv1.ResourceRefStatus {
	return &aigridv1.ResourceRefStatus{
		Name:      stringField(m, "name"),
		Namespace: stringField(m, "namespace"),
		Ready:     nestedBool(m, "ready"),
		Reason:    stringField(m, "reason"),
		Message:   stringField(m, "message"),
	}
}
