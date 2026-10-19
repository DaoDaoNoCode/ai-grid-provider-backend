package draft

import (
	"context"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

const (
	idAcme   = "3f1c9a72-5b6e-4d2a-8c01-2d3e4f5a6b7c"
	idGlobex = "8d2e7b31-0c4f-4a9d-b5e6-7f8a9b0c1d2e"
)

func seedOrgs() []*aigriddraftv1.Tenant {
	return []*aigriddraftv1.Tenant{
		{Id: idGlobex, Name: "globex-research", Alias: "globex", Enabled: proto.Bool(true), Domains: []string{"globex.example.com"}},
		{Id: idAcme, Name: "acme-robotics", Enabled: proto.Bool(true), Description: "Acme", Attributes: map[string]string{"billing-account": "ACME-0042"},
			DomainDetails: []*aigriddraftv1.TenantDomain{{Name: "Acme.example.com", Verified: true, AutoRedirect: true}, {Name: "*.acme.example.org"}}},
		{Id: "c5a4d3e2-f1b0-4987-a6c5-d4e3f2a1b0c9", Name: "initech-labs", Enabled: proto.Bool(false)},
	}
}

func tenantServer(t *testing.T, tenants ...*aigriddraftv1.Tenant) *Server {
	t.Helper()
	if tenants == nil {
		tenants = seedOrgs()
	}
	s := newServer(t, &Fixtures{Tenants: tenants})
	n := 0
	s.SetIDGenerator(func() string { n++; return "00000000-0000-4000-8000-00000000000" + string(rune('0'+n)) })
	return s
}

func mustCode(t *testing.T, err error, want codes.Code, wantMsg string) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("code = %v (%v), want %v", status.Code(err), err, want)
	}
	if wantMsg != "" && status.Convert(err).Message() != wantMsg {
		t.Fatalf("message = %q, want %q", status.Convert(err).Message(), wantMsg)
	}
}

// TestRepoTenantFixturesLookLikeKeycloak proves the shipped tenants.json is a
// realm Keycloak could hold and covers the shapes the UI must render.
func TestRepoTenantFixturesLookLikeKeycloak(t *testing.T) {
	fx, err := Load(repoFixtures)
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.Tenants) < 5 {
		t.Fatalf("expected at least five organizations, got %d", len(fx.Tenants))
	}
	var disabled, wildcard, noDomains, aliasDiffers, twoDomainsWithAttrs, withIdP int
	for _, tn := range fx.Tenants {
		if tn.GetIdentityProvider() != nil {
			withIdP++
		}
		if !uuidPattern.MatchString(tn.GetId()) {
			t.Errorf("%s: id %q is not a UUID", tn.GetName(), tn.GetId())
		}
		if tn.GetAlias() == "" {
			t.Errorf("%s: the backend always returns alias (Keycloak fills it from the name)", tn.GetName())
		}
		if len(tn.GetDomains()) != len(tn.GetDomainDetails()) {
			t.Errorf("%s: domains and domainDetails must list the same domains", tn.GetName())
		}
		for i, d := range tn.GetDomainDetails() {
			if d.GetName() != tn.GetDomains()[i] || d.GetName() != strings.ToLower(d.GetName()) {
				t.Errorf("%s: domain %d: %q vs %q, must be equal and lower-case", tn.GetName(), i, d.GetName(), tn.GetDomains()[i])
			}
			if strings.HasPrefix(d.GetName(), "*.") {
				wildcard++
			}
		}
		if !tn.GetEnabled() {
			disabled++
		}
		if len(tn.GetDomains()) == 0 {
			noDomains++
		}
		if tn.GetAlias() != tn.GetName() {
			aliasDiffers++
		}
		if len(tn.GetDomains()) == 2 && len(tn.GetAttributes()) > 0 {
			twoDomainsWithAttrs++
		}
	}
	if disabled == 0 || wildcard == 0 || noDomains == 0 || aliasDiffers == 0 || twoDomainsWithAttrs == 0 {
		t.Errorf("fixtures must seed a disabled org, a wildcard domain, an org without domains, an alias different from the name and an org with two domains and attributes: %d %d %d %d %d", disabled, wildcard, noDomains, aliasDiffers, twoDomainsWithAttrs)
	}
	if withIdP == 0 || withIdP == len(fx.Tenants) {
		t.Errorf("fixtures must seed organizations with and without an identity provider (P1): %d of %d have one", withIdP, len(fx.Tenants))
	}
	if _, err := New(fx, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
}

func TestSeedRejectsWhatKeycloakWouldReject(t *testing.T) {
	ok := func(tn ...*aigriddraftv1.Tenant) error {
		_, err := New(&Fixtures{Tenants: tn}, slog.New(slog.DiscardHandler))
		return err
	}
	base := &aigriddraftv1.Tenant{Id: idAcme, Name: "acme", Domains: []string{"acme.example.com"}}
	for name, bad := range map[string][]*aigriddraftv1.Tenant{
		"non-uuid id":      {{Id: "1", Name: "x"}},
		"blank name":       {{Id: idAcme, Name: " "}},
		"duplicate name":   {base, {Id: idGlobex, Name: "acme", Alias: "other"}},
		"duplicate alias":  {base, {Id: idGlobex, Name: "other", Alias: "acme"}},
		"shared domain":    {base, {Id: idGlobex, Name: "other", Domains: []string{"ACME.example.com"}}},
		"bad domain":       {{Id: idAcme, Name: "x", Domains: []string{"not a domain"}}},
		"alias with space": {{Id: idAcme, Name: "x", Alias: "a b"}},
		"details disagree": {{Id: idAcme, Name: "x", Domains: []string{"a.example.com"}, DomainDetails: []*aigriddraftv1.TenantDomain{{Name: "b.example.com"}}}},
		// The fake realm has no identity providers, so no domain can name one.
		"identity provider": {{Id: idAcme, Name: "x", DomainDetails: []*aigriddraftv1.TenantDomain{{Name: "x.example.com", IdentityProviderAlias: "x-oidc"}}}},
	} {
		if err := ok(bad...); err == nil {
			t.Errorf("%s: fixture must be rejected", name)
		}
	}
	if err := ok(base); err != nil {
		t.Errorf("valid fixture rejected: %v", err)
	}
}

// TestValidateDomainUsesKeycloakMessages pins Organizations.validateDomain
// (26.8.0) branch for branch, on canonical (trimmed, lower-case) input.
func TestValidateDomainUsesKeycloakMessages(t *testing.T) {
	cases := map[string]string{
		"":                                       MsgDomainEmpty,
		"   ":                                    MsgDomainEmpty,
		"\u2003":                                 MsgDomainEmpty, // String.isBlank: an em space is whitespace to Java
		"not a domain":                           "Invalid domain format: not a domain",
		"-acme.example.com":                      "Invalid domain format: -acme.example.com",
		"a-.example.com":                         "Invalid domain format: a-.example.com",
		"acme..example.com":                      "Invalid domain format: acme..example.com",
		"acme.example.com.":                      "Invalid domain format: acme.example.com.",
		"acme.*.example.com":                     MsgWildcardNotFirst,
		"acme.*.*.com":                           MsgWildcardNotFirst, // startsWith is checked before the second wildcard
		"*.*.example.com":                        "Multiple wildcards are not allowed: *.*.example.com",
		"*.":                                     "Wildcard domain must specify a base domain: *.",
		"*.com":                                  "Domain must have at least 2 parts (e.g. 'example.com'): com", // the base, not the input
		"*.a.b.c.d.e.f.g.h.i.j.k":                "Domain has too many parts (max 10 allowed): a.b.c.d.e.f.g.h.i.j.k",
		"*.not a domain.com":                     "Invalid domain format: *.not a domain.com",
		"acme.example.com":                       "",
		"acme_robotics.example":                  "Invalid domain format: acme_robotics.example", // Keycloak's pattern has no `_`
		"a+b.example.com":                        "",
		"x'y.example.org":                        "",
		"bücher.example":                         "", // IDN accepted through the non-ASCII range
		"localhost":                              "", // the e-mail rule alone governs plain domains
		"[192.0.2.1]":                            "",
		"[ipv6:2001:db8::1]":                     "", // IP_V6_DOMAIN, matched case-insensitively
		"[ipv6:::ffff:192.0.2.1]":                "",
		"[ipv6:2001:db8::zz]":                    "Invalid domain format: [ipv6:2001:db8::zz]",
		"[2001:db8::1]":                          "Invalid domain format: [2001:db8::1]", // the IPv6: tag is required
		"*acme.example.com":                      "",                                     // no `*.`: an ordinary e-mail domain with a `*` in a label
		"ac*me.example.com":                      "",
		"acme*.example.com":                      MsgWildcardNotFirst, // contains `*.` (star then dot): a wildcard that does not start with one
		"*.acme.example.org":                     "",
		"a.b.c.d.e.f.g.h.i.j.k":                  "", // the parts rule is for wildcards only
		strings.Repeat("a", 63) + ".example.com": "",
		strings.Repeat("a", 64) + ".example.com": "Invalid domain format: " + strings.Repeat("a", 64) + ".example.com",
		// java.net.IDN splits labels at U+3002 too, so this is two labels of
		// 40 for the 63 cap; the pattern accepts U+3002 as a label character.
		strings.Repeat("a", 40) + "\u3002" + strings.Repeat("b", 40) + ".com": "",
		strings.Repeat("a.", 126) + strings.Repeat("b", 4):                    "Invalid domain format: " + strings.Repeat("a.", 126) + strings.Repeat("b", 4), // 256 chars
		strings.Repeat("a.", 126) + strings.Repeat("b", 3):                    "",                                                                             // 255 chars
	}
	for domain, want := range cases {
		err := validateDomain(domain)
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: unexpected %v", domain, err)
		case want != "" && (err == nil || err.Error() != want):
			t.Errorf("%q: got %v, want %q", domain, err, want)
		}
	}
}

// TestNormalizeDomainsCanonicalisesFirst: OrganizationDomainModel trims and
// lower-cases before anything looks at the name, so messages print the
// canonical form and case variants collapse.
func TestNormalizeDomainsCanonicalisesFirst(t *testing.T) {
	tn := &aigriddraftv1.Tenant{Domains: []string{" Acme.Example.com ", "ACME.example.COM", "*.Acme.Example.ORG"}}
	if err := normalizeDomains(tn); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tn.GetDomains(), ",") != "acme.example.com,*.acme.example.org" {
		t.Errorf("domains = %v", tn.GetDomains())
	}
	err := normalizeDomains(&aigriddraftv1.Tenant{Domains: []string{"Not A Domain"}})
	if err == nil || err.Error() != "Invalid domain format: not a domain" {
		t.Errorf("message must print the canonical name, got %v", err)
	}
	err = normalizeDomains(&aigriddraftv1.Tenant{Domains: []string{" *.COM "}})
	if err == nil || err.Error() != "Domain must have at least 2 parts (e.g. 'example.com'): com" {
		t.Errorf("wildcard parts message on canonical base, got %v", err)
	}
	if err := normalizeDomains(&aigriddraftv1.Tenant{Domains: []string{"a.example.com"}, DomainDetails: []*aigriddraftv1.TenantDomain{{Name: " A.EXAMPLE.COM "}}}); err != nil {
		t.Errorf("names and details are compared canonically: %v", err)
	}
	// String.trim() strips every character up to U+0020, control
	// characters included, and nothing above it.
	tn = &aigriddraftv1.Tenant{Domains: []string{"\x01Acme.Example.com\x1f\t"}}
	if err := normalizeDomains(tn); err != nil || tn.GetDomains()[0] != "acme.example.com" {
		t.Errorf("control characters are trimmed like Java's trim: %v, %v", tn.GetDomains(), err)
	}
}

func TestValidateAliasUsesKeycloakMessages(t *testing.T) {
	cases := map[string]string{
		"acme": "", "acme-robotics_2": "", "acme robotics": MsgEmptySpace, "acme\trobotics": MsgEmptySpace, "a\vb": MsgEmptySpace,
		"acme/eu": "Character '/' not allowed.", "a@b": "Character '@' not allowed.", `a\b`: `Character '\' not allowed.`,
		"a?b#c": "Character '?' not allowed.", // the first reserved character is named
		"a/b c": MsgEmptySpace,                // whitespace is looked for in the whole alias first
		// java.util.regex \s is ASCII only: Unicode spaces pass.
		"a\u00a0b": "", "a\u2003b": "", "a\u0085b": "",
	}
	for alias, want := range cases {
		err := validateAlias(alias)
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: unexpected %v", alias, err)
		case want != "" && (err == nil || err.Error() != want):
			t.Errorf("%q: got %v, want %q", alias, err, want)
		}
	}
}

func TestListTenants(t *testing.T) {
	s := tenantServer(t)
	ctx := authed(context.Background())
	list := func(req *aigriddraftv1.ListTenantsRequest) *aigriddraftv1.ListTenantsResponse {
		t.Helper()
		resp, err := s.ListTenants(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	all := list(&aigriddraftv1.ListTenantsRequest{})
	if names := tenantNames(all); strings.Join(names, ",") != "acme-robotics,globex-research,initech-labs" || all.GetSize() != 3 || all.GetTotal() != 3 {
		t.Fatalf("list = %v size %d total %d", names, all.GetSize(), all.GetTotal())
	}
	acme := all.GetTenants()[0]
	if acme.GetAlias() != "acme-robotics" || len(acme.GetDomains()) != 2 || acme.GetDomains()[0] != "acme.example.com" || acme.GetDomainDetails()[0].GetName() != "acme.example.com" || !acme.GetDomainDetails()[0].GetVerified() {
		t.Errorf("alias defaults to the name, domains are lower-cased and mirrored from the details: %v", acme)
	}
	if page := list(&aigriddraftv1.ListTenantsRequest{Offset: proto.Int32(1), Limit: proto.Int32(1)}); strings.Join(tenantNames(page), ",") != "globex-research" || page.GetSize() != 1 || page.GetTotal() != 3 {
		t.Errorf("page = %v", page)
	}
	if page := list(&aigriddraftv1.ListTenantsRequest{Limit: proto.Int32(0)}); len(page.GetTenants()) != 0 || page.GetTotal() != 3 {
		t.Errorf("limit 0 counts only: %v", page)
	}
	if page := list(&aigriddraftv1.ListTenantsRequest{Offset: proto.Int32(10)}); len(page.GetTenants()) != 0 || page.GetTotal() != 3 {
		t.Errorf("offset past the end: %v", page)
	}
	// The form the BFF used to write, a case-insensitive contains on the
	// name or a domain, is plain CEL and still works; total follows the filter.
	filter := func(term string) *string {
		return proto.String(`this.name.lowerAscii().contains("` + term + `") || this.domains.exists(d, d.lowerAscii().contains("` + term + `"))`)
	}
	if page := list(&aigriddraftv1.ListTenantsRequest{Filter: filter("globex")}); strings.Join(tenantNames(page), ",") != "globex-research" || page.GetTotal() != 1 {
		t.Errorf("search by name = %v", page)
	}
	if page := list(&aigriddraftv1.ListTenantsRequest{Filter: filter("example.org")}); strings.Join(tenantNames(page), ",") != "acme-robotics" {
		t.Errorf("search by domain = %v", page)
	}
	if page := list(&aigriddraftv1.ListTenantsRequest{Filter: filter("")}); page.GetTotal() != 3 {
		t.Errorf("contains(\"\") is true for every tenant: %v", page)
	}
	// Strict CEL: the term is compared as written. An upper-case term
	// matches nothing (the caller lowers it, as the BFF used to), and a
	// whitespace term is a literal substring, not Keycloak's blank search.
	if page := list(&aigriddraftv1.ListTenantsRequest{Filter: filter("GLOBEX")}); page.GetTotal() != 0 {
		t.Errorf("lowerAscii() lowers the name, not the term: %v", page)
	}
	if page := list(&aigriddraftv1.ListTenantsRequest{Filter: filter("  ")}); page.GetTotal() != 0 {
		t.Errorf("a whitespace term is a substring like any other: %v", page)
	}
	if page := list(&aigriddraftv1.ListTenantsRequest{Filter: filter(`say \"hi\"`)}); page.GetTotal() != 0 {
		t.Errorf("escaped term parses: %v", page)
	}
	// Paging applies after filtering, and total counts the filtered set.
	if page := list(&aigriddraftv1.ListTenantsRequest{Filter: proto.String(`this.name.contains("-")`), Offset: proto.Int32(1), Limit: proto.Int32(1)}); strings.Join(tenantNames(page), ",") != "globex-research" || page.GetSize() != 1 || page.GetTotal() != 3 {
		t.Errorf("page of the filtered list = %v", page)
	}
	if page := list(&aigriddraftv1.ListTenantsRequest{Filter: proto.String(`this.enabled`), Offset: proto.Int32(1)}); strings.Join(tenantNames(page), ",") != "globex-research" || page.GetTotal() != 2 {
		t.Errorf("offset into the filtered list = %v", page)
	}
	// `%` and `_` are literal characters of the term, never LIKE wildcards
	// (the browser mock runs the same cases).
	if _, err := s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "under_score", Domains: []string{"100%.example.com"}}}); err != nil {
		t.Fatal(err)
	}
	for term, want := range map[string]string{"r_s": "under_score", "r%s": "", "_": "under_score", "%": "under_score", "100%": "under_score", "a_c": "", "%ex%": ""} {
		page := list(&aigriddraftv1.ListTenantsRequest{Filter: filter(term)})
		if got := strings.Join(tenantNames(page), ","); got != want {
			t.Errorf("literal search %q = %q, want %q", term, got, want)
		}
	}
	if _, err := s.DeleteTenant(ctx, &aigriddraftv1.DeleteTenantRequest{Id: "00000000-0000-4000-8000-000000000001"}); err != nil {
		t.Fatal(err)
	}
	// Any CEL over the Tenant message, proto field names, the strings
	// extension and `now`.
	for expr, want := range map[string]string{
		`this.name == "initech-labs"`:                                             "initech-labs",
		`this.alias != this.name`:                                                 "globex-research",
		`!this.enabled`:                                                           "initech-labs",
		`this.attributes["billing-account"] == "ACME-0042"`:                       "", // initech-labs and globex have no attributes: an error, below
		`"billing-account" in this.attributes`:                                    "acme-robotics",
		`this.domain_details.exists(d, d.verified && d.auto_redirect)`:            "acme-robotics",
		`this.domains.size() == 0`:                                                "initech-labs",
		`this.name.upperAscii().startsWith("GLOBEX")`:                             "globex-research",
		`this.resource_version == "1" && now > timestamp("2026-01-01T00:00:00Z")`: "acme-robotics,globex-research,initech-labs",
	} {
		if want == "" {
			continue
		}
		page := list(&aigriddraftv1.ListTenantsRequest{Filter: proto.String(expr)})
		if got := strings.Join(tenantNames(page), ","); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
	// Errors are InvalidArgument naming the filter: a syntax or type error, a
	// result that is not a bool, and an evaluation error such as indexing a
	// missing key (CEL has no "false on error"; the `in` guard avoids it).
	for bad, want := range map[string]string{
		`this.name ==`:             "filter this.name == is not a valid CEL expression: ",
		`this.nope == "a"`:         `filter this.nope == "a" is not a valid CEL expression: `,
		`name.contains("a")`:       `filter name.contains("a") is not a valid CEL expression: `,
		`this.name`:                "filter this.name must evaluate to a bool, not string",
		`size(this.domains)`:       "filter size(this.domains) must evaluate to a bool, not int",
		`this.name.contains("\q")`: `filter this.name.contains("\q") is not a valid CEL expression: `,
		`this.attributes["billing-account"] == "ACME-0042"`: `filter this.attributes["billing-account"] == "ACME-0042" failed on '` + idGlobex + `': no such key: billing-account`,
	} {
		_, err := s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{Filter: proto.String(bad)})
		if status.Code(err) != codes.InvalidArgument || !strings.HasPrefix(status.Convert(err).Message(), want) {
			t.Errorf("filter %s = %v, want InvalidArgument starting %q", bad, err, want)
		}
	}
	if _, err := s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{Order: proto.String("name desc")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("order = %v", err)
	}
	if _, err := s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{Limit: proto.Int32(-1)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("negative limit = %v", err)
	}
	all.GetTenants()[0].Domains[0] = "mutated"
	if again := list(&aigriddraftv1.ListTenantsRequest{}); again.GetTenants()[0].GetDomains()[0] != "acme.example.com" {
		t.Error("responses must be copies of the store")
	}
}

func tenantNames(resp *aigriddraftv1.ListTenantsResponse) []string {
	names := make([]string, 0, len(resp.GetTenants()))
	for _, tn := range resp.GetTenants() {
		names = append(names, tn.GetName())
	}
	return names
}

func TestGetTenant(t *testing.T) {
	s := tenantServer(t)
	ctx := authed(context.Background())
	got, err := s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: idGlobex})
	if err != nil || got.GetObject().GetName() != "globex-research" || got.GetObject().GetAlias() != "globex" {
		t.Fatalf("get = %v, %v", got, err)
	}
	_, err = s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: "11111111-2222-4333-8444-555555555555"})
	mustCode(t, err, codes.NotFound, "object with identifier '11111111-2222-4333-8444-555555555555' not found")
	_, err = s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{})
	mustCode(t, err, codes.InvalidArgument, "")
}

func TestCreateTenant(t *testing.T) {
	ctx := authed(context.Background())
	create := func(s *Server, obj *aigriddraftv1.Tenant) (*aigriddraftv1.Tenant, error) {
		resp, err := s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: obj})
		return resp.GetObject(), err
	}

	t.Run("defaults and normalisation", func(t *testing.T) {
		s := tenantServer(t)
		got, err := create(s, &aigriddraftv1.Tenant{Name: "wayne-aerospace", Enabled: proto.Bool(true), Domains: []string{"Wayne-Aero.Example.com"}})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetId() != "00000000-0000-4000-8000-000000000001" || got.GetAlias() != "wayne-aerospace" || !got.GetEnabled() || got.GetDomains()[0] != "wayne-aero.example.com" || got.GetDomainDetails()[0].GetVerified() {
			t.Errorf("created = %v", got)
		}
		if back, err := s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: got.GetId()}); err != nil || !proto.Equal(back.GetObject(), got) {
			t.Errorf("the created object is what Get returns: %v, %v", back, err)
		}
		if list, _ := s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{}); list.GetTotal() != 4 {
			t.Errorf("total = %d", list.GetTotal())
		}
		got2, err := create(s, &aigriddraftv1.Tenant{Name: "Stark Industries", Alias: "stark", Description: "d", Enabled: proto.Bool(false), Attributes: map[string]string{"k": "v"},
			DomainDetails: []*aigriddraftv1.TenantDomain{{Name: " Stark.Example.com ", Verified: true, AutoRedirect: true}}})
		if err != nil || got2.GetAlias() != "stark" || got2.GetEnabled() || got2.GetDomains()[0] != "stark.example.com" || !got2.GetDomainDetails()[0].GetAutoRedirect() || got2.GetAttributes()["k"] != "v" {
			t.Errorf("explicit alias, details and attributes kept, names canonicalised; enabled false stays false as Keycloak stores what it is sent: %v, %v", got2, err)
		}
		got3, err := create(s, &aigriddraftv1.Tenant{Name: "omitted-enabled"})
		if err != nil || got3.Enabled == nil || !got3.GetEnabled() {
			t.Errorf("an omitted enabled is Keycloak's default true: %v, %v", got3, err)
		}
		got4, err := create(s, &aigriddraftv1.Tenant{Name: "star", Domains: []string{"*acme.example.com", "ac*me.example.com"}})
		if err != nil || len(got4.GetDomains()) != 2 {
			t.Errorf("a star inside a label is an ordinary e-mail domain character: %v, %v", got4, err)
		}
		// U+2003 is not java.util.regex \s, so the explicit alias passes
		// validateNoSpace, but it is blank to StringUtil.isBlank, so the
		// alias defaults to the name.
		got5, err := create(s, &aigriddraftv1.Tenant{Name: "em-space", Alias: "\u2003"})
		if err != nil || got5.GetAlias() != "em-space" {
			t.Errorf("a Unicode-blank alias defaults to the name: %v, %v", got5, err)
		}
	})

	t.Run("keycloak validation messages", func(t *testing.T) {
		s := tenantServer(t)
		cases := []struct {
			name string
			obj  *aigriddraftv1.Tenant
			code codes.Code
			msg  string
		}{
			{"blank name", &aigriddraftv1.Tenant{Name: " "}, codes.InvalidArgument, MsgNameRequired},
			{"unicode-blank name", &aigriddraftv1.Tenant{Name: "\u2003\x1c"}, codes.InvalidArgument, MsgNameRequired},
			{"no object", nil, codes.InvalidArgument, ""},
			{"id given", &aigriddraftv1.Tenant{Id: idAcme, Name: "x"}, codes.InvalidArgument, ""},
			{"name with space and no alias", &aigriddraftv1.Tenant{Name: "Acme Robotics EU"}, codes.InvalidArgument, "Name cannot be used as alias: Empty Space not allowed."},
			{"name with slash and no alias", &aigriddraftv1.Tenant{Name: "acme/eu"}, codes.InvalidArgument, "Name cannot be used as alias: Character '/' not allowed."},
			{"explicit alias with space", &aigriddraftv1.Tenant{Name: "x", Alias: "acme eu"}, codes.InvalidArgument, MsgEmptySpace},
			{"explicit alias with reserved char", &aigriddraftv1.Tenant{Name: "x", Alias: "acme?eu"}, codes.InvalidArgument, "Character '?' not allowed."},
			{"duplicate name", &aigriddraftv1.Tenant{Name: "acme-robotics", Alias: "other"}, codes.AlreadyExists, MsgDuplicateName},
			{"duplicate alias", &aigriddraftv1.Tenant{Name: "other", Alias: "globex"}, codes.AlreadyExists, MsgDuplicateAlias},
			{"name equal to another alias", &aigriddraftv1.Tenant{Name: "globex"}, codes.AlreadyExists, MsgDuplicateAlias},
			{"bad domain", &aigriddraftv1.Tenant{Name: "x", Domains: []string{"Not A Domain"}}, codes.InvalidArgument, "Invalid domain format: not a domain"},
			{"bad wildcard", &aigriddraftv1.Tenant{Name: "x", Domains: []string{"*.com"}}, codes.InvalidArgument, "Domain must have at least 2 parts (e.g. 'example.com'): com"},
			{"duplicate name wins over a bad domain", &aigriddraftv1.Tenant{Name: "acme-robotics", Alias: "other", Domains: []string{"not a domain"}}, codes.AlreadyExists, MsgDuplicateName},
			{"duplicate alias wins over a bad domain", &aigriddraftv1.Tenant{Name: "other", Alias: "globex", Domains: []string{"not a domain"}}, codes.AlreadyExists, MsgDuplicateAlias},
			{"explicit alias chars win over a duplicate name", &aigriddraftv1.Tenant{Name: "acme-robotics", Alias: "a b"}, codes.InvalidArgument, MsgEmptySpace},
			{"blank name wins over a bad domain", &aigriddraftv1.Tenant{Name: "", Domains: []string{"not a domain"}}, codes.InvalidArgument, MsgNameRequired},
			{"empty domain", &aigriddraftv1.Tenant{Name: "x", Domains: []string{""}}, codes.InvalidArgument, MsgDomainEmpty},
			{"domain owned by another org", &aigriddraftv1.Tenant{Name: "x", Domains: []string{"GLOBEX.example.com"}}, codes.InvalidArgument, "Domain globex.example.com is already linked to organization globex-research in realm ai-grid"},
			{"details disagree with names", &aigriddraftv1.Tenant{Name: "x", Domains: []string{"a.example.com"}, DomainDetails: []*aigriddraftv1.TenantDomain{{Name: "b.example.com"}}}, codes.InvalidArgument, ""},
			{"explicit alias: whitespace before a reserved char", &aigriddraftv1.Tenant{Name: "x", Alias: "a/b c"}, codes.InvalidArgument, MsgEmptySpace},
			{"name as alias: whitespace before a reserved char", &aigriddraftv1.Tenant{Name: "a/b c"}, codes.InvalidArgument, "Name cannot be used as alias: Empty Space not allowed."},
			{"unknown identity provider", &aigriddraftv1.Tenant{Name: "x", DomainDetails: []*aigriddraftv1.TenantDomain{{Name: "x.example.com", IdentityProviderAlias: "x-oidc"}}}, codes.InvalidArgument, "Identity provider with alias 'x-oidc' does not exist in realm ai-grid"},
			{"bad domain wins over an identity provider", &aigriddraftv1.Tenant{Name: "x", DomainDetails: []*aigriddraftv1.TenantDomain{{Name: "x.example.com", IdentityProviderAlias: "x-oidc"}, {Name: "not a domain"}}}, codes.InvalidArgument, "Invalid domain format: not a domain"},
			{"owned domain wins over an identity provider", &aigriddraftv1.Tenant{Name: "x", DomainDetails: []*aigriddraftv1.TenantDomain{{Name: "x.example.com", IdentityProviderAlias: "x-oidc"}, {Name: "globex.example.com"}}}, codes.InvalidArgument, "Domain globex.example.com is already linked to organization globex-research in realm ai-grid"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := create(s, tc.obj)
				mustCode(t, err, tc.code, tc.msg)
			})
		}
		if list, _ := s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{}); list.GetTotal() != 3 {
			t.Errorf("rejected creates must not change the store, total = %d", list.GetTotal())
		}
	})

	t.Run("duplicate domains within the request collapse", func(t *testing.T) {
		s := tenantServer(t)
		got, err := create(s, &aigriddraftv1.Tenant{Name: "x", Domains: []string{"x.example.com", " X.example.com "}})
		if err != nil || len(got.GetDomains()) != 1 {
			t.Errorf("Keycloak keeps a set of domains: %v, %v", got, err)
		}
	})

	t.Run("concurrent creates of the same name admit one", func(t *testing.T) {
		s := tenantServer(t)
		const n = 8
		var wg sync.WaitGroup
		results := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, results[i] = s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "raced", Domains: []string{"raced.example.com"}}})
			}()
		}
		wg.Wait()
		ok, exists := 0, 0
		for _, err := range results {
			switch status.Code(err) {
			case codes.OK:
				ok++
			case codes.AlreadyExists:
				exists++
			default:
				t.Errorf("unexpected %v", err)
			}
		}
		if ok != 1 || exists != n-1 {
			t.Errorf("ok=%d exists=%d, want 1 and %d", ok, exists, n-1)
		}
	})
}

func TestUpdateTenant(t *testing.T) {
	ctx := authed(context.Background())
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
	update := func(s *Server, obj *aigriddraftv1.Tenant, m *fieldmaskpb.FieldMask) (*aigriddraftv1.Tenant, error) {
		resp, err := s.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{Object: obj, UpdateMask: m})
		return resp.GetObject(), err
	}

	t.Run("mask merge", func(t *testing.T) {
		s := tenantServer(t)
		got, err := update(s, &aigriddraftv1.Tenant{Id: idAcme, Enabled: proto.Bool(false), Name: "ignored"}, mask(PathEnabled))
		if err != nil {
			t.Fatal(err)
		}
		if got.GetEnabled() || got.GetName() != "acme-robotics" || got.GetDescription() != "Acme" || len(got.GetDomains()) != 2 || got.GetAttributes()["billing-account"] != "ACME-0042" {
			t.Errorf("only enabled changes: %v", got)
		}
		got, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Name: "acme-robotics-eu", Description: "", Attributes: map[string]string{}}, mask(PathName, PathDescription, PathAttributes))
		if err != nil {
			t.Fatal(err)
		}
		if got.GetName() != "acme-robotics-eu" || got.GetDescription() != "" || len(got.GetAttributes()) != 0 || got.GetAlias() != "acme-robotics" || len(got.GetDomains()) != 2 {
			t.Errorf("name, description cleared, attributes cleared by {}, alias and domains untouched: %v", got)
		}
	})

	t.Run("domains path keeps the metadata of a domain that stays", func(t *testing.T) {
		s := tenantServer(t)
		got, err := update(s, &aigriddraftv1.Tenant{Id: idAcme, Domains: []string{"ACME.example.com", "new.example.com"}}, mask(PathDomains))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.GetDomainDetails()) != 2 || !got.GetDomainDetails()[0].GetVerified() || !got.GetDomainDetails()[0].GetAutoRedirect() || got.GetDomainDetails()[1].GetName() != "new.example.com" || got.GetDomainDetails()[1].GetVerified() || got.GetDomainDetails()[1].GetAutoRedirect() {
			t.Errorf("details = %v", got.GetDomainDetails())
		}
		if strings.Join(got.GetDomains(), ",") != "acme.example.com,new.example.com" {
			t.Errorf("domains = %v", got.GetDomains())
		}
		// A name submitted with surrounding whitespace and a different case is
		// the same domain (OrganizationDomainModel canonicalises it), so its
		// verified flag and redirect survive.
		got, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Domains: []string{"  ACME.Example.COM  ", "*.ACME.example.org"}}, mask(PathDomains))
		if err != nil {
			t.Fatal(err)
		}
		if d := got.GetDomainDetails()[0]; d.GetName() != "acme.example.com" || !d.GetVerified() || !d.GetAutoRedirect() {
			t.Errorf("metadata lost on a non-canonical resubmission: %v", d)
		}
		got, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Domains: []string{}}, mask(PathDomains))
		if err != nil || len(got.GetDomains()) != 0 || len(got.GetDomainDetails()) != 0 {
			t.Errorf("[] clears the domains: %v, %v", got, err)
		}
	})

	t.Run("domain_details path replaces metadata", func(t *testing.T) {
		s := tenantServer(t)
		got, err := update(s, &aigriddraftv1.Tenant{Id: idAcme, DomainDetails: []*aigriddraftv1.TenantDomain{{Name: "acme.example.com", Verified: false}}}, mask(PathDomainDetails))
		if err != nil || len(got.GetDomainDetails()) != 1 || got.GetDomainDetails()[0].GetVerified() || got.GetDomainDetails()[0].GetAutoRedirect() || len(got.GetDomains()) != 1 {
			t.Errorf("details = %v, %v", got, err)
		}
		_, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, DomainDetails: []*aigriddraftv1.TenantDomain{{Name: "acme.example.com", IdentityProviderAlias: "acme-oidc"}}}, mask(PathDomainDetails))
		mustCode(t, err, codes.InvalidArgument, "Identity provider with alias 'acme-oidc' does not exist in realm ai-grid")
	})

	t.Run("no mask is a keycloak PUT without attributes and domains", func(t *testing.T) {
		s := tenantServer(t)
		if _, err := update(s, &aigriddraftv1.Tenant{Id: idAcme, Enabled: proto.Bool(false)}, mask(PathEnabled)); err != nil {
			t.Fatal(err)
		}
		got, err := update(s, &aigriddraftv1.Tenant{Id: idAcme, Name: "acme-2"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.GetName() != "acme-2" || got.GetDescription() != "" || !got.GetEnabled() || len(got.GetDomains()) != 2 || got.GetAttributes()["billing-account"] != "ACME-0042" {
			t.Errorf("scalars replaced, an omitted enabled re-enables, collections kept: %v", got)
		}
		if got, err := update(s, &aigriddraftv1.Tenant{Id: idAcme, Name: "acme-2", Enabled: proto.Bool(false)}, nil); err != nil || got.GetEnabled() {
			t.Errorf("an explicit false is kept: %v, %v", got, err)
		}
		// The backend's PUT carries the stored alias: a rename of an
		// organization whose alias differs from its name succeeds without
		// `alias` (a bare Keycloak PUT would default it to the new name and
		// answer "Cannot change the alias").
		if got, err := update(s, &aigriddraftv1.Tenant{Id: idGlobex, Name: "globex-labs"}, nil); err != nil || got.GetAlias() != "globex" || got.GetName() != "globex-labs" {
			t.Errorf("omitted alias keeps the stored one: %v, %v", got, err)
		}
		_, err = update(s, &aigriddraftv1.Tenant{Id: idGlobex, Name: "globex-labs", Alias: "globex-labs"}, nil)
		mustCode(t, err, codes.InvalidArgument, MsgAliasImmutable)
		if got, err := update(s, &aigriddraftv1.Tenant{Id: idGlobex, Name: "globex-labs", Alias: "  "}, nil); err != nil || got.GetAlias() != "globex" {
			t.Errorf("a blank alias is an omitted one: %v, %v", got, err)
		}
		// An omitted name is a blank name in the PUT: 409, see below.
		_, err = update(s, &aigriddraftv1.Tenant{Id: idGlobex}, nil)
		mustCode(t, err, codes.AlreadyExists, MsgDuplicateName)
	})

	t.Run("keycloak rejections", func(t *testing.T) {
		s := tenantServer(t)
		_, err := update(s, &aigriddraftv1.Tenant{Id: idAcme, Alias: "acme"}, mask(PathEnabled))
		mustCode(t, err, codes.InvalidArgument, MsgAliasImmutable)
		if got, err := update(s, &aigriddraftv1.Tenant{Id: idAcme, Alias: "acme-robotics", Enabled: proto.Bool(true)}, mask(PathEnabled)); err != nil || !got.GetEnabled() {
			t.Errorf("the same alias is accepted: %v, %v", got, err)
		}
		_, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Name: "globex-research"}, mask(PathName))
		mustCode(t, err, codes.AlreadyExists, MsgDuplicateName)
		_, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Name: "globex.example.com"}, mask(PathName))
		mustCode(t, err, codes.AlreadyExists, MsgDuplicateName) // exact search matches domains too
		_, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Name: "acme.example.com"}, mask(PathName))
		mustCode(t, err, codes.AlreadyExists, MsgDuplicateName) // ... including the organization's own domains
		if got, err := update(s, &aigriddraftv1.Tenant{Id: idAcme, Name: "acme-robotics"}, mask(PathName)); err != nil || got.GetName() != "acme-robotics" {
			t.Errorf("keeping the name is not a conflict: %v, %v", got, err)
		}
		// A blank name differs from the stored one and Keycloak's exact search
		// treats a blank term as no filter, so the organization finds itself:
		// 409, not "Name can not be null" (OrganizationResource L123-125).
		_, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Name: ""}, mask(PathName))
		mustCode(t, err, codes.AlreadyExists, MsgDuplicateName)
		_, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Name: " \u2003"}, mask(PathName))
		mustCode(t, err, codes.AlreadyExists, MsgDuplicateName)
		_, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Domains: []string{"globex.example.com"}}, mask(PathDomains))
		mustCode(t, err, codes.InvalidArgument, "Domain globex.example.com is already linked to organization globex-research in realm ai-grid")
		_, err = update(s, &aigriddraftv1.Tenant{Id: idAcme, Domains: []string{"Bad Domain"}}, mask(PathDomains))
		mustCode(t, err, codes.InvalidArgument, "Invalid domain format: bad domain")
		_, err = update(s, &aigriddraftv1.Tenant{Id: idAcme}, mask("redirect_url"))
		mustCode(t, err, codes.InvalidArgument, "")
		if !strings.Contains(status.Convert(err).Message(), `"redirect_url"`) {
			t.Errorf("bad path must be named: %v", err)
		}
		_, err = update(s, &aigriddraftv1.Tenant{Name: "x"}, mask(PathName))
		mustCode(t, err, codes.InvalidArgument, "")
		_, err = update(s, &aigriddraftv1.Tenant{Id: "11111111-2222-4333-8444-555555555555", Name: "x"}, mask(PathName))
		mustCode(t, err, codes.NotFound, "object with identifier '11111111-2222-4333-8444-555555555555' not found")
		if got, _ := s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: idAcme}); got.GetObject().GetName() != "acme-robotics" || len(got.GetObject().GetDomains()) != 2 {
			t.Errorf("rejected updates leave the store alone: %v", got)
		}
	})
}

// TestTenantCreateTime: the backend stamps a tenant's creation time on
// create (P3) and keeps it through updates; a client can't set it, neither
// as the field nor as the attribute the backend keeps it in, and a seeded
// organization without one has none.
func TestTenantCreateTime(t *testing.T) {
	s := tenantServer(t)
	now := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	ctx := authed(context.Background())
	created, err := s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "hooli",
		CreateTime: timestamppb.New(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)), Attributes: map[string]string{"tier": "gold"}}})
	if err != nil {
		t.Fatal(err)
	}
	c := created.GetObject()
	if !c.GetCreateTime().AsTime().Equal(now) || len(c.GetAttributes()) != 1 {
		t.Fatalf("created %v", c)
	}
	resp, err := s.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{
		Object:     &aigriddraftv1.Tenant{Id: c.GetId(), Attributes: map[string]string{}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathAttributes}},
	})
	if err != nil || !resp.GetObject().GetCreateTime().AsTime().Equal(now) {
		t.Fatalf("attributes replaced: %v %v", resp, err)
	}
	_, err = s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "sneaky",
		Attributes: map[string]string{TenantCreateTimeAttribute: "2001-01-01T00:00:00Z"}}})
	mustCode(t, err, codes.InvalidArgument, `attribute "ai-grid/create-time" is reserved for the tenant's creation time; it cannot be set`)
	got, err := s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: idGlobex})
	if err != nil || got.GetObject().GetCreateTime() != nil {
		t.Fatalf("a seeded organization without a creation time: %v %v", got, err)
	}
}

func TestDeleteTenant(t *testing.T) {
	s := tenantServer(t)
	ctx := authed(context.Background())
	if _, err := s.DeleteTenant(ctx, &aigriddraftv1.DeleteTenantRequest{Id: idGlobex}); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: idGlobex})
	mustCode(t, err, codes.NotFound, "")
	_, err = s.DeleteTenant(ctx, &aigriddraftv1.DeleteTenantRequest{Id: idGlobex})
	mustCode(t, err, codes.NotFound, "object with identifier '"+idGlobex+"' not found")
	_, err = s.DeleteTenant(ctx, &aigriddraftv1.DeleteTenantRequest{})
	mustCode(t, err, codes.InvalidArgument, "")
	// The domain is free again.
	if _, err := s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "globex-2", Domains: []string{"globex.example.com"}}}); err != nil {
		t.Errorf("domain of a deleted org can be claimed: %v", err)
	}
}

// TestTenantMutationsNeedAProviderAdmin: the backend gates Keycloak writes
// with the same provider-admin check as hub writes; reads need only a token.
func TestTenantMutationsNeedAProviderAdmin(t *testing.T) {
	access := Access{DefaultAllowed: true, DeniedTokens: []string{"viewer-token"}, RejectedTokens: []string{"expired-token"}}
	s := newServer(t, &Fixtures{Tenants: seedOrgs(), Access: access})
	as := func(token, user string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token, "x-forwarded-user", user))
	}
	viewer := as("viewer-token", "viewer")
	want := `User "viewer" is not a provider admin, and only provider admins can change tenants (a provider admin may patch ManagedClusters on the hub)`
	_, err := s.CreateTenant(viewer, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "x"}})
	mustCode(t, err, codes.PermissionDenied, want)
	_, err = s.UpdateTenant(viewer, &aigriddraftv1.UpdateTenantRequest{Object: &aigriddraftv1.Tenant{Id: idAcme, Enabled: proto.Bool(false)}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathEnabled}}})
	mustCode(t, err, codes.PermissionDenied, want)
	_, err = s.DeleteTenant(viewer, &aigriddraftv1.DeleteTenantRequest{Id: idAcme})
	mustCode(t, err, codes.PermissionDenied, want)
	if list, err := s.ListTenants(viewer, &aigriddraftv1.ListTenantsRequest{}); err != nil || list.GetTotal() != 3 {
		t.Errorf("viewer list = %v, %v", list, err)
	}
	if _, err := s.GetTenant(viewer, &aigriddraftv1.GetTenantRequest{Id: idAcme}); err != nil {
		t.Errorf("viewer get = %v", err)
	}
	_, err = s.DeleteTenant(as("expired-token", "ghost"), &aigriddraftv1.DeleteTenantRequest{Id: idAcme})
	mustCode(t, err, codes.Unauthenticated, "")
	anon := context.Background()
	for name, call := range map[string]func() error{
		"get": func() error { _, err := s.GetTenant(anon, &aigriddraftv1.GetTenantRequest{Id: idAcme}); return err },
		"create": func() error {
			_, err := s.CreateTenant(anon, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "x"}})
			return err
		},
		"update": func() error {
			_, err := s.UpdateTenant(anon, &aigriddraftv1.UpdateTenantRequest{Object: &aigriddraftv1.Tenant{Id: idAcme}})
			return err
		},
		"delete": func() error {
			_, err := s.DeleteTenant(anon, &aigriddraftv1.DeleteTenantRequest{Id: idAcme})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s without authorization = %v, want Unauthenticated", name, err)
		}
	}
	if _, err := s.DeleteTenant(as("dev-token", "dev-user"), &aigriddraftv1.DeleteTenantRequest{Id: idAcme}); err != nil {
		t.Errorf("admin delete = %v", err)
	}
}

// TestTenantsErrorModes: the three Keycloak failure modes the fixtures can
// select (unavailable, realmMissing, organizationsDisabled), mapped with the
// conventions' codes.
func TestTenantsErrorModes(t *testing.T) {
	ctx := authed(context.Background())
	down, err := New(&Fixtures{TenantsUnavailable: "request Keycloak token: dial tcp 192.0.2.1:8080: connect: connection refused", Access: Access{DefaultAllowed: true}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	_, err = down.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{})
	mustCode(t, err, codes.Unavailable, "Keycloak is unavailable: request Keycloak token: dial tcp 192.0.2.1:8080: connect: connection refused")
	_, err = down.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "x"}})
	mustCode(t, err, codes.Unavailable, "")
	// Keycloak's own validation cannot run while Keycloak is down: the real
	// backend only learns about a bad domain or alias from Keycloak.
	_, err = down.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "x", Alias: "a b", Domains: []string{"not a domain"}}})
	mustCode(t, err, codes.Unavailable, "")
	_, err = down.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{Object: &aigriddraftv1.Tenant{Id: idAcme, Alias: "changed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathEnabled}}})
	mustCode(t, err, codes.Unavailable, "")
	// Request-shape checks still come first.
	_, err = down.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{})
	mustCode(t, err, codes.InvalidArgument, "")

	missing, err := New(&Fixtures{TenantsRealmMissing: true, Access: Access{DefaultAllowed: true}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"list": func() error { _, err := missing.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{}); return err },
		"get": func() error {
			_, err := missing.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: idAcme})
			return err
		},
		"create": func() error {
			_, err := missing.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "x"}})
			return err
		},
		"update": func() error {
			_, err := missing.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{Object: &aigriddraftv1.Tenant{Id: idAcme}})
			return err
		},
		"delete": func() error {
			_, err := missing.DeleteTenant(ctx, &aigriddraftv1.DeleteTenantRequest{Id: idAcme})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != `Keycloak realm "ai-grid": Realm not found.` {
			t.Errorf("%s with the realm missing = %v", name, err)
		}
	}
	// Authentication and the caller's own mistakes still come first.
	if _, err := missing.ListTenants(context.Background(), &aigriddraftv1.ListTenantsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("anonymous = %v", err)
	}

	disabled, err := New(&Fixtures{TenantsOrgsDisabled: true, Access: Access{DefaultAllowed: true}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	_, err = disabled.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{})
	mustCode(t, err, codes.FailedPrecondition, `Keycloak realm "ai-grid": Organizations not enabled for this realm.`)
}

func TestNewUUIDIsVersion4(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := newUUID()
		if !uuidPattern.MatchString(id) || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("not a v4 UUID: %s", id)
		}
		if seen[id] {
			t.Fatalf("duplicate %s", id)
		}
		seen[id] = true
	}
}

// TestTenantVersion: the backend keeps a version per organization (the
// proposed `ai-grid/resource-version` attribute), starting at 1, bumped by
// every write it makes; a stale expected version is Aborted and changes
// nothing; the attribute is never visible or settable.
func TestTenantVersion(t *testing.T) {
	s := tenantServer(t)
	ctx := authed(context.Background())
	get := func(id string) *aigriddraftv1.Tenant {
		t.Helper()
		resp, err := s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetObject()
	}
	if v := get(idAcme).GetResourceVersion(); v != "1" {
		t.Fatalf("a seeded organization starts at 1, got %q", v)
	}
	upd := func(obj *aigriddraftv1.Tenant, paths ...string) (*aigriddraftv1.Tenant, error) {
		resp, err := s.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{Object: obj, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		return resp.GetObject(), err
	}
	got, err := upd(&aigriddraftv1.Tenant{Id: idAcme, Description: "one", ResourceVersion: "1"}, PathDescription)
	if err != nil || got.GetResourceVersion() != "2" {
		t.Fatalf("matching update = %v, %v", got, err)
	}
	got, err = upd(&aigriddraftv1.Tenant{Id: idAcme, Description: "two"}, PathDescription)
	if err != nil || got.GetResourceVersion() != "3" {
		t.Fatalf("an update without a version is not checked and still bumps: %v, %v", got, err)
	}
	_, err = upd(&aigriddraftv1.Tenant{Id: idAcme, Description: "stale", ResourceVersion: "2"}, PathDescription)
	mustCode(t, err, codes.Aborted, "object with identifier '"+idAcme+"' has resource version 3, the request carries \"2\"; fetch it again and retry")
	// The precondition comes before Keycloak's own checks.
	_, err = upd(&aigriddraftv1.Tenant{Id: idAcme, Name: "", ResourceVersion: "2"}, PathName)
	mustCode(t, err, codes.Aborted, "")
	if cur := get(idAcme); cur.GetDescription() != "two" || cur.GetResourceVersion() != "3" {
		t.Errorf("a stale update must not land: %v", cur)
	}

	// The attribute the version lives in is the backend's.
	reserved := "attribute \"ai-grid/resource-version\" is reserved for the tenant's version; it cannot be set"
	_, err = upd(&aigriddraftv1.Tenant{Id: idAcme, Attributes: map[string]string{TenantVersionAttribute: "99"}}, PathAttributes)
	mustCode(t, err, codes.InvalidArgument, reserved)
	_, err = s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "sneaky", Attributes: map[string]string{TenantVersionAttribute: "99"}}})
	mustCode(t, err, codes.InvalidArgument, reserved)
	created, err := s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "fresh", ResourceVersion: "42", Attributes: map[string]string{"k": "v"}}})
	if err != nil || created.GetObject().GetResourceVersion() != "1" || len(created.GetObject().GetAttributes()) != 1 {
		t.Fatalf("a created organization starts at 1 whatever the request says: %v, %v", created, err)
	}

	// Delete: stale is Aborted and keeps the organization, current deletes.
	_, err = s.DeleteTenant(ctx, &aigriddraftv1.DeleteTenantRequest{Id: idAcme, ResourceVersion: "1"})
	mustCode(t, err, codes.Aborted, "")
	get(idAcme)
	if _, err := s.DeleteTenant(ctx, &aigriddraftv1.DeleteTenantRequest{Id: idAcme, ResourceVersion: "3"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.DeleteTenant(ctx, &aigriddraftv1.DeleteTenantRequest{Id: idAcme, ResourceVersion: "3"})
	mustCode(t, err, codes.NotFound, "")

	// Fixtures: an explicit version is kept, the attribute is refused.
	s = tenantServer(t, &aigriddraftv1.Tenant{Id: idAcme, Name: "acme", ResourceVersion: "7"})
	if v := get(idAcme).GetResourceVersion(); v != "7" {
		t.Errorf("fixture version = %q, want 7", v)
	}
	for _, bad := range []*aigriddraftv1.Tenant{
		{Id: idAcme, Name: "acme", Attributes: map[string]string{TenantVersionAttribute: "1"}},
		{Id: idAcme, Name: "acme", ResourceVersion: "v1"},
	} {
		if _, err := New(&Fixtures{Tenants: []*aigriddraftv1.Tenant{bad}}, slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("fixture %v must not load", bad)
		}
	}
}

// TestListTenantsDecodesTheFilterLiteral: the term is a CEL string literal,
// so the escapes for quotes, backslashes and control characters are decoded
// by CEL before the comparison.
func TestListTenantsDecodesTheFilterLiteral(t *testing.T) {
	s := tenantServer(t, &aigriddraftv1.Tenant{Id: idAcme, Name: `a"b\c`, Alias: "quotes"}, &aigriddraftv1.Tenant{Id: idGlobex, Name: "tab\there", Alias: "tab"})
	ctx := authed(context.Background())
	for literal, want := range map[string]string{`"a\"b\\c"`: `a"b\c`, `"tab\there"`: "tab\there", `"tab\u0009"`: "tab\there", `'a"b'`: `a"b\c`, `r"b\c"`: `a"b\c`} {
		filter := `this.name.lowerAscii().contains(` + literal + `) || this.domains.exists(d, d.lowerAscii().contains(` + literal + `))`
		resp, err := s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{Filter: proto.String(filter)})
		if err != nil || len(resp.GetTenants()) != 1 || resp.GetTenants()[0].GetName() != want {
			t.Errorf("%s = %v, %v; want %q", literal, tenantNames(resp), err, want)
		}
	}
}

// TestListTenantsFilterErrorsAfterTheKeycloakGate: a filter that does not
// compile is a request-shape error, answered before Keycloak is asked; one
// that fails on an organization needs the organizations, so with Keycloak
// unreachable it is Unavailable like every other Keycloak-backed answer.
func TestListTenantsFilterErrorsAfterTheKeycloakGate(t *testing.T) {
	s := tenantServer(t)
	s.TenantsErr = status.Error(codes.Unavailable, MsgKeycloakDownPref+"connection refused")
	ctx := authed(context.Background())
	_, err := s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{Filter: proto.String(`this.name ==`)})
	mustCode(t, err, codes.InvalidArgument, "")
	_, err = s.ListTenants(ctx, &aigriddraftv1.ListTenantsRequest{Filter: proto.String(`this.attributes["x"] == "y"`)})
	mustCode(t, err, codes.Unavailable, "")
}

// validIdP is an identity provider the PoC's rules accept.
func validIdP() *aigriddraftv1.IdentityProvider {
	return &aigriddraftv1.IdentityProvider{
		Title: "Acme SSO", Description: "Acme employees sign in here.", Enabled: proto.Bool(true),
		Config: &aigriddraftv1.OidcConfig{
			ClientId: "ai-grid", ClientSecretSecretName: "acme-oidc-client",
			Issuer: "https://sso.example.com/realms/acme", AuthorizationUrl: "https://sso.example.com/realms/acme/auth",
			TokenUrl: "https://sso.example.com/realms/acme/token", DefaultScopes: "openid profile email",
			ValidateSignature: proto.Bool(true),
		},
	}
}

// TestTenantIdentityProviderRoundTrips: the identity provider (P1) is stored
// on create, returned by Get, replaced by a patch that names only
// identity_provider, and the domains stay.
func TestTenantIdentityProviderRoundTrips(t *testing.T) {
	ctx := authed(context.Background())
	s := tenantServer(t)
	created, err := s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "wayne-aerospace", Domains: []string{"wayne-aero.example.com"}, IdentityProvider: validIdP()}})
	if err != nil {
		t.Fatal(err)
	}
	id := created.GetObject().GetId()
	got, err := s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: id})
	if err != nil || !proto.Equal(got.GetObject().GetIdentityProvider(), validIdP()) {
		t.Fatalf("stored identity provider = %v, %v", got.GetObject().GetIdentityProvider(), err)
	}
	next := validIdP()
	next.Title = "Wayne SSO"
	updated, err := s.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{Object: &aigriddraftv1.Tenant{Id: id, IdentityProvider: next}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathIdentityProvider}}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.GetObject().GetIdentityProvider().GetTitle() != "Wayne SSO" || len(updated.GetObject().GetDomains()) != 1 || updated.GetObject().GetDomains()[0] != "wayne-aero.example.com" || updated.GetObject().GetResourceVersion() != "2" {
		t.Errorf("updated = %v", updated.GetObject())
	}
	_, err = s.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{Object: &aigriddraftv1.Tenant{Id: id}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathIdentityProvider}}})
	if fieldViolations(err)["identityProvider"] != "Required" {
		t.Errorf("identity_provider in the mask without a value: %v", err)
	}
	if created, err := s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "no-idp"}}); err != nil || created.GetObject().GetIdentityProvider() != nil {
		t.Errorf("an organization without an identity provider, as Keycloak and other clients create them: %v, %v", created, err)
	}
}

// TestStaleUpdateIsAbortedBeforeItsIdentityProviderIsValidated: a stale
// version is Aborted whatever else the request says, the invalid identity
// provider included; with the current version the same request is
// InvalidArgument with the field violations.
func TestStaleUpdateIsAbortedBeforeItsIdentityProviderIsValidated(t *testing.T) {
	ctx := authed(context.Background())
	s := tenantServer(t)
	created, err := s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "wayne-aerospace", IdentityProvider: validIdP()}})
	if err != nil {
		t.Fatal(err)
	}
	id, version := created.GetObject().GetId(), created.GetObject().GetResourceVersion()
	update := func(rv string) error {
		_, err := s.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{
			Object:     &aigriddraftv1.Tenant{Id: id, ResourceVersion: rv, IdentityProvider: &aigriddraftv1.IdentityProvider{}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathIdentityProvider}},
		})
		return err
	}
	if err := update(version + "9"); status.Code(err) != codes.Aborted {
		t.Errorf("stale and invalid identity provider: %v, want Aborted", err)
	}
	err = update(version)
	if status.Code(err) != codes.InvalidArgument || len(fieldViolations(err)) == 0 {
		t.Errorf("current version, invalid identity provider: %v, want InvalidArgument with field violations", err)
	}
}

// TestPatchWithoutIdentityProviderKeepsIt: a patch of other fields, or an
// update without a mask, leaves the identity provider alone.
func TestPatchWithoutIdentityProviderKeepsIt(t *testing.T) {
	ctx := authed(context.Background())
	acme := seedOrgs()[1]
	acme.IdentityProvider = validIdP()
	s := tenantServer(t, acme)
	for _, m := range []*fieldmaskpb.FieldMask{{Paths: []string{PathDomains}}, nil} {
		resp, err := s.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{Object: &aigriddraftv1.Tenant{Id: idAcme, Name: "acme-robotics", Domains: []string{"acme.example.com"}}, UpdateMask: m})
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(resp.GetObject().GetIdentityProvider(), validIdP()) {
			t.Errorf("mask %v: identity provider = %v", m.GetPaths(), resp.GetObject().GetIdentityProvider())
		}
	}
}

// TestIdentityProviderIsValidatedAndTrimmed: the PoC's identity-provider
// rules (shared/tenant-validation.mjs identityProviderSchema at 62319b8), its
// messages as BadRequest field violations under identityProvider.*, and the
// values trimmed as zod's .trim() outputs them.
func TestIdentityProviderIsValidatedAndTrimmed(t *testing.T) {
	ctx := authed(context.Background())
	create := func(idp *aigriddraftv1.IdentityProvider) (*aigriddraftv1.Tenant, error) {
		s := tenantServer(t)
		resp, err := s.CreateTenant(ctx, &aigriddraftv1.CreateTenantRequest{Object: &aigriddraftv1.Tenant{Name: "wayne-aerospace", IdentityProvider: idp}})
		return resp.GetObject(), err
	}
	with := func(edit func(*aigriddraftv1.IdentityProvider)) *aigriddraftv1.IdentityProvider {
		idp := validIdP()
		edit(idp)
		return idp
	}
	for name, tc := range map[string]struct {
		idp  *aigriddraftv1.IdentityProvider
		want map[string]string
	}{
		"missing client ID": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config.ClientId = "  " }),
			map[string]string{"identityProvider.config.clientId": "Client ID is required."}},
		"ftp issuer": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config.Issuer = "ftp://sso.example.com" }),
			map[string]string{"identityProvider.config.issuer": "Use HTTP or HTTPS."}},
		"issuer that is not a URL": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config.Issuer = "sso.example.com" }),
			map[string]string{"identityProvider.config.issuer": "Use HTTP or HTTPS."}},
		"URL without a host": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config.AuthorizationUrl = "https://" }),
			map[string]string{"identityProvider.config.authorizationUrl": "Enter a valid HTTP or HTTPS URL."}},
		"empty token URL": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config.TokenUrl = "" }),
			map[string]string{"identityProvider.config.tokenUrl": "Use HTTP or HTTPS."}},
		"blank optional URL": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config.LogoutUrl = "   " }),
			map[string]string{"identityProvider.config.logoutUrl": "Use HTTP or HTTPS."}},
		"invalid secret reference name": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config.ClientSecretSecretName = "Acme_Secret" }),
			map[string]string{"identityProvider.config.clientSecretSecret.name": "Enter a valid secret reference name."}},
		"empty secret reference": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config.ClientSecretSecretName = "" }),
			map[string]string{"identityProvider.config.clientSecretSecret.name": "Enter a valid secret reference name."}},
		"disabled identity provider": {with(func(p *aigriddraftv1.IdentityProvider) { p.Enabled = proto.Bool(false) }),
			map[string]string{"identityProvider.enabled": "Invalid literal value, expected true"}},
		"long title and description": {with(func(p *aigriddraftv1.IdentityProvider) {
			p.Title, p.Description = strings.Repeat("t", 254), strings.Repeat("d", 4001)
		}), map[string]string{
			"identityProvider.title":       "String must contain at most 253 character(s)",
			"identityProvider.description": "String must contain at most 4000 character(s)",
		}},
		"long scopes": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config.DefaultScopes = strings.Repeat("s", 2049) }),
			map[string]string{"identityProvider.config.defaultScopes": "String must contain at most 2048 character(s)"}},
		"no config": {with(func(p *aigriddraftv1.IdentityProvider) { p.Config = nil }),
			map[string]string{"identityProvider.config": "Required"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := create(tc.idp)
			mustCode(t, err, codes.InvalidArgument, "")
			if got := fieldViolations(err); !maps.Equal(got, tc.want) {
				t.Errorf("violations = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("trimmed and defaulted", func(t *testing.T) {
		got, err := create(&aigriddraftv1.IdentityProvider{
			Title: "  Acme SSO  ", Description: "  kept as typed  ",
			Config: &aigriddraftv1.OidcConfig{
				ClientId: " ai-grid ", ClientSecretSecretName: " acme-oidc-client ",
				Issuer: " https://sso.example.com ", AuthorizationUrl: "https://sso.example.com/auth\n", TokenUrl: "\thttps://sso.example.com/token",
				DefaultScopes: " openid ", UserInfoUrl: " https://sso.example.com/userinfo ",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := &aigriddraftv1.IdentityProvider{
			Title: "Acme SSO", Description: "  kept as typed  ", Enabled: proto.Bool(true),
			Config: &aigriddraftv1.OidcConfig{
				ClientId: "ai-grid", ClientSecretSecretName: "acme-oidc-client",
				Issuer: "https://sso.example.com", AuthorizationUrl: "https://sso.example.com/auth", TokenUrl: "https://sso.example.com/token",
				DefaultScopes: "openid", UserInfoUrl: "https://sso.example.com/userinfo", ValidateSignature: proto.Bool(true),
			},
		}
		if !proto.Equal(got.GetIdentityProvider(), want) {
			t.Errorf("stored = %v, want %v", got.GetIdentityProvider(), want)
		}
	})

	t.Run("update is checked too", func(t *testing.T) {
		s := tenantServer(t)
		_, err := s.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{
			Object:     &aigriddraftv1.Tenant{Id: idAcme, IdentityProvider: with(func(p *aigriddraftv1.IdentityProvider) { p.Config.ClientId = "" })},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathIdentityProvider}},
		})
		if fieldViolations(err)["identityProvider.config.clientId"] != "Client ID is required." {
			t.Errorf("update = %v", err)
		}
	})
}

// TestTwoTabsOnAnUnversionedOrganization: an organization stored without a
// version (initech-labs in the shipped fixtures has no resourceVersion, as an
// organization the backend never wrote) still gets one, "1", so two tabs that
// read it at once can't overwrite each other: the first save wins, the second
// is Aborted (the BFF's 409) and changes nothing, and so is a delete with the
// version the second tab read.
func TestTwoTabsOnAnUnversionedOrganization(t *testing.T) {
	s := newRepoServer(t)
	ctx := authed(context.Background())
	const idInitech = "c5a4d3e2-f1b0-4987-a6c5-d4e3f2a1b0c9"
	read := func() *aigriddraftv1.Tenant {
		t.Helper()
		resp, err := s.GetTenant(ctx, &aigriddraftv1.GetTenantRequest{Id: idInitech})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetObject()
	}
	tabA, tabB := read(), read()
	if tabA.GetName() != "initech-labs" || tabA.GetResourceVersion() != "1" || tabB.GetResourceVersion() != "1" {
		t.Fatalf("an unversioned organization reads %q (%s), want version 1", tabA.GetResourceVersion(), tabA.GetName())
	}
	save := func(tab *aigriddraftv1.Tenant, domain string) error {
		_, err := s.UpdateTenant(ctx, &aigriddraftv1.UpdateTenantRequest{
			Object:     &aigriddraftv1.Tenant{Id: idInitech, Domains: []string{domain}, ResourceVersion: tab.GetResourceVersion()},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{PathDomains}},
		})
		return err
	}
	if err := save(tabA, "initech-a.example.com"); err != nil {
		t.Fatalf("first tab: %v", err)
	}
	err := save(tabB, "initech-b.example.com")
	mustCode(t, err, codes.Aborted, "object with identifier '"+idInitech+"' has resource version 2, the request carries \"1\"; fetch it again and retry")
	if cur := read(); cur.GetResourceVersion() != "2" || len(cur.GetDomains()) != 1 || cur.GetDomains()[0] != "initech-a.example.com" {
		t.Fatalf("the second tab's stale save landed: %v", cur)
	}
	_, err = s.DeleteTenant(ctx, &aigriddraftv1.DeleteTenantRequest{Id: idInitech, ResourceVersion: tabB.GetResourceVersion()})
	mustCode(t, err, codes.Aborted, "")
	read()
}
