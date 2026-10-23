package servers

import (
	"regexp"
	"strconv"
	"strings"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	defaultModelsNamespace = "ai-grid-internal-models"

	modelLabelKey = "ai-grid/model"

	defaultGatewayName      = "openshift-ai-inference"
	defaultGatewayNamespace = "openshift-ingress"

	annotationCreatedAt = "ai-grid/created-at"

	llmInferenceAPIVersion = "serving.kserve.io/v1alpha2"
	llmInferenceKind       = "LLMInferenceService"

	placementAPIVersion = "cluster.open-cluster-management.io/v1beta1"
	placementKind       = "Placement"

	mwrsAPIVersion = "work.open-cluster-management.io/v1alpha1"
	mwrsKind       = "ManifestWorkReplicaSet"

	maasModelRefAPIVersion = "maas.opendatahub.io/v1alpha1"
	maasModelRefKind       = "MaaSModelRef"
)

var (
	llmInferenceServicesResource = schema.GroupVersionResource{
		Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices",
	}
	placementsResource = schema.GroupVersionResource{
		Group: "cluster.open-cluster-management.io", Version: "v1beta1", Resource: "placements",
	}
	placementDecisionsResource = schema.GroupVersionResource{
		Group: "cluster.open-cluster-management.io", Version: "v1beta1", Resource: "placementdecisions",
	}
	manifestWorkReplicaSetsResource = schema.GroupVersionResource{
		Group: "work.open-cluster-management.io", Version: "v1alpha1", Resource: "manifestworkreplicasets",
	}

	modelNameRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	modelURISchemes = []string{"hf://", "oci://", "s3://", "pvc://", "http://", "https://"}
)

func modelID(object *aigridv1.Model) string {
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

func placementNameForModel(id string) string {
	return "model-" + id
}

func modelFromLLMInferenceService(item unstructured.Unstructured) *aigridv1.Model {
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

	spec := &aigridv1.ModelSpec{
		Model: modelSourceFromUnstructured(item.Object),
		Main:  modelContainerFromUnstructured(item.Object),
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(item.Object, "spec", "replicas"); found {
		v := int32(nestedInt64(item.Object, "spec", "replicas"))
		spec.Replicas = &v
	}
	if refs, found, err := unstructured.NestedSlice(item.Object, "spec", "baseRefs"); err == nil && found {
		for _, raw := range refs {
			switch v := raw.(type) {
			case string:
				if v != "" {
					spec.BaseRefs = append(spec.BaseRefs, v)
				}
			case map[string]interface{}:
				if n := stringField(v, "name"); n != "" {
					spec.BaseRefs = append(spec.BaseRefs, n)
				}
			}
		}
	}
	if runtime, found, err := unstructured.NestedString(item.Object, "spec", "runtime"); err == nil && found {
		spec.Runtime = runtime
	}
	spec.Router = modelRouterFromUnstructured(item.Object)
	spec.Scaling = modelScalingFromUnstructured(item.Object)

	status := modelStatusFromLLMInferenceService(item.Object)
	return &aigridv1.Model{
		Id:       name,
		Metadata: meta,
		Spec:     spec,
		Status:   status,
	}
}

func modelSourceFromUnstructured(obj map[string]interface{}) *aigridv1.ModelSource {
	uri, _, _ := unstructured.NestedString(obj, "spec", "model", "uri")
	name, _, _ := unstructured.NestedString(obj, "spec", "model", "name")
	if uri == "" && name == "" {
		return nil
	}
	return &aigridv1.ModelSource{Uri: uri, Name: name}
}

func modelContainerFromUnstructured(obj map[string]interface{}) *aigridv1.ModelContainer {
	containers, found, err := unstructured.NestedSlice(obj, "spec", "template", "containers")
	if err != nil || !found {
		return nil
	}
	var main map[string]interface{}
	for _, raw := range containers {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if stringField(m, "name") == "main" || main == nil {
			main = m
			if stringField(m, "name") == "main" {
				break
			}
		}
	}
	if main == nil {
		return nil
	}
	out := &aigridv1.ModelContainer{
		Image: stringField(main, "image"),
	}
	if req, found, _ := unstructured.NestedStringMap(main, "resources", "requests"); found {
		out.Requests = req
	}
	if lim, found, _ := unstructured.NestedStringMap(main, "resources", "limits"); found {
		out.Limits = lim
	}
	if env, found, err := unstructured.NestedSlice(main, "env"); err == nil && found {
		for _, raw := range env {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			out.Env = append(out.Env, &aigridv1.ModelEnvVar{
				Name:  stringField(m, "name"),
				Value: stringField(m, "value"),
			})
		}
	}
	return out
}

func modelRouterFromUnstructured(obj map[string]interface{}) *aigridv1.ModelRouter {
	router, found, err := unstructured.NestedMap(obj, "spec", "router")
	if err != nil || !found {
		return nil
	}
	out := &aigridv1.ModelRouter{}
	if _, ok, _ := unstructured.NestedFieldNoCopy(router, "route"); ok {
		out.ManagedRoute = true
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(router, "scheduler"); ok {
		out.Scheduler = true
	}
	if refs, found, err := unstructured.NestedSlice(router, "gateway", "refs"); err == nil && found {
		for _, raw := range refs {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			out.GatewayRefs = append(out.GatewayRefs, &aigridv1.ModelGatewayRef{
				Name:        stringField(m, "name"),
				Namespace:   stringField(m, "namespace"),
				SectionName: stringField(m, "sectionName"),
			})
		}
	}
	return out
}

func modelScalingFromUnstructured(obj map[string]interface{}) *aigridv1.ModelScaling {
	scaling, found, err := unstructured.NestedMap(obj, "spec", "scaling")
	if err != nil || !found {
		return nil
	}
	out := &aigridv1.ModelScaling{
		MinReplicas: int32(nestedInt64(scaling, "minReplicas")),
		MaxReplicas: int32(nestedInt64(scaling, "maxReplicas")),
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(scaling, "keda"); ok {
		out.Keda = true
		if triggers, found, err := unstructured.NestedSlice(scaling, "keda", "triggers"); err == nil && found {
			for _, raw := range triggers {
				m, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				t := &aigridv1.ModelKedaTrigger{Type: stringField(m, "type")}
				if md, found, _ := unstructured.NestedStringMap(m, "metadata"); found {
					t.Metadata = md
				}
				out.Triggers = append(out.Triggers, t)
			}
		}
	}
	return out
}

func modelStatusFromLLMInferenceService(obj map[string]interface{}) *aigridv1.ModelStatus {
	status := &aigridv1.ModelStatus{}
	if url, found, err := unstructured.NestedString(obj, "status", "url"); err == nil && found {
		status.Url = url
	}
	if og := nestedInt64(obj, "status", "observedGeneration"); og != 0 {
		status.ObservedGeneration = og
	}
	if conds, found, err := unstructured.NestedSlice(obj, "status", "conditions"); err == nil && found {
		status.Conditions = conditionsFromSlice(conds)
	}
	deriveModelPhase(status)
	return status
}

func deriveModelPhase(status *aigridv1.ModelStatus) {
	if status == nil {
		return
	}
	ready := conditionByType(status.Conditions, "Ready")
	if ready == nil {
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
		return
	}
	reason := ""
	if ready.Reason != nil {
		reason = *ready.Reason
	}
	message := ""
	if ready.Message != nil {
		message = *ready.Message
	}
	status.Reason = reason
	status.Message = message
	switch ready.Status {
	case aigridv1.ConditionStatus_CONDITION_STATUS_TRUE:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_READY
	case aigridv1.ConditionStatus_CONDITION_STATUS_FALSE:
		switch reason {
		case "ConfigNotFound", "RefsInvalid", "GatewaysNotReady":
			status.Phase = aigridv1.ModelPhase_MODEL_PHASE_FAILED
		default:
			status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
		}
	default:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
	}
}

func conditionByType(conds []*aigridv1.Condition, typ string) *aigridv1.Condition {
	for _, c := range conds {
		if c != nil && c.GetType() == typ {
			return c
		}
	}
	return nil
}

func modelPlacementFromUnstructured(item unstructured.Unstructured) *aigridv1.ModelPlacement {
	out := &aigridv1.ModelPlacement{}
	if sets, found, err := unstructured.NestedStringSlice(item.Object, "spec", "clusterSets"); err == nil && found {
		out.ClusterSets = sets
	}
	if n, found, err := unstructured.NestedInt64(item.Object, "spec", "numberOfClusters"); err == nil && found {
		v := int32(n)
		out.NumberOfClusters = &v
	}
	sel := &aigridv1.ModelLabelSelector{}
	if labels, found, err := unstructured.NestedStringMap(item.Object, "spec", "predicates"); err == nil && found {
		_ = labels
	}
	predicates, found, err := unstructured.NestedSlice(item.Object, "spec", "predicates")
	if err == nil && found && len(predicates) > 0 {
		pred, ok := predicates[0].(map[string]interface{})
		if ok {
			if ml, found, _ := unstructured.NestedStringMap(pred, "requiredClusterSelector", "labelSelector", "matchLabels"); found {
				sel.MatchLabels = ml
			}
			if exprs, found, err := unstructured.NestedSlice(pred, "requiredClusterSelector", "labelSelector", "matchExpressions"); err == nil && found {
				for _, raw := range exprs {
					m, ok := raw.(map[string]interface{})
					if !ok {
						continue
					}
					req := &aigridv1.ModelLabelSelectorRequirement{
						Key:      stringField(m, "key"),
						Operator: stringField(m, "operator"),
					}
					if vals, _, _ := unstructured.NestedStringSlice(m, "values"); len(vals) > 0 {
						req.Values = vals
					}
					sel.MatchExpressions = append(sel.MatchExpressions, req)
				}
			}
		}
	}
	if len(sel.MatchLabels) > 0 || len(sel.MatchExpressions) > 0 {
		out.LabelSelector = sel
	}
	return out
}

func modelRolloutFromMWRS(item unstructured.Unstructured) *aigridv1.ModelRollout {
	refs, found, err := unstructured.NestedSlice(item.Object, "spec", "placementRefs")
	if err != nil || !found || len(refs) == 0 {
		return nil
	}
	ref, ok := refs[0].(map[string]interface{})
	if !ok {
		return nil
	}
	strategyType, _, _ := unstructured.NestedString(ref, "rolloutStrategy", "type")
	out := &aigridv1.ModelRollout{
		Strategy: rolloutStrategyFromString(strategyType),
	}
	if v, found, _ := unstructured.NestedString(ref, "rolloutStrategy", "progressive", "maxConcurrency"); found {
		out.MaxConcurrency = stringifyQuantity(v, ref, "rolloutStrategy", "progressive", "maxConcurrency")
	} else if n := nestedInt64(ref, "rolloutStrategy", "progressive", "maxConcurrency"); n != 0 {
		out.MaxConcurrency = strconv.FormatInt(n, 10)
	}
	if v, found, _ := unstructured.NestedString(ref, "rolloutStrategy", "progressive", "maxFailures"); found {
		out.MaxFailures = v
	} else if n := nestedInt64(ref, "rolloutStrategy", "progressive", "maxFailures"); n != 0 {
		out.MaxFailures = strconv.FormatInt(n, 10)
	}
	if v, found, _ := unstructured.NestedString(ref, "rolloutStrategy", "progressive", "progressDeadline"); found {
		out.ProgressDeadline = v
	}
	if v, found, _ := unstructured.NestedString(ref, "rolloutStrategy", "progressive", "minSuccessTime"); found {
		out.MinSuccessTime = v
	}
	return out
}

func stringifyQuantity(s string, obj map[string]interface{}, fields ...string) string {
	if s != "" {
		return s
	}
	n := nestedInt64(obj, fields...)
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func rolloutStrategyFromString(s string) aigridv1.ModelRolloutStrategy {
	switch strings.ToLower(s) {
	case "all":
		return aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_ALL
	case "progressive":
		return aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_PROGRESSIVE
	case "progressivepergroup":
		return aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_PROGRESSIVE_PER_GROUP
	default:
		return aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_UNSPECIFIED
	}
}

func rolloutStrategyToString(s aigridv1.ModelRolloutStrategy) string {
	switch s {
	case aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_PROGRESSIVE:
		return "Progressive"
	case aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_PROGRESSIVE_PER_GROUP:
		return "ProgressivePerGroup"
	default:
		return "All"
	}
}

func enrichModelFromPlacement(model *aigridv1.Model, placement *unstructured.Unstructured, decision *unstructured.Unstructured) {
	if model == nil || model.Spec == nil || placement == nil {
		return
	}
	model.Spec.Placement = modelPlacementFromUnstructured(*placement)
	if model.Status == nil {
		model.Status = &aigridv1.ModelStatus{}
	}
	ps := &aigridv1.ModelPlacementStatus{}
	if decision != nil {
		decs, found, err := unstructured.NestedSlice(decision.Object, "status", "decisions")
		if err == nil && found {
			for _, raw := range decs {
				m, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				if name := stringField(m, "clusterName"); name != "" {
					ps.SelectedClusters = append(ps.SelectedClusters, name)
				}
			}
		}
	}
	ps.NumberOfSelectedClusters = int32(len(ps.SelectedClusters))
	if conds, found, err := unstructured.NestedSlice(placement.Object, "status", "conditions"); err == nil && found {
		ps.Conditions = conditionsFromSlice(conds)
	}
	model.Status.Placement = ps
}

func enrichModelFromMWRS(model *aigridv1.Model, mwrs *unstructured.Unstructured) {
	if model == nil || model.Spec == nil || mwrs == nil {
		return
	}
	model.Spec.Rollout = modelRolloutFromMWRS(*mwrs)
	if model.Status == nil {
		model.Status = &aigridv1.ModelStatus{}
	}
	rs := &aigridv1.ModelRolloutStatus{}
	if conds, found, err := unstructured.NestedSlice(mwrs.Object, "status", "conditions"); err == nil && found {
		rs.Conditions = conditionsFromSlice(conds)
		if c := conditionByType(rs.Conditions, "PlacementRolledOut"); c != nil && c.Status == aigridv1.ConditionStatus_CONDITION_STATUS_TRUE {
			rs.RolledOut = true
		}
		if c := conditionByType(rs.Conditions, "ManifestworkApplied"); c != nil && c.Status == aigridv1.ConditionStatus_CONDITION_STATUS_TRUE {
			rs.Applied = true
		}
		if c := conditionByType(rs.Conditions, "ManifestworkApplied"); c != nil && c.Reason != nil {
			rs.Reason = *c.Reason
		} else if c := conditionByType(rs.Conditions, "PlacementRolledOut"); c != nil && c.Reason != nil {
			rs.Reason = *c.Reason
		}
	}
	model.Status.Rollout = rs
	if rs.RolledOut && rs.Applied {
		model.Status.Phase = aigridv1.ModelPhase_MODEL_PHASE_READY
	} else if rs.Reason == "NotAsExpected" {
		model.Status.Phase = aigridv1.ModelPhase_MODEL_PHASE_FAILED
		model.Status.Reason = rs.Reason
	} else if model.Status.Phase == aigridv1.ModelPhase_MODEL_PHASE_UNSPECIFIED {
		model.Status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
		model.Status.Reason = rs.Reason
	}
}

// --- builders ---

func llmInferenceServiceFromModel(object *aigridv1.Model, namespace string) *unstructured.Unstructured {
	id := modelID(object)
	meta := map[string]interface{}{
		"name":      id,
		"namespace": namespace,
	}
	labels := map[string]string{modelLabelKey: id, managedByLabelKey: managedByLabelValue}
	annotations := map[string]string{}
	if object.GetMetadata() != nil {
		for k, v := range object.GetMetadata().GetLabels() {
			labels[k] = v
		}
		for k, v := range object.GetMetadata().GetAnnotations() {
			annotations[k] = v
		}
	}
	meta["labels"] = stringMapToInterfaceMap(labels)
	if len(annotations) > 0 {
		meta["annotations"] = stringMapToInterfaceMap(annotations)
	}

	spec := modelSpecToLLMMap(object.GetSpec(), id)
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": llmInferenceAPIVersion,
		"kind":       llmInferenceKind,
		"metadata":   meta,
		"spec":       spec,
	}}
}

func modelSpecToLLMMap(spec *aigridv1.ModelSpec, id string) map[string]interface{} {
	out := map[string]interface{}{}
	if spec == nil {
		return out
	}
	modelName := id
	uri := ""
	if spec.GetModel() != nil {
		uri = spec.GetModel().GetUri()
		if n := spec.GetModel().GetName(); n != "" {
			modelName = n
		}
	}
	out["model"] = map[string]interface{}{
		"uri":  uri,
		"name": modelName,
	}
	if spec.Replicas != nil {
		out["replicas"] = int64(*spec.Replicas)
	}
	if len(spec.GetBaseRefs()) > 0 {
		refs := make([]interface{}, 0, len(spec.GetBaseRefs()))
		for _, name := range spec.GetBaseRefs() {
			refs = append(refs, map[string]interface{}{"name": name})
		}
		out["baseRefs"] = refs
	}
	if spec.GetRuntime() != "" {
		out["runtime"] = spec.GetRuntime()
	}
	out["router"] = modelRouterToMap(spec.GetRouter())
	if main := modelContainerToMap(spec.GetMain()); main != nil {
		out["template"] = map[string]interface{}{
			"containers": []interface{}{main},
		}
	}
	if scaling := modelScalingToMap(spec.GetScaling()); scaling != nil {
		out["scaling"] = scaling
	}
	return out
}

func modelRouterToMap(router *aigridv1.ModelRouter) map[string]interface{} {
	out := map[string]interface{}{}
	managedRoute := true
	scheduler := false
	var refs []*aigridv1.ModelGatewayRef
	if router != nil {
		refs = router.GetGatewayRefs()
		scheduler = router.GetScheduler()
		// Honor explicit managed_route; otherwise default on when nothing else is set.
		if router.GetManagedRoute() || router.GetScheduler() || len(refs) > 0 {
			managedRoute = router.GetManagedRoute()
		}
	}
	if managedRoute {
		out["route"] = map[string]interface{}{}
	}
	if scheduler {
		out["scheduler"] = map[string]interface{}{}
	}
	if len(refs) == 0 {
		refs = []*aigridv1.ModelGatewayRef{{
			Name:      defaultGatewayName,
			Namespace: defaultGatewayNamespace,
		}}
	}
	gwRefs := make([]interface{}, 0, len(refs))
	for _, r := range refs {
		if r == nil {
			continue
		}
		entry := map[string]interface{}{"name": r.GetName()}
		if r.GetNamespace() != "" {
			entry["namespace"] = r.GetNamespace()
		}
		if r.GetSectionName() != "" {
			entry["sectionName"] = r.GetSectionName()
		}
		gwRefs = append(gwRefs, entry)
	}
	out["gateway"] = map[string]interface{}{"refs": gwRefs}
	return out
}

func modelContainerToMap(main *aigridv1.ModelContainer) map[string]interface{} {
	if main == nil {
		return map[string]interface{}{"name": "main"}
	}
	out := map[string]interface{}{"name": "main"}
	if main.GetImage() != "" {
		out["image"] = main.GetImage()
	}
	resources := map[string]interface{}{}
	if len(main.GetRequests()) > 0 {
		resources["requests"] = stringMapToInterfaceMap(main.GetRequests())
	}
	if len(main.GetLimits()) > 0 {
		resources["limits"] = stringMapToInterfaceMap(main.GetLimits())
	}
	if len(resources) > 0 {
		out["resources"] = resources
	}
	if len(main.GetEnv()) > 0 {
		env := make([]interface{}, 0, len(main.GetEnv()))
		for _, e := range main.GetEnv() {
			if e == nil || e.GetName() == "" {
				continue
			}
			env = append(env, map[string]interface{}{
				"name":  e.GetName(),
				"value": e.GetValue(),
			})
		}
		if len(env) > 0 {
			out["env"] = env
		}
	}
	return out
}

func modelScalingToMap(scaling *aigridv1.ModelScaling) map[string]interface{} {
	if scaling == nil {
		return nil
	}
	out := map[string]interface{}{
		"maxReplicas": int64(scaling.GetMaxReplicas()),
	}
	min := scaling.GetMinReplicas()
	if min <= 0 {
		min = 1
	}
	out["minReplicas"] = int64(min)
	if scaling.GetKeda() {
		triggers := make([]interface{}, 0, len(scaling.GetTriggers()))
		for _, t := range scaling.GetTriggers() {
			if t == nil {
				continue
			}
			entry := map[string]interface{}{"type": t.GetType()}
			if len(t.GetMetadata()) > 0 {
				entry["metadata"] = stringMapToInterfaceMap(t.GetMetadata())
			}
			triggers = append(triggers, entry)
		}
		out["keda"] = map[string]interface{}{"triggers": triggers}
	}
	return out
}

func placementFromModel(object *aigridv1.Model, namespace string) *unstructured.Unstructured {
	id := modelID(object)
	name := placementNameForModel(id)
	placement := object.GetSpec().GetPlacement()
	meta := map[string]interface{}{
		"name":      name,
		"namespace": namespace,
		"labels": stringMapToInterfaceMap(map[string]string{
			modelLabelKey:     id,
			managedByLabelKey: managedByLabelValue,
		}),
	}

	matchLabels := map[string]string{
		"ai-grid/enabled":     "true",
		"ai-grid/provisioned": "true",
	}
	var exprs []interface{}
	if placement != nil && placement.GetLabelSelector() != nil {
		sel := placement.GetLabelSelector()
		for k, v := range sel.GetMatchLabels() {
			matchLabels[k] = v
		}
		for _, e := range sel.GetMatchExpressions() {
			if e == nil {
				continue
			}
			entry := map[string]interface{}{
				"key":      e.GetKey(),
				"operator": e.GetOperator(),
			}
			if len(e.GetValues()) > 0 {
				vals := make([]interface{}, 0, len(e.GetValues()))
				for _, v := range e.GetValues() {
					vals = append(vals, v)
				}
				entry["values"] = vals
			}
			exprs = append(exprs, entry)
		}
	}
	labelSelector := map[string]interface{}{
		"matchLabels": stringMapToInterfaceMap(matchLabels),
	}
	if len(exprs) > 0 {
		labelSelector["matchExpressions"] = exprs
	}
	predicate := map[string]interface{}{
		"requiredClusterSelector": map[string]interface{}{
			"labelSelector": labelSelector,
		},
	}
	spec := map[string]interface{}{
		"predicates": []interface{}{predicate},
	}
	if placement != nil {
		if len(placement.GetClusterSets()) > 0 {
			sets := make([]interface{}, 0, len(placement.GetClusterSets()))
			for _, s := range placement.GetClusterSets() {
				sets = append(sets, s)
			}
			spec["clusterSets"] = sets
		}
		if placement.NumberOfClusters != nil {
			spec["numberOfClusters"] = int64(*placement.NumberOfClusters)
		}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": placementAPIVersion,
		"kind":       placementKind,
		"metadata":   meta,
		"spec":       spec,
	}}
}

func mwrsFromModel(object *aigridv1.Model, namespace string, deployment *unstructured.Unstructured) *unstructured.Unstructured {
	id := modelID(object)
	name := placementNameForModel(id)
	strategy := aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_ALL
	rollout := object.GetSpec().GetRollout()
	if rollout != nil && rollout.GetStrategy() != aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_UNSPECIFIED {
		strategy = rollout.GetStrategy()
	}
	placementRef := map[string]interface{}{
		"name": name,
		"rolloutStrategy": map[string]interface{}{
			"type": rolloutStrategyToString(strategy),
		},
	}
	if rollout != nil && strategy == aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_PROGRESSIVE {
		progressive := map[string]interface{}{}
		if rollout.GetMaxConcurrency() != "" {
			progressive["maxConcurrency"] = rollout.GetMaxConcurrency()
		}
		if rollout.GetMaxFailures() != "" {
			progressive["maxFailures"] = rollout.GetMaxFailures()
		}
		if rollout.GetProgressDeadline() != "" {
			progressive["progressDeadline"] = rollout.GetProgressDeadline()
		}
		if rollout.GetMinSuccessTime() != "" {
			progressive["minSuccessTime"] = rollout.GetMinSuccessTime()
		}
		if len(progressive) > 0 {
			placementRef["rolloutStrategy"].(map[string]interface{})["progressive"] = progressive
		}
	}

	// Clone deployment into the work template without status.
	manifest := deployment.DeepCopy()
	delete(manifest.Object, "status")
	if md, ok := manifest.Object["metadata"].(map[string]interface{}); ok {
		delete(md, "resourceVersion")
		delete(md, "uid")
		delete(md, "generation")
		delete(md, "creationTimestamp")
		delete(md, "managedFields")
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": mwrsAPIVersion,
		"kind":       mwrsKind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"labels": stringMapToInterfaceMap(map[string]string{
				modelLabelKey:     id,
				managedByLabelKey: managedByLabelValue,
			}),
		},
		"spec": map[string]interface{}{
			"placementRefs": []interface{}{placementRef},
			"manifestWorkTemplate": map[string]interface{}{
				"workload": map[string]interface{}{
					"manifests": []interface{}{manifest.Object},
				},
			},
		},
	}}
}

func maasModelRefFromModel(object *aigridv1.Model, namespace string) *unstructured.Unstructured {
	id := modelID(object)
	meta := map[string]interface{}{
		"name":      id,
		"namespace": namespace,
		"labels": stringMapToInterfaceMap(map[string]string{
			modelLabelKey:     id,
			managedByLabelKey: managedByLabelValue,
		}),
	}
	annotations := map[string]string{}
	if object.GetMetadata() != nil {
		for k, v := range object.GetMetadata().GetAnnotations() {
			annotations[k] = v
		}
	}
	if len(annotations) > 0 {
		meta["annotations"] = stringMapToInterfaceMap(annotations)
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": maasModelRefAPIVersion,
		"kind":       maasModelRefKind,
		"metadata":   meta,
		"spec": map[string]interface{}{
			"modelRef": map[string]interface{}{
				"kind": llmInferenceKind,
				"name": id,
			},
		},
	}}
}
