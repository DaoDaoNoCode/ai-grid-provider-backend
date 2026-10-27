// Package draft is the console's fake backend for the APIs this backend does
// not implement yet: it serves the console's draft proto, ai_grid.draft.v1
// (proto/ai_grid/draft/v1), from fixtures (fixtures/draft/default), with its
// own in-memory state, behaving like the real service against a real hub would.
// cmd/ai-grid-fake-infra serves it on --draft-address next to the fake hub and
// the fake Keycloak, so `make dev-fake` runs the whole console on a laptop.
// The console's SelfSubjectAccessReview goes to the fake hub
// (internal/fakeinfra/hub), not to this package.
//
// It was moved here from the console (ai-grid-provider-console
// bff/internal/fakebackend at ac80c25) with its behaviour unchanged. Each area's
// draft proto and fake are deleted when the real implementation of that area
// lands in this backend; see docs/draft/README.md.
//
// The point of the fake is faithfulness, not convenience: every behaviour it
// simulates (field shapes, condition strings, error codes, latency) is taken
// from the real cluster and the backend's conventions, see docs/draft/README.md.
// When the real backend gains an RPC, the fake gains the same RPC with the same
// semantics; RPCs that do not exist yet answer codes.Unimplemented, exactly
// as the real server does, so the BFF and the UI can be built against the gap.
package draft

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// Server holds the in-memory state behind the fake RPCs. It is safe for
// concurrent use.
type Server struct {
	aigriddraftv1.UnimplementedAiGridServer
	aigriddraftv1.UnimplementedManagedClustersServer
	aigriddraftv1.UnimplementedTenantsServer
	// Services whose shape is drafted but not agreed answer Unimplemented, as
	// the real server does for a stub; see proto/ai_grid/draft/v1/*_service.proto.
	aigriddraftv1.UnimplementedGridServer
	aigriddraftv1.UnimplementedCatalogItemsServer

	mu       sync.RWMutex
	clusters map[string]*aigriddraftv1.ManagedCluster
	tenants  map[string]*aigriddraftv1.Tenant
	access   Access

	// capabilities is what GetHubCapabilities answers (MWRS gate, observability, ACM version).
	capabilities *aigriddraftv1.HubCapabilities
	// resourceVersion is the last value handed out, like etcd's revision:
	// one counter for the whole store, bumped on every write.
	resourceVersion int64
	// Scenario state: timed transitions evaluated lazily against Clock.
	scenarios []*scenario
	start     time.Time
	speed     float64
	// clock is the time source for scenarios, condition transition times
	// and taint stamps; time.Now unless SetClock replaced it.
	clock func() time.Time
	// newID assigns Keycloak organization ids; random UUID v4 unless
	// SetIDGenerator replaced it.
	newID func() string
	// realmMissing makes every Tenants RPC answer FailedPrecondition, the
	// state of the dev hub today (Keycloak without an ai-grid realm);
	// orgsDisabled does the same for a realm with organizationsEnabled=false.
	realmMissing bool
	orgsDisabled bool
	// Latency is added to every RPC to resemble a hub round trip.
	Latency time.Duration
	// TenantsErr, when set, makes every Tenants RPC fail with it. It models a
	// backend whose Keycloak is unreachable, so the UI's error path is
	// exercised with a real code.
	TenantsErr error
	// stopTimeout bounds GracefulStop in Serve; zero means defaultStopTimeout.
	stopTimeout time.Duration
	logger      *slog.Logger
}

// defaultStopTimeout is how long Serve drains in-flight RPCs on shutdown
// before it closes the connections that are still open, typically a health
// Watch or a reflection stream a client never ends.
const defaultStopTimeout = 5 * time.Second

// New builds a server from fixtures. Fixture validation errors are returned so
// a typo in fixtures/draft/default fails fast instead of serving nonsense. The fixture
// messages are copied: the caller keeps fx and may change it afterwards
// without touching the server's state, which only changes under s.mu.
func New(fx *Fixtures, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		clusters:     map[string]*aigriddraftv1.ManagedCluster{},
		tenants:      map[string]*aigriddraftv1.Tenant{},
		access:       fx.Access,
		capabilities: &aigriddraftv1.HubCapabilities{ManifestWorkReplicaSetEnabled: true, ObservabilityInstalled: fx.ObservabilityInstalled, AcmVersion: fx.ACMVersion},
		speed:        fx.Speed,
		clock:        time.Now,
		newID:        newUUID,
		Latency:      fx.Latency.Duration(),
		logger:       logger,
	}
	if s.speed <= 0 {
		s.speed = 1
	}
	if fx.ManifestWorkReplicaSetEnabled != nil {
		s.capabilities.ManifestWorkReplicaSetEnabled = *fx.ManifestWorkReplicaSetEnabled
	}
	if s.capabilities.AcmVersion == "" {
		s.capabilities.AcmVersion = defaultACMVersion
	}
	for _, c := range fx.Clusters {
		if c.GetName() == "" {
			return nil, fmt.Errorf("fixture cluster without a name")
		}
		if _, dup := s.clusters[c.GetName()]; dup {
			return nil, fmt.Errorf("fixture cluster %q listed twice", c.GetName())
		}
		c = cloneCluster(c)
		sortClaims(c)
		if rv, err := strconv.ParseInt(c.GetResourceVersion(), 10, 64); err == nil && rv > s.resourceVersion {
			s.resourceVersion = rv
		}
		s.clusters[c.GetName()] = c
	}
	names := make([]string, 0, len(s.clusters))
	for name := range s.clusters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if c := s.clusters[name]; c.GetResourceVersion() == "" {
			s.bumpResourceVersion(c)
		}
	}
	scenarios, err := parseScenarios(fx.Scenarios, s.clusters)
	if err != nil {
		return nil, err
	}
	s.scenarios = scenarios
	s.start = s.clock()
	for _, t := range fx.Tenants {
		if err := s.seedTenant(t); err != nil {
			return nil, err
		}
	}
	if fx.Tenants == nil && fx.TenantsUnavailable != "" {
		// backend-conventions.md 5.7: Keycloak unreachable is Unavailable with
		// a message that names Keycloak. The shipped backend still returns
		// Unknown here (keycloak.md 7); the fake implements the convention.
		s.TenantsErr = status.Error(codes.Unavailable, MsgKeycloakDownPref+fx.TenantsUnavailable)
	}
	s.realmMissing = fx.TenantsRealmMissing
	s.orgsDisabled = fx.TenantsOrgsDisabled
	return s, nil
}

// defaultACMVersion is the hub's MultiClusterHub version (acm.md, "Feature
// gate and installation facts").
const defaultACMVersion = "2.17.3"

// SetIDGenerator replaces the source of organization ids, so tests can
// predict the id of a created tenant.
func (s *Server) SetIDGenerator(next func() string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newID = next
}

// SetClock replaces the time source and restarts the scenario clock from it,
// so tests drive transitions deterministically.
func (s *Server) SetClock(clock func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = clock
	s.start = clock()
}

// Register attaches every service of the draft proto, the health service and
// reflection, the same set the real backend registers. Methods the fake does
// not implement answer Unimplemented from the embedded Unimplemented*Server.
func (s *Server) Register(srv *grpc.Server) {
	aigriddraftv1.RegisterAiGridServer(srv, s)
	aigriddraftv1.RegisterManagedClustersServer(srv, s)
	aigriddraftv1.RegisterTenantsServer(srv, s)
	aigriddraftv1.RegisterGridServer(srv, s)
	aigriddraftv1.RegisterCatalogItemsServer(srv, s)
	h := health.NewServer()
	h.SetServingStatus("", healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(srv, h)
	reflection.Register(srv)
}

// ServeListener is Serve on a listener the caller opened, so a caller can
// bind every port before it reports ready.
func (s *Server) ServeListener(ctx context.Context, lis net.Listener) error {
	return s.serve(ctx, lis)
}

// Serve listens on addr until ctx is done. Shutdown is graceful: in-flight
// RPCs finish, but after stopTimeout the streams still open are closed, so a
// client holding a health Watch cannot keep the process alive after SIGTERM.
func (s *Server) Serve(ctx context.Context, addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	return s.serve(ctx, lis)
}

func (s *Server) serve(ctx context.Context, lis net.Listener) error {
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(s.logCalls))
	s.Register(srv)
	go func() {
		<-ctx.Done()
		s.stop(srv)
	}()
	s.logger.Info("fake draft backend listening", "addr", lis.Addr().String(), "package", "ai_grid.draft.v1")
	if err := srv.Serve(lis); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// stop drains srv gracefully and forces the stop once stopTimeout has passed,
// the bounded shutdown gRPC recommends: GracefulStop alone waits for every
// stream to end, which a long-lived Watch never does on its own.
func (s *Server) stop(srv *grpc.Server) {
	timeout := s.stopTimeout
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	drained := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(drained)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		s.logger.Warn("graceful stop timed out, closing the streams still open", "timeout", timeout)
		srv.Stop()
		<-drained
	}
}

// logCalls logs each RPC with the forwarded identity, which is what the real
// backend sees, so developers can check the BFF forwards the right metadata.
func (s *Server) logCalls(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	md, _ := metadata.FromIncomingContext(ctx)
	user := strings.Join(md.Get("x-forwarded-user"), ",")
	code := codes.OK
	if err != nil {
		code = status.Code(err)
	}
	s.logger.Info("rpc", "method", info.FullMethod, "user", user, "hasToken", len(md.Get("authorization")) > 0, "code", code.String(), "duration", time.Since(start).Round(time.Millisecond))
	return resp, err
}

// bumpResourceVersion stamps a written object with the next store revision.
// Must be called under s.mu.
func (s *Server) bumpResourceVersion(c *aigriddraftv1.ManagedCluster) {
	s.resourceVersion++
	c.ResourceVersion = strconv.FormatInt(s.resourceVersion, 10)
}

// authenticate enforces the identity contract of
// docs/draft/research/backend-conventions.md section 5.6: every RPC except Ping,
// health and reflection needs an `authorization` metadata value of the form
// `Bearer <token>`; the x-forwarded-* headers are advisory and only logged.
// It returns the bearer token.
func authenticate(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) != 1 {
		return "", status.Error(codes.Unauthenticated, "missing authorization metadata")
	}
	token, ok := strings.CutPrefix(values[0], "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", status.Error(codes.Unauthenticated, "authorization metadata must be 'Bearer <token>'")
	}
	return token, nil
}

// isProviderAdmin is the hub's RBAC answer for the token, taken from the
// access fixture (fixtures/draft/*/access.json, the same tokens the fake hub's
// fixtures/{default,poc}/access.json converts for the BFF's
// SelfSubjectAccessReview), so a token that is not a provider admin there
// cannot write here either
// (backend-conventions.md section 5.6: hub 401 -> Unauthenticated, hub 403 ->
// PermissionDenied). It returns the advisory user name for the message.
func (s *Server) isProviderAdmin(ctx context.Context, token string) (user string, allowed bool, err error) {
	if err := s.rejected(token); err != nil {
		return "", false, err
	}
	md, _ := metadata.FromIncomingContext(ctx)
	user = strings.Join(md.Get("x-forwarded-user"), ",")
	return user, s.access.DefaultAllowed && !slices.Contains(s.access.DeniedTokens, token), nil
}

// authorizePatch gates a hub write. The message is shaped like the API
// server's Forbidden status for a patch on the named ManagedCluster.
func (s *Server) authorizePatch(ctx context.Context, token, name string) error {
	user, allowed, err := s.isProviderAdmin(ctx, token)
	if err != nil {
		return err
	}
	if !allowed {
		return status.Errorf(codes.PermissionDenied,
			"managedclusters.cluster.open-cluster-management.io %q is forbidden: User %q cannot patch resource \"managedclusters\" in API group \"cluster.open-cluster-management.io\" at the cluster scope", name, user)
	}
	return nil
}

// rejected is the hub's 401 for a token in access.json's rejectedTokens (an
// expired or revoked OpenShift token), which the backend reports as
// Unauthenticated on any call, reads included; a merely denied token still
// reads and fails only on writes (authorizePatch).
func (s *Server) rejected(token string) error {
	if slices.Contains(s.access.RejectedTokens, token) {
		return status.Error(codes.Unauthenticated, "the hub rejected the token: Unauthorized")
	}
	return nil
}

// authorizeProviderAdmin gates a Keycloak mutation: the backend uses its own
// Keycloak credential but first checks that the caller may patch
// ManagedClusters on the hub (backend-conventions.md 5.6, "provider admin").
// The conventions fix only the code; the message text is the console's
// proposal (keycloak.md 8.2), nothing on the hub or in Keycloak produces it.
//
// The message names what the caller tried to change (`what`: "tenants")
// and then the rule, so a viewer who creates a tenant is not told
// about a resource they never touched.
func (s *Server) authorizeProviderAdmin(ctx context.Context, token, what string) error {
	user, allowed, err := s.isProviderAdmin(ctx, token)
	if err != nil {
		return err
	}
	if !allowed {
		return status.Errorf(codes.PermissionDenied,
			"User %q is not a provider admin, and only provider admins can change %s (a provider admin may patch ManagedClusters on the hub)", user, what)
	}
	return nil
}

// begin is what every authenticated RPC does first: check the caller's
// bearer token is present and not rejected, then take the hub round trip.
// It returns the token for the RBAC check a mutating RPC adds.
func (s *Server) begin(ctx context.Context) (string, error) {
	token, err := authenticate(ctx)
	if err != nil {
		return "", err
	}
	if err := s.rejected(token); err != nil {
		return "", err
	}
	return token, s.wait(ctx)
}

// wait simulates the hub round trip and honours cancellation like a real call.
func (s *Server) wait(ctx context.Context) error {
	if s.Latency <= 0 {
		return ctx.Err()
	}
	select {
	case <-time.After(s.Latency):
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

// Ping implements AiGrid. The real server answers "Hello, <name>!".
func (s *Server) Ping(ctx context.Context, req *aigriddraftv1.PingRequest) (*aigriddraftv1.PingResponse, error) {
	if err := s.wait(ctx); err != nil {
		return nil, err
	}
	return &aigriddraftv1.PingResponse{Message: fmt.Sprintf("Hello, %s!", req.GetName())}, nil
}

// Responses are deep copies so a caller cannot mutate the fixture state.
func cloneCluster(c *aigriddraftv1.ManagedCluster) *aigriddraftv1.ManagedCluster {
	return proto.Clone(c).(*aigriddraftv1.ManagedCluster)
}

func cloneTenant(t *aigriddraftv1.Tenant) *aigriddraftv1.Tenant {
	return proto.Clone(t).(*aigriddraftv1.Tenant)
}
