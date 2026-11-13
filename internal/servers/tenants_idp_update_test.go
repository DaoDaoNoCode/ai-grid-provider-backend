package servers

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const discoveryOfValidIdP = "https://idp.example.com/.well-known/openid-configuration"

// idpSecrets holds the client secret the stored IdP names and a second one a
// changed IdP may name.
func idpSecrets() *fakeSecretReader {
	return &fakeSecretReader{secrets: map[string][]byte{
		"acme-idp-secret":   []byte("old-secret"),
		"acme-idp-secret-2": []byte("new-secret"),
		"acme-idp-empty":    {},
	}}
}

// orgWithIdP is organization org-1 (acme) whose stored IdP is validCreateIdP,
// with one member.
func orgWithIdP(t *testing.T) keycloak.Organization {
	t.Helper()
	normalized, err := normalizeIdentityProvider("acme", validCreateIdP())
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	chunks, err := identityProviderToAttributeValues(normalized)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	return keycloak.Organization{
		ID: "org-1", Name: "acme", Alias: "acme",
		Domains: []keycloak.OrganizationDomain{{Name: "acme.example.com", Verified: true, IdentityProviderAlias: "acme", AutoRedirect: true}},
		Attributes: map[string][]string{
			tenantVersionAttribute:          {"1"},
			tenantIdentityProviderAttribute: chunks,
		},
	}
}

// idpStore seeds the store with org and, when org has a stored IdP, the realm
// IdP Create would have made for it, linked and with its mappers, plus keys
// this backend doesn't own (hideOnLogin, pkceEnabled) that an update must keep.
func idpStore(t *testing.T, org keycloak.Organization) *fakeOrganizationStore {
	t.Helper()
	store := newFakeOrganizationStore(org)
	store.members[org.ID] = []keycloak.OrganizationMember{{ID: "user-1", Username: "alice"}}
	stored, err := identityProviderFromOrganizationAttrs(org.Attributes)
	if err != nil {
		t.Fatalf("stored idp: %v", err)
	}
	if stored == nil {
		return store
	}
	idp := keycloakIdentityProvider("acme", stored, []byte("old-secret"))
	idp.Config["pkceEnabled"] = "true"
	idp.Extra = map[string]json.RawMessage{"hideOnLogin": json.RawMessage("true")}
	store.idps["acme"] = idp
	store.links[org.ID] = "acme"
	store.mappers["acme"] = keycloak.IdentityProviderMapper{Name: "AI Grid org id", Config: map[string]string{"attribute.value": org.ID}}
	return store
}

func updateIdP(idp *aigridv1.IdentityProvider) *aigridv1.TenantsUpdateRequest {
	return &aigridv1.TenantsUpdateRequest{
		Object: &aigridv1.Tenant{
			Id: "org-1", Metadata: &aigridv1.Metadata{Version: 1},
			Spec: &aigridv1.TenantSpec{IdentityProvider: idp},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.identity_provider"}},
	}
}

// TestUpdateTenantChangesIdentityProviderInPlace: a changed IdP is written over
// the organization's realm IdP under the same alias, with the client secret
// read again from the Secret it names, the issuer and the signing keys checked
// by Keycloak, and the mappers re-applied. The organization, its id, its
// members and the IdP's link stay; keys the backend doesn't own are kept.
func TestUpdateTenantChangesIdentityProviderInPlace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*aigridv1.IdentityProvider)
		want   map[string]string // Keycloak IdP config keys and their values; "" means absent
		keys   bool              // Keycloak is asked to load the signing keys
	}{
		{
			name:   "client id",
			change: func(idp *aigridv1.IdentityProvider) { idp.Config.ClientId = "client-2" },
			want:   map[string]string{"clientId": "client-2", "clientSecret": "old-secret"},
			keys:   true,
		},
		{
			name:   "secret reference",
			change: func(idp *aigridv1.IdentityProvider) { idp.Config.ClientSecretSecretName = "acme-idp-secret-2" },
			want:   map[string]string{"clientId": "client", "clientSecret": "new-secret"},
			keys:   true,
		},
		{
			name: "endpoints and scopes",
			change: func(idp *aigridv1.IdentityProvider) {
				idp.Config.AuthorizationUrl = "https://idp.example.com/v2/authorize"
				idp.Config.TokenUrl = "https://idp.example.com/v2/token"
				idp.Config.JwksUrl = "https://idp.example.com/v2/keys"
				idp.Config.UserInfoUrl = "https://idp.example.com/v2/userinfo"
				idp.Config.DefaultScopes = "openid email"
			},
			want: map[string]string{
				"issuer":           "https://idp.example.com",
				"authorizationUrl": "https://idp.example.com/v2/authorize",
				"tokenUrl":         "https://idp.example.com/v2/token",
				"jwksUrl":          "https://idp.example.com/v2/keys",
				"userInfoUrl":      "https://idp.example.com/v2/userinfo",
				"defaultScope":     "openid email",
				"clientSecret":     "old-secret",
			},
			keys: true,
		},
		{
			name: "signature validation off drops the JWKS URL",
			change: func(idp *aigridv1.IdentityProvider) {
				off := false
				idp.Config.ValidateSignature = &off
				idp.Config.JwksUrl = ""
			},
			want: map[string]string{"validateSignature": "false", "jwksUrl": "", "useJwksUrl": ""},
			keys: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := idpStore(t, orgWithIdP(t))
			secrets := idpSecrets()
			server := mustTenantsServerWithSecrets(t, store, secrets)
			requested := validCreateIdP()
			tt.change(requested)
			want, err := normalizeIdentityProvider("acme", requested)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}

			got, err := server.Update(context.Background(), updateIdP(requested))
			if err != nil {
				t.Fatalf("Update: %v", err)
			}

			if got.GetObject().GetId() != "org-1" || got.GetObject().GetMetadata().GetVersion() != 2 {
				t.Fatalf("tenant = %v, want org-1 at version 2", got.GetObject())
			}
			if !proto.Equal(got.GetObject().GetSpec().GetIdentityProvider(), want) {
				t.Fatalf("response idp = %v, want %v", got.GetObject().GetSpec().GetIdentityProvider(), want)
			}
			stored, err := identityProviderFromOrganizationAttrs(store.byID["org-1"].Attributes)
			if err != nil || !proto.Equal(stored, want) {
				t.Fatalf("stored idp = %v (%v), want %v", stored, err, want)
			}
			if len(store.idps) != 1 || store.links["org-1"] != "acme" || len(store.members["org-1"]) != 1 {
				t.Fatalf("idps = %v, link = %q, members = %v; want the one IdP acme, still linked, members kept", store.idps, store.links["org-1"], store.members["org-1"])
			}
			kc := store.idps["acme"]
			for key, value := range tt.want {
				if got, ok := kc.Config[key]; got != value || (value == "" && ok) {
					t.Fatalf("keycloak idp config[%q] = %q (present %v), want %q", key, got, ok, value)
				}
			}
			if kc.Config["pkceEnabled"] != "true" || string(kc.Extra["hideOnLogin"]) != "true" || !kc.TrustEmail {
				t.Fatalf("keycloak idp = %+v, want pkceEnabled, hideOnLogin and trustEmail kept", kc)
			}
			if !slices.Contains(secrets.reads, requested.GetConfig().GetClientSecretSecretName()) {
				t.Fatalf("secret reads = %v, want %q read again", secrets.reads, requested.GetConfig().GetClientSecretSecretName())
			}
			// These changes keep the issuer, so it isn't checked again.
			if len(store.imports) != 0 {
				t.Fatalf("discovery checks = %v, want none on a change", store.imports)
			}
			if wantCalls := map[bool]int{true: 1, false: 0}[tt.keys]; store.reloadKeysCalls != wantCalls {
				t.Fatalf("signing key checks = %d, want %d", store.reloadKeysCalls, wantCalls)
			}
			if store.idpUpdates != 1 || store.mapperUpserts != 1 || store.mappers["acme"].Config["attribute.value"] != "org-1" {
				t.Fatalf("idp updates = %d, mapper upserts = %d, mapper = %+v; want one each, for org-1", store.idpUpdates, store.mapperUpserts, store.mappers["acme"])
			}
		})
	}
}

// TestUpdateTenantChangesIssuer: the issuer changes in place like any other
// field, under the same alias, once Keycloak confirms the new issuer's discovery
// document names it; a new issuer it can't confirm is refused with nothing
// written.
func TestUpdateTenantChangesIssuer(t *testing.T) {
	t.Parallel()
	const newIssuer = "https://login.example.org/tenant"
	const newDiscovery = newIssuer + "/.well-known/openid-configuration"

	t.Run("accepted and written", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, orgWithIdP(t))
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())
		requested := validCreateIdP()
		requested.Config.Issuer = newIssuer

		got, err := server.Update(context.Background(), updateIdP(requested))
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got.GetObject().GetSpec().GetIdentityProvider().GetConfig().GetIssuer() != newIssuer || store.puts != 1 {
			t.Fatalf("tenant = %v, puts = %d; want the new issuer saved", got.GetObject(), store.puts)
		}
		if !slices.Equal(store.imports, []string{newDiscovery}) {
			t.Fatalf("discovery checks = %v, want the new issuer's", store.imports)
		}
		if len(store.idps) != 1 || store.idps["acme"].Config["issuer"] != newIssuer || store.links["org-1"] != "acme" {
			t.Fatalf("idps = %v, link = %q; want IdP acme, still linked, with the new issuer", store.idps, store.links["org-1"])
		}
	})

	t.Run("refused once members have signed in", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, orgWithIdP(t))
		store.linkedUsers = map[string]int{"acme": 1}
		before := store.idps["acme"]
		secrets := idpSecrets()
		server := mustTenantsServerWithSecrets(t, store, secrets)
		requested := validCreateIdP()
		requested.Config.Issuer = newIssuer

		_, err := server.Update(context.Background(), updateIdP(requested))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
		if store.puts != 0 || store.idpUpdates != 0 || len(store.imports) != 0 || len(secrets.reads) != 0 || !reflect.DeepEqual(store.idps["acme"], before) {
			t.Fatalf("puts = %d, idp updates = %d, discovery checks = %v, secret reads = %v; want nothing checked or written",
				store.puts, store.idpUpdates, store.imports, secrets.reads)
		}
	})

	// The organization names issuer A while Keycloak's IdP is already on B (an
	// unconfirmed write, a failed restore, an edit in Keycloak), with members
	// linked through B: the change is judged against B, the issuer Keycloak uses.
	liveOnNewIssuer := func(t *testing.T) *fakeOrganizationStore {
		store := idpStore(t, orgWithIdP(t))
		live := store.idps["acme"]
		live.Config = maps.Clone(live.Config)
		live.Config["issuer"] = newIssuer
		store.idps["acme"] = live
		store.linkedUsers = map[string]int{"acme": 2}
		return store
	}

	t.Run("keeping the saved issuer while Keycloak has another is refused", func(t *testing.T) {
		t.Parallel()
		store := liveOnNewIssuer(t)
		before := store.idps["acme"]
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())
		requested := validCreateIdP() // issuer A, the organization's
		requested.Config.ClientId = "client-2"

		_, err := server.Update(context.Background(), updateIdP(requested))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
		if store.puts != 0 || store.idpUpdates != 0 || !reflect.DeepEqual(store.idps["acme"], before) {
			t.Fatalf("puts = %d, idp updates = %d, idp = %v; want nothing written", store.puts, store.idpUpdates, store.idps["acme"].Config)
		}
	})

	t.Run("naming the issuer Keycloak has is no issuer change", func(t *testing.T) {
		t.Parallel()
		store := liveOnNewIssuer(t)
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())
		requested := validCreateIdP()
		requested.Config.Issuer = newIssuer
		requested.Config.ClientId = "client-2"

		got, err := server.Update(context.Background(), updateIdP(requested))
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got.GetObject().GetSpec().GetIdentityProvider().GetConfig().GetIssuer() != newIssuer || store.puts != 1 || len(store.imports) != 0 {
			t.Fatalf("tenant = %v, puts = %d, discovery checks = %v; want the organization brought to B without a discovery check", got.GetObject(), store.puts, store.imports)
		}
		if store.idps["acme"].Config["issuer"] != newIssuer || store.idps["acme"].Config["clientId"] != "client-2" {
			t.Fatalf("keycloak idp = %v, want issuer B with client-2", store.idps["acme"].Config)
		}
	})

	t.Run("a change that keeps the issuer ignores linked members", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, orgWithIdP(t))
		store.linkedUsers = map[string]int{"acme": 2}
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())
		requested := validCreateIdP()
		requested.Config.ClientId = "client-2"

		if _, err := server.Update(context.Background(), updateIdP(requested)); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if store.puts != 1 || store.idps["acme"].Config["clientId"] != "client-2" {
			t.Fatalf("puts = %d, keycloak idp = %v; want the change written", store.puts, store.idps["acme"].Config)
		}
	})

	for _, tt := range []struct {
		name   string
		broken func(*fakeOrganizationStore)
		code   connect.Code
	}{
		{"discovery names another issuer", func(s *fakeOrganizationStore) {
			s.discoveredIssuers = map[string]string{newDiscovery: "https://login.example.org/other"}
		}, connect.CodeInvalidArgument},
		{"discovery document unreadable", func(s *fakeOrganizationStore) {
			s.importErr = &keycloak.APIError{Op: "import identity provider config", StatusCode: http.StatusInternalServerError}
		}, connect.CodeFailedPrecondition},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := idpStore(t, orgWithIdP(t))
			tt.broken(store)
			before := store.idps["acme"]
			server := mustTenantsServerWithSecrets(t, store, idpSecrets())
			requested := validCreateIdP()
			requested.Config.Issuer = newIssuer

			_, err := server.Update(context.Background(), updateIdP(requested))
			assertConnectCode(t, err, tt.code)
			if store.puts != 0 || store.idpUpdates != 0 || !reflect.DeepEqual(store.idps["acme"], before) {
				t.Fatalf("puts = %d, idp updates = %d; want nothing written", store.puts, store.idpUpdates)
			}
		})
	}
}

// TestUpdateTenantSkippedKeyReloadKeepsTheWrite: Keycloak answering false to
// reload-keys means it didn't try (it loaded the keys moments ago, or the IdP
// has no JWKS URL), not that the JWKS is bad, so the write stands: the keys
// load on the next sign-in.
func TestUpdateTenantSkippedKeyReloadKeepsTheWrite(t *testing.T) {
	t.Parallel()
	for name, org := range map[string]func(*testing.T) keycloak.Organization{
		"add": func(*testing.T) keycloak.Organization {
			return keycloak.Organization{ID: "org-1", Name: "acme", Alias: "acme", Attributes: map[string][]string{tenantVersionAttribute: {"1"}}}
		},
		"change": orgWithIdP,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := idpStore(t, org(t))
			store.reloadKeysFalse = true
			server := mustTenantsServerWithSecrets(t, store, idpSecrets())
			requested := validCreateIdP()
			requested.Config.ClientId = "client-2"

			got, err := server.Update(context.Background(), updateIdP(requested))
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if got.GetObject().GetMetadata().GetVersion() != 2 || store.reloadKeysCalls != 1 || store.idps["acme"].Config["clientId"] != "client-2" {
				t.Fatalf("tenant = %v, key reloads = %d, keycloak idp = %v; want the write kept", got.GetObject(), store.reloadKeysCalls, store.idps["acme"].Config)
			}
		})
	}
}

// TestUpdateTenantChangeDoesNotRecheckIssuer: the issuer is checked when it is
// set, so a change that keeps it is written even when the
// stored issuer's discovery document no longer answers or names another issuer.
func TestUpdateTenantChangeDoesNotRecheckIssuer(t *testing.T) {
	t.Parallel()
	discovery := map[string]func(*fakeOrganizationStore){
		"discovery names another issuer": func(s *fakeOrganizationStore) {
			s.discoveredIssuers = map[string]string{discoveryOfValidIdP: "https://idp.example.com/other"}
		},
		"discovery document unreadable": func(s *fakeOrganizationStore) {
			s.importErr = &keycloak.APIError{Op: "import identity provider config", StatusCode: http.StatusInternalServerError}
		},
	}
	changes := map[string]func(*aigridv1.IdentityProvider){
		"client id":        func(idp *aigridv1.IdentityProvider) { idp.Config.ClientId = "client-2" },
		"secret reference": func(idp *aigridv1.IdentityProvider) { idp.Config.ClientSecretSecretName = "acme-idp-secret-2" },
	}
	for dname, broken := range discovery {
		for cname, change := range changes {
			t.Run(dname+"/"+cname, func(t *testing.T) {
				t.Parallel()
				store := idpStore(t, orgWithIdP(t))
				broken(store)
				server := mustTenantsServerWithSecrets(t, store, idpSecrets())
				requested := validCreateIdP()
				change(requested)

				got, err := server.Update(context.Background(), updateIdP(requested))
				if err != nil {
					t.Fatalf("Update: %v", err)
				}
				want, _ := normalizeIdentityProvider("acme", requested)
				if !proto.Equal(got.GetObject().GetSpec().GetIdentityProvider(), want) || store.puts != 1 || store.idpUpdates != 1 || len(store.imports) != 0 {
					t.Fatalf("idp = %v, puts = %d, idp updates = %d, discovery checks = %v; want the change written without a discovery check",
						got.GetObject().GetSpec().GetIdentityProvider(), store.puts, store.idpUpdates, store.imports)
				}
			})
		}
	}
}

// TestUpdateTenantAddTakesOverItsLeftoverIdentityProvider: an add whose
// organization save failed and couldn't be read again leaves the realm IdP
// created and linked while the organization names none. Retrying the add
// writes the request over that IdP instead of answering AlreadyExists; an IdP
// under the alias that isn't linked to the organization is still refused.
func TestUpdateTenantAddTakesOverItsLeftoverIdentityProvider(t *testing.T) {
	t.Parallel()
	newOrg := func() keycloak.Organization {
		return keycloak.Organization{ID: "org-1", Name: "acme", Alias: "acme",
			Domains:    []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
			Attributes: map[string][]string{tenantVersionAttribute: {"1"}}}
	}

	t.Run("retry after an unseen failed save converges", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, newOrg())
		store.orgWriteErr = errors.New("connection reset")
		store.getErrAfterWrite = errors.New("connection refused")
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())

		_, err := server.Update(context.Background(), updateIdP(validCreateIdP()))
		assertConnectCode(t, err, connect.CodeUnavailable)
		if _, ok := store.idps["acme"]; !ok || store.links["org-1"] != "acme" || store.byID["org-1"].Attributes[tenantIdentityProviderAttribute] != nil {
			t.Fatalf("idps = %v, links = %v, org = %+v; want the IdP created and linked, the organization without it", store.idps, store.links, store.byID["org-1"])
		}

		retry := validCreateIdP()
		retry.Config.ClientId = "client-2"
		got, err := server.Update(context.Background(), updateIdP(retry))
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		if got.GetObject().GetMetadata().GetVersion() != 2 || got.GetObject().GetSpec().GetIdentityProvider().GetConfig().GetClientId() != "client-2" {
			t.Fatalf("tenant = %v, want version 2 with client-2", got.GetObject())
		}
		domains := store.byID["org-1"].Domains
		if store.idps["acme"].Config["clientId"] != "client-2" || len(domains) != 1 || domains[0].IdentityProviderAlias != "acme" {
			t.Fatalf("keycloak idp = %v, domains = %+v; want client-2 and the domain routed to it", store.idps["acme"].Config, domains)
		}
	})

	// A takeover is undone like a fresh add: the organization doesn't name the
	// IdP, so it is deleted, unless the organization's save landed.
	leftover := func(t *testing.T) *fakeOrganizationStore {
		store := idpStore(t, newOrg())
		normalized, err := normalizeIdentityProvider("acme", validCreateIdP())
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		store.idps["acme"] = keycloakIdentityProvider("acme", normalized, []byte("old-secret"))
		store.links["org-1"] = "acme"
		// This backend's own leftover carries its org id mapper.
		store.mappers["acme"] = keycloak.IdentityProviderMapper{Name: keycloak.IdentityProviderOrgIDMapperName, Config: map[string]string{"attribute.value": "org-1"}}
		return store
	}
	for _, tt := range []struct {
		name  string
		setup func(*fakeOrganizationStore)
		code  connect.Code // 0: the takeover is saved and the IdP kept
	}{
		{name: "mapper failure deletes the taken-over idp", setup: func(s *fakeOrganizationStore) {
			s.mapperErrOnce = &keycloak.APIError{Op: "update identity provider mapper", StatusCode: http.StatusBadGateway}
		}, code: connect.CodeInternal},
		{name: "a save that surely didn't land deletes the taken-over idp", setup: func(s *fakeOrganizationStore) {
			s.orgWriteErr = errors.New("connection reset")
		}, code: connect.CodeInternal},
		{name: "a saved organization whose answer was lost keeps the taken-over idp", setup: func(s *fakeOrganizationStore) {
			s.lostAnswerErr = errors.New("connection reset")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := leftover(t)
			tt.setup(store)
			server := mustTenantsServerWithSecrets(t, store, idpSecrets())
			requested := validCreateIdP()
			requested.Config.ClientId = "client-2"

			_, err := server.Update(context.Background(), updateIdP(requested))
			if tt.code != 0 {
				assertConnectCode(t, err, tt.code)
				if len(store.idps) != 0 || len(store.links) != 0 || store.byID["org-1"].Attributes[tenantIdentityProviderAttribute] != nil {
					t.Fatalf("idps = %v, links = %v, org = %+v; want the taken-over IdP deleted and the organization without one", store.idps, store.links, store.byID["org-1"])
				}
				return
			}
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if store.idps["acme"].Config["clientId"] != "client-2" || store.links["org-1"] != "acme" || store.byID["org-1"].Attributes[tenantIdentityProviderAttribute] == nil {
				t.Fatalf("idps = %v, links = %v; want the taken-over IdP kept and named by the organization", store.idps, store.links)
			}
		})
	}

	// A Secret failure on the takeover path happens before anything is written,
	// so it deletes nothing.
	for _, tt := range []struct {
		name   string
		secret string
		code   connect.Code
	}{
		{name: "missing secret deletes nothing", secret: "missing", code: connect.CodeNotFound},
		{name: "empty secret deletes nothing", secret: "acme-idp-empty", code: connect.CodeFailedPrecondition},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := leftover(t)
			before := store.idps["acme"]
			server := mustTenantsServerWithSecrets(t, store, idpSecrets())
			requested := validCreateIdP()
			requested.Config.ClientSecretSecretName = tt.secret

			_, err := server.Update(context.Background(), updateIdP(requested))
			assertConnectCode(t, err, tt.code)
			if !reflect.DeepEqual(store.idps["acme"], before) || store.links["org-1"] != "acme" || store.idpUpdates != 0 || store.deleteIdPCalls != 0 {
				t.Fatalf("idp = %+v, link = %q, idp updates = %d, deletes = %d; want the leftover untouched", store.idps["acme"], store.links["org-1"], store.idpUpdates, store.deleteIdPCalls)
			}
		})
	}

	t.Run("an idp this api didn't create is refused and left as it is", func(t *testing.T) {
		t.Parallel()
		// An organization made outside this API, as cluster-gitops imports its example
		// organizations: a linked IdP under the organization's alias, with its own
		// mappers and members, and no IdP attribute on the organization.
		store := idpStore(t, newOrg())
		foreign := keycloak.IdentityProvider{Alias: "acme", ProviderID: "oidc", Enabled: true,
			Config: map[string]string{"issuer": "https://other-idp.example.net/realms/t1", "clientId": "theirs", "clientSecret": "their-secret"}}
		store.idps["acme"] = foreign
		store.links["org-1"] = "acme"
		store.mappers["acme"] = keycloak.IdentityProviderMapper{Name: "org id", Config: map[string]string{"attribute.value": "org-1"}}
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())

		_, err := server.Update(context.Background(), updateIdP(validCreateIdP()))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
		if !reflect.DeepEqual(store.idps["acme"], foreign) || store.links["org-1"] != "acme" || store.mappers["acme"].Name != "org id" {
			t.Fatalf("idp = %+v, link = %q, mapper = %+v; want the foreign IdP, its link and its mapper untouched", store.idps["acme"], store.links["org-1"], store.mappers["acme"])
		}
		if store.puts != 0 || store.idpUpdates != 0 || store.deleteIdPCalls != 0 || len(store.members["org-1"]) != 1 {
			t.Fatalf("puts = %d, idp updates = %d, deletes = %d, members = %v; want nothing written", store.puts, store.idpUpdates, store.deleteIdPCalls, store.members["org-1"])
		}
	})

	t.Run("an unlinked idp under the alias is still refused", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, newOrg())
		store.idps["acme"] = keycloak.IdentityProvider{Alias: "acme", Config: map[string]string{"clientId": "theirs"}}
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())

		_, err := server.Update(context.Background(), updateIdP(validCreateIdP()))
		assertConnectCode(t, err, connect.CodeAlreadyExists)
		if store.idps["acme"].Config["clientId"] != "theirs" || store.puts != 0 {
			t.Fatalf("keycloak idp = %v, puts = %d; want it untouched", store.idps["acme"].Config, store.puts)
		}
	})
}

func TestUpdateTenantRenameWithUsernameMapperConflict(t *testing.T) {
	t.Parallel()
	store := idpStore(t, orgWithIdP(t))
	store.mapperErr = &keycloak.UsernameMapperConflictError{Alias: "acme", MapperName: "Manual username"}
	server := mustTenantsServerWithSecrets(t, store, idpSecrets())
	request := &aigridv1.TenantsUpdateRequest{
		Object:     &aigridv1.Tenant{Id: "org-1", Metadata: &aigridv1.Metadata{Version: 1, Name: "acme-corp"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.name"}},
	}

	_, err := server.Update(context.Background(), request)
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	var conflict *keycloak.UsernameMapperConflictError
	if !errors.As(err, &conflict) || conflict.Alias != "acme" || conflict.MapperName != "Manual username" {
		t.Fatalf("Update error = %v, want the conflicting mapper", err)
	}
	stored := store.byID["org-1"]
	if stored.Name != "acme-corp" || organizationVersion(stored) != 2 {
		t.Fatalf("organization = %+v, want acme-corp at version 2", stored)
	}

	store.mapperErr = nil
	request.Object.Metadata.Version = 2
	got, err := server.Update(context.Background(), request)
	if err != nil {
		t.Fatalf("Update after removing the conflict: %v", err)
	}
	if got.GetObject().GetMetadata().GetName() != "acme-corp" || store.mapperOrgNames["acme"] != "acme-corp" {
		t.Fatalf("tenant = %v, mapper name = %q", got.GetObject(), store.mapperOrgNames["acme"])
	}
}

// TestUpdateTenantAddsIdentityProvider: an organization created without an IdP
// gets one the way Create sets it up, and keeps its id, members and domains,
// which are now routed to the new IdP.
func TestUpdateTenantAddsIdentityProvider(t *testing.T) {
	t.Parallel()
	store := idpStore(t, keycloak.Organization{
		ID: "org-1", Name: "acme", Alias: "acme",
		Domains:    []keycloak.OrganizationDomain{{Name: "acme.example.com", Verified: true}},
		Attributes: map[string][]string{tenantVersionAttribute: {"1"}},
	})
	secrets := idpSecrets()
	server := mustTenantsServerWithSecrets(t, store, secrets)

	got, err := server.Update(context.Background(), updateIdP(validCreateIdP()))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got.GetObject().GetId() != "org-1" || got.GetObject().GetMetadata().GetVersion() != 2 || got.GetObject().GetSpec().GetIdentityProvider() == nil {
		t.Fatalf("tenant = %v, want org-1 at version 2 with an identity provider", got.GetObject())
	}
	kc, ok := store.idps["acme"]
	if !ok || !kc.TrustEmail || kc.Config["clientSecret"] != "old-secret" || kc.Config["clientId"] != "client" {
		t.Fatalf("keycloak idp = %+v, want acme with the client and its secret", kc)
	}
	var flow string
	if err := json.Unmarshal(kc.Extra["firstBrokerLoginFlowAlias"], &flow); err != nil || flow != "first broker login" {
		t.Fatalf("firstBrokerLoginFlowAlias = %q, want first broker login", kc.Extra["firstBrokerLoginFlowAlias"])
	}
	wantMappers := []string{
		"AI Grid org id",
		"AI Grid org name",
		"AI Grid groups",
		"AI Grid roles",
		"AI Grid local username",
		"AI Grid preferred username",
	}
	if !slices.Equal(store.mapperNames["acme"], wantMappers) {
		t.Fatalf("mappers = %v, want %v", store.mapperNames["acme"], wantMappers)
	}
	if store.links["org-1"] != "acme" || store.mappers["acme"].Config["attribute.value"] != "org-1" || len(store.members["org-1"]) != 1 {
		t.Fatalf("link = %q, mapper = %+v, members = %v", store.links["org-1"], store.mappers["acme"], store.members["org-1"])
	}
	domains := store.byID["org-1"].Domains
	if len(domains) != 1 || domains[0].IdentityProviderAlias != "acme" || !domains[0].AutoRedirect || !domains[0].Verified {
		t.Fatalf("domains = %+v, want acme.example.com kept and routed to acme", domains)
	}
	if !slices.Equal(store.imports, []string{discoveryOfValidIdP}) || store.reloadKeysCalls != 1 {
		t.Fatalf("discovery checks = %v, signing key checks = %d; want one each", store.imports, store.reloadKeysCalls)
	}
	if !slices.Equal(secrets.reads, []string{"acme-idp-secret"}) {
		t.Fatalf("secret reads = %v", secrets.reads)
	}
}

// TestUpdateTenantIdentityProviderRefusalsChangeNothing: a request whose IdP
// Keycloak can't use, or that fails after the IdP was written, is refused and
// leaves the organization and its realm IdP as they were, for an added IdP
// and for a changed one alike.
func TestUpdateTenantIdentityProviderRefusalsChangeNothing(t *testing.T) {
	t.Parallel()
	keycloak500 := &keycloak.APIError{Op: "test", StatusCode: http.StatusInternalServerError, Body: `{"error":"unknown_error"}`}
	tests := []struct {
		name   string
		setup  func(*fakeOrganizationStore)
		change func(*aigridv1.TenantsUpdateRequest)
		code   connect.Code
		// addOnly: the issuer is checked when an IdP is added, not on a change.
		addOnly bool
	}{
		{
			name: "discovery names another issuer",
			setup: func(s *fakeOrganizationStore) {
				s.discoveredIssuers = map[string]string{discoveryOfValidIdP: "https://idp.example.com/other"}
			},
			code:    connect.CodeInvalidArgument,
			addOnly: true,
		},
		{
			name:    "discovery document unreadable",
			setup:   func(s *fakeOrganizationStore) { s.importErr = keycloak500 },
			code:    connect.CodeFailedPrecondition,
			addOnly: true,
		},
		{
			name: "secret without a client secret",
			change: func(r *aigridv1.TenantsUpdateRequest) {
				r.Object.Spec.IdentityProvider.Config.ClientSecretSecretName = "acme-idp-empty"
			},
			code: connect.CodeFailedPrecondition,
		},
		{
			name:  "signing keys unreadable",
			setup: func(s *fakeOrganizationStore) { s.reloadKeysErr = keycloak500 },
			code:  connect.CodeFailedPrecondition,
		},
		{
			name: "secret missing",
			change: func(r *aigridv1.TenantsUpdateRequest) {
				r.Object.Spec.IdentityProvider.Config.ClientSecretSecretName = "missing"
			},
			code: connect.CodeNotFound,
		},
		{
			name: "mappers fail",
			setup: func(s *fakeOrganizationStore) {
				s.mapperErrOnce = &keycloak.APIError{Op: "update identity provider mapper", StatusCode: http.StatusBadGateway}
			},
			code: connect.CodeInternal,
		},
		{
			name: "organization write fails",
			setup: func(s *fakeOrganizationStore) {
				s.byID["org-2"] = keycloak.Organization{ID: "org-2", Name: "taken", Alias: "taken"}
			},
			change: func(r *aigridv1.TenantsUpdateRequest) {
				r.Object.Metadata.Name = "taken"
				r.UpdateMask.Paths = append(r.UpdateMask.Paths, "metadata.name")
			},
			code: connect.CodeAlreadyExists,
		},
	}
	scenarios := []struct {
		name string
		org  func(*testing.T) keycloak.Organization
		idp  func() *aigridv1.IdentityProvider
	}{
		{
			name: "add",
			org: func(*testing.T) keycloak.Organization {
				return keycloak.Organization{ID: "org-1", Name: "acme", Alias: "acme",
					Domains:    []keycloak.OrganizationDomain{{Name: "acme.example.com"}},
					Attributes: map[string][]string{tenantVersionAttribute: {"1"}}}
			},
			idp: validCreateIdP,
		},
		{
			name: "change",
			org:  orgWithIdP,
			idp: func() *aigridv1.IdentityProvider {
				idp := validCreateIdP()
				idp.Config.ClientId = "client-2"
				return idp
			},
		},
	}
	for _, sc := range scenarios {
		for _, tt := range tests {
			if tt.addOnly && sc.name != "add" {
				continue
			}
			t.Run(sc.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				org := sc.org(t)
				store := idpStore(t, org)
				if tt.setup != nil {
					tt.setup(store)
				}
				before := store.idps["acme"]
				server := mustTenantsServerWithSecrets(t, store, idpSecrets())
				req := updateIdP(sc.idp())
				if tt.change != nil {
					tt.change(req)
				}

				_, err := server.Update(context.Background(), req)
				assertConnectCode(t, err, tt.code)

				if !reflect.DeepEqual(store.byID["org-1"], org) || store.puts != 0 {
					t.Fatalf("organization = %+v (puts %d), want it unchanged", store.byID["org-1"], store.puts)
				}
				if sc.name == "add" {
					if len(store.idps) != 0 || len(store.links) != 0 {
						t.Fatalf("idps = %v, links = %v; want the added IdP removed", store.idps, store.links)
					}
					return
				}
				if !reflect.DeepEqual(store.idps["acme"], before) || store.links["org-1"] != "acme" {
					t.Fatalf("keycloak idp = %+v, want it restored to %+v", store.idps["acme"], before)
				}
				if name, ok := store.mapperOrgNames["acme"]; ok && name != "acme" {
					t.Fatalf("mapper org name = %q, want it restored to acme", name)
				}
			})
		}
	}
}

// TestUpdateTenantIdentityProviderUndoFailures: when undoing a failed IdP
// write can't finish, the error says what was left.
func TestUpdateTenantIdentityProviderUndoFailures(t *testing.T) {
	t.Parallel()
	keycloak500 := &keycloak.APIError{Op: "reload identity provider keys", StatusCode: http.StatusInternalServerError}

	t.Run("unreadable current secret refuses the change before writing", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, orgWithIdP(t))
		before := store.idps["acme"]
		// The Secret the stored IdP names is gone, so a failed change couldn't be undone.
		secrets := &fakeSecretReader{secrets: map[string][]byte{"acme-idp-secret-2": []byte("new-secret")}}
		server := mustTenantsServerWithSecrets(t, store, secrets)
		requested := validCreateIdP()
		requested.Config.ClientSecretSecretName = "acme-idp-secret-2"
		requested.Config.ClientId = "client-2"

		_, err := server.Update(context.Background(), updateIdP(requested))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)
		// The message names the stored IdP's Secret, not the requested one.
		assertConnectMessage(t, err, `previous client secret, needed to restore the identity provider if this change fails, can't be read from Secret "acme-idp-secret"`)

		if store.idpUpdates != 0 || store.puts != 0 || !reflect.DeepEqual(store.idps["acme"], before) {
			t.Fatalf("idp updates = %d, puts = %d, idp = %+v; want nothing written", store.idpUpdates, store.puts, store.idps["acme"])
		}
	})

	t.Run("restore uses the secret read before the change", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, orgWithIdP(t))
		before := store.idps["acme"]
		store.reloadKeysErr = keycloak500
		secrets := idpSecrets()
		server := mustTenantsServerWithSecrets(t, store, secrets)
		requested := validCreateIdP()
		requested.Config.ClientSecretSecretName = "acme-idp-secret-2"

		_, err := server.Update(context.Background(), updateIdP(requested))
		assertConnectCode(t, err, connect.CodeFailedPrecondition)

		if !reflect.DeepEqual(store.idps["acme"], before) {
			t.Fatalf("keycloak idp = %+v, want it restored with the old secret", store.idps["acme"])
		}
		// Both Secrets were read before the write, and nothing after it.
		if !slices.Equal(secrets.reads, []string{"acme-idp-secret-2", "acme-idp-secret"}) {
			t.Fatalf("secret reads = %v", secrets.reads)
		}
	})

	t.Run("restore write failure is reported", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, orgWithIdP(t))
		store.reloadKeysErr = keycloak500
		store.restoreIdPErr = &keycloak.APIError{Op: "update identity provider", StatusCode: http.StatusBadGateway}
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())
		requested := validCreateIdP()
		requested.Config.ClientId = "client-2"

		_, err := server.Update(context.Background(), updateIdP(requested))
		assertConnectCode(t, err, connect.CodeInternal)
		assertConnectMessage(t, err, "left on the new config while the organization wasn't saved")
		if store.puts != 0 || store.idpUpdates != 1 {
			t.Fatalf("puts = %d, idp updates = %d; want the organization unwritten and only the change written", store.puts, store.idpUpdates)
		}
	})

	t.Run("a saved organization whose answer was lost keeps the change", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, orgWithIdP(t))
		store.lostAnswerErr = errors.New("connection reset")
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())
		requested := validCreateIdP()
		requested.Config.ClientId = "client-2"

		got, err := server.Update(context.Background(), updateIdP(requested))
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got.GetObject().GetMetadata().GetVersion() != 2 || got.GetObject().GetSpec().GetIdentityProvider().GetConfig().GetClientId() != "client-2" {
			t.Fatalf("tenant = %v, want version 2 with client-2", got.GetObject())
		}
		if store.idpUpdates != 1 || store.idps["acme"].Config["clientId"] != "client-2" {
			t.Fatalf("idp updates = %d, keycloak idp = %v; want the change kept, not restored", store.idpUpdates, store.idps["acme"].Config)
		}
	})

	// When the organization's save fails and it can't be read again, nothing is
	// undone: the answer says the change may have landed, and retrying the same
	// request converges whether it did or not.
	for _, tt := range []struct {
		name  string
		saved bool
		// retry is the code of the same request sent again: Aborted when the
		// first one was saved (its version is stale), success when it wasn't.
		retry connect.Code
	}{
		{name: "lost answer of a saved change converges on retry", saved: true, retry: connect.CodeAborted},
		{name: "failed save of a change converges on retry", saved: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := idpStore(t, orgWithIdP(t))
			if tt.saved {
				store.lostAnswerErr = errors.New("connection reset")
			} else {
				store.orgWriteErr = errors.New("connection reset")
			}
			store.getErrAfterWrite = errors.New("connection refused")
			server := mustTenantsServerWithSecrets(t, store, idpSecrets())
			requested := validCreateIdP()
			requested.Config.ClientId = "client-2"

			_, err := server.Update(context.Background(), updateIdP(requested))
			assertConnectCode(t, err, connect.CodeUnavailable)
			if store.idps["acme"].Config["clientId"] != "client-2" {
				t.Fatalf("keycloak idp config = %v, want the change kept, not undone", store.idps["acme"].Config)
			}

			_, err = server.Update(context.Background(), updateIdP(requested))
			if tt.retry != 0 {
				assertConnectCode(t, err, tt.retry)
			} else if err != nil {
				t.Fatalf("retry: %v", err)
			}
			got, err := server.Get(context.Background(), &aigridv1.TenantsGetRequest{Id: "org-1"})
			if err != nil || got.GetObject().GetMetadata().GetVersion() != 2 || got.GetObject().GetSpec().GetIdentityProvider().GetConfig().GetClientId() != "client-2" {
				t.Fatalf("tenant after retry = %v, %v; want version 2 with client-2", got.GetObject(), err)
			}
			if store.idps["acme"].Config["clientId"] != "client-2" || store.idps["acme"].Config["clientSecret"] != "old-secret" {
				t.Fatalf("keycloak idp config = %v, want client-2", store.idps["acme"].Config)
			}
		})
	}

	t.Run("an unreadable save of an issuer change says sign-in already uses it", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, orgWithIdP(t))
		store.lostAnswerErr = errors.New("connection reset")
		store.getErrAfterWrite = errors.New("connection refused")
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())
		requested := validCreateIdP()
		requested.Config.Issuer = "https://login.example.org/tenant"

		_, err := server.Update(context.Background(), updateIdP(requested))
		assertConnectCode(t, err, connect.CodeUnavailable)
		assertConnectMessage(t, err, `sign-in already uses the new issuer "https://login.example.org/tenant"`)
		if store.idps["acme"].Config["issuer"] != "https://login.example.org/tenant" {
			t.Fatalf("keycloak idp issuer = %q, want the new one kept", store.idps["acme"].Config["issuer"])
		}
	})

	t.Run("lost answer of a saved add converges on retry", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, keycloak.Organization{ID: "org-1", Name: "acme", Alias: "acme", Attributes: map[string][]string{tenantVersionAttribute: {"1"}}})
		store.lostAnswerErr = errors.New("connection reset")
		store.getErrAfterWrite = errors.New("connection refused")
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())

		_, err := server.Update(context.Background(), updateIdP(validCreateIdP()))
		assertConnectCode(t, err, connect.CodeUnavailable)
		if _, ok := store.idps["acme"]; !ok || store.links["org-1"] != "acme" {
			t.Fatalf("idps = %v, links = %v; want the added IdP kept", store.idps, store.links)
		}
		_, err = server.Update(context.Background(), updateIdP(validCreateIdP()))
		assertConnectCode(t, err, connect.CodeAborted)
		got, err := server.Get(context.Background(), &aigridv1.TenantsGetRequest{Id: "org-1"})
		if err != nil || got.GetObject().GetSpec().GetIdentityProvider() == nil {
			t.Fatalf("tenant after retry = %v, %v; want the identity provider", got.GetObject(), err)
		}
	})

	t.Run("added idp cleanup failure names both errors", func(t *testing.T) {
		t.Parallel()
		store := idpStore(t, keycloak.Organization{ID: "org-1", Name: "acme", Alias: "acme", Attributes: map[string][]string{tenantVersionAttribute: {"1"}}})
		store.reloadKeysErr = keycloak500
		store.deleteIdPErr = &keycloak.APIError{Op: "delete identity provider", StatusCode: http.StatusBadGateway}
		server := mustTenantsServerWithSecrets(t, store, idpSecrets())

		_, err := server.Update(context.Background(), updateIdP(validCreateIdP()))
		assertConnectCode(t, err, connect.CodeInternal)
		if store.puts != 0 {
			t.Fatalf("organization written: puts=%d", store.puts)
		}
	})
}
