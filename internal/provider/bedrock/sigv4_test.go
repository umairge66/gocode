package bedrock

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Vectors from AWS's public SigV4 signing test suite
// (github.com/awslabs/aws-c-auth tests/aws-signing-test-suite/v4), which all
// use these example credentials, region, service and timestamp.
var (
	suiteCreds = Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	suiteTime  = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
)

func suiteRequest(t *testing.T, method, target string, headers [][2]string, body string) *http.Request {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, "https://example.amazonaws.com"+target, nil)
	} else {
		r, err = http.NewRequest(method, "https://example.amazonaws.com"+target, strings.NewReader(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range headers {
		r.Header.Set(h[0], h[1])
	}
	return r
}

func signatureOf(t *testing.T, r *http.Request) string {
	t.Helper()
	a := r.Header.Get("Authorization")
	i := strings.Index(a, "Signature=")
	if i < 0 {
		t.Fatalf("no signature in %q", a)
	}
	return a[i+len("Signature="):]
}

func TestSigV4SuiteVectors(t *testing.T) {
	tests := []struct {
		name          string
		method        string
		target        string
		headers       [][2]string
		body          string
		creds         Credentials
		wantSig       string
		wantSigned    string
		wantCanonical string
	}{
		{
			name: "get-vanilla", method: "GET", target: "/",
			creds:      suiteCreds,
			wantSig:    "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
			wantSigned: "host;x-amz-date",
		},
		{
			name: "get-vanilla-query-order-key-case", method: "GET", target: "/?Param2=value2&Param1=value1",
			creds:   suiteCreds,
			wantSig: "b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
		},
		{
			name: "get-vanilla-with-session-token", method: "GET", target: "/",
			creds: Credentials{AccessKeyID: suiteCreds.AccessKeyID, SecretAccessKey: suiteCreds.SecretAccessKey,
				SessionToken: "6e86291e8372ff2a2260956d9b8aae1d763fbf315fa00fa31553b73ebf194267"},
			wantSig:    "07ec1639c89043aa0e3e2de82b96708f198cceab042d4a97044c66dd9f74e7f8",
			wantSigned: "host;x-amz-date;x-amz-security-token",
			wantCanonical: "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n" +
				"x-amz-security-token:6e86291e8372ff2a2260956d9b8aae1d763fbf315fa00fa31553b73ebf194267\n\n" +
				"host;x-amz-date;x-amz-security-token\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name: "post-header-key-sort", method: "POST", target: "/",
			headers: [][2]string{{"My-Header1", "value1"}},
			creds:   suiteCreds,
			wantSig: "c5410059b04c1ee005303aed430f6e6645f61f4dc9e1461ec8f8916fdf18852c",
		},
		{
			name: "post-x-www-form-urlencoded", method: "POST", target: "/",
			headers: [][2]string{
				{"Content-Type", "application/x-www-form-urlencoded"},
				{"Content-Length", "13"},
				{"X-Amz-Content-Sha256", "9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e"},
			},
			body:       "Param1=value1",
			creds:      suiteCreds,
			wantSig:    "d3875051da38690788ef43de4db0d8f280229d82040bfac253562e56c3f20e0b",
			wantSigned: "content-length;content-type;host;x-amz-content-sha256;x-amz-date",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := suiteRequest(t, tc.method, tc.target, tc.headers, tc.body)
			var body []byte
			if tc.body != "" {
				body = []byte(tc.body)
			}
			if tc.wantCanonical != "" {
				// Check the canonical request directly too (headers as signRequest sets them).
				r2 := suiteRequest(t, tc.method, tc.target, tc.headers, tc.body)
				r2.Header.Set(headerAmzDate, "20150830T123600Z")
				if tc.creds.SessionToken != "" {
					r2.Header.Set(headerAmzToken, tc.creds.SessionToken)
				}
				got, _ := canonicalRequest(r2, hashHex(body))
				if got != tc.wantCanonical {
					t.Errorf("canonical request:\n%s\nwant:\n%s", got, tc.wantCanonical)
				}
			}
			signRequest(r, body, tc.creds, "us-east-1", "service", suiteTime)
			if got := signatureOf(t, r); got != tc.wantSig {
				t.Errorf("signature = %s, want %s", got, tc.wantSig)
			}
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=") {
				t.Errorf("authorization header = %q", auth)
			}
			if tc.wantSigned != "" && !strings.Contains(auth, "SignedHeaders="+tc.wantSigned+",") {
				t.Errorf("signed headers in %q, want %s", auth, tc.wantSigned)
			}
			if got := r.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
				t.Errorf("x-amz-date = %q", got)
			}
		})
	}
}

// Bedrock model ids contain ':' (and ARNs '/'): they travel as one escaped
// path segment and are double-encoded in the canonical URI, like the AWS
// SDKs do for every non-S3 service.
func TestCanonicalURIDoubleEncodesModelID(t *testing.T) {
	path := modelPath("us.anthropic.claude-haiku-4-5-20251001-v1:0", "converse-stream")
	if path != "/model/us.anthropic.claude-haiku-4-5-20251001-v1%3A0/converse-stream" {
		t.Fatalf("modelPath = %q", path)
	}
	r, err := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.URL.EscapedPath(); got != path {
		t.Fatalf("wire path = %q, want %q", got, path)
	}
	if got, want := canonicalURI(r.URL), "/model/us.anthropic.claude-haiku-4-5-20251001-v1%253A0/converse-stream"; got != want {
		t.Errorf("canonical URI = %q, want %q", got, want)
	}

	arn := "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-4-6"
	p := modelPath(arn, "converse")
	if strings.Count(p, "/") != 3 {
		t.Errorf("ARN must stay one path segment: %q", p)
	}
}

func TestCredentialsNeverPrinted(t *testing.T) {
	c := Credentials{AccessKeyID: "AKIAFAKEFAKEFAKE", SecretAccessKey: "s3cr3t-fake", SessionToken: "tok-fake"}
	for _, s := range []string{c.String(), c.GoString()} {
		if strings.Contains(s, "s3cr3t") || strings.Contains(s, "tok-fake") || strings.Contains(s, "AKIAFAKEFAKEFAKE") {
			t.Errorf("credentials leaked: %s", s)
		}
	}
}
