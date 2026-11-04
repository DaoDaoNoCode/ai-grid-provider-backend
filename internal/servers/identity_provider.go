package servers

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	tenantIdentityProviderAttribute = "ai-grid/identity-provider"
	defaultOIDCScopes               = "openid profile email"
	// keycloakGroupAttributeMax is Keycloak's GROUP_ATTRIBUTE.VALUE column
	// (varchar(255)). Organization attributes longer than this must be split
	// across multiple values of the same attribute name.
	keycloakGroupAttributeMax = 255
	// chunkIndexPrefixLen is "0000|": a stable index so chunks can be rejoined
	// when Keycloak returns multi-valued attributes out of order. The payload
	// is chunked shorter so the stored value still fits in 255 bytes.
	chunkIndexPrefixLen = 5
)

var (
	idpSecretNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	idpHTTPURLRE    = regexp.MustCompile(`^https?://`)
)

// normalizeIdentityProvider validates and normalizes an IdP for create/compare.
// Empty title becomes tenantName; enabled and validate_signature default to true;
// empty default_scopes becomes "openid profile email".
func normalizeIdentityProvider(tenantName string, idp *aigridv1.IdentityProvider) (*aigridv1.IdentityProvider, error) {
	if idp == nil {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.identity_provider is required")
	}
	out := proto.Clone(idp).(*aigridv1.IdentityProvider)

	title, err := trimIDPString("spec.identity_provider.title", out.GetTitle(), 253)
	if err != nil {
		return nil, err
	}
	if title == "" {
		title = tenantName
	}
	out.Title = title

	if err := maxIDPLen("spec.identity_provider.description", out.GetDescription(), 4000); err != nil {
		return nil, err
	}

	if out.Enabled == nil {
		enabled := true
		out.Enabled = &enabled
	} else if !out.GetEnabled() {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.identity_provider.enabled must be true")
	}

	cfg := out.GetConfig()
	if cfg == nil {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.identity_provider.config is required")
	}

	clientID, err := trimIDPString("spec.identity_provider.config.client_id", cfg.GetClientId(), 253)
	if err != nil {
		return nil, err
	}
	if clientID == "" {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.identity_provider.config.client_id is required")
	}
	cfg.ClientId = clientID

	secretName, err := trimIDPString("spec.identity_provider.config.client_secret_secret_name", cfg.GetClientSecretSecretName(), 253)
	if err != nil {
		return nil, err
	}
	if secretName == "" {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.identity_provider.config.client_secret_secret_name is required")
	}
	if !idpSecretNameRE.MatchString(secretName) {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.identity_provider.config.client_secret_secret_name %q is not a valid DNS subdomain name", secretName)
	}
	cfg.ClientSecretSecretName = secretName

	issuer, err := normalizeIDPURL("spec.identity_provider.config.issuer", cfg.GetIssuer(), false)
	if err != nil {
		return nil, err
	}
	cfg.Issuer = issuer

	authURL, err := normalizeIDPURL("spec.identity_provider.config.authorization_url", cfg.GetAuthorizationUrl(), false)
	if err != nil {
		return nil, err
	}
	cfg.AuthorizationUrl = authURL

	tokenURL, err := normalizeIDPURL("spec.identity_provider.config.token_url", cfg.GetTokenUrl(), false)
	if err != nil {
		return nil, err
	}
	cfg.TokenUrl = tokenURL

	scopes, err := trimIDPString("spec.identity_provider.config.default_scopes", cfg.GetDefaultScopes(), 2048)
	if err != nil {
		return nil, err
	}
	if scopes == "" {
		scopes = defaultOIDCScopes
	}
	cfg.DefaultScopes = scopes

	userInfo, err := normalizeIDPURL("spec.identity_provider.config.user_info_url", cfg.GetUserInfoUrl(), true)
	if err != nil {
		return nil, err
	}
	cfg.UserInfoUrl = userInfo

	logout, err := normalizeIDPURL("spec.identity_provider.config.logout_url", cfg.GetLogoutUrl(), true)
	if err != nil {
		return nil, err
	}
	cfg.LogoutUrl = logout

	if cfg.ValidateSignature == nil {
		validate := true
		cfg.ValidateSignature = &validate
	}
	// Keycloak's broker calls PublicKeyStorageManager only when signature
	// validation is on. With no JWKS URL that lookup is nil and every login
	// fails with "token signature validation failed".
	jwks, err := normalizeIDPURL("spec.identity_provider.config.jwks_url", cfg.GetJwksUrl(), !cfg.GetValidateSignature())
	if err != nil {
		return nil, err
	}
	cfg.JwksUrl = jwks

	out.Config = cfg
	return out, nil
}

func identityProviderToAttribute(idp *aigridv1.IdentityProvider) (string, error) {
	if idp == nil {
		return "", fmt.Errorf("identity provider is nil")
	}
	b, err := protojson.Marshal(idp)
	if err != nil {
		return "", fmt.Errorf("marshal identity provider: %w", err)
	}
	return string(b), nil
}

// identityProviderToAttributeValues marshals the IdP and splits it into chunks
// that fit Keycloak's 255-character group attribute limit. Each chunk is
// prefixed with its index so a read can restore order.
func identityProviderToAttributeValues(idp *aigridv1.IdentityProvider) ([]string, error) {
	raw, err := identityProviderToAttribute(idp)
	if err != nil {
		return nil, err
	}
	payloadMax := keycloakGroupAttributeMax - chunkIndexPrefixLen
	parts := chunkString(raw, payloadMax)
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = fmt.Sprintf("%04d|%s", i, part)
		if len(out[i]) > keycloakGroupAttributeMax {
			return nil, fmt.Errorf("identity provider chunk %d is %d bytes", i, len(out[i]))
		}
	}
	return out, nil
}

func identityProviderFromAttribute(value string) (*aigridv1.IdentityProvider, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	idp := &aigridv1.IdentityProvider{}
	if err := protojson.Unmarshal([]byte(value), idp); err != nil {
		return nil, fmt.Errorf("unmarshal identity provider attribute: %w", err)
	}
	return idp, nil
}

func identityProviderFromOrganizationAttrs(attrs map[string][]string) (*aigridv1.IdentityProvider, error) {
	values := attrs[tenantIdentityProviderAttribute]
	if len(values) == 0 {
		return nil, nil
	}
	return identityProviderFromAttribute(joinIdentityProviderChunks(values))
}

// joinIdentityProviderChunks concatenates attribute values. Indexed chunks
// (written by identityProviderToAttributeValues) are ordered by their prefix.
// Values without that prefix are an older write and stay in the order Keycloak returned.
func joinIdentityProviderChunks(values []string) string {
	if ordered, ok := orderedIndexedChunks(values); ok {
		return strings.Join(ordered, "")
	}
	return strings.Join(values, "")
}

func orderedIndexedChunks(values []string) ([]string, bool) {
	out := make([]string, len(values))
	seen := make([]bool, len(values))
	for _, v := range values {
		index, payload, ok := parseChunkIndex(v)
		if !ok || index >= len(values) || seen[index] {
			return nil, false
		}
		seen[index] = true
		out[index] = payload
	}
	return out, true
}

func parseChunkIndex(v string) (int, string, bool) {
	if len(v) < chunkIndexPrefixLen || v[chunkIndexPrefixLen-1] != '|' {
		return 0, "", false
	}
	for i := 0; i < chunkIndexPrefixLen-1; i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, "", false
		}
	}
	index, err := strconv.Atoi(v[:chunkIndexPrefixLen-1])
	if err != nil {
		return 0, "", false
	}
	return index, v[chunkIndexPrefixLen:], true
}

// chunkString splits s into pieces of at most size bytes, on rune boundaries,
// so a multibyte character is never torn in half.
func chunkString(s string, size int) []string {
	if size <= 0 {
		return []string{s}
	}
	if s == "" {
		return nil
	}
	out := make([]string, 0, (len(s)+size-1)/size)
	var b strings.Builder
	n := 0
	for _, r := range s {
		w := utf8.RuneLen(r)
		if w < 0 {
			w = 1
		}
		if n > 0 && n+w > size {
			out = append(out, b.String())
			b.Reset()
			n = 0
		}
		b.WriteRune(r)
		n += w
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func trimIDPString(field, s string, max int) (string, error) {
	s = strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == 0xFEFF })
	if err := maxIDPLen(field, s, max); err != nil {
		return "", err
	}
	return s, nil
}

func maxIDPLen(field, s string, max int) error {
	if len(utf16.Encode([]rune(s))) > max {
		return connect.Errorf(connect.CodeInvalidArgument, "%s exceeds %d characters", field, max)
	}
	return nil
}

func normalizeIDPURL(field, s string, optional bool) (string, error) {
	if optional && s == "" {
		return "", nil
	}
	s, err := trimIDPString(field, s, 2048)
	if err != nil {
		return "", err
	}
	if optional && s == "" {
		return "", nil
	}
	if s == "" {
		return "", connect.Errorf(connect.CodeInvalidArgument, "%s is required", field)
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", connect.Errorf(connect.CodeInvalidArgument, "%s must be a valid URL", field)
	}
	if !idpHTTPURLRE.MatchString(s) {
		return "", connect.Errorf(connect.CodeInvalidArgument, "%s must use HTTP or HTTPS", field)
	}
	return s, nil
}
