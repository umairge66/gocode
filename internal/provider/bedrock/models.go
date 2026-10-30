package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/provider"
)

// Model ids on Bedrock come in three shapes, all accepted by Converse:
//
//   - base model ids:      anthropic.claude-sonnet-4-6, anthropic.claude-haiku-4-5-20251001-v1:0
//   - inference profiles:  us./eu./au./jp./apac./global. + base id
//   - ARNs:                arn:aws:bedrock:<region>:<acct>:inference-profile/…
//
// Newer Claude models are only invocable through an inference profile in most
// regions, which is why the static list leads with global./us. profile ids.

// geoPrefixes are the cross-region inference profile prefixes.
var geoPrefixes = []string{"global.", "us-gov.", "us.", "eu.", "apac.", "au.", "jp.", "ca.", "in."}

// baseModelID strips an inference-profile geo prefix or ARN path, returning
// the foundation-model id ("us.anthropic.claude-x" -> "anthropic.claude-x").
func baseModelID(id string) string {
	s := strings.TrimSpace(id)
	if strings.HasPrefix(s, "arn:") {
		if i := strings.LastIndexByte(s, '/'); i >= 0 {
			s = s[i+1:]
		}
	}
	for _, p := range geoPrefixes {
		if strings.HasPrefix(s, p) {
			return s[len(p):]
		}
	}
	return s
}

// staticModel is one entry of the built-in fallback list.
type staticModel struct {
	id, name string
	context  int
}

// staticModels lists current Claude ids on Bedrock, taken from the AWS
// "Models at a glance" model cards (bedrock-runtime "Programmatic Access"
// tables). Global profiles route worldwide; us. profiles stay in the US.
var staticModels = []staticModel{
	{"global.anthropic.claude-opus-5-5", "Claude Opus 5.5 (global)", 1_000_000},
	{"us.anthropic.claude-opus-5-5", "Claude Opus 5.5 (US)", 1_000_000},
	{"eu.anthropic.claude-opus-5-5", "Claude Opus 5.5 (EU)", 1_000_000},
	{"global.anthropic.claude-sonnet-5-5", "Claude Sonnet 5.5 (global)", 1_000_000},
	{"us.anthropic.claude-sonnet-5-5", "Claude Sonnet 5.5 (US)", 1_000_000},
	{"eu.anthropic.claude-sonnet-5-5", "Claude Sonnet 5.5 (EU)", 1_000_000},
	{"global.anthropic.claude-haiku-5-5", "Claude Haiku 5.5 (global)", 1_000_000},
	{"us.anthropic.claude-haiku-5-5", "Claude Haiku 5.5 (US)", 1_000_000},
	{"eu.anthropic.claude-haiku-5-5", "Claude Haiku 5.5 (EU)", 1_000_000},
	{"global.anthropic.claude-fable-5-1", "Claude Fable 5.1 (global)", 1_000_000},
	{"us.anthropic.claude-fable-5-1", "Claude Fable 5.1 (US)", 1_000_000},
	{"global.anthropic.claude-sonnet-4-6", "Claude Sonnet 4.6 (global)", 1_000_000},
	{"us.anthropic.claude-sonnet-4-6", "Claude Sonnet 4.6 (US)", 1_000_000},
	{"global.anthropic.claude-haiku-4-5-20251001-v1:0", "Claude Haiku 4.5 (global)", 200_000},
	{"us.anthropic.claude-haiku-4-5-20251001-v1:0", "Claude Haiku 4.5 (US)", 200_000},
}

// DefaultModel is the model the coordinator may preselect for Bedrock.
const DefaultModel = "global.anthropic.claude-opus-5-5"

// StaticModels returns the built-in Claude-on-Bedrock list (sorted by ID),
// used when live listing is unavailable.
func StaticModels() []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(staticModels))
	for _, m := range staticModels {
		out = append(out, provider.ModelInfo{ID: m.id, Name: m.name, ContextLength: m.context, Lifecycle: provider.LifecycleActive})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// foundationModelsResponse is ListFoundationModels' body.
type foundationModelsResponse struct {
	ModelSummaries []struct {
		ModelID                    string   `json:"modelId"`
		ModelName                  string   `json:"modelName"`
		ProviderName               string   `json:"providerName"`
		OutputModalities           []string `json:"outputModalities"`
		InferenceTypesSupported    []string `json:"inferenceTypesSupported"`
		ResponseStreamingSupported *bool    `json:"responseStreamingSupported"`
		ModelLifecycle             *struct {
			Status string `json:"status"`
		} `json:"modelLifecycle"`
	} `json:"modelSummaries"`
}

// inferenceProfilesResponse is one ListInferenceProfiles page.
type inferenceProfilesResponse struct {
	InferenceProfileSummaries []struct {
		InferenceProfileID   string `json:"inferenceProfileId"`
		InferenceProfileName string `json:"inferenceProfileName"`
		Description          string `json:"description"`
		Status               string `json:"status"`
	} `json:"inferenceProfileSummaries"`
	NextToken string `json:"nextToken"`
}

// ListModels implements provider.ModelLister for the /model picker. It asks
// the control plane (ListFoundationModels + ListInferenceProfiles, both
// SigV4/bearer-authenticated) for the text models invocable on demand and the
// active system-defined inference profiles. When the live listing fails (an
// inference-only IAM policy commonly lacks bedrock:List*), it falls back to
// StaticModels() so the picker is never empty; use LiveModels to see the
// error.
func (p *Provider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	models, err := p.LiveModels(ctx)
	if err != nil || len(models) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return StaticModels(), nil
	}
	return models, nil
}

// LiveModels lists models from the Bedrock control plane (no fallback).
func (p *Provider) LiveModels(ctx context.Context) ([]provider.ModelInfo, error) {
	byID := map[string]provider.ModelInfo{}

	var fm foundationModelsResponse
	if err := p.getJSON(ctx, "/foundation-models?byOutputModality=TEXT", &fm); err != nil {
		return nil, err
	}
	for _, m := range fm.ModelSummaries {
		if m.ResponseStreamingSupported != nil && !*m.ResponseStreamingSupported {
			continue
		}
		if !contains(m.InferenceTypesSupported, "ON_DEMAND") {
			continue // reachable only through an inference profile (listed below)
		}
		info := provider.ModelInfo{ID: m.ModelID, Name: m.ModelName, Lifecycle: provider.LifecycleActive}
		if m.ProviderName != "" && m.ModelName != "" {
			info.Name = m.ProviderName + " " + m.ModelName
		}
		if m.ModelLifecycle != nil && strings.EqualFold(m.ModelLifecycle.Status, "LEGACY") {
			info.Lifecycle = provider.LifecycleDeprecated
		}
		byID[info.ID] = info
	}

	next := ""
	for page := 0; page < 20; page++ {
		path := "/inference-profiles?maxResults=1000&typeEquals=SYSTEM_DEFINED"
		if next != "" {
			path += "&nextToken=" + url.QueryEscape(next)
		}
		var ip inferenceProfilesResponse
		if err := p.getJSON(ctx, path, &ip); err != nil {
			return nil, err
		}
		for _, s := range ip.InferenceProfileSummaries {
			if s.Status != "" && !strings.EqualFold(s.Status, "ACTIVE") {
				continue
			}
			name := s.InferenceProfileName
			if name == "" {
				name = s.InferenceProfileID
			}
			byID[s.InferenceProfileID] = provider.ModelInfo{ID: s.InferenceProfileID, Name: name, Description: s.Description, Lifecycle: provider.LifecycleActive}
		}
		if ip.NextToken == "" {
			break
		}
		next = ip.NextToken
	}

	// Carry the known context windows over from the static list.
	known := map[string]int{}
	for _, m := range staticModels {
		known[baseModelID(m.id)] = m.context
	}
	out := make([]provider.ModelInfo, 0, len(byID))
	for _, m := range byID {
		if m.ContextLength == 0 {
			m.ContextLength = known[baseModelID(m.ID)]
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (p *Provider) getJSON(ctx context.Context, path string, out any) error {
	resp, err := p.do(ctx, http.MethodGet, p.controlURL, path, nil, "application/json", "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("bedrock: decode model list: %w", err)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
