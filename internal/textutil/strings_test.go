package textutil

import "testing"

func TestCountCodePoints(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"", 0},
		{"héllo", 5},
		{"😀", 1}, // one code point, surrogate pair in UTF-16
	}
	for _, c := range cases {
		if got := CountCodePoints(c.in); got != c.want {
			t.Errorf("CountCodePoints(%q)=%d want %d", c.in, got, c.want)
		}
	}
}

func TestFirstCodePoints(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abcdef", 3, "abc"},
		{"abc", 10, "abc"},
		{"abc", 0, ""},
		{"héllo", 2, "hé"},
	}
	for _, c := range cases {
		if got := FirstCodePoints(c.in, c.n); got != c.want {
			t.Errorf("FirstCodePoints(%q,%d)=%q want %q", c.in, c.n, got, c.want)
		}
	}
}
