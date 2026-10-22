package draft

import (
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// The subscription rules live in package validation, shared with the
// frontend's cases; the identity-provider rules are below.

// invalid returns InvalidArgument with a BadRequest detail, which the BFF turns
// into {"error":{"fieldErrors":{...}}}.
func invalid(summary string, fields map[string]string) error {
	st := status.New(codes.InvalidArgument, summary)
	br := &errdetails.BadRequest{}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{Field: k, Description: fields[k]})
	}
	if withDetails, err := st.WithDetails(br); err == nil {
		st = withDetails
	}
	return st.Err()
}

// joinMessages is the summary the PoC shows above a form: every message, in path order.
func joinMessages(fields map[string]string) string {
	keys := slices.Sorted(maps.Keys(fields))
	msgs := make([]string, 0, len(keys))
	for _, k := range keys {
		msgs = append(msgs, fields[k])
	}
	return strings.Join(msgs, " ")
}

// The PoC's identity-provider rules, from its identityProviderSchema and
// oidcConfigSchema (krisv/ai-grid-cloud-provider-poc
// shared/tenant-validation.mjs at 62319b8), with zod's default messages where
// the PoC sets none. Paths are the console's JSON paths under
// `identityProvider`, the shape the BFF translates from (proposal P1).
const (
	pathIdP          = "identityProvider"
	msgRequired      = "Required"
	msgClientID      = "Client ID is required."
	msgSecretMissing = "Client secret reference is required."
	msgSecretName    = "Enter a valid secret reference name."
	msgURL           = "Enter a valid HTTP or HTTPS URL."
	msgHTTP          = "Use HTTP or HTTPS."
	msgIdPDisabled   = "Invalid literal value, expected true"
)

var (
	secretNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	httpURLPattern    = regexp.MustCompile(`^https?://`)
)

// checkIdentityProvider validates an identity provider and returns it as the
// PoC's zod schema outputs it: trimmed (the description excepted), `enabled`
// and `validate_signature` defaulted to true. Field errors keep the last
// message per path, as the PoC's form does with zod's issues.
func checkIdentityProvider(in *aigriddraftv1.IdentityProvider) (*aigriddraftv1.IdentityProvider, map[string]string) {
	errs := map[string]string{}
	if in == nil {
		errs[pathIdP] = msgRequired
		return nil, errs
	}
	out := proto.Clone(in).(*aigriddraftv1.IdentityProvider)
	out.Title = trimmed(errs, pathIdP+".title", out.GetTitle(), 253)
	maxLen(errs, pathIdP+".description", out.GetDescription(), 4000)
	if out.Enabled == nil {
		out.Enabled = proto.Bool(true)
	} else if !out.GetEnabled() {
		errs[pathIdP+".enabled"] = msgIdPDisabled
	}
	c := out.GetConfig()
	if c == nil {
		errs[pathIdP+".config"] = msgRequired
		return out, errs
	}
	cp := pathIdP + ".config."
	if c.ClientId = trimmed(errs, cp+"clientId", c.GetClientId(), 253); c.ClientId == "" {
		errs[cp+"clientId"] = msgClientID
	}
	sp := cp + "clientSecretSecret.name"
	c.ClientSecretSecretName = trimmed(errs, sp, c.GetClientSecretSecretName(), 253)
	if c.ClientSecretSecretName == "" {
		errs[sp] = msgSecretMissing
	}
	if !secretNamePattern.MatchString(c.ClientSecretSecretName) {
		errs[sp] = msgSecretName
	}
	c.Issuer = checkURL(errs, cp+"issuer", c.GetIssuer(), false)
	c.AuthorizationUrl = checkURL(errs, cp+"authorizationUrl", c.GetAuthorizationUrl(), false)
	c.TokenUrl = checkURL(errs, cp+"tokenUrl", c.GetTokenUrl(), false)
	c.DefaultScopes = trimmed(errs, cp+"defaultScopes", c.GetDefaultScopes(), 2048)
	c.UserInfoUrl = checkURL(errs, cp+"userInfoUrl", c.GetUserInfoUrl(), true)
	c.JwksUrl = checkURL(errs, cp+"jwksUrl", c.GetJwksUrl(), true)
	c.LogoutUrl = checkURL(errs, cp+"logoutUrl", c.GetLogoutUrl(), true)
	if c.ValidateSignature == nil {
		c.ValidateSignature = proto.Bool(true)
	}
	return out, errs
}

// checkURL is the PoC's url schema: trimmed, at most 2048 characters, a URL,
// and http or https. An optional URL may also be empty; one that is only
// whitespace is checked, and fails, as zod's union does.
func checkURL(errs map[string]string, path, s string, optional bool) string {
	if optional && s == "" {
		return s
	}
	s = trimmed(errs, path, s, 2048)
	if !parsesAsURL(s) {
		errs[path] = msgURL
	}
	if !httpURLPattern.MatchString(s) {
		errs[path] = msgHTTP
	}
	return s
}

// parsesAsURL approximates the WHATWG URL parser zod's url() uses (new
// URL(s)): an absolute URL with a scheme, and a host for the schemes that
// need one.
func parsesAsURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ws", "wss", "ftp":
		return u.Host != "" || u.Opaque != ""
	}
	return true
}

// trimmed is zod's .trim().max(n): String.prototype.trim, then the length
// in UTF-16 code units.
func trimmed(errs map[string]string, path, s string, n int) string {
	s = strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == 0xFEFF })
	maxLen(errs, path, s, n)
	return s
}

// maxLen is zod's .max(n) with its default message.
func maxLen(errs map[string]string, path, s string, n int) {
	if len(utf16.Encode([]rune(s))) > n {
		errs[path] = "String must contain at most " + strconv.Itoa(n) + " character(s)"
	}
}

// identityProviderError is checkIdentityProvider's result as the backend's
// error: nil when valid, else InvalidArgument with the field violations.
func identityProviderError(fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	return invalid(joinMessages(fields), fields)
}
