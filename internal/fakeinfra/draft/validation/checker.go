package validation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// checker collects field errors the way the frontend's fieldErrorsOf does
// with a zod result: keyed by JSON path, the last message per path, as the
// PoC keeps them (Object.fromEntries over the issues). The
// helpers mirror the zod behaviour the PoC's schemas rely on, including zod's
// default messages, so the shared cases agree word for word.
type checker struct {
	errs map[string]string
}

func (c *checker) add(path, msg string) {
	if c.errs == nil {
		c.errs = map[string]string{}
	}
	c.errs[path] = msg
}

func (c *checker) result() map[string]string { return c.errs }

// field is a member of a JSON object; present is false when the key is absent
// (zod's undefined), as opposed to null.
type field struct {
	value   any
	present bool
}

// object is a decoded JSON object that remembers its key order, which zod
// uses in its "Unrecognized key(s)" message.
type object struct {
	keys []string
	vals map[string]any
}

func (o *object) get(k string) field {
	v, ok := o.vals[k]
	return field{value: v, present: ok}
}

// object checks a strict zod object: it must be present and an object, and
// may hold only the known keys.
func (c *checker) object(path string, v any, present bool, known []string) (*object, bool) {
	if !present {
		c.add(path, "Required")
		return nil, false
	}
	o, ok := v.(*object)
	if !ok {
		c.add(path, "Expected object, received "+typeOf(v))
		return nil, false
	}
	var unknown []string
	for _, k := range o.keys {
		if !contains(known, k) {
			unknown = append(unknown, "'"+k+"'")
		}
	}
	if len(unknown) > 0 {
		c.add(path, "Unrecognized key(s) in object: "+strings.Join(unknown, ", "))
	}
	return o, true
}

// str describes a zod string schema: trim (unless keepSpace), optional
// lowercasing, then the min, max and regex checks in that order. optional
// means a default of "" when absent.
type str struct {
	min, max   int
	minMsg     string
	pattern    *regexp.Regexp
	patternMsg string
	lower      bool
	optional   bool
	keepSpace  bool
}

// str checks a string and returns its transformed value. ok is false when the
// value is missing or not a string, which zod treats as fatal for the parent
// refinements.
func (c *checker) str(path string, f field, rule str) (string, bool) {
	if !f.present {
		if rule.optional {
			return "", true
		}
		c.add(path, "Required")
		return "", false
	}
	s, ok := f.value.(string)
	if !ok {
		c.add(path, "Expected string, received "+typeOf(f.value))
		return "", false
	}
	if !rule.keepSpace {
		s = strings.TrimFunc(s, isJSSpace)
	}
	if rule.lower {
		s = strings.ToLower(s)
	}
	n := len(utf16.Encode([]rune(s)))
	if rule.min > 0 && n < rule.min {
		msg := rule.minMsg
		if msg == "" {
			msg = "String must contain at least " + itoa(rule.min) + " character(s)"
		}
		c.add(path, msg)
	}
	if rule.max > 0 && n > rule.max {
		c.add(path, "String must contain at most "+itoa(rule.max)+" character(s)")
	}
	if rule.pattern != nil && !rule.pattern.MatchString(s) {
		c.add(path, rule.patternMsg)
	}
	return s, true
}

// isJSSpace is what String.prototype.trim removes.
func isJSSpace(r rune) bool { return unicode.IsSpace(r) || r == 0xFEFF }

// typeOf names a decoded JSON value the way zod's "received" does.
func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	}
	return "object"
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func itoa(i int) string { return strconv.Itoa(i) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// decodeOrdered decodes JSON into *object, []any, string, json.Number, bool
// and nil, keeping each object's key order. A repeated key keeps its first
// position and its last value, as JSON.parse does.
func decodeOrdered(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &object{vals: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := o.vals[k]; !dup {
					o.keys = append(o.keys, k)
				}
				o.vals[k] = v
			}
			_, err := dec.Token()
			return o, err
		case '[':
			list := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			_, err := dec.Token()
			return list, err
		}
		return nil, errors.New("unexpected delimiter")
	default:
		return t, nil
	}
}

// resourceName is a Kubernetes resource name segment (DNS-1123 label characters).
var resourceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// utf16Len is a JavaScript string's length.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// bounds words an integer's range violations; zodBounds is zod's wording.
type bounds struct{ min, max string }

var zodBounds = bounds{min: "Number must be greater than or equal to ", max: "Number must be less than or equal to "}

// integerWith is z.number().int().min(lo).max(hi), with msgs for the range
// violations, each followed by the bound.
func (c *checker) integerWith(path string, f field, lo, hi float64, msgs bounds) {
	if !f.present {
		c.add(path, "Required")
		return
	}
	n, ok := f.value.(json.Number)
	if !ok {
		c.add(path, "Expected number, received "+typeOf(f.value))
		return
	}
	x, err := strconv.ParseFloat(string(n), 64)
	if err != nil && !math.IsInf(x, 0) {
		c.add(path, "Expected number, received "+typeOf(f.value))
		return
	}
	if x != math.Trunc(x) {
		c.add(path, "Expected integer, received float")
	}
	if x < lo {
		c.add(path, msgs.min+strconv.FormatFloat(lo, 'f', -1, 64))
	}
	if x > hi {
		c.add(path, msgs.max+strconv.FormatFloat(hi, 'f', -1, 64))
	}
}
