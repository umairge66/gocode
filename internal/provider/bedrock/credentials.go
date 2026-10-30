package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Environment variables consulted (the AWS-standard names).
const (
	EnvBearerToken       = "AWS_BEARER_TOKEN_BEDROCK"
	EnvAccessKeyID       = "AWS_ACCESS_KEY_ID"
	EnvSecretAccessKey   = "AWS_SECRET_ACCESS_KEY"
	EnvSessionToken      = "AWS_SESSION_TOKEN"
	EnvRegion            = "AWS_REGION"
	EnvDefaultRegion     = "AWS_DEFAULT_REGION"
	EnvProfile           = "AWS_PROFILE"
	EnvCredentialsFile   = "AWS_SHARED_CREDENTIALS_FILE"
	EnvConfigFile        = "AWS_CONFIG_FILE"
	EnvEndpointURL       = "AWS_ENDPOINT_URL"
	EnvEndpointRuntime   = "AWS_ENDPOINT_URL_BEDROCK_RUNTIME"
	EnvEndpointControl   = "AWS_ENDPOINT_URL_BEDROCK"
	defaultRegion        = "us-east-1"
	defaultProfileName   = "default"
	credentialProcessTTL = 2 * time.Minute
	// refreshSkew refreshes expiring credential_process credentials this long
	// before their Expiration.
	refreshSkew = 5 * time.Minute
)

// Credentials are AWS access keys (long-term or temporary). They are never
// printed: String/GoString redact everything but the key id prefix.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Expires is when temporary credentials stop working (zero = never).
	Expires time.Time
}

// String redacts the credentials.
func (c Credentials) String() string {
	id := c.AccessKeyID
	if len(id) > 4 {
		id = id[:4] + "…"
	}
	return "Credentials{AccessKeyID:" + id + ", SecretAccessKey:***}"
}

// GoString redacts the credentials for %#v as well.
func (c Credentials) GoString() string { return c.String() }

// AuthMethod identifies how requests are authenticated.
type AuthMethod string

const (
	// AuthBearer sends "Authorization: Bearer <Bedrock API key>" (no SigV4).
	AuthBearer AuthMethod = "bearer"
	// AuthEnvKeys signs with AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
	AuthEnvKeys AuthMethod = "env"
	// AuthStatic signs with keys passed explicitly in Config.
	AuthStatic AuthMethod = "static"
	// AuthProfileKeys signs with static keys from a shared-config profile.
	AuthProfileKeys AuthMethod = "profile"
	// AuthCredentialProcess signs with keys produced by a profile's
	// credential_process command.
	AuthCredentialProcess AuthMethod = "credential_process"
)

// credentialSource yields signing credentials (possibly refreshing them).
type credentialSource interface {
	retrieve(ctx context.Context) (Credentials, error)
}

type staticSource struct{ creds Credentials }

func (s staticSource) retrieve(context.Context) (Credentials, error) { return s.creds, nil }

// processSource runs a credential_process command and caches its output until
// shortly before it expires.
type processSource struct {
	profile string
	command string
	now     func() time.Time

	mu     sync.Mutex
	cached Credentials
	valid  bool
}

func (p *processSource) retrieve(ctx context.Context) (Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.valid && (p.cached.Expires.IsZero() || p.now().Add(refreshSkew).Before(p.cached.Expires)) {
		return p.cached, nil
	}
	c, err := runCredentialProcess(ctx, p.profile, p.command)
	if err != nil {
		return Credentials{}, err
	}
	p.cached, p.valid = c, true
	return c, nil
}

// resolvedAuth is the outcome of credential resolution.
type resolvedAuth struct {
	method  AuthMethod
	bearer  string
	source  credentialSource
	profile string // profile name used (profile / credential_process)
	// secrets lists every secret string in play, for redaction of error text.
	secrets []string
}

// envLookup bundles the environment and home directory used for resolution,
// so tests never touch the real process environment or ~/.aws.
type envLookup struct {
	getenv func(string) string
	home   string
}

func (e envLookup) get(k string) string { return strings.TrimSpace(e.getenv(k)) }

func (e envLookup) homeDir() string {
	if e.home != "" {
		return e.home
	}
	h, _ := os.UserHomeDir()
	return h
}

func (e envLookup) expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(e.homeDir(), p[1:])
	}
	return p
}

func (e envLookup) credentialsPath() string {
	if p := e.get(EnvCredentialsFile); p != "" {
		return e.expand(p)
	}
	return filepath.Join(e.homeDir(), ".aws", "credentials")
}

func (e envLookup) configPath() string {
	if p := e.get(EnvConfigFile); p != "" {
		return e.expand(p)
	}
	return filepath.Join(e.homeDir(), ".aws", "config")
}

// profileSettings returns the merged settings of profile name: the config
// file's "[profile name]" (or "[default]") section, overridden by the
// credentials file's "[name]" section. found reports whether either exists.
func (e envLookup) profileSettings(name string) (settings map[string]string, found bool, err error) {
	settings = map[string]string{}
	cfg, err := readINI(e.configPath())
	if err != nil {
		return nil, false, fmt.Errorf("bedrock: read AWS config file: %w", err)
	}
	section := "profile " + name
	if name == defaultProfileName {
		if _, ok := cfg[section]; !ok {
			section = defaultProfileName
		}
	}
	if s, ok := cfg[section]; ok {
		found = true
		for k, v := range s {
			settings[k] = v
		}
	}
	creds, err := readINI(e.credentialsPath())
	if err != nil {
		return nil, false, fmt.Errorf("bedrock: read AWS credentials file: %w", err)
	}
	if s, ok := creds[name]; ok {
		found = true
		for k, v := range s {
			settings[k] = v
		}
	}
	return settings, found, nil
}

// resolveRegion picks the region: explicit, AWS_REGION, AWS_DEFAULT_REGION,
// the profile's "region" setting, then us-east-1.
func resolveRegion(explicit string, env envLookup, profile string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if r := env.get(EnvRegion); r != "" {
		return r
	}
	if r := env.get(EnvDefaultRegion); r != "" {
		return r
	}
	if s, ok, err := env.profileSettings(profile); err == nil && ok {
		if r := strings.TrimSpace(s["region"]); r != "" {
			return r
		}
	}
	return defaultRegion
}

// resolveAuth applies the AWS-standard precedence:
//
//  1. a Bedrock API key (explicit BearerToken, else AWS_BEARER_TOKEN_BEDROCK)
//  2. explicit static keys in Config
//  3. AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY (+ AWS_SESSION_TOKEN)
//  4. the shared-config profile (Config.Profile, else AWS_PROFILE, else
//     "default"): static keys, then credential_process.
//
// SSO, assume-role, web-identity and instance/container metadata credentials
// are not resolved (they need AWS SDK flows or network calls); profiles that
// rely on them get an actionable error.
func resolveAuth(cfg Config, env envLookup) (resolvedAuth, error) {
	if tok := strings.TrimSpace(cfg.BearerToken); tok != "" {
		return resolvedAuth{method: AuthBearer, bearer: tok, secrets: []string{tok}}, nil
	}
	if tok := env.get(EnvBearerToken); tok != "" {
		return resolvedAuth{method: AuthBearer, bearer: tok, secrets: []string{tok}}, nil
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
			return resolvedAuth{}, errors.New("bedrock: both an access key id and a secret access key are required")
		}
		c := Credentials{AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey, SessionToken: cfg.SessionToken}
		return resolvedAuth{method: AuthStatic, source: staticSource{c}, secrets: secretsOf(c)}, nil
	}
	if id, secret := env.get(EnvAccessKeyID), env.get(EnvSecretAccessKey); id != "" || secret != "" {
		if id == "" || secret == "" {
			return resolvedAuth{}, fmt.Errorf("bedrock: %s and %s must both be set", EnvAccessKeyID, EnvSecretAccessKey)
		}
		c := Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: env.get(EnvSessionToken)}
		return resolvedAuth{method: AuthEnvKeys, source: staticSource{c}, secrets: secretsOf(c)}, nil
	}

	profile := strings.TrimSpace(cfg.Profile)
	explicitProfile := profile != ""
	if profile == "" {
		profile = env.get(EnvProfile)
		explicitProfile = profile != ""
	}
	if profile == "" {
		profile = defaultProfileName
	}
	settings, found, err := env.profileSettings(profile)
	if err != nil {
		return resolvedAuth{}, err
	}
	if !found {
		if explicitProfile {
			return resolvedAuth{}, fmt.Errorf("bedrock: AWS profile %q not found in %s or %s", profile, env.configPath(), env.credentialsPath())
		}
		return resolvedAuth{}, errNoCredentials
	}
	return profileAuth(profile, settings, cfg.now())
}

var errNoCredentials = errors.New("bedrock: no AWS credentials found — set " + EnvBearerToken +
	" (a Bedrock API key), or " + EnvAccessKeyID + " + " + EnvSecretAccessKey +
	", or configure a profile in ~/.aws/credentials / ~/.aws/config and select it with " + EnvProfile)

// profileAuth turns a profile's settings into an auth method.
func profileAuth(profile string, s map[string]string, now func() time.Time) (resolvedAuth, error) {
	id, secret := s["aws_access_key_id"], s["aws_secret_access_key"]
	if id != "" && secret != "" {
		c := Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: s["aws_session_token"]}
		return resolvedAuth{method: AuthProfileKeys, source: staticSource{c}, profile: profile, secrets: secretsOf(c)}, nil
	}
	if cmd := strings.TrimSpace(s["credential_process"]); cmd != "" {
		return resolvedAuth{
			method:  AuthCredentialProcess,
			source:  &processSource{profile: profile, command: cmd, now: now},
			profile: profile,
		}, nil
	}
	exportHint := fmt.Sprintf("run `aws sso login --profile %[1]s` (if needed) and then either "+
		"`eval \"$(aws configure export-credentials --profile %[1]s --format env)\"`, or add "+
		"`credential_process = aws configure export-credentials --profile %[1]s --format process` "+
		"to a separate profile and use that one", profile)
	switch {
	case s["sso_session"] != "" || s["sso_start_url"] != "" || s["sso_account_id"] != "":
		return resolvedAuth{}, fmt.Errorf("bedrock: AWS profile %q uses IAM Identity Center (SSO), which gocode cannot resolve without the AWS SDK; %s", profile, exportHint)
	case s["role_arn"] != "":
		return resolvedAuth{}, fmt.Errorf("bedrock: AWS profile %q assumes a role (role_arn), which gocode does not resolve; %s", profile, exportHint)
	case s["web_identity_token_file"] != "":
		return resolvedAuth{}, fmt.Errorf("bedrock: AWS profile %q uses web identity federation, which gocode does not resolve; %s", profile, exportHint)
	case id != "" || secret != "":
		return resolvedAuth{}, fmt.Errorf("bedrock: AWS profile %q must set both aws_access_key_id and aws_secret_access_key", profile)
	}
	return resolvedAuth{}, fmt.Errorf("bedrock: AWS profile %q has no credentials (expected aws_access_key_id/aws_secret_access_key or credential_process)", profile)
}

func secretsOf(c Credentials) []string {
	var out []string
	for _, s := range []string{c.SecretAccessKey, c.SessionToken} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// credentialProcessOutput is the documented credential_process JSON.
type credentialProcessOutput struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
}

// runCredentialProcess executes a profile's credential_process command
// (without a shell, like the AWS CLI) and parses its JSON stdout. Neither the
// command output nor its stderr is echoed into errors, since both can carry
// secrets.
func runCredentialProcess(ctx context.Context, profile, command string) (Credentials, error) {
	args := splitCommandLine(command, runtime.GOOS == "windows")
	if len(args) == 0 {
		return Credentials{}, fmt.Errorf("bedrock: AWS profile %q: empty credential_process", profile)
	}
	ctx, cancel := context.WithTimeout(ctx, credentialProcessTTL)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil // discarded: may contain secrets
	if err := cmd.Run(); err != nil {
		return Credentials{}, fmt.Errorf("bedrock: AWS profile %q: credential_process %q failed: %v", profile, args[0], err)
	}
	var out credentialProcessOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return Credentials{}, fmt.Errorf("bedrock: AWS profile %q: credential_process output is not valid JSON", profile)
	}
	if out.Version != 1 {
		return Credentials{}, fmt.Errorf("bedrock: AWS profile %q: credential_process returned unsupported Version %d (want 1)", profile, out.Version)
	}
	if out.AccessKeyID == "" || out.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("bedrock: AWS profile %q: credential_process output lacks AccessKeyId/SecretAccessKey", profile)
	}
	c := Credentials{AccessKeyID: out.AccessKeyID, SecretAccessKey: out.SecretAccessKey, SessionToken: out.SessionToken}
	if out.Expiration != "" {
		t, err := time.Parse(time.RFC3339, out.Expiration)
		if err != nil {
			return Credentials{}, fmt.Errorf("bedrock: AWS profile %q: credential_process Expiration is not RFC 3339", profile)
		}
		c.Expires = t
	}
	return c, nil
}

// splitCommandLine splits a command line into arguments with POSIX-shell-like
// quoting: whitespace separates, '…' is literal, "…" allows \" and \\, and
// (outside Windows) a bare backslash escapes the next character. On Windows
// backslashes are literal so paths like C:\tools\creds.exe survive.
func splitCommandLine(s string, windows bool) []string {
	var (
		args    []string
		cur     strings.Builder
		inArg   bool
		quote   byte
		escaped bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inArg = true
		case c == '\\' && !windows:
			escaped = true
			inArg = true
		case c == ' ' || c == '\t' || c == '\n':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteByte(c)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}
