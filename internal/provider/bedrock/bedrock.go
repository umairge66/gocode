// Package bedrock is a gocode Provider for Amazon Bedrock, speaking the
// Bedrock Runtime Converse / ConverseStream API directly over HTTPS.
//
// It is dependency-free (stdlib only): requests are signed with a hand-written
// AWS Signature Version 4 implementation (sigv4.go), streaming responses are
// decoded from the AWS event-stream binary framing (eventstream.go), and
// credentials are resolved from the environment and the shared AWS
// config/credentials files with a minimal INI parser (credentials.go,
// ini.go). Bedrock API keys (AWS_BEARER_TOKEN_BEDROCK) are sent as
// "Authorization: Bearer" without SigV4.
//
// Scope rules honoured: no telemetry, nothing is sent anywhere but the
// configured Bedrock endpoints, credentials are never logged or echoed in
// errors, and no instance-metadata / SSO / STS calls are made.
package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/provider"
)

// signingService is the SigV4 service name for both bedrock-runtime and the
// bedrock control plane.
const signingService = "bedrock"

// Config configures a Provider. Every field is optional: zero values fall back
// to the AWS-standard environment variables and ~/.aws files.
type Config struct {
	// Region overrides AWS_REGION / AWS_DEFAULT_REGION / the profile region
	// (default us-east-1).
	Region string
	// Profile overrides AWS_PROFILE (default "default").
	Profile string
	// BearerToken is a Bedrock API key; overrides AWS_BEARER_TOKEN_BEDROCK.
	BearerToken string
	// AccessKeyID / SecretAccessKey / SessionToken are explicit static keys;
	// when set they take precedence over the environment and profiles (but
	// not over a bearer token).
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// BaseURL overrides the runtime endpoint
	// (default https://bedrock-runtime.<region>.amazonaws.com; env
	// AWS_ENDPOINT_URL_BEDROCK_RUNTIME, then AWS_ENDPOINT_URL).
	BaseURL string
	// ControlBaseURL overrides the control-plane endpoint used for model
	// listing (default https://bedrock.<region>.amazonaws.com; env
	// AWS_ENDPOINT_URL_BEDROCK, then AWS_ENDPOINT_URL).
	ControlBaseURL string
	// HTTP is the transport. Nil means a client that refuses redirects (so
	// signed requests and bearer tokens never leave the configured host).
	HTTP provider.Doer
	// Getenv reads environment variables (nil = os.Getenv). Tests inject a map.
	Getenv func(string) string
	// HomeDir is the home directory for ~/.aws (empty = os.UserHomeDir()).
	HomeDir string
	// Now is the clock used for signing (nil = time.Now).
	Now func() time.Time
}

func (c Config) now() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

// Provider is the Amazon Bedrock provider. Create it with New.
type Provider struct {
	region     string
	runtimeURL string
	controlURL string
	auth       resolvedAuth
	http       provider.Doer
	now        func() time.Time
}

var (
	_ provider.Provider    = (*Provider)(nil)
	_ provider.ModelLister = (*Provider)(nil)
)

// noRedirectClient never follows redirects: Bedrock does not redirect, and
// following one would replay a bearer token / signed request elsewhere.
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("bedrock: refusing to follow a redirect (credentials are only sent to the configured endpoint)")
	},
}

// New resolves the region, endpoints and credential source and returns a
// ready Provider. It fails when no credentials can be found or a profile needs
// an unsupported flow (SSO, assume-role, web identity); the error says how to
// fix it and never contains secret values. credential_process commands run
// lazily, on the first request.
func New(cfg Config) (*Provider, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	env := envLookup{getenv: getenv, home: cfg.HomeDir}

	auth, err := resolveAuth(cfg, env)
	if err != nil {
		return nil, err
	}
	profile := strings.TrimSpace(cfg.Profile)
	if profile == "" {
		profile = env.get(EnvProfile)
	}
	if profile == "" {
		profile = defaultProfileName
	}
	region := resolveRegion(cfg.Region, env, profile)
	if !validRegion(region) {
		return nil, fmt.Errorf("bedrock: invalid AWS region %q", region)
	}

	runtimeURL, err := pickEndpoint(cfg.BaseURL, env.get(EnvEndpointRuntime), env.get(EnvEndpointURL), "https://bedrock-runtime."+region+"."+dnsSuffix(region))
	if err != nil {
		return nil, err
	}
	controlURL, err := pickEndpoint(cfg.ControlBaseURL, env.get(EnvEndpointControl), env.get(EnvEndpointURL), "https://bedrock."+region+"."+dnsSuffix(region))
	if err != nil {
		return nil, err
	}
	doer := cfg.HTTP
	if doer == nil {
		doer = noRedirectClient
	}
	return &Provider{
		region:     region,
		runtimeURL: runtimeURL,
		controlURL: controlURL,
		auth:       auth,
		http:       doer,
		now:        cfg.now(),
	}, nil
}

// Name identifies the provider.
func (p *Provider) Name() string { return providerName }

// Region is the AWS region requests go to.
func (p *Provider) Region() string { return p.region }

// AuthMethod reports which credential source is in use (for /status-style
// displays; it never includes secret material).
func (p *Provider) AuthMethod() AuthMethod { return p.auth.method }

// AuthDescription is a human-readable, secret-free description of the
// credential source, e.g. `AWS profile "dev" (credential_process)`.
func (p *Provider) AuthDescription() string {
	switch p.auth.method {
	case AuthBearer:
		return "Bedrock API key (" + EnvBearerToken + ")"
	case AuthEnvKeys:
		return "AWS access keys from the environment"
	case AuthStatic:
		return "AWS access keys (configured)"
	case AuthProfileKeys:
		return fmt.Sprintf("AWS profile %q (static keys)", p.auth.profile)
	case AuthCredentialProcess:
		return fmt.Sprintf("AWS profile %q (credential_process)", p.auth.profile)
	}
	return string(p.auth.method)
}

func pickEndpoint(candidates ...string) (string, error) {
	for _, c := range candidates {
		if strings.TrimSpace(c) == "" {
			continue
		}
		u, err := provider.ValidateBaseURL(c)
		if err != nil {
			return "", fmt.Errorf("bedrock: %w", err)
		}
		return u, nil
	}
	return "", errors.New("bedrock: no endpoint")
}

func dnsSuffix(region string) string {
	if strings.HasPrefix(region, "cn-") {
		return "amazonaws.com.cn"
	}
	return "amazonaws.com"
}

func validRegion(r string) bool {
	if r == "" || len(r) > 32 {
		return false
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------------------
// Transport.
// ----------------------------------------------------------------------------

// do sends an authenticated request to base+path (path already escaped) and
// returns the 2xx response; non-2xx responses become *provider.APIError.
func (p *Provider) do(ctx context.Context, method, base, path string, body []byte, accept, model string) (*http.Response, error) {
	u, err := url.Parse(base + path)
	if err != nil {
		return nil, fmt.Errorf("bedrock: build url: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, fmt.Errorf("bedrock: build request: %w", err)
	}
	req.URL = u // keep the exact escaping (model ids carry %3A)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)

	secrets := p.auth.secrets
	switch p.auth.method {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+p.auth.bearer)
	default:
		creds, err := p.auth.source.retrieve(ctx)
		if err != nil {
			return nil, err
		}
		secrets = append(append([]string(nil), secrets...), secretsOf(creds)...)
		signRequest(req, body, creds, p.region, signingService, p.now())
	}

	resp, err := p.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, &provider.APIError{
			Provider:  providerName,
			Category:  provider.CategoryNetwork,
			Message:   "Network error — couldn't reach Amazon Bedrock (" + p.region + "). Check your internet connection and try again.",
			Retryable: true,
			Detail:    scrub(err.Error(), secrets),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, httpErrorFrom(resp, model, secrets)
	}
	return resp, nil
}

// modelPath returns the escaped "/model/{id}/{op}" path. The id is
// URI-encoded with the AWS rules (":" -> %3A, "/" -> %2F) so profile ids and
// ARNs travel as one path segment.
func modelPath(modelID, op string) string {
	return "/model/" + awsURIEncode(modelID, true) + "/" + op
}

// secretsFor returns the redaction list for errors outside do().
func (p *Provider) secretsFor() []string { return p.auth.secrets }

// ----------------------------------------------------------------------------
// Converse (non-streaming).
// ----------------------------------------------------------------------------

// Chat performs one Converse call.
func (p *Provider) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	modelID := provider.WireModelID(req.Model)
	payload, err := buildConverseBody(req, modelID)
	if err != nil {
		return provider.ChatResponse{}, fmt.Errorf("bedrock: marshal request: %w", err)
	}
	resp, err := p.do(ctx, http.MethodPost, p.runtimeURL, modelPath(modelID, "converse"), payload, "application/json", modelID)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	defer resp.Body.Close()

	var parsed cvResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return provider.ChatResponse{}, fmt.Errorf("bedrock: decode response: %w", err)
	}
	out := provider.ChatResponse{
		ID:           resp.Header.Get("X-Amzn-Requestid"),
		Model:        req.Model,
		FinishReason: finishReason(parsed.StopReason),
		Usage:        parsed.Usage.chatUsage(),
	}
	var text strings.Builder
	if m := parsed.Output.Message; m != nil {
		for _, b := range m.Content {
			switch {
			case b.Text != nil:
				text.WriteString(*b.Text)
			case b.ToolUse != nil:
				args := "{}"
				if len(b.ToolUse.Input) > 0 && string(b.ToolUse.Input) != "null" {
					args = string(b.ToolUse.Input)
				}
				out.ToolCalls = append(out.ToolCalls, provider.ChatToolCall{ID: b.ToolUse.ToolUseID, Name: b.ToolUse.Name, Arguments: args})
			}
		}
	}
	out.Content = text.String()
	return out, nil
}

// ----------------------------------------------------------------------------
// ConverseStream.
// ----------------------------------------------------------------------------

// streamEvent is the union of ConverseStream event payloads.
type streamEvent struct {
	Role              string `json:"role"`
	ContentBlockIndex int    `json:"contentBlockIndex"`
	Start             *struct {
		ToolUse *struct {
			ToolUseID string `json:"toolUseId"`
			Name      string `json:"name"`
		} `json:"toolUse"`
	} `json:"start"`
	Delta *struct {
		Text    *string `json:"text"`
		ToolUse *struct {
			Input string `json:"input"`
		} `json:"toolUse"`
		ReasoningContent *struct {
			Text            string `json:"text"`
			Signature       string `json:"signature"`
			RedactedContent string `json:"redactedContent"`
		} `json:"reasoningContent"`
	} `json:"delta"`
	StopReason string   `json:"stopReason"`
	Usage      *cvUsage `json:"usage"`
}

type blockAcc struct {
	isTool bool
	id     string
	name   string
	text   strings.Builder
	input  strings.Builder
}

// StreamChat performs one ConverseStream call, decoding the event stream
// (messageStart, contentBlockStart/Delta/Stop, messageStop, metadata, and
// exception frames) into a ChatResponse. Text deltas go to onDelta; reasoning
// deltas (reasoningContent.text) go to req.OnReasoning.
func (p *Provider) StreamChat(ctx context.Context, req provider.ChatRequest, onDelta func(delta string)) (provider.ChatResponse, error) {
	modelID := provider.WireModelID(req.Model)
	payload, err := buildConverseBody(req, modelID)
	if err != nil {
		return provider.ChatResponse{}, fmt.Errorf("bedrock: marshal request: %w", err)
	}
	resp, err := p.do(ctx, http.MethodPost, p.runtimeURL, modelPath(modelID, "converse-stream"), payload, "application/vnd.amazon.eventstream", modelID)
	if err != nil {
		return provider.ChatResponse{}, err
	}
	defer resp.Body.Close()

	out := provider.ChatResponse{ID: resp.Header.Get("X-Amzn-Requestid"), Model: req.Model}
	var (
		usage   cvUsage
		order   []int
		blocks  = map[int]*blockAcc{}
		stopped bool
	)
	get := func(i int) *blockAcc {
		if b, ok := blocks[i]; ok {
			return b
		}
		b := &blockAcc{}
		blocks[i] = b
		order = append(order, i)
		return b
	}

	rd := newEventStreamReader(resp.Body)
	for {
		msg, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return provider.ChatResponse{}, ctx.Err()
			}
			return provider.ChatResponse{}, err
		}
		switch msg.Headers[":message-type"] {
		case "exception":
			return provider.ChatResponse{}, streamErrorFrom(msg.Headers[":exception-type"], string(msg.Payload), modelID, p.secretsFor())
		case "error":
			return provider.ChatResponse{}, streamErrorFrom(msg.Headers[":error-code"], msg.Headers[":error-message"], modelID, p.secretsFor())
		case "event", "":
		default:
			continue
		}
		var ev streamEvent
		if len(msg.Payload) > 0 {
			if err := json.Unmarshal(msg.Payload, &ev); err != nil {
				return provider.ChatResponse{}, fmt.Errorf("bedrock: decode stream event: %w", err)
			}
		}
		switch msg.Headers[":event-type"] {
		case "messageStart":
		case "contentBlockStart":
			b := get(ev.ContentBlockIndex)
			if ev.Start != nil && ev.Start.ToolUse != nil {
				b.isTool = true
				b.id = ev.Start.ToolUse.ToolUseID
				b.name = ev.Start.ToolUse.Name
			}
		case "contentBlockDelta":
			if ev.Delta == nil {
				continue
			}
			b := get(ev.ContentBlockIndex)
			switch {
			case ev.Delta.Text != nil:
				b.text.WriteString(*ev.Delta.Text)
				if onDelta != nil && *ev.Delta.Text != "" {
					onDelta(*ev.Delta.Text)
				}
			case ev.Delta.ToolUse != nil:
				b.isTool = true
				b.input.WriteString(ev.Delta.ToolUse.Input)
			case ev.Delta.ReasoningContent != nil:
				if req.OnReasoning != nil && ev.Delta.ReasoningContent.Text != "" {
					req.OnReasoning(ev.Delta.ReasoningContent.Text)
				}
			}
		case "contentBlockStop":
		case "messageStop":
			stopped = true
			out.FinishReason = finishReason(ev.StopReason)
		case "metadata":
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		}
	}
	if !stopped && out.FinishReason == "" {
		// The stream ended without messageStop: a truncated response.
		return provider.ChatResponse{}, fmt.Errorf("bedrock: stream ended before messageStop: %w", io.ErrUnexpectedEOF)
	}

	var text strings.Builder
	for _, i := range order {
		b := blocks[i]
		if b.isTool {
			args := b.input.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, provider.ChatToolCall{ID: b.id, Name: b.name, Arguments: args})
			continue
		}
		text.WriteString(b.text.String())
	}
	out.Content = text.String()
	out.Usage = usage.chatUsage()
	return out, nil
}
