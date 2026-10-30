package pathguard

import "testing"

// winEngine is an allowOnly engine over C:\Work with a directory deny rule
// (secret/**) and a file deny rule (key.txt).
func winEngine() RuleSetEngine {
	return RuleSetEngine{
		Mode:        ModeAllowOnly,
		Platform:    "windows",
		WorkingDirs: []string{`C:\Work`},
		Rules:       []Rule{{Root: `C:\Work`, Pattern: "**"}},
		DenyWithinAllow: []Rule{
			{Root: `C:\Work`, Pattern: "secret/**", Reason: "secret dir denied"},
			{Root: `C:\Work`, Pattern: "key.txt", Reason: "key denied"},
		},
	}
}

func one(p string) PathSpellings { return PathSpellings{Requested: p, Spellings: []string{p}} }

// TestDenyRuleCoversWindowsSpellings checks that no Windows spelling of a
// denied file gets through: case and slash variants match the deny rule, and
// every spelling string matching cannot fold is refused outright.
func TestDenyRuleCoversWindowsSpellings(t *testing.T) {
	e := winEngine()
	type want struct {
		typ, reason, breaker string
	}
	rule := func(r string) want { return want{ReasonTypeRule, r, ""} }
	readSusp := want{ReasonTypeOther, ReasonSuspiciousWindowsRead, ""}
	writeSusp := func(p string) want {
		return want{ReasonTypeSafetyCheck, MsgSuspiciousWindowsWrite(p), circuitBreakerSuspiciousWinPath}
	}
	cases := []struct {
		name  string
		path  string
		read  want
		write want
	}{
		{"exact", `C:\Work\key.txt`, rule("key denied"), rule("key denied")},
		{"case", `c:\WORK\KEY.TXT`, rule("key denied"), rule("key denied")},
		{"forward slashes", `C:/Work/key.txt`, rule("key denied"), rule("key denied")},
		{"mixed slashes", `C:/Work\secret/a.txt`, rule("secret dir denied"), rule("secret dir denied")},
		{"case dir", `C:\work\SECRET\a.txt`, rule("secret dir denied"), rule("secret dir denied")},
		{"8.3 short name", `C:\Work\SECRET~1\a.txt`, readSusp, writeSusp(`C:\Work\SECRET~1\a.txt`)},
		{"8.3 file", `C:\Work\KEY~1.TXT`, readSusp, writeSusp(`C:\Work\KEY~1.TXT`)},
		{"ADS ::$DATA", `C:\Work\key.txt::$DATA`, readSusp, writeSusp(`C:\Work\key.txt::$DATA`)},
		{"ADS named", `C:\Work\key.txt:s`, readSusp, writeSusp(`C:\Work\key.txt:s`)},
		{"trailing dot", `C:\Work\key.txt.`, readSusp, writeSusp(`C:\Work\key.txt.`)},
		{"trailing space", `C:\Work\key.txt `, readSusp, writeSusp(`C:\Work\key.txt `)},
		{"\\\\?\\ prefix", `\\?\C:\Work\key.txt`, readSusp, writeSusp(`\\?\C:\Work\key.txt`)},
		{"\\\\.\\ prefix", `\\.\C:\Work\key.txt`, readSusp, writeSusp(`\\.\C:\Work\key.txt`)},
		{"//?/ prefix", `//?/C:/Work/key.txt`, readSusp, writeSusp(`//?/C:/Work/key.txt`)},
		{"\\??\\ prefix", `\??\C:\Work\key.txt`, readSusp, writeSusp(`\??\C:\Work\key.txt`)},
		{"UNC admin share", `\\localhost\C$\Work\key.txt`, readSusp, writeSusp(`\\localhost\C$\Work\key.txt`)},
		{"device suffix", `C:\Work\key.nul`, readSusp, writeSusp(`C:\Work\key.nul`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, op := range []Operation{OpRead, OpWrite} {
				w := c.read
				if op == OpWrite {
					w = c.write
				}
				d := e.EvaluateSpellings(one(c.path), op)
				if d.Allowed || d.Reason == nil {
					t.Fatalf("%s %q allowed: %+v", op, c.path, d)
				}
				if d.Reason.Type != w.typ || d.Reason.Reason != w.reason || d.Reason.CircuitBreaker != w.breaker {
					t.Errorf("%s %q = %+v, want %+v", op, c.path, *d.Reason, w)
				}
			}
		})
	}

	// Sanity: an ordinary file in the tree is allowed, and the foldWindowsSpelling fold makes
	// the drive letter case irrelevant even on the (unfolded) allow side.
	for _, p := range []string{`C:\Work\src\a.go`, `c:\Work\src\a.go`, `C:/Work/src/a.go`} {
		if d := e.EvaluateSpellings(one(p), OpRead); !d.Allowed {
			t.Errorf("read %q refused: %+v", p, d.Reason)
		}
	}
}

// TestAllowSideIsNotCaseFolded: only deny matching is case-folded, so an allow rule
// does not cover a differently-cased spelling (the safe direction).
func TestAllowSideIsNotCaseFolded(t *testing.T) {
	e := winEngine()
	if d := e.EvaluateSpellings(one(`C:\work\src\a.go`), OpRead); d.Allowed {
		t.Fatalf("allow rule C:\\Work matched C:\\work: %+v", d)
	}
	posix := RuleSetEngine{
		Mode:            ModeAllowOnly,
		CaseInsensitive: true,
		Rules:           []Rule{{Root: "/work", Pattern: "**"}},
		DenyWithinAllow: []Rule{{Root: "/work", Pattern: "Secret/**"}},
	}
	if d := posix.EvaluateSpellings(one("/work/SECRET/x"), OpRead); d.Allowed || d.Reason.Type != ReasonTypeRule {
		t.Errorf("caseInsensitiveTarget deny should fold: %+v", d)
	}
	if d := posix.EvaluateSpellings(one("/WORK/x"), OpRead); d.Allowed {
		t.Errorf("caseInsensitiveTarget must not fold the allow side: %+v", d)
	}
}

// TestDenyScanCoversEverySpelling: a deny rule hit on any spelling (the
// requested symlink name, a hop, the landing, or a deny-only extra such as
// the long form of an 8.3 name) refuses; an allow needs every spelling.
func TestDenyScanCoversEverySpelling(t *testing.T) {
	e := winEngine()
	d := e.EvaluateSpellings(PathSpellings{
		Requested: `C:\Work\link.txt`,
		Spellings: []string{`C:\Work\link.txt`, `C:\Work\key.txt`},
	}, OpWrite)
	if d.Allowed || d.Reason.Reason != "key denied" {
		t.Errorf("landing spelling not deny-scanned: %+v", d)
	}
	d = e.EvaluateSpellings(PathSpellings{
		Requested: `C:\Work\link.txt`,
		Spellings: []string{`C:\Work\link.txt`},
		DenyOnly:  []string{`C:\Work\key.txt`},
	}, OpRead)
	if d.Allowed || d.Reason.Reason != "key denied" {
		t.Errorf("deny-only spelling not scanned: %+v", d)
	}
	d = e.EvaluateSpellings(PathSpellings{
		Requested: `C:\Work\a.txt`,
		Spellings: []string{`C:\Work\a.txt`, `D:\Elsewhere\a.txt`},
	}, OpRead)
	if d.Allowed {
		t.Errorf("allow must cover every spelling: %+v", d)
	}
}

func TestEvaluateSpellingsCircuitBreakers(t *testing.T) {
	base := RuleSetEngine{
		Mode:        ModeAllowOnly,
		WorkingDirs: []string{"/work"},
		Rules:       []Rule{{Root: "/", Pattern: "**"}},
	}
	undetermined := MsgUndetermined("/work/x")
	unresolved := PathSpellings{Requested: "/work/x", Spellings: []string{"/work/x"}, Unresolved: true}

	check := func(name string, d *Decision, typ, reason, cb string) {
		t.Helper()
		if d.Allowed || d.Reason == nil || d.Reason.Type != typ || d.Reason.Reason != reason || d.Reason.CircuitBreaker != cb {
			t.Errorf("%s: got %+v, want %s/%q/%q", name, d.Reason, typ, reason, cb)
		}
	}

	check("unresolved read", base.EvaluateSpellings(unresolved, OpRead), ReasonTypeOther, undetermined, "")
	check("unresolved write", base.EvaluateSpellings(unresolved, OpWrite), ReasonTypeSafetyCheck, undetermined, "")

	blk := base
	blk.BlockReadsOutsideWorkingDirectories = true
	check("unresolved read, reads blocked", blk.EvaluateSpellings(unresolved, OpRead), ReasonTypeSafetyCheck, undetermined, circuitBreakerOutsideReads)
	check("read outside, reads blocked", blk.EvaluateSpellings(one("/etc/passwd"), OpRead), ReasonTypeSafetyCheck, ReasonReadsOutsideBlocked, circuitBreakerOutsideReads)
	if d := blk.EvaluateSpellings(one("/work/x"), OpRead); !d.Allowed {
		t.Errorf("read inside with reads blocked refused: %+v", d.Reason)
	}
	check("read via UNC spelling of a working dir", blk.EvaluateSpellings(one("//work/x"), OpRead), ReasonTypeSafetyCheck, ReasonReadsOutsideBlocked, circuitBreakerOutsideReads)

	rst := base
	rst.Restricted = true
	check("unresolved read, restricted", rst.EvaluateSpellings(unresolved, OpRead), ReasonTypeSafetyCheck, undetermined, circuitBreakerRestrictedMode)
	check("restricted outside", rst.EvaluateSpellings(one("/etc/passwd"), OpRead), ReasonTypeOther, ReasonRestrictedOutside, "")
	check("restricted write outside", rst.EvaluateSpellings(one("/etc/passwd"), OpWrite), ReasonTypeOther, ReasonRestrictedOutside, "")
	check("restricted suspicious write", rst.EvaluateSpellings(one("/work/PROGRA~1"), OpWrite), ReasonTypeSafetyCheck, MsgSuspiciousWindowsWrite("/work/PROGRA~1"), circuitBreakerRestrictedMode)
	if d := rst.EvaluateSpellings(one("/work/x"), OpWrite); !d.Allowed {
		t.Errorf("restricted write inside refused: %+v", d.Reason)
	}

	// The suspicious check is not Windows-only for the platform-free kinds.
	check("8.3 write on linux", base.EvaluateSpellings(one("/work/PROGRA~1/x"), OpCreate), ReasonTypeSafetyCheck, MsgSuspiciousWindowsWrite("/work/PROGRA~1/x"), circuitBreakerSuspiciousWinPath)
	check("8.3 read on linux", base.EvaluateSpellings(one("/work/PROGRA~1/x"), OpRead), ReasonTypeOther, ReasonSuspiciousWindowsRead, "")
}

// TestTrustedNetworkDirectory: a UNC spelling under a trusted share is not
// suspicious, so the ordinary rules decide.
func TestTrustedNetworkDirectory(t *testing.T) {
	e := RuleSetEngine{
		Mode:                      ModeAllowOnly,
		Platform:                  "windows",
		Rules:                     []Rule{{Root: `\\nas\team`, Pattern: "**"}},
		TrustedNetworkDirectories: []string{`\\nas\team`},
	}
	if d := e.EvaluateSpellings(one(`\\nas\team\doc.txt`), OpRead); !d.Allowed {
		t.Errorf("trusted share read refused: %+v", d.Reason)
	}
	if d := e.EvaluateSpellings(one(`\\other\share\doc.txt`), OpRead); d.Allowed || d.Reason.Reason != ReasonSuspiciousWindowsRead {
		t.Errorf("untrusted share read: %+v", d)
	}
}
