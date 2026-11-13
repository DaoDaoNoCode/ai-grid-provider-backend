package fakeinfra_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	fakekeycloak "github.com/osac-project/ai-grid-provider-backend/internal/fakeinfra/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/servers"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type mapSecrets map[string]string

func (m mapSecrets) Read(_ context.Context, name string) ([]byte, error) {
	if v, ok := m[name]; ok {
		return []byte(v), nil
	}
	return nil, io.EOF
}

// TestRealTenantsServerIdentityProviderOnFakeKeycloak runs the tenant IdP
// flow against the fake Keycloak, which fetches the IdP's discovery document
// and JWKS as Keycloak does: an organization created without an IdP gets one,
// the IdP is corrected in place, and an IdP whose issuer or keys Keycloak
// can't confirm is refused with the organization and its IdP left as they were.
func TestRealTenantsServerIdentityProviderOnFakeKeycloak(t *testing.T) {
	idp := httptest.NewServer(http.NewServeMux())
	t.Cleanup(idp.Close)
	mux := idp.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": idp.URL, "jwks_uri": idp.URL + "/keys"})
	})
	// A second issuer on the same server, for an issuer change.
	mux.HandleFunc("GET /second/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": idp.URL + "/second", "jwks_uri": idp.URL + "/keys"})
	})
	mux.HandleFunc("GET /elsewhere/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": idp.URL})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"keys":[]}`)
	})

	kc, err := fakekeycloak.New(fakekeycloak.Config{AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "admin", Logger: slog.New(slog.DiscardHandler)}, nil)
	if err != nil {
		t.Fatalf("keycloak fake New() error = %v", err)
	}
	srv := httptest.NewServer(kc.Handler())
	t.Cleanup(srv.Close)
	admin := keycloak.New(keycloak.Config{BaseURL: srv.URL, AdminRealm: "master", Realm: "ai-grid", Username: "admin", Password: "admin", HTTPClient: srv.Client()})
	s, err := servers.NewTenantsServer().SetLogger(slog.New(slog.DiscardHandler)).SetOrganizationClient(admin).
		SetSecretReader(mapSecrets{"acme-idp": "s1", "acme-idp-2": "s2"}).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	ctx := context.Background()

	created, err := s.Create(ctx, &aigridv1.TenantsCreateRequest{Object: &aigridv1.Tenant{
		Metadata: &aigridv1.Metadata{Name: "acme"},
		Spec:     &aigridv1.TenantSpec{Domains: []string{"acme.example.com"}},
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.GetObject().GetId()
	if created.GetObject().GetSpec().GetIdentityProvider() != nil {
		t.Fatalf("Create() idp = %v, want none: sign-in not configured", created.GetObject().GetSpec().GetIdentityProvider())
	}

	oidc := func(issuer, clientID, secret, jwks string) *aigridv1.IdentityProvider {
		return &aigridv1.IdentityProvider{Config: &aigridv1.OidcConfig{
			ClientId: clientID, ClientSecretSecretName: secret, Issuer: issuer,
			AuthorizationUrl: issuer + "/auth", TokenUrl: issuer + "/token", JwksUrl: jwks,
		}}
	}
	update := func(version int32, idp *aigridv1.IdentityProvider) (*aigridv1.TenantsUpdateResponse, error) {
		return s.Update(ctx, &aigridv1.TenantsUpdateRequest{
			Object:     &aigridv1.Tenant{Id: id, Metadata: &aigridv1.Metadata{Version: version}, Spec: &aigridv1.TenantSpec{IdentityProvider: idp}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.identity_provider"}},
		})
	}

	added, err := update(1, oidc(idp.URL, "client", "acme-idp", idp.URL+"/keys"))
	if err != nil {
		t.Fatalf("Update(add idp) error = %v", err)
	}
	if added.GetObject().GetId() != id || added.GetObject().GetMetadata().GetVersion() != 2 || added.GetObject().GetSpec().GetIdentityProvider() == nil {
		t.Fatalf("Update(add idp) = %v", added.GetObject())
	}
	org, err := admin.GetOrganization(ctx, id)
	if err != nil || len(org.Domains) != 1 || org.Domains[0].IdentityProviderAlias != "acme" || !org.Domains[0].AutoRedirect {
		t.Fatalf("organization = %+v, %v; want acme.example.com routed to acme", org, err)
	}

	// An operator tunes the IdP in Keycloak, in fields this backend doesn't own.
	tuned, err := admin.GetIdentityProvider(ctx, "acme")
	if err != nil {
		t.Fatalf("GetIdentityProvider() error = %v", err)
	}
	tuned.Extra = map[string]json.RawMessage{"hideOnLogin": json.RawMessage("true"), "firstBrokerLoginFlowAlias": json.RawMessage(`"custom flow"`)}
	tuned.Config["pkceEnabled"] = "true"
	if err := admin.UpdateIdentityProvider(ctx, tuned); err != nil {
		t.Fatalf("UpdateIdentityProvider() error = %v", err)
	}

	changed, err := update(2, oidc(idp.URL, "client-2", "acme-idp-2", idp.URL+"/keys"))
	if err != nil {
		t.Fatalf("Update(change idp) error = %v", err)
	}
	if changed.GetObject().GetMetadata().GetVersion() != 3 || changed.GetObject().GetSpec().GetIdentityProvider().GetConfig().GetClientId() != "client-2" {
		t.Fatalf("Update(change idp) = %v", changed.GetObject())
	}
	kcIdP, err := admin.GetIdentityProvider(ctx, "acme")
	if err != nil || kcIdP.Config["clientId"] != "client-2" || kcIdP.Config["pkceEnabled"] != "true" ||
		string(kcIdP.Extra["hideOnLogin"]) != "true" || string(kcIdP.Extra["firstBrokerLoginFlowAlias"]) != `"custom flow"` {
		t.Fatalf("keycloak idp = %+v, %v; want client-2 with the operator's settings kept", kcIdP, err)
	}

	refusals := []struct {
		name string
		idp  *aigridv1.IdentityProvider
		code connect.Code
	}{
		{"new issuer the discovery document doesn't name", oidc(idp.URL+"/elsewhere", "client-2", "acme-idp-2", idp.URL+"/keys"), connect.CodeInvalidArgument},
		{"new issuer without a discovery document", oidc(idp.URL+"/nowhere", "client-2", "acme-idp-2", idp.URL+"/keys"), connect.CodeFailedPrecondition},
		{"JWKS URL that doesn't answer", oidc(idp.URL, "client-3", "acme-idp", idp.URL+"/missing"), connect.CodeFailedPrecondition},
		{"removed idp", nil, connect.CodeInvalidArgument},
	}
	for _, r := range refusals {
		if _, err := update(3, r.idp); connect.CodeOf(err) != r.code {
			t.Fatalf("Update(%s) error = %v, want %v", r.name, err, r.code)
		}
		got, err := s.Get(ctx, &aigridv1.TenantsGetRequest{Id: id})
		if err != nil || got.GetObject().GetMetadata().GetVersion() != 3 || got.GetObject().GetSpec().GetIdentityProvider().GetConfig().GetClientId() != "client-2" {
			t.Fatalf("after Update(%s): tenant = %v, %v; want it unchanged", r.name, got.GetObject(), err)
		}
		kcIdP, err := admin.GetIdentityProvider(ctx, "acme")
		if err != nil || kcIdP.Config["clientId"] != "client-2" || kcIdP.Config["jwksUrl"] != idp.URL+"/keys" {
			t.Fatalf("after Update(%s): keycloak idp = %+v, %v; want it unchanged", r.name, kcIdP.Config, err)
		}
	}

	// Keycloak checks the issuer of an identity provider that is added or created.
	for _, r := range []struct {
		name   string
		issuer string
		code   connect.Code
	}{
		{"issuer the discovery document doesn't name", idp.URL + "/elsewhere", connect.CodeInvalidArgument},
		{"no discovery document", idp.URL + "/nowhere", connect.CodeFailedPrecondition},
	} {
		_, err := s.Create(ctx, &aigridv1.TenantsCreateRequest{Object: &aigridv1.Tenant{
			Metadata: &aigridv1.Metadata{Name: "beta"},
			Spec:     &aigridv1.TenantSpec{Domains: []string{"beta.example.com"}, IdentityProvider: oidc(r.issuer, "client", "acme-idp", idp.URL+"/keys")},
		}})
		if connect.CodeOf(err) != r.code {
			t.Fatalf("Create(%s) error = %v, want %v", r.name, err, r.code)
		}
		if orgs, err := admin.ListOrganizations(ctx); err != nil || len(orgs) != 1 {
			t.Fatalf("after Create(%s): organizations = %v, %v; want only acme", r.name, orgs, err)
		}
	}

	// The issuer changes in place, under the same alias, once Keycloak reads the new
	// issuer's discovery document.
	moved, err := update(3, oidc(idp.URL+"/second", "client-2", "acme-idp-2", idp.URL+"/keys"))
	if err != nil || moved.GetObject().GetMetadata().GetVersion() != 4 || moved.GetObject().GetSpec().GetIdentityProvider().GetConfig().GetIssuer() != idp.URL+"/second" {
		t.Fatalf("Update(issuer) = %v, %v; want version 4 with the second issuer", moved.GetObject(), err)
	}
	if kcIdP, err := admin.GetIdentityProvider(ctx, "acme"); err != nil || kcIdP.Config["issuer"] != idp.URL+"/second" {
		t.Fatalf("keycloak idp = %+v, %v; want the second issuer", kcIdP.Config, err)
	}
	if org, err := admin.GetOrganization(ctx, id); err != nil || len(org.Domains) != 1 || org.Domains[0].IdentityProviderAlias != "acme" {
		t.Fatalf("organization = %+v, %v; want its domain still routed to acme", org, err)
	}

	// Once a member has signed in through it, the issuer can't change.
	kc.LinkFederatedIdentity("acme", "user-1", "sub-1")
	if _, err := update(4, oidc(idp.URL, "client-2", "acme-idp-2", idp.URL+"/keys")); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Update(issuer with a linked member) error = %v, want FailedPrecondition", err)
	}
	if kcIdP, err := admin.GetIdentityProvider(ctx, "acme"); err != nil || kcIdP.Config["issuer"] != idp.URL+"/second" {
		t.Fatalf("keycloak idp = %+v, %v; want the issuer unchanged", kcIdP.Config, err)
	}

	if _, err := s.Delete(ctx, &aigridv1.TenantsDeleteRequest{Id: id, Version: 4}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := admin.GetIdentityProvider(ctx, "acme"); !keycloak.IsNotFound(err) {
		t.Fatalf("keycloak idp after Delete: %v, want not found", err)
	}
}
