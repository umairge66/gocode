package envtrust

// DangerousEnvNames lists the environment variables that can hijack a freshly
// spawned child process by redirecting its dynamic loader, compiler/tooling
// lookup or OpenSSL configuration. The CLI strips or pins these before exec.
var DangerousEnvNames = []string{
	"GIT_EXEC_PATH", "DEVELOPER_DIR", "GIT_TEXTDOMAINDIR",
	"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "GCONV_PATH",
	"NLSPATH", "LOCPATH", "PSMODULEPATH",
	"DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "DYLD_FRAMEWORK_PATH",
	"DYLD_FALLBACK_LIBRARY_PATH", "DYLD_FALLBACK_FRAMEWORK_PATH",
	"DYLD_VERSIONED_LIBRARY_PATH", "DYLD_VERSIONED_FRAMEWORK_PATH",
	"OPENSSL_CONF", "OPENSSL_CONF_INCLUDE", "OPENSSL_MODULES", "OPENSSL_ENGINES",
}
