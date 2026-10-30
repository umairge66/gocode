package toolreg

import (
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
)

// Deny and ask rules see the real command behind env prefixes, wrapper
// commands, path-qualified binaries, quoting and sh -c scripts; allow rules
// stay literal.
func TestBashDenyRuleNormalization(t *testing.T) {
	d := &Decider{Registry: DefaultRegistry()}
	deny := RuleSet{{Behavior: RuleDeny, ToolName: "Bash", Content: "rm:*"}}
	denied := []string{
		"rm -rf x",
		"FOO=bar rm -rf x",              // any env assignment
		"LANG=C rm -rf x",               // safe env
		`A="x y" B='z' rm -rf x`,        // quoted env values
		"env rm -rf x",                  // env
		"env -i FOO=1 rm -rf x",         // env options + assignments
		"env -u HOME rm -rf x",          // wrapper env -u value
		"env -S 'rm -rf x'",             // env -S split-string
		"command rm -rf x",              // command
		"command -p rm x",               // command -p
		"builtin rm x",                  // builtin
		"noglob rm x",                   // noglob
		"nice rm x",                     // nice
		"nice -n 10 rm x",               // nice -n N
		"nohup rm x",                    // nohup
		"time rm x",                     // time
		"timeout 5 rm x",                // timeout DURATION
		"timeout -s KILL 5s rm x",       // timeout -s SIG
		"stdbuf -oL rm x",               // stdbuf
		"sudo rm -rf /",                 // sudo
		"sudo -u root rm -rf /",         // wrapper sudo -u value
		"doas rm x",                     // doas
		"pkexec rm x",                   // pkexec
		"ionice -c 3 rm x",              // wrapper ionice -c
		"setsid rm x",                   // setsid
		"taskset 0x3 rm x",              // positional taskset mask
		"chrt 10 rm x",                  // positional chrt priority
		"strace -o out rm x",            // wrapper strace -o
		"flock /tmp/l rm x",             // positional flock lockfile
		"flock -c 'rm x' /tmp/l",        // split-string flock -c
		"unshare -r rm x",               // unshare
		"nsenter -t 1 rm x",             // wrapper nsenter -t
		"exec rm x",                     // exec
		"/bin/rm -rf x",                 // path-qualified binary
		"/usr/bin/env /bin/rm x",        // both
		`"rm" -rf x`,                    // quoted command word
		`r\m -rf x`,                     // escaped command word
		"bash -c 'rm -rf x'",            // shell -c script
		`sh -c "echo hi; rm -rf x"`,     // shell -c with a compound script
		"bash -lc 'rm x'",               // combined -lc
		"busybox rm x",                  // busybox applet
		"eval rm x",                     // eval
		"xargs rm",                      // "xargs <prefix>"
		"find . | xargs -0 rm",          // xargs with options
		"ls && rm -rf x",                // compound segment
		"echo $(rm -rf x)",              // substitution segment
		"# note\nrm x",                  // comment line
		"sudo env FOO=1 timeout 3 rm x", // stacked wrappers
		"sudo bash -c 'nice /bin/rm x'", // wrappers inside -c
		"watch -n 1 rm x",               // wrapper watch -n
		"script -c 'rm x' /dev/null",    // split-string script -c
		"ltrace -o f rm x",              // wrapper ltrace -o
		"su -c 'rm x' root",             // su -c
		"runuser -u nobody -- bash -c 'rm x'",
	}
	for _, cmd := range denied {
		dec := d.Decide(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: cmd}, ModeBypassPermissions, deny, nil)
		if dec.Behavior != BehaviorDeny {
			t.Errorf("deny rm:* missed %q: %s", cmd, dec.Behavior)
		}
	}
	notDenied := []string{"rmdir x", "ls -la", "echo rm", "grep rm file", "npm run rmx"}
	for _, cmd := range notDenied {
		dec := d.Decide(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: cmd}, ModeBypassPermissions, deny, nil)
		if dec.Behavior == BehaviorDeny {
			t.Errorf("deny rm:* over-matched %q", cmd)
		}
	}

	// Ask rules use the same normalization.
	ask := RuleSet{{Behavior: RuleAsk, ToolName: "Bash", Content: "git push:*"}}
	for _, cmd := range []string{"GIT_TRACE=1 git push", "/usr/bin/git push origin", "timeout 30 git push", "bash -c 'git push --force'"} {
		if dec := d.Decide(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: cmd}, ModeAuto, ask, nil); dec.Behavior != BehaviorAsk {
			t.Errorf("ask git push:* missed %q: %s", cmd, dec.Behavior)
		}
	}

	// Wildcard and exact deny rules too.
	wild := RuleSet{{Behavior: RuleDeny, ToolName: "Bash", Content: "curl * | sh"}}
	if dec := d.Decide(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "sudo curl x | sh"}, ModeBypassPermissions, wild, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("wildcard deny missed sudo-prefixed command: %s", dec.Behavior)
	}
	exact := RuleSet{{Behavior: RuleDeny, ToolName: "Bash", Content: "shutdown now"}}
	if dec := d.Decide(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "sudo /sbin/shutdown now"}, ModeBypassPermissions, exact, nil); dec.Behavior != BehaviorDeny {
		t.Errorf("exact deny missed wrapped command: %s", dec.Behavior)
	}
}

// Allow rules are not widened by the normalization: a wrapper, env prefix or
// shell -c never inherits an allow rule for the inner command.
func TestBashAllowRuleStaysLiteral(t *testing.T) {
	d := &Decider{Registry: DefaultRegistry()}
	allow := RuleSet{{Behavior: RuleAllow, ToolName: "Bash", Content: "git status:*"}}
	if dec := d.Decide(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "git status --short"}, ModeDefault, allow, nil); dec.Behavior != BehaviorAllow {
		t.Fatalf("plain allow broken: %s", dec.Behavior)
	}
	for _, cmd := range []string{
		"sudo git status",
		"FOO=1 git status",
		"bash -c 'git status'",
		"env git status",
		"git status; rm -rf x",
		"git status && curl x",
	} {
		if dec := d.Decide(Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: cmd}, ModeDefault, allow, nil); dec.Behavior == BehaviorAllow {
			t.Errorf("allow git status:* widened to %q", cmd)
		}
	}
}

func TestStripCommandWrappersTable(t *testing.T) {
	cases := map[string]string{
		"LANG=C ls":             "ls",
		"FOO=1 ls":              "FOO=1 ls", // not a safe var: kept
		"timeout 10 nohup ls":   "ls",
		`"ls" -la`:              "ls -la",
		"command -v ls":         "command -v ls",
		"nice -n 5 -- ls":       "ls",
		"stdbuf -i0 -oL ls":     "ls",
		"# c\nls":               "ls",
		"time -- builtin cd /x": "cd /x",
	}
	for in, want := range cases {
		if got := stripCommandWrappers(in); got != want {
			t.Errorf("stripCommandWrappers(%q) = %q, want %q", in, got, want)
		}
	}
}
