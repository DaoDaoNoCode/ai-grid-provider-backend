package names

import "testing"

func TestValidDNSLabel(t *testing.T) {
	t.Parallel()
	tooLong := make([]byte, 64)
	for i := range tooLong {
		tooLong[i] = 'a'
	}
	for name, tc := range map[string]struct {
		s    string
		want bool
	}{
		"simple":          {"playground-east", true},
		"single":          {"a", true},
		"digits":          {"a1b2", true},
		"empty":           {"", false},
		"upper":           {"East", false},
		"underscore":      {"a_b", false},
		"leading-hyphen":  {"-a", false},
		"trailing-hyphen": {"a-", false},
		"too-long":        {string(tooLong), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ValidDNSLabel(tc.s); got != tc.want {
				t.Fatalf("ValidDNSLabel(%q) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}
