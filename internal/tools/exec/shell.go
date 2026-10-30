package exec

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// Shell kinds a caller (e.g. a command's `shell:` frontmatter) may request.
const (
	ShellDefault        = ""
	ShellBash           = "bash"
	ShellPowerShellKind = "powershell"
	ShellSh             = "sh"
	ShellZsh            = "zsh"
)

// ErrNoPowerShell is returned when PowerShell is required but neither pwsh nor
// powershell can be found.
var ErrNoPowerShell = errors.New("no PowerShell executable (pwsh or powershell) was found on PATH; install PowerShell (https://aka.ms/powershell)")

// installGitForWindows is the install hint shown when Git Bash is missing.
const installGitForWindows = "Install Git for Windows (https://git-scm.com/downloads/win)"

// errNoWindowsShell is the message raised when a Windows host has
// neither Git Bash nor PowerShell.
var errNoWindowsShell = errors.New("gocode on Windows requires either Git for Windows (for bash) or PowerShell. Install one of:\n" +
	"  - Git for Windows: https://git-scm.com/downloads/win\n" +
	"  - PowerShell 7: https://aka.ms/powershell\n" +
	"Or set GOCODE_CODE_GIT_BASH_PATH to your bash.exe location.")

// NoBashNotice is the one-line warning shown when a `!`-block with
// no `shell:` falls back to PowerShell because Git Bash is missing.
const NoBashNotice = "Git Bash wasn't found, so a skill's shell command ran in PowerShell. Commands written for bash can fail there. " +
	installGitForWindows + " and start a new session, or set \"shell\" to \"powershell\" on commands written for PowerShell."

// ShellEnv is the injectable host view used by shell discovery, so the
// selection logic is unit-testable on any OS. Zero fields take the real host
// implementation (see DefaultShellEnv).
type ShellEnv struct {
	// GOOS is the target platform ("windows", "linux", "darwin", ...).
	GOOS string
	// LookPath resolves a program on PATH (os/exec.LookPath).
	LookPath func(file string) (string, error)
	// IsFile reports whether path names an existing regular file.
	IsFile func(path string) bool
	// Readlink resolves a symlink / app-execution alias (os.Readlink).
	Readlink func(path string) (string, error)
	// Getenv reads an environment variable (os.Getenv).
	Getenv func(key string) string
}

// DefaultShellEnv is the real host environment.
func DefaultShellEnv() ShellEnv {
	return ShellEnv{}.withDefaults()
}

func (e ShellEnv) withDefaults() ShellEnv {
	if e.GOOS == "" {
		e.GOOS = runtime.GOOS
	}
	if e.LookPath == nil {
		e.LookPath = exec.LookPath
	}
	if e.IsFile == nil {
		e.IsFile = func(p string) bool {
			st, err := os.Stat(p)
			return err == nil && st.Mode().IsRegular()
		}
	}
	if e.Readlink == nil {
		e.Readlink = os.Readlink
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	return e
}

// setting reads a GOCODE_CODE_* setting, accepting the lower-case
// prefix and the upper-case spelling.
func (e ShellEnv) setting(name string) string {
	if v := e.Getenv("GOCODE_CODE_" + name); v != "" {
		return v
	}
	return e.Getenv("GOCODE_CODE_" + name)
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func falsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

// winJoin joins Windows path segments with a backslash (usable on any host).
func winJoin(parts ...string) string {
	out := strings.TrimRight(parts[0], `\/`)
	for _, p := range parts[1:] {
		out += `\` + strings.Trim(p, `\/`)
	}
	return out
}

func winDir(p string) string {
	i := strings.LastIndexAny(p, `\/`)
	if i < 0 {
		return "."
	}
	return p[:i]
}

func winBase(p string) string {
	return p[strings.LastIndexAny(p, `\/`)+1:]
}

// FindGitBash locates Git Bash on Windows; it returns "" when none is found
// (and on non-Windows hosts, where a POSIX shell is always assumed).
func FindGitBash(env ShellEnv) string {
	env = env.withDefaults()
	if env.GOOS != "windows" {
		return ""
	}
	if p := env.setting("GIT_BASH_PATH"); p != "" {
		switch strings.ToLower(winBase(p)) {
		case "bash.exe", "sh.exe", "bash", "sh":
			if env.IsFile(p) {
				return p
			}
		}
	}
	for _, p := range []string{`C:\Program Files\Git\bin\bash.exe`, `C:\Program Files (x86)\Git\bin\bash.exe`} {
		if env.IsFile(p) {
			return p
		}
	}
	if git, err := env.LookPath("git"); err == nil && git != "" {
		root := winDir(winDir(git)) // path.join(git, "..", "..")
		if p := winJoin(root, "bin", "bash.exe"); env.IsFile(p) {
			return p
		}
	}
	return ""
}

// FindPowerShell returns the PowerShell binary to run commands with:
// PowerShell 7 (pwsh) when installed, else Windows PowerShell 5.1.
func FindPowerShell(env ShellEnv) (string, error) {
	env = env.withDefaults()
	if p, err := env.LookPath("pwsh"); err == nil && p != "" {
		if env.GOOS == "linux" && strings.HasPrefix(p, "/snap/") {
			for _, alt := range []string{"/opt/microsoft/powershell/7/pwsh", "/usr/bin/pwsh"} {
				if env.IsFile(alt) {
					return alt, nil
				}
			}
		}
		return p, nil
	}
	if env.GOOS == "windows" {
		if pf := env.Getenv("ProgramFiles"); pf != "" {
			if p := winJoin(pf, "PowerShell", "7", "pwsh.exe"); env.IsFile(p) {
				return p, nil
			}
		}
		if la := env.Getenv("LOCALAPPDATA"); la != "" {
			alias := winJoin(la, "Microsoft", "WindowsApps", "pwsh.exe")
			if target, err := env.Readlink(alias); err == nil && env.IsFile(target) {
				return target, nil
			}
		}
		if up := env.Getenv("USERPROFILE"); up != "" {
			if p := winJoin(up, ".dotnet", "tools", "pwsh.exe"); env.IsFile(p) {
				return p, nil
			}
		}
	}
	if p, err := env.LookPath("powershell"); err == nil && p != "" {
		return p, nil
	}
	if env.GOOS == "windows" {
		root := env.Getenv("SYSTEMROOT")
		if root == "" {
			root = `C:\Windows`
		}
		if p := winJoin(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"); env.IsFile(p) {
			return p, nil
		}
	}
	return "", ErrNoPowerShell
}

// powerShellEnabled reports whether PowerShell may be used: off Windows only
// when GOCODE_CODE_USE_POWERSHELL_TOOL is truthy; on Windows the setting
// decides when present, and PowerShell is used when Git Bash is missing.
//
// When Git Bash IS present and the setting is unset, an explicit
// `shell: powershell` is treated as enabled.
func powerShellEnabled(env ShellEnv) bool {
	v := env.setting("USE_POWERSHELL_TOOL")
	if env.GOOS != "windows" {
		return truthy(v)
	}
	if truthy(v) {
		return true
	}
	if falsy(v) {
		return false
	}
	return true
}

// PowerShellFlags are the leading PowerShell options: -NoProfile
// -NonInteractive, plus -ExecutionPolicy Bypass unless
// GOCODE_CODE_POWERSHELL_RESPECT_EXECUTION_POLICY is set.
func PowerShellFlags(env ShellEnv) []string {
	env = env.withDefaults()
	f := []string{"-NoProfile", "-NonInteractive"}
	if env.setting("POWERSHELL_RESPECT_EXECUTION_POLICY") == "" {
		f = append(f, "-ExecutionPolicy", "Bypass")
	}
	return f
}

// powerShellPrelude forces UTF-8 output and plain-text rendering.
const powerShellPrelude = "try { $PSDefaultParameterValues['Out-File:Encoding'] = 'utf8' } catch {}; " +
	"if ($ExecutionContext.SessionState.LanguageMode -eq 'FullLanguage') { try { $OutputEncoding = [System.Text.UTF8Encoding]::new() } catch {}; " +
	"if ($null -ne $PSStyle) { try { $PSStyle.OutputRendering = 'PlainText' } catch {} } }; "

// powerShellEpilogue propagates the last native exit code (or $? failure) as
// the process exit status.
const powerShellEpilogue = "\n; $_ec = if ($null -ne $LASTEXITCODE) { $LASTEXITCODE } elseif ($?) { 0 } else { 1 }" +
	"\n; if ($ExecutionContext.SessionState.LanguageMode -eq 'FullLanguage') { $host.SetShouldExit($_ec) } else { exit $_ec }"

var (
	rePSUsing  = regexp.MustCompile(`(?i)^using\s+(namespace|module|assembly)\b`)
	rePSParam  = regexp.MustCompile(`(?i)^param\s*\(`)
	rePSBlock  = regexp.MustCompile(`(?i)^(begin|process|end|clean|dynamicparam)\s*\{`)
	rePSAttr   = regexp.MustCompile(`^\[\w`)
	rePSStatic = regexp.MustCompile(`^\[[\w.]+\]::`)
)

// psNeedsBareStart reports scripts that must begin with
// using/param/a named block/an attribute cannot take the prelude in front.
func psNeedsBareStart(s string) bool {
	i := 0
	for {
		for i < len(s) && strings.ContainsRune(" \t\r\n;", rune(s[i])) {
			i++
		}
		if i < len(s) && s[i] == '#' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(s[i:], "<#") {
			if j := strings.Index(s[i+2:], "#>"); j >= 0 {
				i += 2 + j + 2
			} else {
				i = len(s)
			}
			continue
		}
		break
	}
	r := s[i:]
	return rePSUsing.MatchString(r) || rePSParam.MatchString(r) || rePSBlock.MatchString(r) ||
		(rePSAttr.MatchString(r) && !rePSStatic.MatchString(r))
}

// WrapPowerShellScript returns the -Command text for a PowerShell command:
// the UTF-8 prelude (unless the script must start bare), the command, and the
// exit-code epilogue.
func WrapPowerShellScript(command string) string {
	pre := powerShellPrelude
	if psNeedsBareStart(command) {
		pre = ""
	}
	return pre + command + powerShellEpilogue
}

// PowerShellArgs is the argv (after the binary) for running command:
// PowerShellFlags, "-Command", the wrapped script.
func PowerShellArgs(command string, env ShellEnv) []string {
	return append(PowerShellFlags(env), "-Command", WrapPowerShellScript(command))
}

// powerShellEnvOverrides are set for a PowerShell child unless the variable is
// already present (NO_COLOR is also skipped when FORCE_COLOR is set).
func powerShellEnvOverrides(env ShellEnv) map[string]string {
	out := map[string]string{}
	if env.Getenv("PYTHONIOENCODING") == "" {
		out["PYTHONIOENCODING"] = "utf-8:surrogateescape"
	}
	if env.Getenv("NO_COLOR") == "" && env.Getenv("FORCE_COLOR") == "" {
		out["NO_COLOR"] = "1"
	}
	return out
}

// ShellInvocation is a resolved interpreter for one command.
type ShellInvocation struct {
	// Path is the interpreter binary.
	Path string
	// Args is the full argument vector after Path (the command included).
	Args []string
	// PowerShell is true when Path is pwsh/powershell.
	PowerShell bool
	// Env holds variables to add to the child environment.
	Env map[string]string
	// Notice is a user-facing warning about the choice (e.g. NoBashNotice).
	Notice string
}

// NormalizeShellKind lower-cases and trims a requested shell, mapping the
// "pwsh" alias to "powershell". ok is false for an unrecognised value.
func NormalizeShellKind(kind string) (string, bool) {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case ShellDefault, ShellBash, ShellPowerShellKind, ShellSh, ShellZsh:
		return k, true
	case "pwsh":
		return ShellPowerShellKind, true
	}
	return "", false
}

// ResolveShell picks the interpreter that runs command for the requested shell
// kind (a command's `shell:` frontmatter; "" when unset).
//
// Selection:
//
//	shell=="powershell" && powerShellEnabled() ? PowerShell : bashAvailable() ? Bash : PowerShell
//
// with `shell: bash` on a Windows host without Git Bash failing with "requires
// bash ... Git Bash was not found", and a no-`shell` command that lands on
// PowerShell carrying NoBashNotice.
//
// Behaviour per kind:
//   - "" (unset): POSIX hosts run "/bin/sh -c <cmd>" (unchanged Bash-tool
//     default); Windows runs Git Bash ("bash.exe -c -l <cmd>") or, without Git Bash, PowerShell with NoBashNotice.
//   - "bash": a real bash (`bash -c`; falls back to /bin/sh when bash is not on
//     PATH on POSIX); on Windows, Git Bash or the "requires bash" error.
//   - "powershell"/"pwsh": PowerShell when enabled (see powerShellEnabled),
//     otherwise the default shell.
//   - "sh", "zsh": "sh -c" / "zsh -c" from PATH, or a
//     "requires <shell>" error when not installed.
func ResolveShell(kind, command string, env ShellEnv) (ShellInvocation, error) {
	env = env.withDefaults()
	k, ok := NormalizeShellKind(kind)
	if !ok {
		return ShellInvocation{}, fmt.Errorf("unrecognised shell %q (valid: bash, powershell, sh, zsh)", kind)
	}
	powershell := func(notice string) (ShellInvocation, error) {
		ps, err := FindPowerShell(env)
		if err != nil {
			return ShellInvocation{}, err
		}
		return ShellInvocation{Path: ps, Args: PowerShellArgs(command, env), PowerShell: true, Env: powerShellEnvOverrides(env), Notice: notice}, nil
	}
	windowsDefault := func(explicitBash bool) (ShellInvocation, error) {
		if gb := FindGitBash(env); gb != "" {
			return ShellInvocation{Path: gb, Args: []string{"-c", "-l", command}}, nil
		}
		if explicitBash {
			return ShellInvocation{}, fmt.Errorf("requires bash (`shell: bash` in frontmatter) but Git Bash was not found. %s, or change the frontmatter to `shell: powershell`", installGitForWindows)
		}
		inv, err := powershell(NoBashNotice)
		if errors.Is(err, ErrNoPowerShell) {
			return ShellInvocation{}, errNoWindowsShell
		}
		return inv, err
	}

	switch k {
	case ShellPowerShellKind:
		if powerShellEnabled(env) {
			return powershell("")
		}
		return ResolveShell(ShellDefault, command, env)
	case ShellBash:
		if env.GOOS == "windows" {
			return windowsDefault(true)
		}
		if p, err := env.LookPath("bash"); err == nil && p != "" {
			return ShellInvocation{Path: p, Args: []string{"-c", command}}, nil
		}
		return ShellInvocation{Path: shellPOSIX, Args: []string{shellArgPOSIX, command}}, nil
	case ShellSh, ShellZsh:
		if env.GOOS == "windows" && k == ShellSh {
			if gb := FindGitBash(env); gb != "" {
				return ShellInvocation{Path: winJoin(winDir(gb), "sh.exe"), Args: []string{"-c", command}}, nil
			}
		}
		if p, err := env.LookPath(k); err == nil && p != "" {
			return ShellInvocation{Path: p, Args: []string{"-c", command}}, nil
		}
		if k == ShellSh && env.GOOS != "windows" {
			return ShellInvocation{Path: shellPOSIX, Args: []string{shellArgPOSIX, command}}, nil
		}
		return ShellInvocation{}, fmt.Errorf("requires %s (`shell: %s` in frontmatter) but %s was not found on PATH", k, k, k)
	}
	// Default.
	if env.GOOS == "windows" {
		return windowsDefault(false)
	}
	return ShellInvocation{Path: shellPOSIX, Args: []string{shellArgPOSIX, command}}, nil
}

// envWith returns the current environment plus extra (overriding).
func envWith(extra map[string]string) []string {
	base := os.Environ()
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if _, ok := extra[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}
