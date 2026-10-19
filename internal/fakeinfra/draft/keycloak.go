package draft

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	aigriddraftv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/draft/v1"
)

// Strings Keycloak 26.8.0 writes for Organizations, verbatim: the real
// backend forwards Keycloak's message as the gRPC status message and the UI
// shows it. Each one names the file and line of the 26.8.0 source that
// writes it (paths below are relative to the Keycloak repository: jpa =
// model/jpa/src/main/java/org/keycloak/organization/jpa, resource =
// services/src/main/java/org/keycloak/organization/admin/resource).
//
// Not everything the fake says comes from Keycloak. The texts the console
// invented, because the real backend will have to choose them itself, are
// listed completely in docs/draft/areas/tenants.md ("Invented here") and are kept
// out of this block.
const (
	// KeycloakRealm is the realm the backend reads tenants from
	// (`KEYCLOAK_REALM`, default `ai-grid`).
	KeycloakRealm = "ai-grid"

	// Create and update.
	MsgNameRequired        = "Name can not be null"                              // jpa/JpaOrganizationProvider.java L103-104
	MsgNameNotAliasPrefix  = "Name cannot be used as alias: "                    // jpa/JpaOrganizationProvider.java L107-111
	MsgDuplicateName       = "A organization with the same name already exists." // jpa/JpaOrganizationProvider.java L116-117; resource/OrganizationResource.java L123-125
	MsgDuplicateAlias      = "A organization with the same alias already exists" // sic, no period; jpa/JpaOrganizationProvider.java L120-121
	MsgAliasImmutable      = "Cannot change the alias"                           // jpa/OrganizationAdapter.java L107-108
	MsgOrganizationMissing = "Organization not found."                           // resource/OrganizationsResource.java L212-214

	// ReservedCharValidator (server-spi-private/src/main/java/org/keycloak/
	// utils/ReservedCharValidator.java): validateNoSpace (L60-63) first looks
	// for `\s` anywhere, then for the first RFC 3986 reserved character
	// (RESERVED_CHARS_PATTERN, L36), whose message (L53) names it.
	MsgEmptySpace      = "Empty Space not allowed."    // ReservedCharValidator.java L61
	MsgReservedCharFmt = "Character '%s' not allowed." // ReservedCharValidator.java L53
	reservedAliasChars = `:/?#@!$&()*+,;=[]\`          // ReservedCharValidator.java L36
	// javaRegexSpace is java.util.regex `\s` without UNICODE_CHARACTER_CLASS,
	// which is ASCII only: [ \t\n\x0B\f\r]. A no-break or other Unicode space
	// passes the alias check.
	javaRegexSpace = " \t\n\v\f\r"

	// Realm-level 404s. "Realm not found." is a JAX-RS NotFoundException
	// (services/.../resources/admin/RealmsAdminResource.java L223), whose body
	// KeycloakErrorHandler writes as {"error": "Realm not found."};
	// "Organizations not enabled ..." is an ErrorResponse
	// (services/.../organization/utils/Organizations.java L263-266), body
	// {"errorMessage": ...}.
	MsgRealmMissing   = "Realm not found."
	MsgOrgsNotEnabled = "Organizations not enabled for this realm."

	// Domain checks. Blank is OrganizationAdapter.validateDomain
	// (jpa/OrganizationAdapter.java L297-299); the format messages are
	// Organizations.validateDomain (services/.../organization/utils/
	// Organizations.java L308-344): the wildcard "parts" messages print the
	// base domain (the input with `*.` stripped, L324, L333, L337), the
	// others print the whole input, already trimmed and lower-cased by
	// OrganizationDomainModel (server-spi/.../models/OrganizationDomainModel.java
	// L59). The cross-organization check is jpa/OrganizationAdapter.java
	// L304-309, the identity provider lookup L314-327.
	MsgDomainEmpty          = "Domain name cannot be empty"                                  // jpa/OrganizationAdapter.java L298
	MsgDomainInvalidFmt     = "Invalid domain format: %s"                                    // Organizations.java L341-342
	MsgWildcardNoBaseFmt    = "Wildcard domain must specify a base domain: %s"               // Organizations.java L316-317
	MsgWildcardNotFirst     = "Wildcard domain must start with the wildcard"                 // Organizations.java L320-321
	MsgWildcardMultipleFmt  = "Multiple wildcards are not allowed: %s"                       // Organizations.java L326-327
	MsgDomainTooFewPartsFmt = "Domain must have at least 2 parts (e.g. 'example.com'): %s"   // Organizations.java L332-333, MIN_DOMAIN_PARTS L79
	MsgDomainTooManyParts   = "Domain has too many parts (max 10 allowed): %s"               // Organizations.java L336-337, MAX_DOMAIN_PARTS L80
	MsgDomainLinkedFmt      = "Domain %s is already linked to organization %s in realm %s"   // jpa/OrganizationAdapter.java L306-308
	MsgIdPMissingFmt        = "Identity provider with alias '%s' does not exist in realm %s" // jpa/OrganizationAdapter.java L318-320

	// Field-mask paths UpdateTenant accepts.
	PathName          = "name"
	PathAlias         = "alias"
	PathDomains       = "domains"
	PathEnabled       = "enabled"
	PathDescription   = "description"
	PathAttributes    = "attributes"
	PathDomainDetails = "domain_details"
	// PathIdentityProvider is the console's proposal P1, not a Keycloak field.
	PathIdentityProvider = "identity_provider"
)

// MsgKeycloakDownPref is invented by the console (docs/draft/areas/tenants.md):
// the prefix of the Unavailable message when Keycloak cannot be reached.
// Keycloak says nothing then; the shipped backend answers Unknown.
const MsgKeycloakDownPref = "Keycloak is unavailable: "

// wildcardPrefix is Organizations.WILDCARD_PREFIX (Organizations.java L78).
const wildcardPrefix = "*."

// tenantPaths is the list UpdateTenant names in its InvalidArgument messages.
var tenantPaths = []string{PathName, PathAlias, PathDomains, PathEnabled, PathDescription, PathAttributes, PathDomainDetails, PathIdentityProvider}

// uuidPattern is a lower-case version 4 UUID as KeycloakModelUtils.generateId()
// (java.util.UUID.randomUUID) writes it: version nibble 4, variant 8-b.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The e-mail domain rule Keycloak applies to a plain domain and to the base
// of a wildcard: Organizations.validateDomain (L341) calls
// EmailValidationUtil.isValidEmail("user@" + domain)
// (server-spi-private/src/main/java/org/keycloak/utils/EmailValidationUtil.java,
// a copy of hibernate-validator's AbstractEmailValidator). Its domain part
// (L81-106) is rejected with a trailing dot (L84), when java.net.IDN.toASCII
// throws (L90-93), when the ASCII form exceeds 255 characters (L96, limit L29),
// and otherwise must match EMAIL_DOMAIN_PATTERN (L30-38, CASE_INSENSITIVE):
// labels of letters, digits, `!#$%&'*+/=?^`{|}~` (no underscore: Keycloak's
// copy omits it, L30) and any character U+0080-U+FFFF, with hyphens only
// inside a label, joined by dots; or an IPv4 literal `[1.2.3.4]`; or an IPv6
// literal `[IPv6:...]`. The patterns below are those of L30-38; Java's
// possessive `++` that DOMAIN (L32) produces cannot change the result here
// because the label characters never include `.` or `-`.
//
// Known differences, all of them only for non-ASCII input or the length
// caps (ASCII domains are decided exactly as Keycloak decides them):
//   - IDN.toASCII (JDK, not in the Keycloak tree) runs RFC 3491 nameprep on
//     every non-ASCII label and throws on prohibited or unassigned code
//     points (for example U+00A0 or U+0085), on bidi violations and on a
//     non-ASCII label that starts with `xn--`; the fake does not, so it
//     accepts some non-ASCII domains Keycloak rejects.
//   - The 63-character label cap (IDN.toASCII) and the 255-character domain
//     cap (L96) apply to the punycode (`xn--...`) form; the fake counts the
//     characters of the input, so a long non-ASCII label or domain can pass
//     the fake and fail Keycloak. IDN also splits labels at U+3002, U+FF0E
//     and U+FF61, which the fake does for the label cap only.
//   - Canonicalisation is Java's String.trim() and String.toLowerCase() in
//     the server's default locale (OrganizationDomainModel L59). The fake's
//     trim is exact (javaTrim); its lower-casing is Go's strings.ToLower,
//     which differs for U+0130 (Java gives "i̇" outside a Turkish
//     locale), for a word-final capital sigma (Java gives U+03C2) and under
//     a Turkish default locale.
//   - Keycloak checks each domain fully (format, then the other
//     organizations, OrganizationAdapter L192 and L294-311) in the
//     iteration order of a HashSet (RepresentationToModel.toModel L1950-1954),
//     so with several bad domains the one it reports is not predictable; the
//     fake checks every format in request order first, then every owner,
//     then every identity provider alias.
var (
	emailDomainChars = "[a-z\\x{80}-\\x{ffff}0-9!#$%&'*+/=?^`{|}~]"                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    // DOMAIN_CHARS_WITHOUT_DASH, L30
	emailDomainLabel = "(?:" + emailDomainChars + "-*)*" + emailDomainChars + "+"                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      // DOMAIN_LABEL, L31
	emailDomainName  = emailDomainLabel + "(?:\\." + emailDomainLabel + ")*"                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           // DOMAIN, L32
	emailIPv4        = `[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                // IP_DOMAIN, L33
	emailIPv6        = `(?:(?:[0-9a-fA-F]{1,4}:){7,7}[0-9a-fA-F]{1,4}|(?:[0-9a-fA-F]{1,4}:){1,7}:|(?:[0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|(?:[0-9a-fA-F]{1,4}:){1,5}(?::[0-9a-fA-F]{1,4}){1,2}|(?:[0-9a-fA-F]{1,4}:){1,4}(?::[0-9a-fA-F]{1,4}){1,3}|(?:[0-9a-fA-F]{1,4}:){1,3}(?::[0-9a-fA-F]{1,4}){1,4}|(?:[0-9a-fA-F]{1,4}:){1,2}(?::[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:(?:(?::[0-9a-fA-F]{1,4}){1,6})|:(?:(?::[0-9a-fA-F]{1,4}){1,7}|:)|fe80:(?::[0-9a-fA-F]{0,4}){0,4}%[0-9a-zA-Z]{1,}|::(?:ffff(:0{1,4}){0,1}:){0,1}(?:(?:25[0-5]|(?:2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(?:25[0-5]|(?:2[0-4]|1{0,1}[0-9]){0,1}[0-9])|(?:[0-9a-fA-F]{1,4}:){1,4}:(?:(?:25[0-5]|(?:2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(?:25[0-5]|(?:2[0-4]|1{0,1}[0-9]){0,1}[0-9]))` // IP_V6_DOMAIN, L34
	// EMAIL_DOMAIN_PATTERN, L38, matched whole (Matcher.matches, L100-101).
	emailDomain = regexp.MustCompile(`(?i)^(?:` + emailDomainName + `|\[` + emailIPv4 + `\]|\[IPv6:` + emailIPv6 + `\])$`)
)

const (
	maxDomainLength = 255 // EmailValidationUtil.MAX_DOMAIN_PART_LENGTH, L29 (on the IDN ASCII form)
	maxLabelLength  = 63  // java.net.IDN MAX_LABEL_LENGTH (on the IDN ASCII form)
)

// keycloakError is a validation message exactly as Keycloak's `errorMessage`
// carries it (capitalised, sometimes with a trailing period), which the real
// backend forwards verbatim as the gRPC status message.
type keycloakError struct{ msg string }

func (e *keycloakError) Error() string { return e.msg }

// keycloakErrorf formats one of the Msg* templates.
func keycloakErrorf(format string, args ...any) error {
	return &keycloakError{msg: fmt.Sprintf(format, args...)}
}

// newUUID returns a random version 4 UUID, which is what Keycloak assigns to
// an organization (keycloak.md 5.8).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// validateAlias is Keycloak's ReservedCharValidator.validateNoSpace
// (ReservedCharValidator.java L60-63): two passes over the whole alias, so
// any ASCII whitespace wins over a reserved character that comes before it
// ("a/b c" is "Empty Space not allowed."); then the first reserved character
// (Matcher.find, L50-53) is named.
func validateAlias(alias string) error {
	if strings.ContainsAny(alias, javaRegexSpace) {
		return &keycloakError{msg: MsgEmptySpace}
	}
	if i := strings.IndexAny(alias, reservedAliasChars); i >= 0 {
		r, _ := utf8.DecodeRuneInString(alias[i:])
		return keycloakErrorf(MsgReservedCharFmt, string(r))
	}
	return nil
}

// javaIsWhitespace is java.lang.Character.isWhitespace: a Unicode space,
// line or paragraph separator other than the no-break spaces U+00A0, U+2007
// and U+202F, or one of U+0009-U+000D and U+001C-U+001F.
func javaIsWhitespace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r >= 0x1c && r <= 0x1f:
		return true
	case r == 0xa0 || r == 0x2007 || r == 0x202f:
		return false
	}
	return unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp)
}

// javaIsBlank is Keycloak's StringUtil.isBlank (server-spi/src/main/java/
// org/keycloak/utils/StringUtil.java L26-35), which is String.isBlank:
// empty or only Character.isWhitespace code points. It is not Go's
// strings.TrimSpace: U+00A0 is not blank, U+001C is.
func javaIsBlank(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return !javaIsWhitespace(r) }) < 0
}

// javaTrim is String.trim(): it strips every character up to U+0020 (all
// ASCII control characters and the space) from both ends and nothing else,
// so U+00A0 or U+0085 stay.
func javaTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
}

// validateDomain is the check Keycloak makes of one canonical (trimmed,
// lower-cased) domain name: blank first (OrganizationAdapter.validateDomain,
// jpa/OrganizationAdapter.java L297-299), then Organizations.validateDomain
// (Organizations.java L308-344) branch for branch: a name containing `*.` is
// a wildcard that must be exactly `*.<base>` with no further `*` and a base
// of 2 to 10 parts (getDomainPartsSize, L294-299: dots plus one, zero when
// blank); then the name, or the base of a wildcard, must not be blank and
// must be valid as the domain part of an e-mail address (L341). A `*` that is
// not followed by a dot is an ordinary character of that rule, so
// `*acme.example.com` is accepted.
func validateDomain(domain string) error {
	if javaIsBlank(domain) {
		return &keycloakError{msg: MsgDomainEmpty}
	}
	base := domain
	if strings.Contains(domain, wildcardPrefix) {
		if len(domain) == len(wildcardPrefix) {
			return keycloakErrorf(MsgWildcardNoBaseFmt, domain)
		}
		if !strings.HasPrefix(domain, wildcardPrefix) {
			return &keycloakError{msg: MsgWildcardNotFirst}
		}
		base = domain[len(wildcardPrefix):]
		if strings.Contains(base, "*") {
			return keycloakErrorf(MsgWildcardMultipleFmt, domain)
		}
		parts := 0
		if !javaIsBlank(base) {
			parts = strings.Count(base, ".") + 1
		}
		if parts < 2 {
			return keycloakErrorf(MsgDomainTooFewPartsFmt, base)
		}
		if parts > 10 {
			return keycloakErrorf(MsgDomainTooManyParts, base)
		}
	}
	if javaIsBlank(base) || !isEmailDomain(base) {
		return keycloakErrorf(MsgDomainInvalidFmt, domain)
	}
	return nil
}

// isIDNDot reports the label separators of java.net.IDN.
func isIDNDot(r rune) bool { return r == '.' || r == 0x3002 || r == 0xff0e || r == 0xff61 }

// isEmailDomain applies the rule documented on emailDomain.
func isEmailDomain(domain string) bool {
	if strings.HasSuffix(domain, ".") || utf8.RuneCountInString(domain) > maxDomainLength {
		return false
	}
	for _, label := range strings.FieldsFunc(domain, isIDNDot) {
		if utf8.RuneCountInString(label) > maxLabelLength {
			return false
		}
	}
	return emailDomain.MatchString(domain)
}

// normalizeDomains makes `domains` and `domain_details` one list the way
// Keycloak builds its domain set: each name is trimmed and lower-cased
// (OrganizationDomainModel L59), then validated, then kept once (the set
// collapses names that are equal after that, OrganizationDomainModel
// equals/hashCode L81-98, keeping the first). The two fields must agree when
// both are set. Details are built with Keycloak's defaults (`verified:
// false`, no identity provider, `autoRedirect: false`,
// OrganizationDomainModel L46-48) for a name that has none.
func normalizeDomains(t *aigriddraftv1.Tenant) error {
	details := t.GetDomainDetails()
	names := t.GetDomains()
	switch {
	case len(details) == 0:
		for _, name := range names {
			details = append(details, &aigriddraftv1.TenantDomain{Name: name})
		}
	case len(names) != 0:
		if len(names) != len(details) {
			return fmt.Errorf("domains and domain_details name different domains")
		}
		for i, name := range names {
			if canonicalDomain(name) != canonicalDomain(details[i].GetName()) {
				return fmt.Errorf("domains and domain_details name different domains")
			}
		}
	}
	seen := map[string]bool{}
	out := make([]*aigriddraftv1.TenantDomain, 0, len(details))
	for _, d := range details {
		name := canonicalDomain(d.GetName())
		if err := validateDomain(name); err != nil {
			return err
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, &aigriddraftv1.TenantDomain{Name: name, Verified: d.GetVerified(), IdentityProviderAlias: d.GetIdentityProviderAlias(), AutoRedirect: d.GetAutoRedirect()})
	}
	t.DomainDetails = out
	t.Domains = make([]string, 0, len(out))
	for _, d := range out {
		t.Domains = append(t.Domains, d.GetName())
	}
	return nil
}

// checkIdentityProviders is OrganizationAdapter.resolveIdentityProvider
// (jpa/OrganizationAdapter.java L314-329), which setDomains calls for every
// domain that carries an alias (L203, L224) after all domains passed
// validation (L191-193). The fake realm has no identity providers, so every
// non-empty alias fails as an unknown one does in Keycloak (L318-320); an
// empty alias is "none" (the backend omits the key, which Keycloak reads as
// null, L315-317). Keycloak's other answer, "Identity provider '<alias>' is
// not associated with organization <name>" (L325-326), needs an identity
// provider and so cannot happen here. `autoRedirect` is stored without a
// check (L204, L225).
func checkIdentityProviders(t *aigriddraftv1.Tenant) error {
	for _, d := range t.GetDomainDetails() {
		if alias := d.GetIdentityProviderAlias(); alias != "" {
			return keycloakErrorf(MsgIdPMissingFmt, alias, KeycloakRealm)
		}
	}
	return nil
}

// canonicalDomain is what OrganizationDomainModel's constructor stores
// (OrganizationDomainModel.java L59): `name.trim().toLowerCase()`, with the
// lower-casing difference documented on emailDomain.
func canonicalDomain(name string) string {
	return strings.ToLower(javaTrim(name))
}
