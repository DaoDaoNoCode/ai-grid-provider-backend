// Package names validates RFC 1123 DNS labels used as resource ids and
// hostname parts across Clusters, Tenants, Models, and Subscriptions.
package names

import "regexp"

// dnsLabelRE matches a single RFC 1123 DNS label: 1–63 chars, lowercase
// alphanumeric, hyphens allowed inside.
var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidDNSLabel reports whether s is a valid RFC 1123 DNS label.
func ValidDNSLabel(s string) bool {
	return dnsLabelRE.MatchString(s)
}
