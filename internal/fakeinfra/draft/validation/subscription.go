package validation

import (
	"encoding/json"
	"math"
	"regexp"
)

// The subscription rules are the PoC's createSchema and updateSchema
// (krisv/ai-grid-cloud-provider-poc shared/subscription-validation.mjs at
// 62319b8), with zod's default messages where the PoC sets none, plus the
// console's organization: a new subscription needs tokenMetadata.organizationId.
// The frontend runs the same rules as createSubscriptionSchema and
// updateSubscriptionSchema.
//
// The PoC's objects are passthrough, so unknown keys are allowed. As in zod,
// an object-level refinement runs only when nothing below it failed with a
// missing value or a wrong type; the helpers return false in that case.

// msgSelectOrganization is the console's message for a subscription without
// an organization.
const msgSelectOrganization = "Select an organization"

var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// The MaaSSubscription CRD's token rate limit rules (docs/draft/research/models-maas.md
// 3.5): the CEL messages are the CRD's, verbatim; the window pattern and the
// limit range are OpenAPI schema checks, worded as the API server's schema
// validator (kube-openapi) words them. That wording is unverified against a hub.
const (
	MsgTokenRateLimitsRequired  = "tokenRateLimits is required unless unlimited is true"
	MsgTokenRateLimitsUnlimited = "tokenRateLimits must not be set when unlimited is true"
	rateWindowPattern           = `^[1-9]\d{0,3}(s|m|h)$`
	MsgRateWindow               = "should match '" + rateWindowPattern + "'"
)

var rateWindow = regexp.MustCompile(rateWindowPattern)

// limitBounds is the schema validator's wording of the limit's minimum and maximum.
var limitBounds = bounds{min: "should be greater than or equal to ", max: "should be less than or equal to "}

// CreateSubscription checks the body of POST /api/v1/subscriptions.
func CreateSubscription(raw json.RawMessage) map[string]string {
	return checkSubscription(raw, true)
}

// UpdateSubscription checks the body of PUT /api/v1/subscriptions/{name}.
func UpdateSubscription(raw json.RawMessage) map[string]string {
	return checkSubscription(raw, false)
}

func checkSubscription(raw json.RawMessage, create bool) map[string]string {
	v, err := decodeOrdered(raw)
	if err != nil {
		return map[string]string{"": "Invalid JSON."}
	}
	c := &checker{}
	o, ok := v.(*object)
	if !ok {
		c.add("", "Expected object, received "+typeOf(v))
		return c.result()
	}
	c.never("namespace", o.get("namespace"))
	if f := o.get("displayName"); f.present {
		c.str("displayName", f, str{min: 1})
	}
	if f := o.get("description"); f.present {
		c.str("description", f, str{keepSpace: true})
	}
	c.number("priority", o.get("priority"), -1_000_000, 1_000_000)
	c.owner("owner", o.get("owner"))
	c.modelRefs("modelRefs", o.get("modelRefs"))
	switch f := o.get("tokenMetadata"); {
	case create && !f.present:
		c.add("tokenMetadata.organizationId", msgSelectOrganization)
	case f.present:
		c.metadata("tokenMetadata", f, create)
	}
	if f := o.get("inferencePriority"); f.present {
		c.number("inferencePriority", f, math.MinInt32, math.MaxInt32)
	}
	if f := o.get("guardrails"); f.present {
		c.guardrails("guardrails", f)
	}
	if create {
		c.str("name", o.get("name"), str{min: 1, max: 253, pattern: resourceName, patternMsg: "Invalid", keepSpace: true})
		c.boolean("createAuthPolicy", o.get("createAuthPolicy"))
	}
	return c.result()
}

// never is z.never().optional(): any value is wrong.
func (c *checker) never(path string, f field) {
	if f.present {
		c.add(path, "Expected never, received "+typeOf(f.value))
	}
}

// boolean is z.boolean().
func (c *checker) boolean(path string, f field) bool {
	if !f.present {
		c.add(path, "Required")
		return false
	}
	if _, ok := f.value.(bool); !ok {
		c.add(path, "Expected boolean, received "+typeOf(f.value))
		return false
	}
	return true
}

// number is z.number().int().min(lo).max(hi); false when the value is missing
// or not a number.
func (c *checker) number(path string, f field, lo, hi float64) bool {
	return c.numberWith(path, f, lo, hi, zodBounds)
}

// numberWith is number with its own range messages.
func (c *checker) numberWith(path string, f field, lo, hi float64, msgs bounds) bool {
	if !f.present {
		c.add(path, "Required")
		return false
	}
	n, ok := f.value.(json.Number)
	if !ok {
		c.add(path, "Expected number, received "+typeOf(f.value))
		return false
	}
	c.integerWith(path, field{value: n, present: true}, lo, hi, msgs)
	return true
}

// list is z.array() with min and max element counts (0 for none); false when
// the value is missing or not an array.
func (c *checker) list(path string, f field, minLen, maxLen int) ([]any, bool) {
	if !f.present {
		c.add(path, "Required")
		return nil, false
	}
	l, ok := f.value.([]any)
	if !ok {
		c.add(path, "Expected array, received "+typeOf(f.value))
		return nil, false
	}
	if minLen > 0 && len(l) < minLen {
		c.add(path, "Array must contain at least "+itoa(minLen)+" element(s)")
	}
	if maxLen > 0 && len(l) > maxLen {
		c.add(path, "Array must contain at most "+itoa(maxLen)+" element(s)")
	}
	return l, true
}

// passthrough is a passthrough zod object: present and an object.
func (c *checker) passthrough(path string, f field) (*object, bool) {
	if !f.present {
		c.add(path, "Required")
		return nil, false
	}
	o, ok := f.value.(*object)
	if !ok {
		c.add(path, "Expected object, received "+typeOf(f.value))
		return nil, false
	}
	return o, true
}

// optionalString is z.string().optional().
func (c *checker) optionalString(path string, f field) bool {
	if !f.present {
		return true
	}
	_, ok := c.str(path, f, str{keepSpace: true})
	return ok
}

func (c *checker) owner(path string, f field) {
	o, ok := c.passthrough(path, f)
	if !ok {
		return
	}
	if groups, ok := c.list(join(path, "groups"), o.get("groups"), 1, 0); ok {
		for i, g := range groups {
			p := join(join(path, "groups"), itoa(i))
			if gobj, ok := c.passthrough(p, field{value: g, present: true}); ok {
				c.str(join(p, "name"), gobj.get("name"), str{min: 1})
			}
		}
	}
	if u := o.get("users"); u.present {
		if users, ok := c.list(join(path, "users"), u, 0, 0); ok {
			for i, name := range users {
				c.str(join(join(path, "users"), itoa(i)), field{value: name, present: true}, str{min: 1, keepSpace: true})
			}
		}
	}
}

// modelRefs is the array of inputModelSchema, at least one, with unique names.
func (c *checker) modelRefs(path string, f field) {
	refs, ok := c.list(path, f, 1, 0)
	if !ok {
		return
	}
	names := make([]string, 0, len(refs))
	aborted := false
	for i, r := range refs {
		name, ok := c.modelRef(join(path, itoa(i)), r)
		if !ok {
			aborted = true
			continue
		}
		names = append(names, name)
	}
	// zod skips the refinement when an element failed with a wrong type.
	if aborted {
		return
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			c.add(path, "Models must be unique")
			return
		}
		seen[n] = true
	}
}

// modelRef is inputModelSchema; it returns the name, and false when the
// refinement could not run.
func (c *checker) modelRef(path string, v any) (string, bool) {
	o, ok := c.passthrough(path, field{value: v, present: true})
	if !ok {
		return "", false
	}
	valid := true
	name, ok := c.str(join(path, "name"), o.get("name"), str{min: 1, max: 253, pattern: dnsName, patternMsg: "Invalid", keepSpace: true})
	if ok && utf16Len(name) > 63 {
		c.add(join(path, "name"), "String must contain at most 63 character(s)")
	}
	valid = valid && ok
	// P6: the model's namespace; the backend checks it as a namespace name.
	valid = c.optionalString(join(path, "namespace"), o.get("namespace")) && valid
	valid = c.optionalString(join(path, "displayName"), o.get("displayName")) && valid
	valid = c.optionalString(join(path, "description"), o.get("description")) && valid
	unlimited := false
	if u := o.get("unlimited"); u.present {
		if c.boolean(join(path, "unlimited"), u) {
			unlimited, _ = u.value.(bool)
		} else {
			valid = false
		}
	}
	limits, ok := c.list(join(path, "tokenRateLimits"), o.get("tokenRateLimits"), 0, 0)
	if ok {
		for i, l := range limits {
			valid = c.rate(join(join(path, "tokenRateLimits"), itoa(i)), l) && valid
		}
	} else {
		valid = false
	}
	if b := o.get("billingRate"); b.present {
		if bo, ok := c.passthrough(join(path, "billingRate"), b); ok {
			_, ok := c.str(join(join(path, "billingRate"), "perToken"), bo.get("perToken"), str{keepSpace: true})
			valid = ok && valid
		} else {
			valid = false
		}
	}
	if g := o.get("guardrails"); g.present {
		valid = c.guardrails(join(path, "guardrails"), g) && valid
	}
	if !valid {
		return name, false
	}
	switch {
	case !unlimited && len(limits) == 0:
		c.add(path, MsgTokenRateLimitsRequired)
	case unlimited && len(limits) > 0:
		c.add(path, MsgTokenRateLimitsUnlimited)
	}
	return name, true
}

// rate is {limit: 1 to 1,000,000,000 tokens, window: `<n><s|m|h>` with n from
// 1 to 9999}, with the CRD's messages.
func (c *checker) rate(path string, v any) bool {
	o, ok := c.passthrough(path, field{value: v, present: true})
	if !ok {
		return false
	}
	okLimit := c.numberWith(join(path, "limit"), o.get("limit"), 1, 1_000_000_000, limitBounds)
	_, okWindow := c.str(join(path, "window"), o.get("window"), str{pattern: rateWindow, patternMsg: MsgRateWindow, keepSpace: true})
	return okLimit && okWindow
}

// guardrails is at most 64 of {ref: {name: dnsName} (strict), checks?: 1-63 characters, at most 64}.
func (c *checker) guardrails(path string, f field) bool {
	list, ok := c.list(path, f, 0, 64)
	if !ok {
		return false
	}
	valid := true
	for i, g := range list {
		p := join(path, itoa(i))
		o, ok := c.passthrough(p, field{value: g, present: true})
		if !ok {
			valid = false
			continue
		}
		if ref, ok := c.object(join(p, "ref"), o.get("ref").value, o.get("ref").present, []string{"name"}); ok {
			_, ok := c.str(join(join(p, "ref"), "name"), ref.get("name"), str{min: 1, max: 253, pattern: dnsName, patternMsg: "Invalid", keepSpace: true})
			valid = ok && valid
		} else {
			valid = false
		}
		if ch := o.get("checks"); ch.present {
			checks, ok := c.list(join(p, "checks"), ch, 0, 64)
			if !ok {
				valid = false
				continue
			}
			for j, s := range checks {
				_, ok := c.str(join(join(p, "checks"), itoa(j)), field{value: s, present: true}, str{min: 1, max: 63, keepSpace: true})
				valid = ok && valid
			}
		}
	}
	return valid
}

// metadata is {organizationId?, costCenter?, labels?: record of strings}; on
// create the organization is required.
func (c *checker) metadata(path string, f field, create bool) {
	o, ok := c.passthrough(path, f)
	if !ok {
		return
	}
	org := o.get("organizationId")
	switch {
	case create && !org.present:
		c.add(join(path, "organizationId"), msgSelectOrganization)
	case create:
		c.str(join(path, "organizationId"), org, str{min: 1, minMsg: msgSelectOrganization, keepSpace: true})
	default:
		c.optionalString(join(path, "organizationId"), org)
	}
	c.optionalString(join(path, "costCenter"), o.get("costCenter"))
	if l := o.get("labels"); l.present {
		labels, ok := l.value.(*object)
		if !ok {
			c.add(join(path, "labels"), "Expected object, received "+typeOf(l.value))
			return
		}
		for _, k := range labels.keys {
			c.str(join(join(path, "labels"), k), labels.get(k), str{keepSpace: true})
		}
	}
}
