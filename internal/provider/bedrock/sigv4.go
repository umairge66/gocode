package bedrock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// This file is a small, self-contained AWS Signature Version 4 signer
// (header-based "Authorization: AWS4-HMAC-SHA256 ..."), written against the
// public specification:
//
//	https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html
//
// and verified against AWS's published signing test suite (see
// sigv4_test.go). It covers exactly what Bedrock needs: JSON POST/GET
// requests, temporary credentials (X-Amz-Security-Token) and the non-S3
// "double encode" rule for the canonical URI.

const (
	sigV4Algorithm  = "AWS4-HMAC-SHA256"
	amzDateFormat   = "20060102T150405Z"
	amzShortFormat  = "20060102"
	headerAmzDate   = "X-Amz-Date"
	headerAmzToken  = "X-Amz-Security-Token"
	headerAuthz     = "Authorization"
	emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// unsignedHeaders are never part of the signature: they are hop-by-hop or
// commonly rewritten by proxies/transports.
var unsignedHeaders = map[string]bool{
	"authorization":   true,
	"user-agent":      true,
	"x-amzn-trace-id": true,
	"expect":          true,
	"connection":      true,
	"accept-encoding": true,
}

// signRequest signs req in place with SigV4. body is the exact request
// payload (nil for none). It sets X-Amz-Date, X-Amz-Security-Token (when the
// credentials carry a session token) and Authorization. Every header already
// on req (other than unsignedHeaders) plus Host is signed.
func signRequest(req *http.Request, body []byte, creds Credentials, region, service string, now time.Time) {
	t := now.UTC()
	amzDate := t.Format(amzDateFormat)
	req.Header.Set(headerAmzDate, amzDate)
	if creds.SessionToken != "" {
		req.Header.Set(headerAmzToken, creds.SessionToken)
	}
	canonical, signedHeaders := canonicalRequest(req, hashHex(body))
	scope := t.Format(amzShortFormat) + "/" + region + "/" + service + "/aws4_request"
	sts := stringToSign(amzDate, scope, canonical)
	key := signingKey(creds.SecretAccessKey, t.Format(amzShortFormat), region, service)
	sig := hex.EncodeToString(hmacSHA256(key, []byte(sts)))
	req.Header.Set(headerAuthz, sigV4Algorithm+" Credential="+creds.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
}

// canonicalRequest builds the SigV4 canonical request for req (whose payload
// hashes to payloadHash) and returns it with the signed-headers list.
func canonicalRequest(req *http.Request, payloadHash string) (canonical, signedHeaders string) {
	// Canonical headers: lowercase names, trimmed values with runs of spaces
	// collapsed, multiple values joined by commas, sorted by name.
	headers := map[string][]string{}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers["host"] = []string{host}
	for name, vals := range req.Header {
		lname := strings.ToLower(name)
		if unsignedHeaders[lname] || lname == "host" {
			continue
		}
		for _, v := range vals {
			headers[lname] = append(headers[lname], collapseSpaces(v))
		}
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n)
		ch.WriteByte(':')
		ch.WriteString(strings.Join(headers[n], ","))
		ch.WriteByte('\n')
	}
	signedHeaders = strings.Join(names, ";")

	canonical = strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		ch.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	return canonical, signedHeaders
}

// canonicalURI returns the canonical path. For every service except S3, AWS
// URI-encodes each segment of the path *as sent on the wire* once more
// ("double encoding"): a Bedrock model id "a:0" travels as "a%3A0" and is
// signed as "a%253A0".
func canonicalURI(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsURIEncode(s, false)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery sorts the query parameters by encoded name, then value.
func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	q := u.Query()
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range q {
		ek := awsURIEncode(k, true)
		for _, v := range vs {
			pairs = append(pairs, kv{ek, awsURIEncode(v, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// awsURIEncode percent-encodes every byte except the RFC 3986 unreserved set
// (A-Z a-z 0-9 - _ . ~), using uppercase hex. encodeSlash controls '/'.
func awsURIEncode(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xF])
		}
	}
	return b.String()
}

func stringToSign(amzDate, scope, canonical string) string {
	return sigV4Algorithm + "\n" + amzDate + "\n" + scope + "\n" + hashHex([]byte(canonical))
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hashHex(b []byte) string {
	if len(b) == 0 {
		return emptyBodySHA256
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
