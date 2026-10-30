package hooks

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/envutil"
)

// Environment sanitisation for command hooks.
//
// Environment variables that steer how a child process loads code or picks
// helper programs — compiler/linker flags, library preloads, dynamic-loader
// paths, toolchain roots, pagers/editors/diff drivers that run arbitrary
// commands — are scrubbed before a hook's environment crosses a trust
// boundary. A variable is "dangerous" when its name (upper-cased, "-" -> "_")
// is in the name set or matches the pattern. PATH-like search paths are
// treated as dangerous names too, so SanitizeEnv drops them wholesale rather
// than filtering them entry-by-entry (a conservative, telemetry-free choice —
// a hook that needs them can be granted them explicitly by the caller).
//
// Note on scope: the scrub is meant for the distrusted-source / forwarded
// path, not every local hook, so hookhost exposes it as an opt-in
// (Options.SanitizeEnv).

// dangerousEnvNames lists (upper-cased) environment variables whose values
// influence code execution or launch helper programs.
var dangerousEnvNames = toSet([]string{
	// PATH-like search paths.
	"PATH", "PYTHONPATH", "NODE_PATH", "RUBYLIB", "PERL5LIB", "PERLLIB",
	"GEM_PATH", "CLASSPATH", "DYLD_LIBRARY_PATH", "DYLD_FRAMEWORK_PATH",
	"LD_LIBRARY_PATH", "LIBRARY_PATH", "CPATH", "C_INCLUDE_PATH",
	"CPLUS_INCLUDE_PATH", "R_LIBS", "R_LIBS_USER", "R_LIBS_SITE",
	// Code-execution / helper-program vars.
	"GOFLAGS", "RUSTFLAGS", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR",
	"GIT_EXTERNAL_DIFF", "GIT_PROXY_COMMAND", "SUDO_ASKPASS", "ZDOTDIR",
	"CDPATH", "GCONV_PATH", "NLSPATH", "DOTNET_STARTUP_HOOKS",
	"NPM_CONFIG_NODE_OPTIONS", "LESSOPEN", "LESSCLOSE", "MANPAGER",
	"MAVEN_OPTS", "MAVEN_ARGS", "MAVEN_CONFIG", "ANT_OPTS", "ANT_ARGS",
	"GRADLE_OPTS", "JAVA_OPTS", "JVM_OPTS", "LEIN_JVM_OPTS", "ES_JAVA_OPTS",
	"SBT_OPTS", "SONAR_SCANNER_OPTS", "SONAR_SCANNER_JAVA_OPTS", "MAKEFLAGS",
	"GNUMAKEFLAGS", "MFLAGS", "MAKEFILES", "AWKPATH", "AWKLIBPATH",
	"DEVELOPER_DIR", "CFLAGS", "CXXFLAGS", "CPPFLAGS", "LDFLAGS", "LDLIBS",
	"CC", "CXX", "CPP", "MAKE", "AR", "AS", "LD", "LEX", "YACC", "RANLIB",
	"ARFLAGS", "RANLIBFLAGS", "NM", "STRIP", "OBJCOPY", "OBJDUMP",
	"PKG_CONFIG", "FC", "F77", "FFLAGS", "FCFLAGS", "CGO_CFLAGS",
	"CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_FFLAGS", "CGO_LDFLAGS", "HOST_CC",
	"HOST_CXX", "HOST_AR", "HOST_RANLIB", "HOST_ARFLAGS", "HOST_CFLAGS",
	"HOST_CXXFLAGS", "TARGET_RANLIB", "TARGET_ARFLAGS", "TARGET_CC",
	"TARGET_CXX", "TARGET_AR", "TARGET_CFLAGS", "TARGET_CXXFLAGS",
	"GCC_EXEC_PREFIX", "COMPILER_PATH", "DEPENDENCIES_OUTPUT",
	"SUNPRO_DEPENDENCIES", "GOAUTH", "GOTOOLCHAIN", "OPENSSL_ENGINES",
	"SHELLOPTS", "BASHOPTS", "PS4", "PROMPT_COMMAND", "GIT_SSH_COMMAND",
	"GIT_SSH", "GIT_EXEC_PATH", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_PAGER",
	"PAGER", "EDITOR", "VISUAL", "BROWSER", "PYTHONWARNINGS",
	"PYTHONBREAKPOINT", "PERL5DB", "NODE_REPL_EXTERNAL_MODULE",
	"JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS",
	"IBM_JAVA_OPTIONS", "OPENJ9_JAVA_OPTIONS", "NODE_OPTIONS",
	"NODE_COMPILE_CACHE", "PYTHONSTARTUP", "PYTHONHOME", "PYTHONUSERBASE",
	"BASH_ENV", "ENV", "PERL5OPT", "RUBYOPT", "GEM_HOME", "RUBYGEMS_GEMDEPS",
	"BUNDLE_GEMFILE", "OPENSSL_CONF", "OPENSSL_CONF_INCLUDE",
	"OPENSSL_MODULES", "DYLD_INSERT_LIBRARIES", "LD_PRELOAD", "LD_AUDIT",
	"BUN_OPTIONS", "NPM_CONFIG_SCRIPT_SHELL", "RUSTC", "RUSTC_WRAPPER",
	"RUSTC_WORKSPACE_WRAPPER", "RUSTDOC", "CARGO_BUILD_RUSTC",
	"CARGO_BUILD_RUSTC_WRAPPER", "CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER",
	"CARGO_BUILD_RUSTDOC", "CARGO_BUILD_RUSTFLAGS", "RUSTDOCFLAGS",
	"CARGO_BUILD_RUSTDOCFLAGS", "CARGO_ENCODED_RUSTDOCFLAGS",
	"CARGO_ENCODED_RUSTFLAGS", "GOROOT", "PSMODULEPATH", "JAVA_HOME",
	"JAVACMD", "JDK_HOME", "DOTNET_ROOT", "COREPACK_HOME", "VIRTUAL_ENV",
	"CONDA_PREFIX", "PYENV_ROOT", "PYENV_VERSION", "VOLTA_HOME", "ASDF_DIR",
	"ASDF_DATA_DIR", "RBENV_ROOT", "RBENV_VERSION", "NODENV_ROOT",
	"NODENV_VERSION", "GOENV_ROOT", "GOENV_VERSION", "JENV_ROOT",
	"JENV_VERSION", "PLENV_ROOT", "PLENV_VERSION", "PERL5SHELL", "RUBYSHELL",
	"NODE_V8_COVERAGE", "NODE_REDIRECT_WARNINGS", "AWS_PAGER", "GH_PAGER",
	"CLOUDSDK_PYTHON", "CLOUDSDK_PYTHON_ARGS", "CLOUDSDK_GSUTIL_PYTHON",
	"CLOUDSDK_BQ_PYTHON", "UV_PYTHON", "GH_BROWSER", "SOPS_GPG_EXEC",
	"RSYNC_RSH", "CVS_RSH", "TAR_OPTIONS", "DOCKER_CLI_PLUGIN_EXTRA_DIRS",
	"R_PROFILE", "R_PROFILE_USER", "R_ENVIRON", "R_ENVIRON_USER",
	// The lowercase npm spellings (npm_config_node_options,
	// npm_config_script_shell) normalise to the upper-case keys above.
})

// dangerousEnvPattern matches further dangerous name families (an RE2
// pattern — no backreferences or lookaround).
var dangerousEnvPattern = regexp.MustCompile(`^(?:DYLD_|LD_|BASH_FUNC_|GIT_CONFIG_|LUA_|TF_CLI_ARGS|CARGO_ALIAS_|CARGO_TARGET_[A-Z0-9_]+_(?:RUNNER|LINKER|RUSTFLAGS|RUSTDOCFLAGS)$|(?:CC|CXX|AR|RANLIB|(?:AR|RANLIB)FLAGS|FC?FLAGS|CFLAGS|CXXFLAGS|LDFLAGS)_(?:X86_64|I[3-6]86|AARCH64|ARM|RISCV|WASM|POWERPC|S390X|MIPS|SPARC|LOONGARCH|THUMB)[A-Z0-9_-]*$|[A-Z0-9][A-Z0-9_]*_(?:EXTERNAL_DIFF|PAGER|EDITOR)$)`)

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// normalizeEnvName upper-cases and maps "-" to "_".
func normalizeEnvName(name string) string {
	return strings.ReplaceAll(strings.ToUpper(name), "-", "_")
}

// DangerousEnvName reports whether an environment variable name influences code
// execution and should be scrubbed from a hook's environment.
func DangerousEnvName(name string) bool {
	n := normalizeEnvName(name)
	return dangerousEnvNames[n] || dangerousEnvPattern.MatchString(n)
}

// SanitizeEnv returns a copy of env ("K=V" entries) with every dangerous
// variable removed. Entries without "=" are kept unchanged.
func SanitizeEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if DangerousEnvName(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// EnvAdditions describes the standard gocode-set variables added to every
// command hook's environment. SessionID is required; the rest are
// optional.
type EnvAdditions struct {
	SessionID string
	Attended  bool
	PID       int
	// Effort, when set, is exported as GOCODE_EFFORT.
	Effort string
}

// Env renders the additions as "K=V" entries.
func (a EnvAdditions) Env() []string {
	attended := "0"
	if a.Attended {
		attended = "1"
	}
	out := []string{
		"gocode=1",
		"GOCODE_CODE_SESSION_ID=" + a.SessionID,
		"GOCODE_CODE_CHILD_SESSION=1",
		"GOCODE_CODE_SESSION_ATTENDED=" + attended,
	}
	if a.PID != 0 {
		out = append(out, "GOCODE_PID="+strconv.Itoa(a.PID))
	}
	if a.Effort != "" {
		out = append(out, "GOCODE_EFFORT="+a.Effort)
	}
	// Hooks written for older gocode versions read the lower-case spelling.
	return envutil.WithLegacyEnv(out)
}
