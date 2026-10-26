package draft

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// Field-mask paths UpdateManagedCluster accepts. Anything else is
// InvalidArgument; `hub_accepts_client` is not writable through the console
// (checkConsoleKeys explains why).
const (
	PathLabels      = "labels"
	PathAnnotations = "annotations"
)

// consoleKeyPrefixes and consoleLabels are the label and annotation keys the
// console may change on a ManagedCluster: its own `ai-grid/*` conventions
// (enrolment, site, region, map position), the `location` label, and
// `ai-grid-mock/*`, the fake's own test switches (`ai-grid-mock/fail-apply`).
var (
	consoleKeyPrefixes = []string{"ai-grid/", "ai-grid-mock/"}
	consoleLabels      = map[string]bool{"location": true}
)

func isConsoleKey(key string, extra map[string]bool) bool {
	for _, p := range consoleKeyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return extra[key]
}

// checkConsoleKeys is the console's proposal for the backend, not hub
// behaviour (a provider admin may patch any label on the hub): a provider
// console writes only what the POC writes, so an update that adds, changes or
// removes a label or annotation outside those keys is InvalidArgument. That
// keeps a buggy page or script from moving a site to another cluster set (the
// `cluster.open-cluster-management.io/clusterset` label decides Placements
// and RBAC scope) or stripping labels the hub does not put back. It compares
// both sides after the hub has put back the labels it owns
// (reconcileMetadata), so a client that leaves one of those out still
// passes, and a label the hub adds on any write is not the client's change. For the same reason `hub_accepts_client` is not writable: turning it
// off makes the hub drop a live site, and no page needs it yet. Console text,
// listed in docs/draft/areas/clusters.md.
func checkConsoleKeys(next, cur *aigriddraftv1.ManagedCluster) error {
	for _, m := range []struct {
		kind       string
		next, cur  map[string]string
		extraAllow map[string]bool
	}{{"label", next.GetLabels(), cur.GetLabels(), consoleLabels}, {"annotation", next.GetAnnotations(), cur.GetAnnotations(), nil}} {
		keys := make([]string, 0, len(m.next)+len(m.cur))
		for k := range m.next {
			keys = append(keys, k)
		}
		for k := range m.cur {
			if _, ok := m.next[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			if isConsoleKey(k, m.extraAllow) {
				continue
			}
			nv, nok := m.next[k]
			cv, cok := m.cur[k]
			if nok != cok || nv != cv {
				return status.Errorf(codes.InvalidArgument, "%s %q cannot be changed through the console; only ai-grid/* labels and annotations and the location label can", m.kind, k)
			}
		}
	}
	return nil
}

// The AI Grid annotations the backend owns the convention of: the map
// position of a site (docs/draft/research/acm.md, the console's labels and
// annotations). UpdateManagedCluster checks them (checkCoordinates).
const (
	AnnotationLatitude  = "ai-grid/latitude"
	AnnotationLongitude = "ai-grid/longitude"
)

// checkCoordinates is the console's proposal for the backend, not
// API-server behaviour (the hub stores any annotation string): an
// `ai-grid/latitude` or `ai-grid/longitude` annotation that is present must
// be a finite decimal number within -90..90 or -180..180 (surrounding
// whitespace and an empty value are allowed, as the read side ignores them),
// so a bad value is
// refused on write instead of vanishing from the map on read (the BFF omits a
// coordinate it cannot parse). Console text, listed in docs/draft/areas/clusters.md.
func checkCoordinates(annotations map[string]string) error {
	for _, c := range []struct {
		key   string
		limit float64
	}{{AnnotationLatitude, 90}, {AnnotationLongitude, 180}} {
		v, ok := annotations[c.key]
		if !ok {
			continue
		}
		// The read side (the BFF's parseCoordinate) trims the value and shows
		// an empty one as no coordinate, so a value it displays fine must not
		// fail here when a client sends the map it loaded back.
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < -c.limit || f > c.limit {
			return status.Errorf(codes.InvalidArgument, "annotation %q must be a number between %v and %v, got %q", c.key, -c.limit, c.limit, v)
		}
	}
	return nil
}

// notFound is the NotFound message shape from the conventions' error table
// (`object with identifier '<x>' not found`).
func notFound(name string) error {
	return status.Errorf(codes.NotFound, "object with identifier '%s' not found", name)
}

// ListManagedClusters implements ManagedClusters. Like a hub list, the result
// is sorted by name; `offset` and `limit` page it (omitted limit means all,
// zero means count only, negative is InvalidArgument). `filter` and `order`
// are drafted but not implemented here, so a non-empty value is
// InvalidArgument rather than silently ignored.
func (s *Server) ListManagedClusters(ctx context.Context, req *aigriddraftv1.ListManagedClustersRequest) (*aigriddraftv1.ListManagedClustersResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	if req.GetFilter() != "" {
		return nil, status.Error(codes.InvalidArgument, "filter is not supported by this server yet")
	}
	if req.GetOrder() != "" {
		return nil, status.Error(codes.InvalidArgument, "order is not supported by this server yet")
	}
	if req.GetOffset() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "offset must not be negative, got %d", req.GetOffset())
	}
	if req.Limit != nil && req.GetLimit() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "limit must not be negative, got %d", req.GetLimit())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	all := make([]*aigriddraftv1.ManagedCluster, 0, len(s.clusters))
	for _, c := range s.clusters {
		all = append(all, withDerivedState(c))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].GetName() < all[j].GetName() })
	total := len(all)
	offset := min(int(req.GetOffset()), total)
	page := all[offset:]
	if req.Limit != nil && int(req.GetLimit()) < len(page) {
		page = page[:req.GetLimit()]
	}
	return &aigriddraftv1.ListManagedClustersResponse{Clusters: page, Size: int32(len(page)), Total: int32(total)}, nil
}

// GetManagedCluster implements ManagedClusters.
func (s *Server) GetManagedCluster(ctx context.Context, req *aigriddraftv1.GetManagedClusterRequest) (*aigriddraftv1.GetManagedClusterResponse, error) {
	if _, err := s.begin(ctx); err != nil {
		return nil, err
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	c, ok := s.clusters[req.GetName()]
	if !ok {
		return nil, notFound(req.GetName())
	}
	return &aigriddraftv1.GetManagedClusterResponse{Object: withDerivedState(c)}, nil
}

// UpdateManagedCluster implements ManagedClusters with the semantics of
// docs/draft/research/backend-conventions.md section 5.3: the mask names whole
// fields of `object` (a map path replaces the map), `lock` compares
// `resource_version` and fails with Aborted, and every write bumps the
// version. The hub's webhook and controllers then react as reconcileMetadata
// describes (default clusterset, `name`, created-via, add-on and autodetect
// labels come back). Only the console's keys may change (checkConsoleKeys).
//
// An update that changes nothing is not written: when the object after the
// update, with the hub-owned labels put back, has the stored labels and
// annotations, the stored cluster comes back with its
// `resource_version` and `generation` unchanged, as the Kubernetes API server
// answers an unchanged update (k8s.io/apiserver v0.35.8
// pkg/storage/etcd3/store.go:553-577, GuaranteedUpdate skips the write when the
// new object encodes to the stored bytes). The `lock` precondition is checked
// before, as the API server checks the resource version before it gets there
// (pkg/registry/generic/registry/store.go:735-736), and so is the console's
// coordinate check (checkCoordinates), so a stale or invalid no-op still fails.
func (s *Server) UpdateManagedCluster(ctx context.Context, req *aigriddraftv1.UpdateManagedClusterRequest) (*aigriddraftv1.UpdateManagedClusterResponse, error) {
	token, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	obj := req.GetObject()
	if obj.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "object.name is required")
	}
	if err := s.authorizePatch(ctx, token, obj.GetName()); err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "update_mask must name at least one of %s", strings.Join([]string{PathLabels, PathAnnotations}, ", "))
	}
	for _, p := range paths {
		switch p {
		case PathLabels, PathAnnotations:
		default:
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not supported; supported paths: %s", p, strings.Join([]string{PathLabels, PathAnnotations}, ", "))
		}
	}
	if slices.Contains(paths, PathAnnotations) {
		if err := checkCoordinates(obj.GetAnnotations()); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance()
	c, ok := s.clusters[obj.GetName()]
	if !ok {
		return nil, notFound(obj.GetName())
	}
	if req.GetLock() && obj.GetResourceVersion() != c.GetResourceVersion() {
		return nil, status.Errorf(codes.Aborted, "object with identifier '%s' has resource version %s, the request carries %q; fetch it again and retry", obj.GetName(), c.GetResourceVersion(), obj.GetResourceVersion())
	}
	// The no-op check runs the update on a copy first, the webhook's and
	// controllers' follow-up included, and compares what a write could change.
	probe := cloneCluster(c)
	for _, p := range paths {
		switch p {
		case PathLabels:
			probe.Labels = copyMap(obj.GetLabels())
		case PathAnnotations:
			probe.Annotations = copyMap(obj.GetAnnotations())
		}
	}
	reconcileMetadata(probe, c)
	// Compared with the stored object as the hub would leave it after any
	// write, so a label the hub fills in itself (the default clusterset on a
	// cluster that has none) is not counted as the client's change.
	base := cloneCluster(c)
	reconcileMetadata(base, c)
	if err := checkConsoleKeys(probe, base); err != nil {
		return nil, err
	}
	if maps.Equal(probe.GetLabels(), c.GetLabels()) && maps.Equal(probe.GetAnnotations(), c.GetAnnotations()) {
		s.logger.Debug("cluster update changes nothing", "name", c.GetName(), "paths", paths, "resourceVersion", c.GetResourceVersion())
		return &aigriddraftv1.UpdateManagedClusterResponse{Object: withDerivedState(c)}, nil
	}
	before := cloneCluster(c)
	for _, p := range paths {
		switch p {
		case PathLabels:
			c.Labels = copyMap(obj.GetLabels())
		case PathAnnotations:
			c.Annotations = copyMap(obj.GetAnnotations())
		}
	}
	reconcileMetadata(c, before)
	s.bumpResourceVersion(c)
	s.logger.Debug("cluster updated", "name", c.GetName(), "paths", paths, "resourceVersion", c.GetResourceVersion())
	return &aigriddraftv1.UpdateManagedClusterResponse{Object: withDerivedState(c)}, nil
}

// copyMap returns an owned, non-nil copy so the request cannot alias the
// store. An empty map is kept as empty (whole-map replacement semantics).
func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return maps.Clone(m)
}
