package settingsload

import (
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

func TestSkillShellExecutionDisabled(t *testing.T) {
	ctx := newCtx()
	userPath, _ := ctx.PathForSource(settingspolicy.SourceUser)
	projPath, _ := ctx.PathForSource(settingspolicy.SourceProject)
	localPath, _ := ctx.PathForSource(settingspolicy.SourceLocal)
	const policyPath = "/etc/gocode/managed-settings.json"

	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"unset", nil, false},
		{"user true", map[string]string{userPath: `{"disableSkillShellExecution":true}`}, true},
		{"project true", map[string]string{projPath: `{"disableSkillShellExecution":true}`}, true},
		// A repo-controlled source (local/project — a cloned repository can
		// ship both) cannot re-enable `!`-blocks the user turned off.
		{"local false over user true", map[string]string{
			userPath:  `{"disableSkillShellExecution":true}`,
			localPath: `{"disableSkillShellExecution":false}`,
		}, true},
		{"project false over user true", map[string]string{
			userPath: `{"disableSkillShellExecution":true}`,
			projPath: `{"disableSkillShellExecution":false}`,
		}, true},
		// Policy true wins over any lower source.
		{"policy true over local false", map[string]string{
			policyPath: `{"disableSkillShellExecution":true}`,
			localPath:  `{"disableSkillShellExecution":false}`,
		}, true},
		// Managed drop-ins are restrictive: one true is decisive even when the
		// base managed file says false.
		{"drop-in true over managed false", map[string]string{
			policyPath: `{"disableSkillShellExecution":false}`,
			"/etc/gocode/managed-settings.d/10-x.json": `{"disableSkillShellExecution":true}`,
		}, true},
		// Only a JSON true counts.
		{"string true ignored", map[string]string{userPath: `{"disableSkillShellExecution":"true"}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fsfacade.NewMemFS()
			for p, c := range tc.files {
				fsys.WriteFile(p, []byte(c))
			}
			if got := SkillShellExecutionDisabled(fsys, ctx); got != tc.want {
				t.Fatalf("SkillShellExecutionDisabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSkillShellExecutionDisabledCoworkEnv(t *testing.T) {
	ctx := newCtx()
	ctx.Env = func(k string) string {
		if k == "GOCODE_CODE_IS_COWORK" {
			return "1"
		}
		return ""
	}
	if !SkillShellExecutionDisabled(fsfacade.NewMemFS(), ctx) {
		t.Fatal("cowork session should disable skill shell execution")
	}
}
