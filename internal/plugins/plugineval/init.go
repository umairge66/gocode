package plugineval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var caseNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// windowsNameProblem: names Windows treats specially.
func windowsNameProblem(name string) string {
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return `ends in "." or a space, which Windows treats as another name`
	}
	if strings.Contains(name, ":") {
		return `contains ":", a stream selector on Windows`
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return "is a reserved device name on Windows"
	}
	return ""
}

// ValidateCaseName checks a new case's name.
func ValidateCaseName(name string) error {
	if !caseNameRe.MatchString(name) {
		return fmt.Errorf("case name %q is invalid — use letters, digits, '.', '_', '-' only (no '/', no '..')", name)
	}
	if p := windowsNameProblem(name); p != "" {
		return fmt.Errorf("case name %q is invalid — it %s", name, p)
	}
	return nil
}

// WriteBareCase writes <dir>/<evalDir>/<name>/prompt.md and
// graders/criteria.md from the blank templates, refusing to overwrite. It
// returns the two files written, relative to dir.
func WriteBareCase(dir string, evalDir []string, name string) (promptRel, graderRel string, err error) {
	if err := ValidateCaseName(name); err != nil {
		return "", "", err
	}
	if len(evalDir) == 0 {
		evalDir = []string{DefaultEvalDir}
	}
	caseDir := filepath.Join(append(append([]string{dir}, evalDir...), name)...)
	if err := os.MkdirAll(filepath.Dir(caseDir), 0o755); err != nil {
		return "", "", err
	}
	if err := os.Mkdir(caseDir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", "", fmt.Errorf("%s already exists; refusing to overwrite it", caseDir)
		}
		return "", "", err
	}
	prompt := filepath.Join(caseDir, "prompt.md")
	grader := filepath.Join(caseDir, "graders", "criteria.md")
	cleanup := func() { _ = os.RemoveAll(caseDir) }
	if err := os.Mkdir(filepath.Dir(grader), 0o755); err != nil {
		cleanup()
		return "", "", err
	}
	for path, body := range map[string]string{prompt: BarePromptTemplate, grader: BareGraderTemplate} {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			cleanup()
			return "", "", err
		}
		if _, err := f.WriteString(body); err != nil {
			f.Close()
			cleanup()
			return "", "", err
		}
		if err := f.Close(); err != nil {
			cleanup()
			return "", "", err
		}
	}
	pr, _ := filepath.Rel(dir, prompt)
	gr, _ := filepath.Rel(dir, grader)
	return pr, gr, nil
}

// LooksLikePluginOrSkill reports whether dir is a plugin or skill folder, or
// already holds an eval directory.
func LooksLikePluginOrSkill(dir string, evalDir []string) bool {
	if HasPluginManifest(dir) {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
		return true
	}
	if len(evalDir) == 0 {
		evalDir = []string{DefaultEvalDir}
	}
	st, err := os.Stat(filepath.Join(append([]string{dir}, evalDir...)...))
	return err == nil && st.IsDir()
}
