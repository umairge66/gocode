package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// writeScopeFixtures writes one memory file, skill, command and agent per
// scope (user under cfgDir, project and local under cwd), plus user and
// project settings files.
func writeScopeFixtures(t *testing.T, cfgDir, cwd string) {
	t.Helper()
	writeTestFile(t, filepath.Join(cfgDir, "gocode.md"), "USER-MEM\n")
	writeTestFile(t, filepath.Join(cwd, "gocode.md"), "PROJECT-MEM\n")
	writeTestFile(t, filepath.Join(cwd, "gocode.local.md"), "LOCAL-MEM\n")
	writeTestFile(t, filepath.Join(cfgDir, "skills", "uskill", "SKILL.md"), "---\ndescription: user skill\n---\nbody\n")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "skills", "pskill", "SKILL.md"), "---\ndescription: project skill\n---\nbody\n")
	writeTestFile(t, filepath.Join(cfgDir, "commands", "ucmd.md"), "---\ndescription: user cmd\n---\nU\n")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "commands", "pcmd.md"), "---\ndescription: project cmd\n---\nP\n")
	writeTestFile(t, filepath.Join(cfgDir, "agents", "uagent.md"), "---\nname: uagent\ndescription: user agent\n---\nU\n")
	writeTestFile(t, filepath.Join(cwd, ".gocode", "agents", "pagent.md"), "---\nname: pagent\ndescription: project agent\n---\nP\n")
	writeTestFile(t, filepath.Join(cfgDir, "settings.json"), `{"model":"user-model"}`)
	writeTestFile(t, filepath.Join(cwd, ".gocode", "settings.json"), `{"model":"project-model"}`)
}

// TestSettingSources_GatesSessionInputs: --setting-sources selects which
// scopes' settings, memory, skills, commands and agents a session loads.
func TestSettingSources_GatesSessionInputs(t *testing.T) {
	cases := []struct {
		name          string
		set           bool
		sources       []settingspolicy.Source
		user, project bool
		local         bool
	}{
		{name: "unset", user: true, project: true, local: true},
		{name: "user", set: true, sources: []settingspolicy.Source{settingspolicy.SourceUser}, user: true},
		{name: "project,local", set: true, sources: []settingspolicy.Source{settingspolicy.SourceProject, settingspolicy.SourceLocal}, project: true, local: true},
		{name: "isolated", set: true, sources: []settingspolicy.Source{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, _, _, cwd := sessionEnv(t, "")
			cfgDir := env.Getenv("GOCODE_CONFIG_DIR")
			writeScopeFixtures(t, cfgDir, cwd)
			env.SettingSources, env.SettingSourcesSet = c.sources, c.set

			s := &printSession{Model: "parent-model", Provider: providerOpenRouter}
			rt := buildSession(context.Background(), env, s, fakeCaller{reply: "ok"}, sessionConfig{})
			defer rt.Close("other")

			check := func(what string, got, want bool) {
				t.Helper()
				if got != want {
					t.Errorf("%s loaded = %v, want %v", what, got, want)
				}
			}
			check("user memory", strings.Contains(rt.conv.System, "USER-MEM"), c.user)
			check("project memory", strings.Contains(rt.conv.System, "PROJECT-MEM"), c.project)
			check("local memory", strings.Contains(rt.conv.System, "LOCAL-MEM"), c.local)
			_, ok := rt.conv.Skills.Lookup("uskill")
			check("user skill", ok, c.user)
			_, ok = rt.conv.Skills.Lookup("pskill")
			check("project skill", ok, c.project)
			check("user command", rt.conv.Commands != nil && rt.conv.Commands.Resolve("ucmd") != nil, c.user)
			check("project command", rt.conv.Commands != nil && rt.conv.Commands.Resolve("pcmd") != nil, c.project)
			_, ok = rt.conv.Agents.Lookup("uagent")
			check("user agent", ok, c.user)
			_, ok = rt.conv.Agents.Lookup("pagent")
			check("project agent", ok, c.project)

			// The settings files follow the same selection; --settings and
			// policy always load.
			got := map[settingspolicy.Source]bool{}
			for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder) {
				if or.Result.LoadState == settingspolicy.LoadLoaded {
					got[or.Source] = true
				}
			}
			check("user settings", got[settingspolicy.SourceUser], c.user)
			check("project settings", got[settingspolicy.SourceProject], c.project)
		})
	}
}

// TestSettingSources_FlagParsing: the root flag is parsed (empty = isolated)
// and an invalid source is a startup error.
func TestSettingSources_FlagParsing(t *testing.T) {
	r := parseRootConfig([]string{"--setting-sources", "", "-p", "hi"})
	if !r.Config.SettingSourcesSet || r.Config.SettingSources != "" {
		t.Fatalf("empty value: %+v", r.Config)
	}
	r = parseRootConfig([]string{"--setting-sources=user,project"})
	if r.Config.SettingSources != "user,project" || len(r.Rest) != 0 {
		t.Fatalf("inline value: %+v rest %v", r.Config, r.Rest)
	}
	srcs, err := settingsload.ParseSettingSources(" user , local,")
	if err != nil || len(srcs) != 2 || srcs[0] != settingspolicy.SourceUser || srcs[1] != settingspolicy.SourceLocal {
		t.Fatalf("parse = %v, %v", srcs, err)
	}

	f := newSubcmdFixture(t)
	code, _, errOut := f.run(t, "--setting-sources", "user,managed", "-p", "hi")
	if code != 1 || !strings.Contains(errOut, "Invalid setting source: managed. Valid options are: user, project, local") {
		t.Fatalf("exit %d err %q", code, errOut)
	}
}
