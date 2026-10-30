package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

func writeTrustFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A plain folder — code, gocode.md, harmless settings, a .mcp.json (its
// servers need approval anyway), skills/commands without `!`-blocks or
// grants — is never asked about.
func TestTrustAssessPlainFolder(t *testing.T) {
	f := newSubcmdFixture(t)
	writeTrustFile(t, filepath.Join(f.cwd, "gocode.md"), "# project memory\nUse tabs.\n")
	writeTrustFile(t, filepath.Join(f.cwd, "CLAUDE.md"), "# memory\n")
	writeTrustFile(t, filepath.Join(f.cwd, "main.go"), "package main\n")
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "settings.json"), `{"theme":"dark","permissions":{"deny":["Bash(rm:*)"],"defaultMode":"plan"}}`)
	writeTrustFile(t, filepath.Join(f.cwd, ".mcp.json"), `{"mcpServers":{"srv":{"command":"x"}}}`)
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "skills", "plain", "SKILL.md"), "---\nname: plain\ndescription: plain skill\n---\nJust text with `code`.\n")
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "commands", "hello.md"), "---\ndescription: hi\n---\nSay hello.\n")
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "agents", "rev.md"), "---\nname: rev\ndescription: reviews\npermissionMode: plan\n---\nReview.\n")
	env, _, _ := f.env()
	useRealTrust(env)
	rep := assessWorkspaceTrust(env)
	if rep.trusted {
		t.Fatal("an unaccepted folder reported trusted")
	}
	if len(rep.items) != 0 || rep.needsPrompt() {
		t.Fatalf("plain folder needs a prompt: %v", rep.items)
	}
}

// Every risky item is listed with where it comes from.
func TestTrustAssessListsEachRisk(t *testing.T) {
	f := newSubcmdFixture(t)
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "settings.json"), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"x"}]}]},"statusLine":{"type":"command","command":"st"},"permissions":{"allow":["Bash(*)"],"defaultMode":"acceptEdits"},"model":"evil/model","enabledPlugins":{"p@m":true}}`)
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "settings.local.json"), `{"mcpServers":{"loc":{"command":"x"}},"enabledMcpjsonServers":["srv"],"sandbox":{"enabled":false},"permissions":{"additionalDirectories":["../x"]}}`)
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "skills", "deploy", "SKILL.md"), "---\nname: deploy\ndescription: deploys\n---\nRun !`make deploy` now.\n")
	writeTrustFile(t, filepath.Join(f.cwd, ".claude", "skills", "granty", "SKILL.md"), "---\nname: granty\ndescription: grants\nallowed-tools: Bash(curl:*)\n---\nText.\n")
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "skills", "hooky", "SKILL.md"), "---\nname: hooky\ndescription: hooks\nhooks:\n  PreToolUse:\n    - hooks:\n        - type: command\n          command: x\n---\nText.\n")
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "commands", "build.md"), "---\ndescription: builds\nallowed-tools: Bash(make:*)\n---\nBuild.\n")
	writeTrustFile(t, filepath.Join(f.cwd, ".claude", "commands", "sh.md"), "---\ndescription: shell\n---\nStatus: !`git status`\n")
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "agents", "rev.md"), "---\nname: rev\ndescription: reviews\npermissionMode: bypassPermissions\n---\nReview.\n")
	env, _, _ := f.env()
	useRealTrust(env)
	rep := assessWorkspaceTrust(env)
	all := strings.Join(rep.items, "\n")
	for _, want := range []string{
		"hooks: SessionStart (.gocode/settings.json)",
		"status line command: st (.gocode/settings.json)",
		"permissions: Bash(*) (.gocode/settings.json)",
		"permission mode: acceptEdits (.gocode/settings.json)",
		"model: evil/model (.gocode/settings.json)",
		"plugins: p@m (.gocode/settings.json)",
		"MCP servers: 1 (loc) (.gocode/settings.local.json)",
		"MCP server approvals: enabledMcpjsonServers [srv] (.gocode/settings.local.json)",
		"sandbox: enabled: false (.gocode/settings.local.json)",
		"additional directories: ../x (.gocode/settings.local.json)",
		"skills/commands with ! shell blocks:",
		"deploy",
		"skills/commands with allowed-tools:",
		"granty (Bash(curl:*))",
		"(Bash(make:*))",
		"skills with hooks: hooky",
		"agents with permissionMode: rev (bypassPermissions)",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if !strings.Contains(all, "sh") {
		t.Errorf("command shell block not listed:\n%s", all)
	}
	if !rep.needsPrompt() || !strings.Contains(rep.untrustedNotice(), "gocode trust") {
		t.Fatalf("needsPrompt=%v notice=%q", rep.needsPrompt(), rep.untrustedNotice())
	}
}

// Trust is stored in the user's global config, inherited by subfolders, and
// revocable; the home directory and its parents never vouch for the folders
// inside them, and a repository is not trusted through a folder above it.
func TestTrustStore(t *testing.T) {
	f := newSubcmdFixture(t)
	env, _, _ := f.env()
	useRealTrust(env)
	sub := filepath.Join(f.cwd, "a", "b")
	other := filepath.Join(filepath.Dir(f.cwd), "other")
	for _, d := range []string{sub, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if folderTrustedInStore(env, f.cwd) {
		t.Fatal("trusted before accepting")
	}
	key, err := setFolderTrust(env, f.cwd, true)
	if err != nil {
		t.Fatal(err)
	}
	if key != canonicalDir(f.cwd) {
		t.Fatalf("key = %q", key)
	}
	cfg := readJSONObject(filepath.Join(f.home, ".config", "gocode", ".gocode.json"))
	if fmt.Sprint(cfg["projects"]) != fmt.Sprintf("map[%s:map[hasTrustDialogAccepted:true]]", key) {
		t.Fatalf("state file = %v", cfg)
	}
	if _, err := os.Stat(filepath.Join(f.cwd, ".gocode.json")); err == nil {
		t.Fatal("trust written into the repository")
	}
	if !folderTrustedInStore(env, f.cwd) || !folderTrustedInStore(env, sub) {
		t.Fatal("folder / subfolder not trusted after accepting")
	}
	if folderTrustedInStore(env, other) {
		t.Fatal("sibling folder trusted")
	}
	if !workspaceTrusted(env) {
		t.Fatal("workspaceTrusted false for an accepted cwd")
	}
	if _, err := setFolderTrust(env, f.cwd, false); err != nil {
		t.Fatal(err)
	}
	if folderTrustedInStore(env, f.cwd) || folderTrustedInStore(env, sub) {
		t.Fatal("still trusted after revoke")
	}

	// Home directory: trusted only for itself.
	inHome := filepath.Join(f.home, "proj2")
	if err := os.MkdirAll(inHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := setFolderTrust(env, f.home, true); err != nil {
		t.Fatal(err)
	}
	if !folderTrustedInStore(env, f.home) || folderTrustedInStore(env, inHome) {
		t.Fatalf("home accept: home=%v inside=%v", folderTrustedInStore(env, f.home), folderTrustedInStore(env, inHome))
	}
	// The filesystem root never vouches for anything below it.
	if err := os.WriteFile(filepath.Join(f.home, ".gocode.json"), []byte(`{"projects":{"/":{"hasTrustDialogAccepted":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if folderTrustedInStore(env, f.cwd) {
		t.Fatal("folder trusted through /")
	}

	// A repository is bounded at its root: a folder above it does not vouch for
	// it, and the repository root is the key for its subfolders.
	outer := filepath.Join(filepath.Dir(f.cwd), "outer")
	repo := filepath.Join(outer, "repo")
	repoSub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := setFolderTrust(env, outer, true); err != nil {
		t.Fatal(err)
	}
	if folderTrustedInStore(env, repoSub) {
		t.Fatal("repository trusted through a folder above it")
	}
	if key, err := setFolderTrust(env, repoSub, true); err != nil || key != canonicalDir(repo) {
		t.Fatalf("subfolder key = %q, %v (want the repository root)", key, err)
	}
	if !folderTrustedInStore(env, repoSub) || !folderTrustedInStore(env, repo) {
		t.Fatal("repository not trusted after accepting from a subfolder")
	}
}

// `gocode trust [path]` / `gocode trust --revoke [path]`.
func TestTrustCommand(t *testing.T) {
	f := newSubcmdFixture(t)
	writeTrustFile(t, filepath.Join(f.cwd, ".gocode", "settings.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]}}`)
	code, out, errOut := f.run(t, "trust")
	if code != 0 || !strings.Contains(out, "Trusted "+canonicalDir(f.cwd)) || !strings.Contains(out, "hooks: Stop") {
		t.Fatalf("trust: code=%d out=%q err=%q", code, out, errOut)
	}
	env, _, _ := f.env()
	if !folderTrustedInStore(env, f.cwd) {
		t.Fatal("not trusted after `gocode trust`")
	}
	code, out, _ = f.run(t, "trust", "--revoke", f.cwd)
	if code != 0 || !strings.Contains(out, "Revoked trust") || folderTrustedInStore(env, f.cwd) {
		t.Fatalf("revoke: code=%d out=%q", code, out)
	}
	if code, _, errOut = f.run(t, "trust", filepath.Join(f.cwd, "missing")); code == 0 || !strings.Contains(errOut, "not a directory") {
		t.Fatalf("missing dir: code=%d err=%q", code, errOut)
	}
}

// trustPrintFixture is a project with a SessionStart hook and an allow rule in
// .gocode/settings.json and an MCP server in settings.local.json.
func trustPrintFixture(t *testing.T) (*Env, *strings.Builder, string) {
	env, _, errb, cwd := sessionEnv(t, "")
	useRealTrust(env)
	writeTrustFile(t, filepath.Join(cwd, ".gocode", "settings.json"), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"start"}]}]},"permissions":{"allow":["Bash(echo trust-marker)"]}}`)
	writeTrustFile(t, filepath.Join(cwd, ".gocode", "settings.local.json"), `{"mcpServers":{"loc":{"command":"unused"}}}`)
	return env, errb, cwd
}

func runTrustPrint(t *testing.T, env *Env) (started int32, rt *sessionRuntime) {
	t.Helper()
	var n atomic.Int32
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	s.mcpTransport = echoServer().factory()
	s.hookRunner = fakeHookRunner(func(cmd string, _ hooks.Payload) (int, string, string) {
		if cmd == "start" {
			n.Add(1)
		}
		return 0, "", ""
	})
	s.onSession = func(r *sessionRuntime) { rt = r }
	if code := runPrintSession(env, s, "hello"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	return n.Load(), rt
}

// -p in an untrusted folder: no prompt, the risky settings are ignored, and
// one stderr line says how to trust it.
func TestTrustPrintUntrusted(t *testing.T) {
	env, errb, _ := trustPrintFixture(t)
	started, rt := runTrustPrint(t, env)
	if started != 0 {
		t.Fatal("project SessionStart hook ran in an untrusted folder")
	}
	if _, ok := rt.gathered.Servers["loc"]; ok {
		t.Fatal("local MCP server started in an untrusted folder")
	}
	if strings.Contains(fmt.Sprint(loadPermissionRules(env)), "trust-marker") {
		t.Fatal("project allow rule applied in an untrusted folder")
	}
	if n := strings.Count(errb.String(), "This folder is not trusted"); n != 1 || !strings.Contains(errb.String(), "run `gocode trust`") {
		t.Fatalf("stderr notice count=%d: %q", n, errb.String())
	}
}

// -p in a trusted folder: everything applies, no notice.
func TestTrustPrintTrusted(t *testing.T) {
	env, errb, cwd := trustPrintFixture(t)
	if _, err := setFolderTrust(env, cwd, true); err != nil {
		t.Fatal(err)
	}
	started, rt := runTrustPrint(t, env)
	if started != 1 {
		t.Fatalf("SessionStart hook ran %d times, want 1", started)
	}
	if _, ok := rt.gathered.Servers["loc"]; !ok {
		t.Fatalf("local MCP server not started: %+v", rt.gathered)
	}
	if !strings.Contains(fmt.Sprint(loadPermissionRules(env)), "trust-marker") {
		t.Fatal("project allow rule not applied in a trusted folder")
	}
	if strings.Contains(errb.String(), "not trusted") {
		t.Fatalf("notice in a trusted folder: %q", errb.String())
	}
}

// The interactive dialog is shown before the session (and its SessionStart
// hooks) exists; Yes remembers the folder and the hook runs, No leaves it
// untrusted, the hook does not run and the notice is shown.
func TestTrustInteractiveDialog(t *testing.T) {
	for _, yes := range []bool{true, false} {
		name := "No"
		if yes {
			name = "Yes"
		}
		t.Run(name, func(t *testing.T) {
			env, _, _, cwd := sessionEnv(t, "")
			useRealTrust(env)
			writeTrustFile(t, filepath.Join(cwd, ".gocode", "settings.json"), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"start"}]}]}}`)
			var started atomic.Int32
			progCh := make(chan *fakeProgram, 2)
			newProg := func(m *ui.Model, _ *Env) teaProgram {
				p := newFakeProgram(m)
				progCh <- p
				return p
			}
			tweak := func(s *interactiveSession) {
				s.print.newCaller = func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "x"}, nil }
				s.print.hookRunner = fakeHookRunner(func(cmd string, _ hooks.Payload) (int, string, string) {
					if cmd == "start" {
						started.Add(1)
					}
					return 0, "", ""
				})
			}
			done := make(chan int, 1)
			go func() { done <- runInteractiveWith(env, &DispatchDefaults{}, "", newProg, tweak) }()

			dialog := mustProgram(t, progCh)
			if !dialog.waitFor(func(m tea.Msg) bool {
				o, ok := m.(ui.OpenDialogMsg)
				return ok && strings.HasPrefix(o.Dialog.Title, "Accessing workspace:") &&
					strings.Contains(strings.Join(o.Dialog.Body, "\n"), "hooks: SessionStart (.gocode/settings.json)") &&
					o.Dialog.Options[0].Label == "Yes, trust this folder" && o.Dialog.Options[1].Label == "No, ignore its settings"
			}, 2*time.Second) {
				t.Fatal("trust dialog not shown")
			}
			time.Sleep(20 * time.Millisecond)
			if started.Load() != 0 {
				t.Fatal("SessionStart hook ran before the trust answer")
			}
			if yes {
				dialog.press(tea.KeyPressMsg{Code: tea.KeyUp})
			}
			dialog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

			main := mustProgram(t, progCh)
			if yes {
				deadline := time.Now().Add(2 * time.Second)
				for started.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if started.Load() != 1 {
					t.Fatalf("SessionStart hook ran %d times after Yes, want 1", started.Load())
				}
				if !folderTrustedInStore(env, cwd) {
					t.Fatal("Yes was not remembered")
				}
			} else {
				if !sawSystemMsg(main, "This folder is not trusted", "gocode trust") {
					t.Fatal("no untrusted notice after No")
				}
				if started.Load() != 0 || folderTrustedInStore(env, cwd) {
					t.Fatalf("after No: hook ran %d times, trusted=%v", started.Load(), folderTrustedInStore(env, cwd))
				}
			}
			main.Quit()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("session did not exit")
			}
		})
	}
}

// A trusted folder, or one with nothing risky, gets no dialog.
func TestTrustInteractiveNoDialogWhenNotNeeded(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	useRealTrust(env)
	writeTrustFile(t, filepath.Join(cwd, "gocode.md"), "memory\n")
	progCh := make(chan *fakeProgram, 2)
	askWorkspaceTrust(env, func(m *ui.Model, _ *Env) teaProgram {
		p := newFakeProgram(m)
		progCh <- p
		return p
	})
	if len(progCh) != 0 {
		t.Fatal("dialog shown for a plain folder")
	}
}

// A committed settings.local.json is treated as project settings: its MCP
// servers need approval, its .mcp.json approvals do not count, and a warning is
// printed — even in a trusted folder.
func TestTrustTrackedLocalSettings(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	env, _, errb, cwd := sessionEnv(t, "")
	gitEnv := []string{"HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "PATH=" + os.Getenv("PATH")}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Dir, cmd.Env = cwd, gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	writeTrustFile(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"proj":{"command":"unused"}}}`)
	writeTrustFile(t, filepath.Join(cwd, ".gocode", "settings.local.json"), `{"mcpServers":{"loc":{"command":"unused"}},"enableAllProjectMcpServers":true}`)
	git("add", ".gocode/settings.local.json")

	if !settingsContext(env).LocalSettingsTracked {
		t.Fatal("tracked settings.local.json not detected")
	}
	g := gatherMCP(env, &printSession{Mode: toolreg.ModeDefault})
	for _, name := range []string{"loc", "proj"} {
		if _, ok := g.Servers[name]; ok {
			t.Errorf("%s started without approval: %+v", name, g)
		}
	}
	pending := 0
	for _, sk := range g.Skipped {
		if sk.Reason == mcphost.ReasonPendingApproval {
			pending++
		}
	}
	if pending != 2 {
		t.Errorf("pending = %d, want 2: %+v", pending, g.Skipped)
	}
	runTrustPrint(t, env)
	if !strings.Contains(errb.String(), "settings.local.json is tracked by git") {
		t.Fatalf("no warning: %q", errb.String())
	}

	// The same file untracked keeps its local scope.
	env2, _, _, cwd2 := sessionEnv(t, "")
	cmd := exec.Command(gitPath, "init", "-q")
	cmd.Dir, cmd.Env = cwd2, gitEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	writeTrustFile(t, filepath.Join(cwd2, ".gocode", "settings.local.json"), `{"mcpServers":{"loc":{"command":"unused"}}}`)
	if settingsContext(env2).LocalSettingsTracked {
		t.Fatal("untracked settings.local.json reported tracked")
	}
	if _, ok := gatherMCP(env2, &printSession{Mode: toolreg.ModeDefault}).Servers["loc"]; !ok {
		t.Fatal("untracked local server not started")
	}
}

// An untrusted folder's skills and commands run no `!`-blocks and grant no
// tools; its agents cannot loosen the permission mode.
func TestTrustUntrustedSkillsCommandsAgents(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	useRealTrust(env)
	writeTrustFile(t, filepath.Join(cwd, ".gocode", "skills", "deploy", "SKILL.md"), "---\nname: deploy\ndescription: deploys\nallowed-tools: Bash(make:*)\n---\nRun !`make deploy` now.\n")
	writeTrustFile(t, filepath.Join(cwd, ".gocode", "agents", "rev.md"), "---\nname: rev\ndescription: reviews\npermissionMode: bypassPermissions\n---\nReview.\n")
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	var rt *sessionRuntime
	s.onSession = func(r *sessionRuntime) { rt = r }
	if code := runPrintSession(env, s, "hello"); code != 0 {
		t.Fatal("exit != 0")
	}
	if !s.grants.projectSkill("deploy") {
		t.Fatal("project skill not gated")
	}
	if got := rt.skillGrants(rt.dispatcher, "deploy", []string{"Bash(make:*)"}); len(got) != 0 {
		t.Fatalf("untrusted skill granted %v", got)
	}
	if d, ok := rt.conv.Agents.Lookup("rev"); !ok || d.PermissionMode != "" {
		t.Fatalf("agent permissionMode = %q", d.PermissionMode)
	}
	cmdCtx := commands.WithShellInvocation(context.Background(), commands.ShellInvocation{Name: "x", Source: commands.SourceProject})
	if out, err := rt.conv.ExpandShell(cmdCtx, "Status: !`echo hi`"); err != nil || !strings.Contains(out, "folder is not trusted") || strings.Contains(out, "echo hi") {
		t.Fatalf("project command block: %q, %v", out, err)
	}
	skillCtx := commands.WithShellInvocation(context.Background(), commands.ShellInvocation{Name: "deploy"})
	if out, err := rt.skillShell(rt.dispatcher)(skillCtx, "Run !`make deploy` now."); err != nil || !strings.Contains(out, "folder is not trusted") {
		t.Fatalf("project skill block: %q, %v", out, err)
	}
}
