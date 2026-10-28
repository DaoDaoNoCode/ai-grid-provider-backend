package servers

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// quantityRE is the buf.validate pattern on ModelContainer.requests and
// limits: the syntax resource.ParseQuantity accepts (its splitREString in
// k8s.io/apimachinery/pkg/api/resource/quantity.go), without a minus sign.
// The server checks it too, so the annotation and the server refuse the same
// values ("-0" parses as zero but is refused by both).
var quantityRE = regexp.MustCompile(`^\+?[0-9.]+[eEinumkKMGTP]*[-+]?[0-9]*$`)

// The rules below are Kubernetes' own for a container's resources, which
// every placed cluster would apply to the model's workload: a model saved
// with values they refuse would fail on each cluster after Argo CD delivers
// it. validateResourceRequirements and ValidateResourceQuantityValue in
// kubernetes/kubernetes pkg/apis/core/validation/validation.go, and
// IsOvercommitAllowed, IsNativeResource, IsExtendedResourceName and
// IsHugePageResourceName in pkg/apis/core/helper/helpers.go, at v1.35.8.

// overcommitAllowed reports whether a request may be below its limit: only
// for the native resources (no domain, or a kubernetes.io domain) other than
// hugepages. Extended resources such as nvidia.com/gpu and hugepages need a
// limit equal to the request.
func overcommitAllowed(name string) bool {
	return nativeResource(name) && !strings.HasPrefix(name, "hugepages-")
}

func nativeResource(name string) bool {
	return !strings.Contains(name, "/") || strings.Contains(name, "kubernetes.io/")
}

// extendedResource reports whether Kubernetes treats name as an extended
// resource, whose quantity must be a whole number.
func extendedResource(name string) bool {
	if nativeResource(name) || strings.HasPrefix(name, "requests.") {
		return false
	}
	return len(validation.IsQualifiedName("requests."+name)) == 0
}

// normalizeModelResources gives each resource Kubernetes can't overcommit
// that has a request and no limit the request as its limit: Kubernetes
// requires the limit ("Limit must be set for non overcommitable resources"),
// and defaults only the other way, a missing request to the limit. A form
// that asks for a number of GPUs sends the request; the saved model then
// carries both, as the console's BFF wrote them before.
func normalizeModelResources(main *aigridv1.ModelContainer) {
	if main == nil {
		return
	}
	for name, quantity := range main.GetRequests() {
		if overcommitAllowed(name) {
			continue
		}
		if _, ok := main.GetLimits()[name]; ok {
			continue
		}
		if main.Limits == nil {
			main.Limits = map[string]string{}
		}
		main.Limits[name] = quantity
	}
}

// dropDerivedLimits prepares an update that replaces spec.main.requests and
// keeps spec.main.limits: it removes from merged the limit of each resource
// that can't be overcommitted whose saved limit equals its saved request.
// Such a limit follows the request (normalizeModelResources set it, or the
// caller set it to the only value Kubernetes allows), so it must follow the
// new request: normalizeModelResources then gives it the new request, and a
// removed request takes its limit with it. Without this, a GPU count could
// not change through the requests alone, and removing the GPU request would
// keep the GPU, because Kubernetes defaults a missing request to the limit.
// A limit with no saved request stays.
func dropDerivedLimits(saved, merged *aigridv1.ModelContainer) {
	for name, request := range saved.GetRequests() {
		if overcommitAllowed(name) {
			continue
		}
		limit, ok := merged.GetLimits()[name]
		if ok && sameQuantity(request, limit) {
			delete(merged.Limits, name)
		}
	}
}

// sameQuantity reports whether two quantities are equal ("1" and "1000m"),
// or, when either doesn't parse, the same string.
func sameQuantity(a, b string) bool {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return qa.Cmp(qb) == 0
}

// validateModelResources refuses the requests and limits Kubernetes would
// refuse: a quantity that doesn't parse or is negative, an extended resource
// that isn't a whole number, a request above its limit, and a request that
// differs from its limit for a resource that can't be overcommitted. Run
// normalizeModelResources first. Names are checked in sorted order, so the
// message is stable.
func validateModelResources(main *aigridv1.ModelContainer) error {
	parse := func(path string, values map[string]string) (map[string]resource.Quantity, error) {
		parsed := map[string]resource.Quantity{}
		for _, name := range slices.Sorted(maps.Keys(values)) {
			if name == "" {
				return nil, connect.Errorf(connect.CodeInvalidArgument, "%s has an empty resource name %q", path, name)
			}
			quantity, err := parseResourceQuantity(name, values[name])
			if err != nil {
				return nil, connect.Errorf(connect.CodeInvalidArgument, "%s[%q] %q %v", path, name, values[name], err)
			}
			parsed[name] = quantity
		}
		return parsed, nil
	}
	requests, err := parse("spec.main.requests", main.GetRequests())
	if err != nil {
		return err
	}
	limits, err := parse("spec.main.limits", main.GetLimits())
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(requests)) {
		request := requests[name]
		limit, ok := limits[name]
		if !ok {
			continue
		}
		switch {
		case !overcommitAllowed(name) && request.Cmp(limit) != 0:
			return connect.Errorf(connect.CodeInvalidArgument,
				"spec.main.requests[%q] %q must equal spec.main.limits[%q] %q: Kubernetes doesn't overcommit %s",
				name, main.GetRequests()[name], name, main.GetLimits()[name], name)
		case request.Cmp(limit) > 0:
			return connect.Errorf(connect.CodeInvalidArgument,
				"spec.main.requests[%q] %q must be at most spec.main.limits[%q] %q",
				name, main.GetRequests()[name], name, main.GetLimits()[name])
		}
	}
	return nil
}

// parseResourceQuantity parses one value of requests or limits.
func parseResourceQuantity(name, value string) (resource.Quantity, error) {
	if !quantityRE.MatchString(value) {
		return resource.Quantity{}, errors.New("is not a non-negative Kubernetes quantity such as 500m, 2 or 8Gi")
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("is not a Kubernetes quantity: %w", err)
	}
	if quantity.Sign() < 0 {
		return resource.Quantity{}, errors.New("must not be negative")
	}
	if extendedResource(name) && quantity.MilliValue()%1000 != 0 {
		return resource.Quantity{}, fmt.Errorf("must be a whole number: %s is an extended resource", name)
	}
	return quantity, nil
}
