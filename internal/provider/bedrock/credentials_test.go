package bedrock

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeHome creates a temp home directory with the given ~/.aws files and
// returns a Config whose environment is exactly vars (never the real one).
func fakeHome(t *testing.T, credentials, config string, vars map[string]string) Config {
	t.Helper()
	home := t.TempDir()
	if credentials != "" || config != "" {
		if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if credentials != "" {
		if err := os.WriteFile(filepath.Join(home, ".aws", "credentials"), []byte(credentials), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Config{
		HomeDir: home,
		Getenv:  func(k string) string { return vars[k] },
	}
}

const fakeCredsFile = `
# fake credentials for tests
[default]
aws_access_key_id = AKIDDEFAULTFAKE
aws_secret_access_key = defaultsecretfake

[dev]
aws_access_key_id=AKIDDEVFAKE
aws_secret_access_key=devsecretfake  ; inline comment
aws_session_token = devtokenfake
`

const fakeConfigFile = `
[default]
region = eu-west-1

[profile dev]
region = us-west-2
s3 =
    max_concurrent_requests = 10

[profile proc]
credential_process = /bin/false

[profile sso]
sso_session = corp
sso_account_id = 111122223333
sso_role_name = Dev

[profile role]
role_arn = arn:aws:iam::111122223333:role/x
source_profile = default
`

func retrieve(t *testing.T, p *Provider) Credentials {
	t.Helper()
	c, err := p.auth.source.retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCredentialResolutionOrder(t *testing.T) {
	all := map[string]string{
		EnvBearerToken:     "bedrock-api-key-fake",
		EnvAccessKeyID:     "AKIDENVFAKE",
		EnvSecretAccessKey: "envsecretfake",
		EnvSessionToken:    "envtokenfake",
		EnvProfile:         "dev",
	}
	drop := func(keys ...string) map[string]string {
		m := map[string]string{}
		for k, v := range all {
			m[k] = v
		}
		for _, k := range keys {
			delete(m, k)
		}
		return m
	}

	// 1. Bearer token wins over everything.
	p, err := New(fakeHome(t, fakeCredsFile, fakeConfigFile, all))
	if err != nil {
		t.Fatal(err)
	}
	if p.AuthMethod() != AuthBearer || p.auth.bearer != "bedrock-api-key-fake" {
		t.Fatalf("method = %s", p.AuthMethod())
	}

	// 2. Env keys beat the profile.
	p, err = New(fakeHome(t, fakeCredsFile, fakeConfigFile, drop(EnvBearerToken)))
	if err != nil {
		t.Fatal(err)
	}
	if p.AuthMethod() != AuthEnvKeys {
		t.Fatalf("method = %s", p.AuthMethod())
	}
	if c := retrieve(t, p); c.AccessKeyID != "AKIDENVFAKE" || c.SessionToken != "envtokenfake" {
		t.Errorf("env creds = %v", c)
	}
	if p.Region() != "us-west-2" { // from profile dev (no AWS_REGION set)
		t.Errorf("region = %s", p.Region())
	}

	// 3. AWS_PROFILE selects the profile (credentials file keys + session token).
	p, err = New(fakeHome(t, fakeCredsFile, fakeConfigFile, drop(EnvBearerToken, EnvAccessKeyID, EnvSecretAccessKey, EnvSessionToken)))
	if err != nil {
		t.Fatal(err)
	}
	if p.AuthMethod() != AuthProfileKeys || !strings.Contains(p.AuthDescription(), `"dev"`) {
		t.Fatalf("method = %s (%s)", p.AuthMethod(), p.AuthDescription())
	}
	if c := retrieve(t, p); c.AccessKeyID != "AKIDDEVFAKE" || c.SecretAccessKey != "devsecretfake" || c.SessionToken != "devtokenfake" {
		t.Errorf("profile creds = %+v", c)
	}

	// 4. No AWS_PROFILE: the default profile, region from [default].
	p, err = New(fakeHome(t, fakeCredsFile, fakeConfigFile, map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if c := retrieve(t, p); c.AccessKeyID != "AKIDDEFAULTFAKE" {
		t.Errorf("default creds = %v", c)
	}
	if p.Region() != "eu-west-1" {
		t.Errorf("region = %s", p.Region())
	}

	// AWS_REGION beats AWS_DEFAULT_REGION beats the profile.
	p, err = New(fakeHome(t, fakeCredsFile, fakeConfigFile, map[string]string{EnvRegion: "ap-northeast-1", EnvDefaultRegion: "ca-central-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Region() != "ap-northeast-1" || p.runtimeURL != "https://bedrock-runtime.ap-northeast-1.amazonaws.com" {
		t.Errorf("region = %s, url = %s", p.Region(), p.runtimeURL)
	}
	p, _ = New(fakeHome(t, fakeCredsFile, fakeConfigFile, map[string]string{EnvDefaultRegion: "ca-central-1"}))
	if p.Region() != "ca-central-1" {
		t.Errorf("region = %s", p.Region())
	}
}

func TestCredentialErrors(t *testing.T) {
	// Nothing configured at all.
	_, err := New(fakeHome(t, "", "", map[string]string{}))
	if err == nil || !strings.Contains(err.Error(), EnvBearerToken) {
		t.Errorf("no creds: err = %v", err)
	}
	// Explicit profile that does not exist.
	_, err = New(fakeHome(t, fakeCredsFile, fakeConfigFile, map[string]string{EnvProfile: "nope"}))
	if err == nil || !strings.Contains(err.Error(), `"nope" not found`) {
		t.Errorf("missing profile: err = %v", err)
	}
	// Half-set env keys.
	_, err = New(fakeHome(t, "", "", map[string]string{EnvAccessKeyID: "AKIDFAKE"}))
	if err == nil || strings.Contains(err.Error(), "AKIDFAKE") {
		t.Errorf("half env: err = %v", err)
	}
	// SSO and assume-role profiles get actionable guidance.
	for _, prof := range []string{"sso", "role"} {
		_, err = New(fakeHome(t, fakeCredsFile, fakeConfigFile, map[string]string{EnvProfile: prof}))
		if err == nil || !strings.Contains(err.Error(), "aws configure export-credentials") {
			t.Errorf("%s: err = %v", prof, err)
		}
	}
	_, err = New(fakeHome(t, fakeCredsFile, fakeConfigFile, map[string]string{EnvProfile: "sso"}))
	if err == nil || !strings.Contains(err.Error(), "aws sso login") {
		t.Errorf("sso: err = %v", err)
	}
	// Error text never includes a secret.
	_, err = New(fakeHome(t, "[x]\naws_secret_access_key = onlysecretfake\n", "", map[string]string{EnvProfile: "x"}))
	if err == nil || strings.Contains(err.Error(), "onlysecretfake") {
		t.Errorf("partial profile: err = %v", err)
	}
}

func TestSharedFileOverridesAndExplicitConfig(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds")
	if err := os.WriteFile(credPath, []byte("[other]\naws_access_key_id=AKIDOTHERFAKE\naws_secret_access_key=othersecretfake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fakeHome(t, "", "", map[string]string{EnvCredentialsFile: credPath, EnvProfile: "other"})
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := retrieve(t, p); c.AccessKeyID != "AKIDOTHERFAKE" {
		t.Errorf("creds = %v", c)
	}

	// Explicit Config keys beat the environment; explicit bearer beats them.
	cfg = fakeHome(t, "", "", map[string]string{EnvAccessKeyID: "AKIDENVFAKE", EnvSecretAccessKey: "envsecretfake"})
	cfg.AccessKeyID, cfg.SecretAccessKey = "AKIDCFGFAKE", "cfgsecretfake"
	p, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.AuthMethod() != AuthStatic || retrieve(t, p).AccessKeyID != "AKIDCFGFAKE" {
		t.Errorf("explicit keys not preferred: %s", p.AuthMethod())
	}
	cfg.BearerToken = "cfg-bearer-fake"
	p, _ = New(cfg)
	if p.AuthMethod() != AuthBearer {
		t.Errorf("explicit bearer not preferred: %s", p.AuthMethod())
	}

	// Endpoint overrides.
	cfg = fakeHome(t, "", "", map[string]string{EnvBearerToken: "x-fake", EnvEndpointRuntime: "http://127.0.0.1:9/rt/", EnvEndpointURL: "http://127.0.0.1:9"})
	p, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.runtimeURL != "http://127.0.0.1:9/rt" || p.controlURL != "http://127.0.0.1:9" {
		t.Errorf("endpoints = %s %s", p.runtimeURL, p.controlURL)
	}
	cfg = fakeHome(t, "", "", map[string]string{EnvBearerToken: "x-fake", EnvRegion: "cn-north-1"})
	p, _ = New(cfg)
	if p.runtimeURL != "https://bedrock-runtime.cn-north-1.amazonaws.com.cn" {
		t.Errorf("china endpoint = %s", p.runtimeURL)
	}
	if _, err := New(fakeHome(t, "", "", map[string]string{EnvBearerToken: "x-fake", EnvRegion: "us-east-1/../evil"})); err == nil {
		t.Errorf("invalid region accepted")
	}
}

func TestCredentialProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "creds helper.sh")
	counter := filepath.Join(dir, "count")
	body := "#!/bin/sh\necho x >> \"" + counter + "\"\n" +
		`echo '{"Version":1,"AccessKeyId":"AKIDPROCFAKE","SecretAccessKey":"procsecretfake","SessionToken":"proctokenfake","Expiration":"` +
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}'` + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[profile proc]\ncredential_process = \"" + script + "\" --profile proc\n"
	p, err := New(fakeHome(t, "", config, map[string]string{EnvProfile: "proc"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.AuthMethod() != AuthCredentialProcess {
		t.Fatalf("method = %s", p.AuthMethod())
	}
	c := retrieve(t, p)
	if c.AccessKeyID != "AKIDPROCFAKE" || c.SessionToken != "proctokenfake" || c.Expires.IsZero() {
		t.Errorf("process creds = %+v", c)
	}
	retrieve(t, p) // cached: no second run
	b, _ := os.ReadFile(counter)
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Errorf("credential_process ran %d times, want 1 (cached)", n)
	}

	// A failing / malformed helper yields an error without its output.
	bad := filepath.Join(dir, "bad.sh")
	_ = os.WriteFile(bad, []byte("#!/bin/sh\necho '{\"Version\":1,\"SecretAccessKey\":\"leakfake\"}'\n"), 0o700)
	p, _ = New(fakeHome(t, "", "[profile b]\ncredential_process = "+bad+"\n", map[string]string{EnvProfile: "b"}))
	if _, err := p.auth.source.retrieve(context.Background()); err == nil || strings.Contains(err.Error(), "leakfake") {
		t.Errorf("bad helper err = %v", err)
	}
}

func TestSplitCommandLine(t *testing.T) {
	tests := []struct {
		in      string
		windows bool
		want    []string
	}{
		{`aws configure export-credentials --profile x --format process`, false, []string{"aws", "configure", "export-credentials", "--profile", "x", "--format", "process"}},
		{`"/opt/my tools/helper" 'a b' c\ d "q\"x"`, false, []string{"/opt/my tools/helper", "a b", "c d", `q"x`}},
		{`C:\tools\helper.exe --x`, true, []string{`C:\tools\helper.exe`, "--x"}},
		{`   `, false, nil},
	}
	for _, tc := range tests {
		got := splitCommandLine(tc.in, tc.windows)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
			t.Errorf("split(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseINI(t *testing.T) {
	f := parseINI(fakeConfigFile)
	if f["profile dev"]["region"] != "us-west-2" {
		t.Errorf("dev = %v", f["profile dev"])
	}
	if _, ok := f["profile dev"]["max_concurrent_requests"]; ok {
		t.Errorf("nested sub-property leaked: %v", f["profile dev"])
	}
	c := parseINI(fakeCredsFile)
	if c["dev"]["aws_secret_access_key"] != "devsecretfake" {
		t.Errorf("inline comment not stripped: %q", c["dev"]["aws_secret_access_key"])
	}
}
