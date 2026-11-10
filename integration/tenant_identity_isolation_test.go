package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

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

	createTargetRealm(t, admin, env)
	createUpstreamRealmA(t, admin, env)
	createUpstreamRealmB(t, admin, env)
	t.Cleanup(func() {
		deleteRealmBestEffort(admin, env.UpstreamRealmB)
		deleteRealmBestEffort(admin, env.UpstreamRealmA)
		deleteRealmBestEffort(admin, env.TargetRealm)
	})

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
		if identityA.PreferredUsername != "alice" || identityB.PreferredUsername != "alice" {
			t.Fatalf("preferred usernames = %q and %q, want alice", identityA.PreferredUsername, identityB.PreferredUsername)
		}
		if identityA.Organization != tenantA.Id || identityB.Organization != tenantB.Id {
			t.Fatalf("organizations = %q and %q, want %q and %q", identityA.Organization, identityB.Organization, tenantA.Id, tenantB.Id)
		}
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
	})

	t.Run("profile collection rejects a forged local username", func(t *testing.T) {
		tenant := createIdentityIsolationTenant(t, ctx, client, env, "profile-org", env.UpstreamRealmA, env.ProfileClient, env.ProfileSecret, "openid profile email")
		defer deleteIdentityIsolationTenant(t, ctx, client, tenant)

		rejected := loginViaBrokerWithForgedUsername(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "profile-org", "alice-profile", "password-profile")
		if rejected.Subject != "" || rejected.LocalUsername != "" {
			t.Fatalf("forged local username created an identity: %+v", rejected)
		}
		identity := loginViaBroker(t, issuerURL(env.KeycloakURL, env.TargetRealm), env.BrokerClientID, "profile-org", "alice-profile", "password-profile")
		if identity.LocalUsername == "forged-local-username" {
			t.Fatalf("profile form accepted forged local username %q", identity.LocalUsername)
		}
		if identity.PreferredUsername != "alice-profile" {
			t.Fatalf("preferred username = %q, want alice-profile", identity.PreferredUsername)
		}
	})
}

func requireIdentityIsolationInputs(t *testing.T) identityIsolationEnv {
	t.Helper()
	env := identityIsolationEnv{
		GRPCAddress:         os.Getenv("AI_GRID_GRPC_ADDRESS"),
		KeycloakURL:         os.Getenv("KEYCLOAK_URL"),
		InternalKeycloakURL: os.Getenv("IDENTITY_ISOLATION_INTERNAL_KEYCLOAK_URL"),
		AdminRealm:          os.Getenv("KEYCLOAK_ADMIN_REALM"),
		TargetRealm:         os.Getenv("KEYCLOAK_REALM"),
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
	callbackPort, err := strconv.Atoi(os.Getenv("IDENTITY_ISOLATION_CALLBACK_PORT"))
	if err != nil {
		t.Fatalf("parse IDENTITY_ISOLATION_CALLBACK_PORT: %v", err)
	}
	env.CallbackPort = callbackPort
	brokerLoginCallbackPort = callbackPort
	missing := []string{}
	if env.GRPCAddress == "" {
		missing = append(missing, "AI_GRID_GRPC_ADDRESS")
	}
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
		missing = append(missing, "KEYCLOAK_REALM")
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
	if len(missing) > 0 {
		t.Fatalf("missing identity-isolation runtime inputs: %s", strings.Join(missing, ", "))
	}
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

func deleteIdentityIsolationTenant(t *testing.T, ctx context.Context, client *clientpkg.Client, tenant *aigridv1.Tenant) {
	t.Helper()
	if err := client.DeleteTenant(ctx, tenant.Id, tenant.Metadata.Version); err != nil {
		t.Errorf("delete tenant %s: %v", tenant.Id, err)
	}
}

type keycloakRealm struct {
	Realm                string                         `json:"realm"`
	Enabled              bool                           `json:"enabled"`
	OrganizationsEnabled bool                           `json:"organizationsEnabled,omitempty"`
	ClientScopes         []keycloakClientScope          `json:"clientScopes,omitempty"`
	AuthenticationFlows  []keycloakAuthenticationFlow   `json:"authenticationFlows,omitempty"`
	Clients              []keycloakClient               `json:"clients,omitempty"`
	Users                []keycloakUser                 `json:"users,omitempty"`
	Components           map[string][]keycloakComponent `json:"components,omitempty"`
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

type keycloakAuthenticationFlow struct {
	Alias                    string                            `json:"alias"`
	Description              string                            `json:"description,omitempty"`
	ProviderID               string                            `json:"providerId"`
	TopLevel                 bool                              `json:"topLevel"`
	BuiltIn                  bool                              `json:"builtIn"`
	AuthenticationExecutions []keycloakAuthenticationExecution `json:"authenticationExecutions,omitempty"`
}

type keycloakAuthenticationExecution struct {
	Authenticator     string `json:"authenticator"`
	Requirement       string `json:"requirement"`
	Priority          int    `json:"priority"`
	AuthenticatorFlow bool   `json:"authenticatorFlow"`
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

type keycloakComponent struct {
	ID            string              `json:"id,omitempty"`
	Name          string              `json:"name,omitempty"`
	ProviderID    string              `json:"providerId"`
	SubComponents map[string]any      `json:"subComponents,omitempty"`
	Config        map[string][]string `json:"config,omitempty"`
}

func createTargetRealm(t *testing.T, admin adminContext, env identityIsolationEnv) {
	t.Helper()
	deleteRealmIfExists(t, admin, env.TargetRealm)
	realm := keycloakRealm{
		Realm:                env.TargetRealm,
		Enabled:              true,
		OrganizationsEnabled: true,
		AuthenticationFlows: []keycloakAuthenticationFlow{{
			Alias:       "ai-grid-first-broker-login",
			Description: "Create a unique local account for each brokered identity.",
			ProviderID:  "basic-flow",
			TopLevel:    true,
			AuthenticationExecutions: []keycloakAuthenticationExecution{{
				Authenticator: "idp-create-user-if-unique",
				Requirement:   "REQUIRED",
				Priority:      10,
			}},
		}},
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
				{
					Name:           "AI Grid preferred username",
					Protocol:       "openid-connect",
					ProtocolMapper: "oidc-usermodel-attribute-mapper",
					Config: map[string]string{
						"user.attribute":       "maas_preferred_username",
						"claim.name":           "preferred_username",
						"jsonType.label":       "String",
						"id.token.claim":       "true",
						"userinfo.token.claim": "true",
						"access.token.claim":   "true",
					},
				},
				{
					Name:           "MaaS org id",
					Protocol:       "openid-connect",
					ProtocolMapper: "oidc-usermodel-attribute-mapper",
					Config: map[string]string{
						"user.attribute":       "maas_org_id",
						"claim.name":           "org_id",
						"jsonType.label":       "String",
						"multivalued":          "false",
						"id.token.claim":       "true",
						"userinfo.token.claim": "true",
						"access.token.claim":   "true",
					},
				},
				{
					Name:           "MaaS org name",
					Protocol:       "openid-connect",
					ProtocolMapper: "oidc-usermodel-attribute-mapper",
					Config: map[string]string{
						"user.attribute":       "maas_org_name",
						"claim.name":           "org_name",
						"jsonType.label":       "String",
						"multivalued":          "false",
						"id.token.claim":       "true",
						"userinfo.token.claim": "true",
						"access.token.claim":   "true",
					},
				},
				{
					Name:           "MaaS groups",
					Protocol:       "openid-connect",
					ProtocolMapper: "oidc-usermodel-attribute-mapper",
					Config: map[string]string{
						"user.attribute":       "maas_groups",
						"claim.name":           "groups",
						"jsonType.label":       "String",
						"multivalued":          "true",
						"id.token.claim":       "true",
						"userinfo.token.claim": "true",
						"access.token.claim":   "true",
					},
				},
				{
					Name:           "MaaS roles",
					Protocol:       "openid-connect",
					ProtocolMapper: "oidc-usermodel-attribute-mapper",
					Config: map[string]string{
						"user.attribute":       "maas_roles",
						"claim.name":           "roles",
						"jsonType.label":       "String",
						"multivalued":          "true",
						"id.token.claim":       "true",
						"userinfo.token.claim": "true",
						"access.token.claim":   "true",
					},
				},
			},
		}},
		Components: map[string][]keycloakComponent{
			"org.keycloak.userprofile.UserProfileProvider": {userProfileComponent(t)},
		},
	}
	postRealm(t, admin, realm)
	requireAuthenticationFlow(t, admin, env.TargetRealm, "ai-grid-first-broker-login")
}

func requireAuthenticationFlow(t *testing.T, admin adminContext, realm, alias string) {
	t.Helper()
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/authentication/flows"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("create authentication flow request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("get authentication flow %s: %v", alias, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read authentication flows response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list authentication flows: status %d body %s", resp.StatusCode, body)
	}
	var flows []struct {
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(body, &flows); err != nil {
		t.Fatalf("decode authentication flows response: %v", err)
	}
	for _, flow := range flows {
		if flow.Alias == alias {
			return
		}
	}
	t.Fatalf("authentication flow %s not found", alias)
}

func userProfileComponent(t *testing.T) keycloakComponent {
	t.Helper()
	return keycloakComponent{
		ID:         randomHex(t, 16),
		Name:       "identity-isolation-user-profile",
		ProviderID: "declarative-user-profile",
		Config: map[string][]string{
			"config-pieces-count": {"1"},
			"config-piece-0":      {`{"unmanagedAttributePolicy":"ADMIN_EDIT","attributes":[{"name":"username","required":{},"permissions":{"view":["admin"],"edit":["admin"]}}]}`},
		},
	}
}

func configureProfileScope(t *testing.T, admin adminContext, realm, userAttribute, mapperType string) {
	t.Helper()
	scopesEndpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes"
	req, err := http.NewRequest(http.MethodGet, scopesEndpoint, nil)
	if err != nil {
		t.Fatalf("create client scopes request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("list client scopes: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read client scopes response: %v", err)
	}
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
	req, err = http.NewRequest(http.MethodGet, mappersEndpoint, nil)
	if err != nil {
		t.Fatalf("create profile mappers request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	resp, err = admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("list profile mappers: %v", err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read profile mappers response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list profile mappers: status %d body %s", resp.StatusCode, body)
	}
	var mappers []keycloakProtocolMapper
	if err := json.Unmarshal(body, &mappers); err != nil {
		t.Fatalf("decode profile mappers response: %v", err)
	}
	for _, mapper := range mappers {
		if mapper.Name != "username" && mapper.Config["claim.name"] != "preferred_username" {
			continue
		}
		deleteReq, err := http.NewRequest(http.MethodDelete, mappersEndpoint+"/"+url.PathEscape(mapper.ID), nil)
		if err != nil {
			t.Fatalf("create delete profile mapper request: %v", err)
		}
		deleteReq.Header.Set("Authorization", "Bearer "+admin.Token)
		deleteResp, err := admin.HTTPClient.Do(deleteReq)
		if err != nil {
			t.Fatalf("delete profile mapper %s: %v", mapper.Name, err)
		}
		_ = deleteResp.Body.Close()
		if deleteResp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete profile mapper %s: status %d", mapper.Name, deleteResp.StatusCode)
		}
	}
	preferredMapper := keycloakProtocolMapper{
		Name:           "AI Grid preferred username",
		Protocol:       "openid-connect",
		ProtocolMapper: mapperType,
		Config: map[string]string{
			"user.attribute":       userAttribute,
			"claim.name":           "preferred_username",
			"jsonType.label":       "String",
			"id.token.claim":       "true",
			"access.token.claim":   "true",
			"userinfo.token.claim": "true",
		},
	}
	mapperBody, err := json.Marshal(preferredMapper)
	if err != nil {
		t.Fatalf("marshal preferred username mapper: %v", err)
	}
	postReq, err := http.NewRequest(http.MethodPost, mappersEndpoint, strings.NewReader(string(mapperBody)))
	if err != nil {
		t.Fatalf("create preferred username mapper request: %v", err)
	}
	postReq.Header.Set("Authorization", "Bearer "+admin.Token)
	postReq.Header.Set("Content-Type", "application/json")
	postResp, err := admin.HTTPClient.Do(postReq)
	if err != nil {
		t.Fatalf("create preferred username mapper: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusCreated {
		responseBody, _ := io.ReadAll(postResp.Body)
		t.Fatalf("create preferred username mapper: status %d body %s", postResp.StatusCode, responseBody)
	}
}

func createUpstreamRealmA(t *testing.T, admin adminContext, env identityIsolationEnv) {
	t.Helper()
	deleteRealmIfExists(t, admin, env.UpstreamRealmA)
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
			newUpstreamUser("alice-shared", "shared@example.com", "password-shared"),
			newUpstreamUser("alice-profile", "alice-profile@example.com", "password-profile"),
			newUpstreamUserWithoutEmail("shared-subject", "password-shared-subject"),
		},
	}
	postRealm(t, admin, realm)
	configureProfileScope(t, admin, env.UpstreamRealmA, "firstName", "oidc-usermodel-property-mapper")
}

func createUpstreamRealmB(t *testing.T, admin adminContext, env identityIsolationEnv) {
	t.Helper()
	deleteRealmIfExists(t, admin, env.UpstreamRealmB)
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
		},
	}
	postRealm(t, admin, realm)
	configureProfileScope(t, admin, env.UpstreamRealmB, "firstName", "oidc-usermodel-property-mapper")
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

func postRealm(t *testing.T, admin adminContext, realm keycloakRealm) {
	t.Helper()
	body, err := json.Marshal(realm)
	if err != nil {
		t.Fatalf("marshal realm %s: %v", realm.Realm, err)
	}
	endpoint := admin.BaseURL + "/admin/realms"
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("create realm request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("create realm %s: %v", realm.Realm, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("create realm %s: status %d body %s", realm.Realm, resp.StatusCode, responseBody)
	}
}

func deleteRealmIfExists(t *testing.T, admin adminContext, realm string) {
	t.Helper()
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm)
	req, err := http.NewRequest(http.MethodDelete, endpoint, nil)
	if err != nil {
		t.Fatalf("create delete realm request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("delete realm %s: %v", realm, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete realm %s: status %d body %s", realm, resp.StatusCode, body)
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
	req, err := http.NewRequest(http.MethodPut, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("create rename user request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("rename user %s: %v", currentUsername, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		responseBody, _ := io.ReadAll(resp.Body)
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
	req, err := http.NewRequest(http.MethodPut, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("create updated user email request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("update user email for %s: %v", username, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("update user email for %s: status %d body %s", username, resp.StatusCode, responseBody)
	}
}

func deleteIdentityProviderMapper(t *testing.T, admin adminContext, realm, providerAlias, mapperName string) {
	t.Helper()
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/identity-provider/instances/" + url.PathEscape(providerAlias) + "/mappers"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("create identity provider mapper list request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("list identity provider mappers: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read identity provider mappers: %v", err)
	}
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
		deleteReq, err := http.NewRequest(http.MethodDelete, endpoint+"/"+url.PathEscape(mapper.ID), nil)
		if err != nil {
			t.Fatalf("create identity provider mapper delete request: %v", err)
		}
		deleteReq.Header.Set("Authorization", "Bearer "+admin.Token)
		deleteResp, err := admin.HTTPClient.Do(deleteReq)
		if err != nil {
			t.Fatalf("delete identity provider mapper %s: %v", mapperName, err)
		}
		_ = deleteResp.Body.Close()
		if deleteResp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete identity provider mapper %s: status %d", mapperName, deleteResp.StatusCode)
		}
		return
	}
	t.Fatalf("identity provider mapper %s not found", mapperName)
}

func adminUserByUsername(t *testing.T, admin adminContext, realm, username string) adminUser {
	t.Helper()
	endpoint := fmt.Sprintf("%s/admin/realms/%s/users?username=%s&exact=true",
		admin.BaseURL, url.PathEscape(realm), url.QueryEscape(username))
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("create user search request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("search user %s: %v", username, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read user search response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search user %s: status %d body %s", username, resp.StatusCode, body)
	}
	var users []adminUser
	if err := json.Unmarshal(body, &users); err != nil {
		t.Fatalf("decode user search response: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("user search for %s returned %d users, want 1", username, len(users))
	}
	return users[0]
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
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("create legacy user request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("create legacy user: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		responseBody, _ := io.ReadAll(resp.Body)
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
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("create federated identity link request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("create federated identity link: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("create federated identity link: status %d body %s", resp.StatusCode, responseBody)
	}
}

func getAdminUser(t *testing.T, admin adminContext, realm, userID string) adminUser {
	t.Helper()
	endpoint := admin.BaseURL + "/admin/realms/" + url.PathEscape(realm) + "/users/" + url.PathEscape(userID)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("create get user request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	resp, err := admin.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("get user %s: %v", userID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read user response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get user %s: status %d body %s", userID, resp.StatusCode, body)
	}
	var user adminUser
	if err := json.Unmarshal(body, &user); err != nil {
		t.Fatalf("decode user response: %v", err)
	}
	return user
}
