package redact

import "testing"

func TestMatchesTokenHeader(t *testing.T) {
	cases := map[string]bool{
		"TOKEN abcdefghij1234567890XYZ":     true, // upper-case spelling
		"token\nabcdefghij1234567890XYZ":    true, // \s includes newline
		"Token abcdefghij1234567890XY":      true, // and NBSP
		"Token abcdefghij1234567890XYZ.com": false,
		"Token abcdefghijklmnopqrstuvwxyz":  false, // no digit / upper
		"ToKeN abcdefghij1234567890XYZ":     false,
	}
	for in, want := range cases {
		if got := matchesTokenHeader(in); got != want {
			t.Errorf("matchesTokenHeader(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMatchesAuthSchemeHumps(t *testing.T) {
	cases := map[string]bool{
		"Bearer abcdefGhijkl":   false, // one hump only
		"Bearer abcDefghIjklmn": false, // humps followed by lower case
		"Bearer abcDEfghIJklm":  true,  // two humps, upper not followed by lower
		"Bearer abcdefgh1":      true,
		"Basic\tabcdefgh=":      true,
		"Bearer abc1":           false, // shorter than 8
	}
	for in, want := range cases {
		if got := matchesAuthScheme(in); got != want {
			t.Errorf("matchesAuthScheme(%q) = %v, want %v", in, got, want)
		}
	}
}
