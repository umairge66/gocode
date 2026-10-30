package provider

import (
	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/toolsearch"
)

// The built-in provider specs, registered in display / auto-detection order.
// The first three keep their historical precedence (openrouter > anthropic >
// openai) and their own concrete types; the rest are OpenAI-compatible and
// built by the shared Compatible client.

func init() {
	for _, s := range builtinSpecs() {
		MustRegister(s)
	}
}

// keyVars returns internal/auth's key variables for id.
func keyVars(id auth.Provider) []string { return auth.ProviderEnvVars(id) }

func builtinSpecs() []Spec {
	specs := []Spec{
		{
			ID:             string(auth.ProviderOpenRouter),
			DisplayName:    "OpenRouter",
			KeyEnvVars:     keyVars(auth.ProviderOpenRouter),
			BaseURLEnvVars: []string{EnvOpenRouterBaseURL},
			DefaultBaseURL: openRouterAPIRoot,
			API:            APIOpenAIChat,
			Auth:           AuthBearer,
			ModelsPath:     "/models",
			TokenParam:     TokenParamMaxTokens,
			Reasoning:      ReasoningObject,
			Docs:           "https://openrouter.ai/docs/api-reference/overview",
			New: func(c Config) (Provider, error) {
				// OPENROUTER_BASE_URL is the API root; the struct takes the
				// chat-completions endpoint.
				base := OpenRouterEndpoint(c.BaseURL)
				if c.ExactBaseURL != "" {
					base = c.ExactBaseURL
				}
				return &OpenRouter{
					APIKey:  c.APIKey,
					Referer: c.Referer,
					Title:   c.Title,
					BaseURL: base,
					HTTP:    c.HTTP,
				}, nil
			},
		},
		{
			ID:             string(auth.ProviderAnthropic),
			DisplayName:    "Anthropic",
			KeyEnvVars:     keyVars(auth.ProviderAnthropic),
			BaseURLEnvVars: []string{EnvAnthropicBaseURL},
			DefaultBaseURL: anthropicBaseURL,
			API:            APIAnthropicMessages,
			Auth:           AuthXAPIKey,
			ExtraHeaders:   map[string]string{"anthropic-version": anthropicDefaultVersion},
			ChatPath:       "/v1/messages",
			ModelsPath:     "/v1/models",
			StaticModels:   anthropicFallbackModels,
			TokenParam:     TokenParamMaxTokens,
			Docs:           "https://docs.anthropic.com/en/api/messages",
			New: func(c Config) (Provider, error) {
				return &Anthropic{APIKey: c.APIKey, BaseURL: c.base(), HTTP: c.HTTP,
					NativeToolSearch: toolsearch.NativeAllowed(c.base(), anthropicBaseURL, c.Getenv)}, nil
			},
		},
		{
			ID:             string(auth.ProviderOpenAI),
			DisplayName:    "OpenAI",
			KeyEnvVars:     keyVars(auth.ProviderOpenAI),
			BaseURLEnvVars: []string{EnvOpenAIBaseURL},
			DefaultBaseURL: openAIBaseURL,
			API:            APIOpenAIChat,
			Auth:           AuthBearer,
			ModelsPath:     "/models",
			TokenParam:     TokenParamMaxCompletionTokens,
			Reasoning:      ReasoningEffort,
			Docs:           "https://platform.openai.com/docs/api-reference/chat",
			New: func(c Config) (Provider, error) {
				return &OpenAI{APIKey: c.APIKey, BaseURL: c.base(), HTTP: c.HTTP}, nil
			},
		},
	}
	return append(specs, compatibleSpecs()...)
}

// compat fills the fields every OpenAI-compatible spec shares (bearer auth,
// the Compatible constructor, the key variables from internal/auth).
func compat(s Spec) Spec {
	s.API = APIOpenAIChat
	if s.Auth == "" {
		s.Auth = AuthBearer
	}
	if s.TokenParam == "" {
		s.TokenParam = TokenParamMaxTokens
	}
	if s.Reasoning == "" {
		s.Reasoning = ReasoningNone
	}
	if len(s.KeyEnvVars) == 0 {
		s.KeyEnvVars = keyVars(auth.Provider(s.ID))
	}
	s.New = NewCompatible(s.ID)
	return s
}

// models builds a static fallback list from ids.
func models(ids ...string) []ModelInfo {
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, ModelInfo{ID: id, Name: id})
	}
	return out
}

// compatibleSpecs are the OpenAI-compatible providers, in auto-detection
// order. Every one of them speaks OpenAI Chat Completions with function
// tools; where a provider also offers an Anthropic-compatible Messages API
// (Vercel, Z.ai, MiniMax, Synthetic, Alibaba, Moonshot, OpenCode), Chat
// Completions is used so every provider shares one tool-calling path (the
// one the adapter's tests cover), the key travels as a plain Bearer token,
// and Name() stays the registry id. Base URLs, list endpoints and quirks are
// from each provider's official documentation (Docs), read 2026-10-27;
// Unverified records what the docs did not settle.
func compatibleSpecs() []Spec {
	return []Spec{
		compat(Spec{
			ID:             string(auth.ProviderVercel),
			DisplayName:    "Vercel AI Gateway",
			BaseURLEnvVars: []string{"VERCEL_BASE_URL"},
			DefaultBaseURL: "https://ai-gateway.vercel.sh/v1",
			// GET /v1/models: data[].context_window and max_tokens.
			ModelsPath: "/models",
			// The gateway's reasoning extension: "reasoning":{"effort"}
			// (reasoning_effort is an alias); text streams as
			// delta.reasoning.
			Reasoning:    ReasoningObject,
			StaticModels: models("anthropic/claude-sonnet-5", "anthropic/claude-opus-5", "openai/gpt-6-astra", "google/gemini-3.1-pro-preview"),
			Docs:         "https://vercel.com/docs/ai-gateway/sdks-and-apis/openai-chat-completions",
			Unverified:   "stream_options.include_usage is not documented (sent, as for OpenAI); key format undocumented",
		}),
		compat(Spec{
			ID:             string(auth.ProviderGemini),
			DisplayName:    "Google Gemini",
			BaseURLEnvVars: []string{"GEMINI_BASE_URL"},
			DefaultBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			// OpenAI-shaped list (ids only; no context fields).
			ModelsPath:   "/models",
			Reasoning:    ReasoningEffort, // minimal/low/medium/high (+none on 2.5)
			StaticModels: models("gemini-3.1-pro", "gemini-3.5-flash", "gemini-3.8-flash"),
			Docs:         "https://ai.google.dev/gemini-api/docs/openai",
			Unverified:   "how thoughts are streamed on the OpenAI endpoint is undocumented; Gemini 3 thought signatures are not round-tripped",
		}),
		compat(Spec{
			ID:             string(auth.ProviderZAI),
			DisplayName:    "Z.ai",
			BaseURLEnvVars: []string{"ZAI_BASE_URL"},
			// The general endpoint; Coding Plan keys use
			// https://api.z.ai/api/coding/paas/v4 (set ZAI_BASE_URL).
			DefaultBaseURL: "https://api.z.ai/api/paas/v4",
			Reasoning:      ReasoningEffort,
			// stream_options is not in the spec; usage arrives in the last
			// chunk anyway.
			NoStreamUsage: true,
			StaticModels:  models("glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5", "glm-4.7"),
			Docs:          "https://docs.z.ai/api-reference/llm/chat-completion",
			Unverified:    "no model-list endpoint documented (static list); reasoning_content is not echoed back on tool turns",
		}),
		compat(Spec{
			ID:             string(auth.ProviderMiniMax),
			DisplayName:    "MiniMax",
			BaseURLEnvVars: []string{"MINIMAX_BASE_URL"},
			DefaultBaseURL: "https://api.minimax.io/v1",
			TokenParam:     TokenParamMaxCompletionTokens,
			// reasoning_effort works on M3.1-Flash only; nothing is sent.
			Reasoning: ReasoningNone,
			// Thinking goes to reasoning_content instead of <think> tags in
			// the content.
			ExtraBody:    map[string]any{"reasoning_split": true},
			StaticModels: models("MiniMax-M3", "MiniMax-M3.1-Flash", "MiniMax-M2.7", "MiniMax-M2.5"),
			Docs:         "https://platform.minimax.io/docs/api-reference/text-openai-api",
			Unverified:   "no model-list endpoint documented (static list); MiniMax recommends its Anthropic-compatible API, gocode uses the OpenAI-compatible one",
		}),
		compat(Spec{
			ID:             string(auth.ProviderSynthetic),
			DisplayName:    "Synthetic",
			BaseURLEnvVars: []string{"SYNTHETIC_BASE_URL"},
			DefaultBaseURL: "https://api.synthetic.new/openai/v1",
			ModelsPath:     "/models",
			StaticModels:   models("hf:zai-org/GLM-5.3", "hf:moonshotai/Kimi-K3", "hf:deepseek-ai/DeepSeek-V4.1-Flash", "syn:large:text"),
			Docs:           "https://dev.synthetic.new/docs/api/overview",
			Unverified:     "model-list response fields and reasoning parameters are undocumented",
		}),
		compat(Spec{
			ID:             string(auth.ProviderCerebras),
			DisplayName:    "Cerebras",
			BaseURLEnvVars: []string{"CEREBRAS_BASE_URL"},
			DefaultBaseURL: "https://api.cerebras.ai/v1",
			ModelsPath:     "/models", // ids only
			TokenParam:     TokenParamMaxCompletionTokens,
			Reasoning:      ReasoningEffort,
			StaticModels:   models("gpt-oss-120b", "qwen-3.8-27b", "gemma-4-31b"),
			Docs:           "https://inference-docs.cerebras.ai/api-reference/chat-completions",
			Unverified:     "stream_options.include_usage is not documented (sent)",
		}),
		compat(Spec{
			ID:             string(auth.ProviderIONet),
			DisplayName:    "io.net Intelligence",
			BaseURLEnvVars: []string{"IONET_BASE_URL"},
			DefaultBaseURL: "https://api.intelligence.io.solutions/api/v1",
			ModelsPath:     "/models", // vLLM shape (max_model_len, often null)
			Reasoning:      ReasoningObject,
			StaticModels:   models("moonshotai/Kimi-K2.6", "deepseek-ai/DeepSeek-V4-Pro", "openai/gpt-oss-120b", "meta-llama/Llama-3.3-70B-Instruct"),
			Docs:           "https://io.net/docs/reference/ai-models/get-started-with-io-intelligence-api.md",
			Unverified:     "stream_options.include_usage is not documented (sent)",
		}),
		compat(Spec{
			ID:             string(auth.ProviderAlibabaSG),
			DisplayName:    "Alibaba Cloud Model Studio (Singapore)",
			BaseURLEnvVars: []string{"ALIBABA_SINGAPORE_BASE_URL"},
			// The shared international domain; a workspace domain
			// (https://<WorkspaceId>.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1)
			// or the Coding Plan (https://coding-intl.dashscope.aliyuncs.com/v1)
			// is set with ALIBABA_SINGAPORE_BASE_URL.
			DefaultBaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
			Reasoning:      ReasoningEnableThinking,
			StaticModels:   models("qwen3.8-max", "qwen3.7-plus", "qwen3.6-plus", "qwen-plus", "qwen3.8-flash"),
			Docs:           "https://www.alibabacloud.com/help/en/model-studio/compatibility-of-openai-with-dashscope",
			Unverified:     "no model-list endpoint documented (static list)",
		}),
		compat(Spec{
			ID:             string(auth.ProviderAlibabaUS),
			DisplayName:    "Alibaba Cloud Model Studio (US)",
			BaseURLEnvVars: []string{"ALIBABA_US_BASE_URL"},
			// Virginia; workspace domain:
			// https://<WorkspaceId>.us-east-1.maas.aliyuncs.com/compatible-mode/v1
			DefaultBaseURL: "https://dashscope-us.aliyuncs.com/compatible-mode/v1",
			Reasoning:      ReasoningEnableThinking,
			StaticModels:   models("qwen3.8-max", "qwen3.7-plus", "qwen-plus", "qwen3.8-flash"),
			Docs:           "https://www.alibabacloud.com/help/en/model-studio/base-url",
			Unverified:     "no model-list endpoint documented; the US model catalogue is not published (static list copied from Singapore)",
		}),
		compat(Spec{
			ID:             string(auth.ProviderGroq),
			DisplayName:    "Groq",
			BaseURLEnvVars: []string{"GROQ_BASE_URL"},
			DefaultBaseURL: "https://api.groq.com/openai/v1",
			// data[].context_window (+ max_completion_tokens).
			ModelsPath:   "/models",
			TokenParam:   TokenParamMaxCompletionTokens,
			Reasoning:    ReasoningEffort,
			StaticModels: models("openai/gpt-oss-120b", "openai/gpt-oss-20b", "qwen/qwen3.8-27b"),
			Docs:         "https://console.groq.com/docs/openai",
			Unverified:   "streamed reasoning field name not shown (delta.reasoning assumed, handled)",
		}),
		compat(Spec{
			ID:             string(auth.ProviderAvian),
			DisplayName:    "Avian",
			BaseURLEnvVars: []string{"AVIAN_BASE_URL"},
			DefaultBaseURL: "https://api.avian.io/v1",
			NoStreamUsage:  true, // only model/messages/temperature/max_tokens/stream/tools are documented
			// GET /v1/models: pricing.{input,output,cache_read}_per_million.
			PricesPath:   "/models",
			StaticModels: models("deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4-pro-0813", "z-ai/glm-5.3", "moonshotai/kimi-k3", "minimax/minimax-m3"),
			Docs:         "https://avian.io/docs",
			Unverified:   "no model-list endpoint documented (static list); reasoning parameters undocumented",
		}),
		compat(Spec{
			ID:             string(auth.ProviderOpenCodeZen),
			DisplayName:    "OpenCode Zen",
			BaseURLEnvVars: []string{"OPENCODE_BASE_URL"},
			DefaultBaseURL: "https://opencode.ai/inference/openai/v1",
			// The /inference/v1/models list mixes models served over the
			// Messages / Responses / Google endpoints, which this
			// chat-completions client cannot call, so the picker offers
			// the documented chat-completions models.
			StaticModels: models("glm-5.3", "kimi-k3", "deepseek-v4-pro", "deepseek-v4.1-flash", "qwen3.8-max", "minimax-m3"),
			Docs:         "https://opencode.ai/docs/zen/",
			Unverified:   "Bearer auth shown in one example only; Claude/GPT/Gemini models on Zen use other endpoints and are not reachable here",
		}),
		compat(Spec{
			ID:             string(auth.ProviderOpenCodeGo),
			DisplayName:    "OpenCode Go",
			BaseURLEnvVars: []string{"OPENCODE_GO_BASE_URL"},
			DefaultBaseURL: "https://opencode.ai/inference/go/openai/v1",
			StaticModels:   models("glm-5.3", "kimi-k3.1-pro", "kimi-k3", "deepseek-v4.1-flash", "deepseek-v4-pro"),
			Docs:           "https://opencode.ai/docs/go/",
			Unverified:     "same as OpenCode Zen; shares OPENCODE_API_KEY",
		}),
		compat(Spec{
			ID:             string(auth.ProviderMoonshot),
			DisplayName:    "Moonshot AI (Kimi)",
			BaseURLEnvVars: []string{"MOONSHOT_BASE_URL"},
			// International; China accounts use https://api.moonshot.cn/v1
			// (set MOONSHOT_BASE_URL).
			DefaultBaseURL: "https://api.moonshot.ai/v1",
			ModelsPath:     "/models", // data[].context_length
			TokenParam:     TokenParamMaxCompletionTokens,
			// K3 always thinks and takes only low|high|max; K2.x uses a
			// thinking object. Nothing is sent.
			Reasoning:    ReasoningNone,
			StaticModels: models("kimi-k3", "kimi-k3.1-pro", "kimi-k2.7-code", "kimi-k2.6"),
			Docs:         "https://platform.kimi.ai/docs/api/chat",
			Unverified:   "api.moonshot.cn is not on the international docs",
		}),
		compat(Spec{
			ID:             string(auth.ProviderXAI),
			DisplayName:    "xAI",
			BaseURLEnvVars: []string{"XAI_BASE_URL"},
			DefaultBaseURL: "https://api.x.ai/v1",
			ModelsPath:     "/models", // data[].context_length
			// GET /v1/language-models: per-model prices (pricing.go).
			PricesPath:   "/language-models",
			TokenParam:   TokenParamMaxCompletionTokens,
			Reasoning:    ReasoningEffort,
			StaticModels: models("grok-4.7", "grok-4.6", "grok-build-0.1", "grok-4.3"),
			Docs:         "https://docs.x.ai/developers/rest-api-reference/inference/chat-completions",
			Unverified:   "streamed reasoning on Chat Completions is documented only for the SDK (reasoning_content, handled)",
		}),
		// Hugging Face is last in auto-detection order: HF_TOKEN is often
		// exported for the Hub, not for inference.
		compat(Spec{
			ID:             string(auth.ProviderHuggingFace),
			DisplayName:    "Hugging Face Inference Providers",
			BaseURLEnvVars: []string{"HF_BASE_URL"},
			DefaultBaseURL: "https://router.huggingface.co/v1",
			// data[].providers[].context_length (largest is used).
			ModelsPath:   "/models",
			StaticModels: models("openai/gpt-oss-120b", "deepseek-ai/DeepSeek-V4-Pro", "zai-org/GLM-5.2"),
			Docs:         "https://huggingface.co/docs/inference-providers/index",
			Unverified:   "parameters pass through to the routed backend; reasoning/include_usage support varies per backend",
		}),
	}
}
