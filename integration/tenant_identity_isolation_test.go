package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	clientpkg "github.com/osac-project/ai-grid-provider-backend/client"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

type identityIsolationEnv struct {
	GRPCAddress         string
	KeycloakURL         string
	InternalKeycloakURL string
	AdminRealm          string
	TargetRealm         string
	UpstreamRealmA      string
	UpstreamRealmB      string
	AdminUsername       string
	AdminPassword       string
	BrokerClientID      string
	UpstreamClientA     string
	UpstreamClientB     string
	UpstreamSecretA     string
	UpstreamSecretB     string
	SharedClient1       string
	SharedClient2       string
	SharedSecret1       string
	SharedSecret2       string
	NoProfileClient     string
	NoProfileSecret     string
	ProfileClient       string
	ProfileSecret       string
	CallbackPort        int
}

func TestTenantBrokerIdentityIsolation(t *testing.T) {
	env := requireIdentityIsolationInputs(t)
	ctx := context.Background()
	client := newIdentityIsolationClient(t, env.GRPCAddress)
	admin := newIdentityIsolationAdmin(t, env)
	brokerLoginAdmin = admin

	for _, realm := range []string{env.TargetRealm, env.UpstreamRealmA, env.UpstreamRealmB} {
		if err := requireRealmAbsent(t, admin, realm); err != nil {
			t.Fatalf("refusing to use realm %q: %v", realm, err)
		}
	}
	createTargetRealm(t, admin, env)
	t.Cleanup(func() { deleteRealmBestEffort(admin, env.TargetRealm) })
	createUpstreamRealmA(t, admin, env)
	t.Cleanup(func() { deleteRealmBestEffort(admin, env.UpstreamRealmA) })
	createUpstreamRealmB(t, admin, env)
	t.Cleanup(func() { deleteRealmBestEffort(admin, env.UpstreamRealmB) })

	t.Run("two companies use the same profile username", func(t *testing.T) {
		tenantA := createIdentityIsolationTenant(t, ctx, client, env, "acme", env.UpstreamRealmA, env.UpstreamClientA, env.UpstreamSecretA, "openid profile email")
		tenantB := createIdentityIsolationTenant(t, ctx, client, env, "globex", env.UpstreamRealmB, env.UpstreamClientB, env.UpstreamSecretB, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenantA)
		defer deleteIdentityIsolationTenant(t, ctx, client, tenantB)

		identityA := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "acme", "alice", "password-a")
		identityB := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "globex", "alice", "password-b")

		if identityA.Subject == identityB.Subject {
			t.Fatalf("broker subjects are equal: %q", identityA.Subject)
		}
		if identityA.LocalUsername == identityB.LocalUsername {
			t.Fatalf("local usernames are equal: %q", identityA.LocalUsername)
		}
		assertGeneratedLocalUsername(t, identityA.LocalUsername)
		assertGeneratedLocalUsername(t, identityB.LocalUsername)
		if identityA.PreferredUsername != "alice" || identityB.PreferredUsername != "alice" {
			t.Fatalf("preferred usernames = %q and %q, want alice", identityA.PreferredUsername, identityB.PreferredUsername)
		}
		if identityA.Organization != tenantA.Id || identityB.Organization != tenantB.Id {
			t.Fatalf("organizations = %q and %q, want %q and %q", identityA.Organization, identityB.Organization, tenantA.Id, tenantB.Id)
		}
		if len(identityA.Groups) != 0 || len(identityB.Groups) != 0 {
			t.Fatalf("groups = %v and %v, want none", identityA.Groups, identityB.Groups)
		}
		if len(identityA.Roles) != 0 || len(identityB.Roles) != 0 {
			t.Fatalf("roles = %v and %v, want none", identityA.Roles, identityB.Roles)
		}
		assertTenantMemberMatchesIdentity(t, ctx, client, tenantA.Id, identityA)
		assertTenantMemberMatchesIdentity(t, ctx, client, tenantB.Id, identityB)
	})

	t.Run("repeat login and upstream rename keep identity stable", func(t *testing.T) {
		tenant := createIdentityIsolationTenant(t, ctx, client, env, "rename-org", env.UpstreamRealmA, env.UpstreamClientA, env.UpstreamSecretA, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenant)

		first := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "rename-org", "rename-alice", "password-rename-alice")
		renameUpstreamUser(t, admin, env.UpstreamRealmA, "rename-alice", "alice-renamed")
		second := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "rename-org", "rename-alice", "password-rename-alice")

		if first.Subject != second.Subject || first.LocalUsername != second.LocalUsername || first.ProviderSubject != second.ProviderSubject {
			t.Fatalf("identity changed: first=%+v second=%+v", first, second)
		}
		if second.PreferredUsername != "alice-renamed" {
			t.Fatalf("preferred username after rename = %q, want alice-renamed", second.PreferredUsername)
		}
		account, err := adminUserByEmail(t, second.Email)
		if err != nil {
			t.Fatalf("read renamed broker account by email %q: %v", second.Email, err)
		}
		if account.ID != second.Subject || account.Username != second.LocalUsername {
			t.Fatalf("renamed login produced an extra account: identity=%+v account=%+v", second, account)
		}
	})

	t.Run("without the local username mapper the original collision returns", func(t *testing.T) {
		tenantA := createIdentityIsolationTenant(t, ctx, client, env, "baseline-a", env.UpstreamRealmA, env.UpstreamClientA, env.UpstreamSecretA, "openid profile email")
		tenantB := createIdentityIsolationTenant(t, ctx, client, env, "baseline-b", env.UpstreamRealmB, env.UpstreamClientB, env.UpstreamSecretB, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenantA)
		defer deleteIdentityIsolationTenant(t, ctx, client, tenantB)

		deleteIdentityProviderMapper(t, admin, env.TargetRealm, "baseline-a", "AI Grid local username")
		deleteIdentityProviderMapper(t, admin, env.TargetRealm, "baseline-b", "AI Grid local username")
		first := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "baseline-a", "baseline-alice", "password-baseline-a")
		if first.LocalUsername != "baseline-alice" {
			t.Fatalf("baseline local username = %q, want baseline-alice", first.LocalUsername)
		}
		second := loginViaBrokerExpectConflict(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "baseline-b", "baseline-alice", "password-baseline-b")
		if second.Subject != "" || second.LocalUsername != "" {
			t.Fatalf("missing mapper allowed a second identity: %+v", second)
		}
	})

	t.Run("missing preferred username remains optional", func(t *testing.T) {
		tenant := createIdentityIsolationTenant(t, ctx, client, env, "no-profile-org", env.UpstreamRealmA, env.NoProfileClient, env.NoProfileSecret, "openid email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenant)

		identity := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "no-profile-org", "alice-no-profile", "password-no-profile")
		if identity.PreferredUsername != "" {
			t.Fatalf("preferred username = %q, want empty", identity.PreferredUsername)
		}
		if identity.LocalUsername == "" || identity.Subject == "" {
			t.Fatalf("identity is incomplete: %+v", identity)
		}
	})

	t.Run("required profile collection accepts missing names", func(t *testing.T) {
		tenant := createIdentityIsolationTenant(t, ctx, client, env, "missing-name-org", env.UpstreamRealmA, env.ProfileClient, env.ProfileSecret, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenant)

		identity := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "missing-name-org", "alice-missing-name", "password-missing-name")
		if identity.Subject == "" {
			t.Fatalf("identity is incomplete: %+v", identity)
		}
		assertGeneratedLocalUsername(t, identity.LocalUsername)
		if identity.PreferredUsername != "Test" {
			t.Fatalf("preferred username = %q, want Test", identity.PreferredUsername)
		}
	})

	t.Run("case and separator variants stay separate", func(t *testing.T) {
		tenant := createIdentityIsolationTenant(t, ctx, client, env, "variants-org", env.UpstreamRealmA, env.UpstreamClientA, env.UpstreamSecretA, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenant)

		usernames := []string{"alice-case", "alice-1", "alice_1"}
		seenSubjects := map[string]struct{}{}
		seenUsernames := map[string]struct{}{}
		for _, username := range usernames {
			identity := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "variants-org", username, "password-"+strings.ToLower(username))
			if _, ok := seenSubjects[identity.Subject]; ok {
				t.Fatalf("duplicate broker subject for %q: %q", username, identity.Subject)
			}
			if _, ok := seenUsernames[identity.LocalUsername]; ok {
				t.Fatalf("duplicate local username for %q: %q", username, identity.LocalUsername)
			}
			seenSubjects[identity.Subject] = struct{}{}
			seenUsernames[identity.LocalUsername] = struct{}{}
		}
	})

	t.Run("the same upstream subject through two providers stays distinct", func(t *testing.T) {
		tenant1 := createIdentityIsolationTenant(t, ctx, client, env, "shared-1", env.UpstreamRealmA, env.SharedClient1, env.SharedSecret1, "openid profile email")
		tenant2 := createIdentityIsolationTenant(t, ctx, client, env, "shared-2", env.UpstreamRealmA, env.SharedClient2, env.SharedSecret2, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenant1)
		defer deleteIdentityIsolationTenant(t, ctx, client, tenant2)

		identity1 := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "shared-1", "shared-subject", "password-shared-subject")
		updateUpstreamUserEmail(t, admin, env.UpstreamRealmA, "shared-subject", "shared-2@example.com")
		identity2 := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "shared-2", "shared-subject", "password-shared-subject")

		if identity1.ProviderSubject != identity2.ProviderSubject {
			t.Fatalf("upstream subjects differ: %q and %q", identity1.ProviderSubject, identity2.ProviderSubject)
		}
		if identity1.Subject == identity2.Subject || identity1.LocalUsername == identity2.LocalUsername {
			t.Fatalf("broker identities are not distinct: %+v and %+v", identity1, identity2)
		}
	})

	t.Run("matching emails conflict without a cross-company link", func(t *testing.T) {
		tenantA := createIdentityIsolationTenant(t, ctx, client, env, "email-a", env.UpstreamRealmA, env.UpstreamClientA, env.UpstreamSecretA, "openid profile email")
		tenantB := createIdentityIsolationTenant(t, ctx, client, env, "email-b", env.UpstreamRealmB, env.UpstreamClientB, env.UpstreamSecretB, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenantA)
		defer deleteIdentityIsolationTenant(t, ctx, client, tenantB)

		first := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "email-a", "alice-shared", "password-shared")
		second := loginViaBrokerExpectConflict(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "email-b", "alice-shared", "password-shared-b")
		if second.Subject != "" || second.LocalUsername != "" {
			t.Fatalf("matching email created a second identity: %+v", second)
		}
		if first.Organization != tenantA.Id {
			t.Fatalf("first organization = %q, want %q", first.Organization, tenantA.Id)
		}
		account := getAdminUser(t, admin, env.TargetRealm, first.Subject)
		if account.ID != first.Subject || account.Username != first.LocalUsername {
			t.Fatalf("matching email changed the first account: identity=%+v account=%+v", first, account)
		}
		if len(account.FederatedIdentities) != 1 {
			t.Fatalf("matching email changed provider links: %+v", account.FederatedIdentities)
		}
		link := account.FederatedIdentities[0]
		if link.IdentityProvider != "email-a" || link.UserID != first.ProviderSubject {
			t.Fatalf("matching email changed provider link: %+v", link)
		}
	})

	t.Run("a legacy local username survives new onboarding", func(t *testing.T) {
		tenantA := createIdentityIsolationTenant(t, ctx, client, env, "legacy-a", env.UpstreamRealmA, env.UpstreamClientA, env.UpstreamSecretA, "openid profile email")
		tenantB := createIdentityIsolationTenant(t, ctx, client, env, "legacy-b", env.UpstreamRealmB, env.UpstreamClientB, env.UpstreamSecretB, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenantA)
		defer deleteIdentityIsolationTenant(t, ctx, client, tenantB)

		legacy := createLegacyLocalUser(t, admin, env.TargetRealm, "alice", "legacy@example.com")
		linkFederatedIdentity(t, admin, env.TargetRealm, legacy.ID, "legacy-a", "legacy-upstream-subject", "alice")
		identity := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "legacy-b", "legacy-alice", "password-legacy-alice")

		if identity.LocalUsername == legacy.Username {
			t.Fatalf("new company reused legacy local username %q", legacy.Username)
		}
		current := getAdminUser(t, admin, env.TargetRealm, legacy.ID)
		if current.ID != legacy.ID || current.Username != legacy.Username {
			t.Fatalf("legacy user changed: before=%+v after=%+v", legacy, current)
		}
		if len(current.FederatedIdentities) != 1 {
			t.Fatalf("legacy user federated identities = %+v, want 1", current.FederatedIdentities)
		}
		link := current.FederatedIdentities[0]
		if link.IdentityProvider != "legacy-a" || link.UserID != "legacy-upstream-subject" || link.UserName != "alice" {
			t.Fatalf("legacy provider link = %+v", link)
		}
	})

	t.Run("profile collection rejects a forged local username", func(t *testing.T) {
		tenant := createIdentityIsolationTenant(t, ctx, client, env, "forged-profile-org", env.UpstreamRealmA, env.NoProfileClient, env.NoProfileSecret, "openid email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenant)
		otherTenant := createIdentityIsolationTenant(t, ctx, client, env, "forged-other-org", env.UpstreamRealmB, env.UpstreamClientB, env.UpstreamSecretB, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, otherTenant)

		other := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "forged-other-org", "forged-other-alice", "password-forged-other-alice")

		rejected := loginViaBrokerWithForgedUsername(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "forged-profile-org", "alice-no-profile", "password-no-profile")
		if rejected.Subject != "" || rejected.LocalUsername != "" {
			t.Fatalf("forged local username created an identity: %+v", rejected)
		}
		if users := adminUsersByUsername(t, admin, env.TargetRealm, "forged-local-username"); len(users) != 0 {
			t.Fatalf("forged local username exists in target realm: %+v", users)
		}
		otherAccount := getAdminUser(t, admin, env.TargetRealm, other.Subject)
		if otherAccount.ID != other.Subject || otherAccount.Username != other.LocalUsername {
			t.Fatalf("forged username changed another company account: identity=%+v account=%+v", other, otherAccount)
		}
		if len(otherAccount.FederatedIdentities) != 1 || otherAccount.FederatedIdentities[0].IdentityProvider != "forged-other-org" || otherAccount.FederatedIdentities[0].UserID != other.ProviderSubject {
			t.Fatalf("forged username changed another company provider link: %+v", otherAccount.FederatedIdentities)
		}
	})
}

func TestRequireRealmAbsent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		wantError  bool
	}{
		{name: "missing realm", statusCode: http.StatusNotFound},
		{name: "existing realm", statusCode: http.StatusOK, wantError: true},
		{name: "admin API failure", statusCode: http.StatusForbidden, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var method, path string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte("realm response"))
			}))
			defer server.Close()
			admin := adminContext{BaseURL: server.URL, HTTPClient: server.Client()}
			err := requireRealmAbsent(t, admin, "identity-isolation-target")
			if (err != nil) != tc.wantError {
				t.Fatalf("requireRealmAbsent() error = %v, wantError %t", err, tc.wantError)
			}
			if method != http.MethodGet || path != "/admin/realms/identity-isolation-target" {
				t.Fatalf("realm preflight request = %s %s", method, path)
			}
		})
	}
}

func TestFormActionBelongsToRealm(t *testing.T) {
	issuer := "http://keycloak.example/realms/target"
	for _, tc := range []struct {
		action string
		want   bool
	}{
		{action: "http://keycloak.example/realms/target/login-actions/required-action", want: true},
		{action: "/realms/target/login-actions/required-action", want: true},
		{action: "http://keycloak.example/realms/upstream/login-actions/required-action", want: false},
		{action: "http://other.example/realms/target/login-actions/required-action", want: false},
		{action: "http://keycloak.example/realms/target-other/login-actions/required-action", want: false},
	} {
		if got := formActionBelongsToRealm(tc.action, issuer); got != tc.want {
			t.Errorf("formActionBelongsToRealm(%q, %q) = %t, want %t", tc.action, issuer, got, tc.want)
		}
	}
}

func requireIdentityIsolationInputs(t *testing.T) identityIsolationEnv {
	t.Helper()
	address := os.Getenv("AI_GRID_GRPC_ADDRESS")
	if address == "" {
		t.Skip("AI_GRID_GRPC_ADDRESS is not set; run via make test-tenant-identity-isolation")
	}
	env := identityIsolationEnv{
		GRPCAddress:         address,
		KeycloakURL:         os.Getenv("KEYCLOAK_URL"),
		InternalKeycloakURL: os.Getenv("IDENTITY_ISOLATION_INTERNAL_KEYCLOAK_URL"),
		AdminRealm:          os.Getenv("KEYCLOAK_ADMIN_REALM"),
		TargetRealm:         os.Getenv("IDENTITY_ISOLATION_TARGET_REALM"),
		UpstreamRealmA:      os.Getenv("IDENTITY_ISOLATION_UPSTREAM_REALM_A"),
		UpstreamRealmB:      os.Getenv("IDENTITY_ISOLATION_UPSTREAM_REALM_B"),
		AdminUsername:       os.Getenv("KEYCLOAK_ADMIN_USERNAME"),
		AdminPassword:       os.Getenv("KEYCLOAK_ADMIN_PASSWORD"),
		BrokerClientID:      os.Getenv("IDENTITY_ISOLATION_BROKER_CLIENT_ID"),
		UpstreamClientA:     os.Getenv("IDENTITY_ISOLATION_UPSTREAM_CLIENT_A"),
		UpstreamClientB:     os.Getenv("IDENTITY_ISOLATION_UPSTREAM_CLIENT_B"),
		UpstreamSecretA:     os.Getenv("IDENTITY_ISOLATION_UPSTREAM_SECRET_A"),
		UpstreamSecretB:     os.Getenv("IDENTITY_ISOLATION_UPSTREAM_SECRET_B"),
		SharedClient1:       os.Getenv("IDENTITY_ISOLATION_SHARED_CLIENT_1"),
		SharedClient2:       os.Getenv("IDENTITY_ISOLATION_SHARED_CLIENT_2"),
		SharedSecret1:       os.Getenv("IDENTITY_ISOLATION_SHARED_SECRET_1"),
		SharedSecret2:       os.Getenv("IDENTITY_ISOLATION_SHARED_SECRET_2"),
		NoProfileClient:     os.Getenv("IDENTITY_ISOLATION_NO_PROFILE_CLIENT"),
		NoProfileSecret:     os.Getenv("IDENTITY_ISOLATION_NO_PROFILE_SECRET"),
		ProfileClient:       os.Getenv("IDENTITY_ISOLATION_PROFILE_CLIENT"),
		ProfileSecret:       os.Getenv("IDENTITY_ISOLATION_PROFILE_SECRET"),
	}
	missing := []string{}
	if env.KeycloakURL == "" {
		missing = append(missing, "KEYCLOAK_URL")
	}
	if env.InternalKeycloakURL == "" {
		missing = append(missing, "IDENTITY_ISOLATION_INTERNAL_KEYCLOAK_URL")
	}
	if env.AdminRealm == "" {
		missing = append(missing, "KEYCLOAK_ADMIN_REALM")
	}
	if env.TargetRealm == "" {
		missing = append(missing, "IDENTITY_ISOLATION_TARGET_REALM")
	}
	if env.UpstreamRealmA == "" {
		missing = append(missing, "IDENTITY_ISOLATION_UPSTREAM_REALM_A")
	}
	if env.UpstreamRealmB == "" {
		missing = append(missing, "IDENTITY_ISOLATION_UPSTREAM_REALM_B")
	}
	if env.AdminUsername == "" {
		missing = append(missing, "KEYCLOAK_ADMIN_USERNAME")
	}
	if env.AdminPassword == "" {
		missing = append(missing, "KEYCLOAK_ADMIN_PASSWORD")
	}
	if env.BrokerClientID == "" {
		missing = append(missing, "IDENTITY_ISOLATION_BROKER_CLIENT_ID")
	}
	if env.UpstreamClientA == "" {
		missing = append(missing, "IDENTITY_ISOLATION_UPSTREAM_CLIENT_A")
	}
	if env.UpstreamClientB == "" {
		missing = append(missing, "IDENTITY_ISOLATION_UPSTREAM_CLIENT_B")
	}
	if env.UpstreamSecretA == "" {
		missing = append(missing, "IDENTITY_ISOLATION_UPSTREAM_SECRET_A")
	}
	if env.UpstreamSecretB == "" {
		missing = append(missing, "IDENTITY_ISOLATION_UPSTREAM_SECRET_B")
	}
	if env.SharedClient1 == "" {
		missing = append(missing, "IDENTITY_ISOLATION_SHARED_CLIENT_1")
	}
	if env.SharedClient2 == "" {
		missing = append(missing, "IDENTITY_ISOLATION_SHARED_CLIENT_2")
	}
	if env.SharedSecret1 == "" {
		missing = append(missing, "IDENTITY_ISOLATION_SHARED_SECRET_1")
	}
	if env.SharedSecret2 == "" {
		missing = append(missing, "IDENTITY_ISOLATION_SHARED_SECRET_2")
	}
	if env.NoProfileClient == "" {
		missing = append(missing, "IDENTITY_ISOLATION_NO_PROFILE_CLIENT")
	}
	if env.NoProfileSecret == "" {
		missing = append(missing, "IDENTITY_ISOLATION_NO_PROFILE_SECRET")
	}
	if env.ProfileClient == "" {
		missing = append(missing, "IDENTITY_ISOLATION_PROFILE_CLIENT")
	}
	if env.ProfileSecret == "" {
		missing = append(missing, "IDENTITY_ISOLATION_PROFILE_SECRET")
	}
	callbackPortInput := os.Getenv("IDENTITY_ISOLATION_CALLBACK_PORT")
	if callbackPortInput == "" {
		missing = append(missing, "IDENTITY_ISOLATION_CALLBACK_PORT")
	}
	if len(missing) > 0 {
		t.Skipf("missing identity-isolation runtime inputs: %s", strings.Join(missing, ", "))
	}
	callbackPort, err := strconv.Atoi(callbackPortInput)
	if err != nil {
		t.Fatalf("parse IDENTITY_ISOLATION_CALLBACK_PORT: %v", err)
	}
	env.CallbackPort = callbackPort
	brokerLoginCallbackPort = callbackPort
	return env
}

func newIdentityIsolationClient(t *testing.T, address string) *clientpkg.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := clientpkg.NewBuilder().SetAddress(address).SetPlaintext(true).Build(ctx)
	if err != nil {
		t.Fatalf("build backend client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func newIdentityIsolationAdmin(t *testing.T, env identityIsolationEnv) adminContext {
	t.Helper()
	tokenURL := strings.TrimRight(env.KeycloakURL, "/") + "/realms/" + env.AdminRealm + "/protocol/openid-connect/token"
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", "admin-cli")
	form.Set("username", env.AdminUsername)
	form.Set("password", env.AdminPassword)
	resp, err := http.PostForm(tokenURL, form)
	if err != nil {
		t.Fatalf("request Keycloak admin token: %v", err)
	}
	defer resp.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		t.Fatalf("decode Keycloak admin token: %v", err)
	}
	if token.AccessToken == "" {
		t.Fatal("Keycloak admin token is empty")
	}
	return adminContext{
		BaseURL:    strings.TrimRight(env.KeycloakURL, "/"),
		AdminRealm: env.AdminRealm,
		Realm:      env.TargetRealm,
		Username:   env.AdminUsername,
		Password:   env.AdminPassword,
		Token:      token.AccessToken,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func requireRealmAbsent(t *testing.T, admin adminContext, realm string) error {
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm)
	resp, body := admin.request(t, http.MethodGet, endpoint, nil)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil
	case http.StatusOK:
		return fmt.Errorf("Keycloak realm %q already exists", realm)
	default:
		return fmt.Errorf("check Keycloak realm %q: status %d: %s", realm, resp.StatusCode, body)
	}
}

func issuerURL(baseURL, realm string) string {
	return strings.TrimRight(baseURL, "/") + "/realms/" + realm
}

func createIdentityIsolationTenant(t *testing.T, ctx context.Context, client *clientpkg.Client, env identityIsolationEnv, name, upstreamRealm, clientID, secretName, defaultScopes string) *aigridv1.Tenant {
	t.Helper()
	browserIssuer := issuerURL(env.KeycloakURL, upstreamRealm)
	internalIssuer := issuerURL(env.InternalKeycloakURL, upstreamRealm)
	tenant, err := client.CreateTenant(ctx, &aigridv1.Tenant{
		Metadata: &aigridv1.Metadata{Name: name},
		Spec: &aigridv1.TenantSpec{
			// An organization needs a domain. The users' emails are at
			// example.com, so this one routes none of them: they sign in
			// through the broker hint, as before.
			Domains: []string{name + ".example.com"},
			IdentityProvider: &aigridv1.IdentityProvider{
				Title: name,
				Config: &aigridv1.OidcConfig{
					ClientId:               clientID,
					ClientSecretSecretName: secretName,
					Issuer:                 browserIssuer,
					AuthorizationUrl:       browserIssuer + "/protocol/openid-connect/auth",
					TokenUrl:               internalIssuer + "/protocol/openid-connect/token",
					JwksUrl:                internalIssuer + "/protocol/openid-connect/certs",
					DefaultScopes:          defaultScopes,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("create tenant %s: %v", name, err)
	}
	return tenant
}

func assertTenantMemberMatchesIdentity(t *testing.T, ctx context.Context, client *clientpkg.Client, tenantID string, identity brokerIdentity) {
	t.Helper()
	members, err := client.ListTenantMembers(ctx, tenantID)
	if err != nil {
		t.Fatalf("list members for tenant %s: %v", tenantID, err)
	}
	if len(members) != 1 {
		t.Fatalf("tenant %s members = %d, want 1", tenantID, len(members))
	}
	member := members[0]
	if member.GetId() != identity.Subject || member.GetUsername() != identity.LocalUsername {
		t.Fatalf("tenant %s member = %+v, want id %q and local username %q", tenantID, member, identity.Subject, identity.LocalUsername)
	}
	if member.GetPreferredUsername() != identity.PreferredUsername {
		t.Fatalf("tenant %s preferred username = %q, want %q", tenantID, member.GetPreferredUsername(), identity.PreferredUsername)
	}
}

func assertGeneratedLocalUsername(t *testing.T, username string) {
	t.Helper()
	if _, err := uuid.Parse(username); err != nil {
		t.Fatalf("local username %q is not a UUID: %v", username, err)
	}
}

func deleteIdentityIsolationTenant(t *testing.T, ctx context.Context, client *clientpkg.Client, tenant *aigridv1.Tenant) {
	t.Helper()
	if err := client.DeleteTenant(ctx, tenant.Id, tenant.Metadata.Version); err != nil {
		t.Errorf("delete tenant %s: %v", tenant.Id, err)
	}
}

type keycloakRealm struct {
	Realm                string                `json:"realm"`
	Enabled              bool                  `json:"enabled"`
	OrganizationsEnabled bool                  `json:"organizationsEnabled,omitempty"`
	ClientScopes         []keycloakClientScope `json:"clientScopes,omitempty"`
	Clients              []keycloakClient      `json:"clients,omitempty"`
	Users                []keycloakUser        `json:"users,omitempty"`
}

type keycloakClientScope struct {
	ID              string                   `json:"id,omitempty"`
	Name            string                   `json:"name"`
	Description     string                   `json:"description,omitempty"`
	Protocol        string                   `json:"protocol"`
	Attributes      map[string]string        `json:"attributes,omitempty"`
	ProtocolMappers []keycloakProtocolMapper `json:"protocolMappers,omitempty"`
}

type keycloakProtocolMapper struct {
	ID             string            `json:"id,omitempty"`
	Name           string            `json:"name"`
	Protocol       string            `json:"protocol"`
	ProtocolMapper string            `json:"protocolMapper"`
	Config         map[string]string `json:"config,omitempty"`
}

func keycloakUserAttributeMapper(name, attribute, claim string, multivalued bool) keycloakProtocolMapper {
	return keycloakProtocolMapper{
		Name:           name,
		Protocol:       "openid-connect",
		ProtocolMapper: "oidc-usermodel-attribute-mapper",
		Config: map[string]string{
			"user.attribute":       attribute,
			"claim.name":           claim,
			"jsonType.label":       "String",
			"multivalued":          strconv.FormatBool(multivalued),
			"id.token.claim":       "true",
			"userinfo.token.claim": "true",
			"access.token.claim":   "true",
		},
	}
}

type keycloakClient struct {
	ClientID                  string                   `json:"clientId"`
	Name                      string                   `json:"name,omitempty"`
	Enabled                   bool                     `json:"enabled"`
	Protocol                  string                   `json:"protocol"`
	PublicClient              bool                     `json:"publicClient"`
	ClientAuthenticatorType   string                   `json:"clientAuthenticatorType,omitempty"`
	Secret                    string                   `json:"secret,omitempty"`
	StandardFlowEnabled       bool                     `json:"standardFlowEnabled"`
	DirectAccessGrantsEnabled bool                     `json:"directAccessGrantsEnabled"`
	RedirectUris              []string                 `json:"redirectUris"`
	DefaultClientScopes       []string                 `json:"defaultClientScopes,omitempty"`
	ProtocolMappers           []keycloakProtocolMapper `json:"protocolMappers,omitempty"`
}

type keycloakUser struct {
	Username    string               `json:"username"`
	Enabled     bool                 `json:"enabled"`
	Email       string               `json:"email,omitempty"`
	FirstName   string               `json:"firstName,omitempty"`
	LastName    string               `json:"lastName,omitempty"`
	Credentials []keycloakCredential `json:"credentials,omitempty"`
}

type keycloakCredential struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}

func createTargetRealm(t *testing.T, admin adminContext, env identityIsolationEnv) {
	t.Helper()
	realm := keycloakRealm{
		Realm:                env.TargetRealm,
		Enabled:              true,
		OrganizationsEnabled: true,
		Clients: []keycloakClient{{
			ClientID:                  env.BrokerClientID,
			Name:                      "Identity isolation broker client",
			Enabled:                   true,
			Protocol:                  "openid-connect",
			PublicClient:              true,
			StandardFlowEnabled:       true,
			DirectAccessGrantsEnabled: false,
			RedirectUris:              []string{fmt.Sprintf("http://127.0.0.1:%d/callback", env.CallbackPort)},
			DefaultClientScopes:       []string{"basic", "profile", "email", "roles", "web-origins"},
			ProtocolMappers: []keycloakProtocolMapper{
				keycloakUserAttributeMapper("MaaS org id", "maas_org_id", "org_id", false),
				keycloakUserAttributeMapper("MaaS org name", "maas_org_name", "org_name", false),
				keycloakUserAttributeMapper("MaaS groups", "maas_groups", "groups", true),
				keycloakUserAttributeMapper("MaaS roles", "maas_roles", "roles", true),
			},
		}},
	}
	postRealm(t, admin, realm)
	requireUniqueBrokerUserCreation(t, admin, env.TargetRealm)
	configureUserProfile(t, admin, env.TargetRealm)
	configureProfileScope(t, admin, env.TargetRealm, "maas_preferred_username")
}

func configureUserProfile(t *testing.T, admin adminContext, realm string) {
	t.Helper()
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/users/profile"
	resp, body := admin.request(t, http.MethodGet, endpoint, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read user profile: status %d body %s", resp.StatusCode, body)
	}
	var profile map[string]any
	if err := json.Unmarshal(body, &profile); err != nil {
		t.Fatalf("decode user profile: %v", err)
	}
	profile["unmanagedAttributePolicy"] = "ADMIN_EDIT"
	attributes, ok := profile["attributes"].([]any)
	if !ok {
		t.Fatalf("user profile attributes = %T, want array", profile["attributes"])
	}
	var username map[string]any
	for _, item := range attributes {
		attribute, ok := item.(map[string]any)
		if ok && attribute["name"] == "username" {
			username = attribute
			break
		}
	}
	if username == nil {
		t.Fatal("user profile has no username attribute")
	}
	username["required"] = map[string]any{}
	username["permissions"] = map[string][]string{
		"view": {"admin"},
		"edit": {"admin"},
	}
	body, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal user profile configuration: %v", err)
	}
	resp, responseBody := admin.request(t, http.MethodPut, endpoint, body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		t.Fatalf("update user profile: status %d body %s", resp.StatusCode, responseBody)
	}
}

func requireUniqueBrokerUserCreation(t *testing.T, admin adminContext, realm string) {
	t.Helper()
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/authentication/flows/" + url.PathEscape("User creation or linking") + "/executions"
	resp, body := admin.request(t, http.MethodGet, endpoint, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list broker user-creation executions: status %d body %s", resp.StatusCode, body)
	}
	var executions []map[string]any
	if err := json.Unmarshal(body, &executions); err != nil {
		t.Fatalf("decode broker user-creation executions: %v", err)
	}
	var uniqueUserExecution map[string]any
	for _, execution := range executions {
		if execution["providerId"] == "idp-create-user-if-unique" {
			if uniqueUserExecution != nil {
				t.Fatal("user-creation flow has multiple idp-create-user-if-unique executions")
			}
			uniqueUserExecution = execution
		}
	}
	if uniqueUserExecution == nil {
		t.Fatal("user-creation flow has no idp-create-user-if-unique execution")
	}
	if uniqueUserExecution["requirement"] == "REQUIRED" {
		return
	}
	uniqueUserExecution["requirement"] = "REQUIRED"
	update, err := json.Marshal(uniqueUserExecution)
	if err != nil {
		t.Fatalf("marshal user-creation execution: %v", err)
	}
	resp, body = admin.request(t, http.MethodPut, endpoint, update)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		t.Fatalf("require unique broker user: status %d body %s", resp.StatusCode, body)
	}
}

func configureProfileScope(t *testing.T, admin adminContext, realm, userAttribute string) {
	t.Helper()
	scopesEndpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes"
	resp, body := admin.request(t, http.MethodGet, scopesEndpoint, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list client scopes: status %d body %s", resp.StatusCode, body)
	}
	var scopes []keycloakClientScope
	if err := json.Unmarshal(body, &scopes); err != nil {
		t.Fatalf("decode client scopes response: %v", err)
	}
	var profileScopeID string
	for _, scope := range scopes {
		if scope.Name == "profile" {
			profileScopeID = scope.ID
			break
		}
	}
	if profileScopeID == "" {
		t.Fatal("profile client scope not found")
	}
	mappersEndpoint := scopesEndpoint + "/" + url.PathEscape(profileScopeID) + "/protocol-mappers/models"
	resp, body = admin.request(t, http.MethodGet, mappersEndpoint, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list profile mappers: status %d body %s", resp.StatusCode, body)
	}
	var mappers []keycloakProtocolMapper
	if err := json.Unmarshal(body, &mappers); err != nil {
		t.Fatalf("decode profile mappers response: %v", err)
	}
	var usernameMapper *keycloakProtocolMapper
	for i := range mappers {
		mapper := &mappers[i]
		if mapper.Name != "username" || mapper.Config["claim.name"] != "preferred_username" {
			continue
		}
		if usernameMapper != nil {
			t.Fatal("multiple profile username mappers found")
		}
		usernameMapper = mapper
	}
	if usernameMapper == nil {
		t.Fatal("profile username mapper not found")
	}
	if usernameMapper.ProtocolMapper == "oidc-usermodel-attribute-mapper" && usernameMapper.Config["user.attribute"] == userAttribute {
		return
	}
	usernameMapper.ProtocolMapper = "oidc-usermodel-attribute-mapper"
	usernameMapper.Config["user.attribute"] = userAttribute
	mapperBody, err := json.Marshal(usernameMapper)
	if err != nil {
		t.Fatalf("marshal profile username mapper: %v", err)
	}
	updateURL := mappersEndpoint + "/" + url.PathEscape(usernameMapper.ID)
	updateResp, _ := admin.request(t, http.MethodPut, updateURL, mapperBody)
	if updateResp.StatusCode != http.StatusNoContent {
		responseBody, _ := io.ReadAll(updateResp.Body)
		t.Fatalf("update profile username mapper: status %d body %s", updateResp.StatusCode, responseBody)
	}
}

func createUpstreamRealmA(t *testing.T, admin adminContext, env identityIsolationEnv) {
	t.Helper()
	realm := keycloakRealm{
		Realm:   env.UpstreamRealmA,
		Enabled: true,
		Clients: []keycloakClient{
			newUpstreamClient(env.UpstreamClientA, env.UpstreamSecretA, true),
			newUpstreamClient(env.SharedClient1, env.SharedSecret1, true),
			newUpstreamClient(env.SharedClient2, env.SharedSecret2, true),
			newUpstreamClient(env.NoProfileClient, env.NoProfileSecret, false),
			newUpstreamClient(env.ProfileClient, env.ProfileSecret, true),
		},
		Users: []keycloakUser{
			newUpstreamUser("alice", "alice-a@example.com", "password-a"),
			newUpstreamUser("baseline-alice", "baseline-a@example.com", "password-baseline-a"),
			newUpstreamUser("rename-alice", "rename-alice@example.com", "password-rename-alice"),
			newUpstreamUser("alice-case", "alice-case@example.com", "password-alice-case"),
			newUpstreamUser("alice-1", "alice-sep1@example.com", "password-alice-1"),
			newUpstreamUser("alice_1", "alice-sep2@example.com", "password-alice_1"),
			newUpstreamUser("alice-no-profile", "alice-no-profile@example.com", "password-no-profile"),
			newUpstreamUserWithoutName("alice-missing-name", "alice-missing-name@example.com", "password-missing-name"),
			newUpstreamUser("alice-shared", "shared@example.com", "password-shared"),
			newUpstreamUser("alice-profile", "alice-profile@example.com", "password-profile"),
			newUpstreamUserWithoutEmail("shared-subject", "password-shared-subject"),
		},
	}
	postRealm(t, admin, realm)
	configureProfileScope(t, admin, env.UpstreamRealmA, "firstName")
}

func createUpstreamRealmB(t *testing.T, admin adminContext, env identityIsolationEnv) {
	t.Helper()
	realm := keycloakRealm{
		Realm:   env.UpstreamRealmB,
		Enabled: true,
		Clients: []keycloakClient{
			newUpstreamClient(env.UpstreamClientB, env.UpstreamSecretB, true),
		},
		Users: []keycloakUser{
			newUpstreamUser("alice", "alice-b@example.com", "password-b"),
			newUpstreamUser("baseline-alice", "baseline-b@example.com", "password-baseline-b"),
			newUpstreamUser("alice-shared", "shared@example.com", "password-shared-b"),
			newUpstreamUser("legacy-alice", "legacy-b-alice@example.com", "password-legacy-alice"),
			newUpstreamUser("forged-other-alice", "forged-other-alice@example.com", "password-forged-other-alice"),
		},
	}
	postRealm(t, admin, realm)
	configureProfileScope(t, admin, env.UpstreamRealmB, "firstName")
}

func newUpstreamClient(clientID, secret string, includeProfile bool) keycloakClient {
	scopes := []string{"basic", "email", "roles", "web-origins"}
	if includeProfile {
		scopes = append([]string{"profile"}, scopes...)
	}
	return keycloakClient{
		ClientID:                  clientID,
		Name:                      "Identity isolation upstream client",
		Enabled:                   true,
		Protocol:                  "openid-connect",
		PublicClient:              false,
		ClientAuthenticatorType:   "client-secret",
		Secret:                    secret,
		StandardFlowEnabled:       true,
		DirectAccessGrantsEnabled: false,
		RedirectUris:              []string{"*"},
		DefaultClientScopes:       scopes,
	}
}

func newUpstreamUser(username, email, password string) keycloakUser {
	return keycloakUser{
		Username:  username,
		FirstName: username,
		Enabled:   true,
		Email:     email,
		Credentials: []keycloakCredential{{
			Type:  "password",
			Value: password,
		}},
	}
}

func newUpstreamUserWithoutEmail(username, password string) keycloakUser {
	return newUpstreamUser(username, "", password)
}

func newUpstreamUserWithoutName(username, email, password string) keycloakUser {
	return keycloakUser{
		Username: username,
		Enabled:  true,
		Email:    email,
		Credentials: []keycloakCredential{{
			Type:  "password",
			Value: password,
		}},
	}
}

func postRealm(t *testing.T, admin adminContext, realm keycloakRealm) {
	t.Helper()
	body, err := json.Marshal(realm)
	if err != nil {
		t.Fatalf("marshal realm %s: %v", realm.Realm, err)
	}
	endpoint := admin.BaseURL + "/admin/realms"
	resp, _ := admin.request(t, http.MethodPost, endpoint, body)
	if resp.StatusCode != http.StatusCreated {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("create realm %s: status %d body %s", realm.Realm, resp.StatusCode, responseBody)
	}
}

func deleteRealmBestEffort(admin adminContext, realm string) {
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm)
	req, err := http.NewRequest(http.MethodDelete, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func renameUpstreamUser(t *testing.T, admin adminContext, realm, currentUsername, newUsername string) {
	t.Helper()
	user := getAdminUser(t, admin, realm, adminUserByUsername(t, admin, realm, currentUsername).ID)
	user.FirstName = newUsername
	body, err := json.Marshal(user)
	if err != nil {
		t.Fatalf("marshal renamed user: %v", err)
	}
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/users/" + url.PathEscape(user.ID)
	resp, responseBody := admin.request(t, http.MethodPut, endpoint, body)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rename user %s: status %d body %s", currentUsername, resp.StatusCode, responseBody)
	}
	updated := getAdminUser(t, admin, realm, user.ID)
	if updated.FirstName != newUsername {
		t.Fatalf("renamed user first name = %q, want %q", updated.FirstName, newUsername)
	}
}

func updateUpstreamUserEmail(t *testing.T, admin adminContext, realm, username, email string) {
	t.Helper()
	user := getAdminUser(t, admin, realm, adminUserByUsername(t, admin, realm, username).ID)
	user.Email = email
	body, err := json.Marshal(user)
	if err != nil {
		t.Fatalf("marshal updated user email: %v", err)
	}
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/users/" + url.PathEscape(user.ID)
	resp, responseBody := admin.request(t, http.MethodPut, endpoint, body)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("update user email for %s: status %d body %s", username, resp.StatusCode, responseBody)
	}
}

func deleteIdentityProviderMapper(t *testing.T, admin adminContext, realm, providerAlias, mapperName string) {
	t.Helper()
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/identity-provider/instances/" + url.PathEscape(providerAlias) + "/mappers"
	resp, body := admin.request(t, http.MethodGet, endpoint, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list identity provider mappers: status %d body %s", resp.StatusCode, body)
	}
	var mappers []keycloakProtocolMapper
	if err := json.Unmarshal(body, &mappers); err != nil {
		t.Fatalf("decode identity provider mappers: %v", err)
	}
	for _, mapper := range mappers {
		if mapper.Name != mapperName {
			continue
		}
		deleteResp, _ := admin.request(t, http.MethodDelete, endpoint+"/"+url.PathEscape(mapper.ID), nil)
		if deleteResp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete identity provider mapper %s: status %d", mapperName, deleteResp.StatusCode)
		}
		return
	}
	t.Fatalf("identity provider mapper %s not found", mapperName)
}

func adminUserByUsername(t *testing.T, admin adminContext, realm, username string) adminUser {
	t.Helper()
	users := adminUsersByUsername(t, admin, realm, username)
	if len(users) != 1 {
		t.Fatalf("user search for %s returned %d users, want 1", username, len(users))
	}
	return users[0]
}

func adminUsersByUsername(t *testing.T, admin adminContext, realm, username string) []adminUser {
	t.Helper()
	endpoint := fmt.Sprintf("%s/admin/realms/%s/users?username=%s&exact=true",
		admin.BaseURL, url.PathEscape(realm), url.QueryEscape(username))
	resp, body := admin.request(t, http.MethodGet, endpoint, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search user %s: status %d body %s", username, resp.StatusCode, body)
	}
	var users []adminUser
	if err := json.Unmarshal(body, &users); err != nil {
		t.Fatalf("decode user search response: %v", err)
	}
	return users
}

func createLegacyLocalUser(t *testing.T, admin adminContext, realm, username, email string) adminUser {
	t.Helper()
	body, err := json.Marshal(keycloakUser{
		Username: username,
		Enabled:  true,
		Email:    email,
	})
	if err != nil {
		t.Fatalf("marshal legacy user: %v", err)
	}
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/users"
	resp, responseBody := admin.request(t, http.MethodPost, endpoint, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create legacy user: status %d body %s", resp.StatusCode, responseBody)
	}
	return adminUserByUsername(t, admin, realm, username)
}

func linkFederatedIdentity(t *testing.T, admin adminContext, realm, userID, providerAlias, providerSubject, providerUsername string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"identityProvider": providerAlias,
		"userId":           providerSubject,
		"userName":         providerUsername,
	})
	if err != nil {
		t.Fatalf("marshal federated identity link: %v", err)
	}
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/users/" + url.PathEscape(userID) + "/federated-identity/" + url.PathEscape(providerAlias)
	resp, responseBody := admin.request(t, http.MethodPost, endpoint, body)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		t.Fatalf("create federated identity link: status %d body %s", resp.StatusCode, responseBody)
	}
}

func getAdminUser(t *testing.T, admin adminContext, realm, userID string) adminUser {
	t.Helper()
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/users/" + url.PathEscape(userID)
	resp, body := admin.request(t, http.MethodGet, endpoint, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get user %s: status %d body %s", userID, resp.StatusCode, body)
	}
	var user adminUser
	if err := json.Unmarshal(body, &user); err != nil {
		t.Fatalf("decode user response: %v", err)
	}
	return user
}
