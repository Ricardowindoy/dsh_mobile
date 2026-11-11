package update

import "testing"

func TestIsVersionGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.0.1", "1.0.0", true},
		{"1.0.0", "1.0.1", false},
		{"0.2.46", "0.2.45", true},
		{"v1.2.3", "1.2.2", true},
		{"1.2.3", "v1.2.3", false},
		{"1.10.0", "1.9.9", true},
		{"1.2.3", "1.2.3", false},
		{"2.0", "1.99.99", true},
		{"1.2", "1.2.0", false},
		{"0.1.9", "0.1.10", false},
	}
	for _, c := range cases {
		if got := isVersionGreater(c.a, c.b); got != c.want {
			t.Errorf("isVersionGreater(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
