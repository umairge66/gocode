package cli

import (
	"strings"
	"testing"
)

func TestParseMaxTurnsFlag(t *testing.T) {
	for in, want := range map[string]int{"3": 3, " 10 ": 10, "1,000": 1000} {
		if got, err := parseMaxTurnsFlag(in); err != nil || got != want {
			t.Errorf("parseMaxTurnsFlag(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "-2", "1.5", "abc"} {
		_, err := parseMaxTurnsFlag(in)
		if err == nil || !strings.Contains(err.Error(), "--max-turns must be a positive integer; got") {
			t.Errorf("parseMaxTurnsFlag(%q) err = %v", in, err)
		}
	}
}

func TestMaxTurnsFlagParsedAndWinsOverEnv(t *testing.T) {
	d, rest := parseDispatchDefaults([]string{"-p", "--max-turns", "4", "hello"})
	if d == nil || !d.MaxTurnsSet || d.MaxTurns != "4" {
		t.Fatalf("defaults = %+v", d)
	}
	if strings.Join(rest, " ") != "-p hello" {
		t.Fatalf("rest = %v (the value must not become the prompt)", rest)
	}
	d2, _ := parseDispatchDefaults([]string{"--max-turns=7"})
	if d2 == nil || d2.MaxTurns != "7" {
		t.Fatalf("inline = %+v", d2)
	}

	env := &Env{Getenv: func(k string) string {
		if k == "GOCODE_CODE_MAX_TURNS" {
			return "zero"
		}
		return ""
	}}
	if n, err := resolveMaxTurns(env, d); err != nil || n != 4 {
		t.Fatalf("flag should win: %d %v", n, err)
	}
	if _, err := resolveMaxTurns(env, &DispatchDefaults{}); err == nil {
		t.Fatal("invalid env accepted without a flag")
	}
	s := newPrintSession(&Env{Getenv: func(string) string { return "" }, Cwd: func() (string, error) { return "/", nil }}, &DispatchDefaults{MaxTurnsLimit: 4})
	if s.MaxTurns != 4 {
		t.Fatalf("print session MaxTurns = %d", s.MaxTurns)
	}
}

func TestInvalidMaxTurnsFlagAbortsStartup(t *testing.T) {
	f := newSubcmdFixture(t)
	code, _, errOut := f.run(t, "-p", "--max-turns", "abc", "hi")
	if code != 1 || !strings.Contains(errOut, `--max-turns must be a positive integer; got "abc"`) {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	// A valid flag overrides an invalid variable: startup proceeds past the
	// check (and then stops for lack of an API key).
	f.vars["GOCODE_CODE_MAX_TURNS"] = "zero"
	_, _, errOut = f.run(t, "-p", "--max-turns", "2", "hi")
	if strings.Contains(errOut, "must be a positive integer") {
		t.Fatalf("flag did not win over the variable: %q", errOut)
	}
}
