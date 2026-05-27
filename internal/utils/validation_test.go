package utils

import "testing"

func TestIsValidName(t *testing.T) {
	cases := []struct {
		desc string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"alphanumeric", "abc123", true},
		{"hyphen and underscore", "a-b_c", true},
		{"space", "a b", false},
		{"dot", "a.b", false},
		{"forward slash", "a/b", false},
		{"backslash", "a\\b", false},
		{"unicode (Japanese)", "日本語", false},
		{"tab", "a\tb", false},
		{"newline", "a\nb", false},
		{"single dot", ".", false},
		{"double dot", "..", false},
		// Decision: a leading hyphen is ALLOWED. Names are only ever used with
		// filepath.Join and go-git (never passed to a shell), so there is no
		// flag-injection risk, and rejecting it could break existing names.
		{"leading hyphen", "-name", true},
		{"only underscore", "_", true},
	}
	for _, c := range cases {
		if got := IsValidName(c.in); got != c.want {
			t.Errorf("%s: IsValidName(%q) = %v, want %v", c.desc, c.in, got, c.want)
		}
	}
}
