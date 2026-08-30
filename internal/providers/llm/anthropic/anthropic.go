// Package anthropic is the Anthropic Messages backend.
//
// It implements [llm.Provider] over the official anthropic-sdk-go, and it is
// deliberately thin: it translates the neutral request into Anthropic's wire
// shape, makes exactly one HTTP attempt per credential, and translates the
// answer back. Everything about what a failure MEANS is the contract's
// (llm.KindForStatus), everything about which credential to use next is the
// pool's, and everything about which model to try next is the chain's.
//
// Two details here are the ones worth checking against the vendor rather than
// against intuition:
//
//   - MAX RETRIES IS ZERO. The SDK retries twice by default, and its retry
//     predicate (internal/requestconfig: shouldRetry) fires on exactly what
//     the layers above need to see first — 408, 409, 429, every 5xx and every
//     connection error. Left alone it would burn both retries against a key
//     that is out of quota and report the last failure, so the pool would
//     bench the key three round trips late or, on a connection error, learn
//     nothing about it at all.
//   - INPUT TOKENS ARE A SUM. Anthropic's usage.input_tokens counts only the
//     UNCACHED remainder; the vendor's own field doc says "Total input tokens
//     in a request is the summation of input_tokens, cache_creation_input_
//     tokens and cache_read_input_tokens". The contract requires InputTokens
//     to be the full prompt count, so all three are added. Getting this wrong
//     under-bills every cached round, which is most of them.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/httpapi"
)

var log = logging.Get("llm.anthropic")

// providerName labels errors and log lines. It is the config's type name.
const providerName = "anthropic"

// KeyEnv is the conventional variable consulted when a config names no key,
// so a credential already exported in a shell works with no YAML change.
// internal/config/providers.go documents the fallback; this is where it
// happens.
const KeyEnv = "ANTHROPIC_API_KEY"

// Defaults. The timeout matches the config layer's defaultLLMTimeoutSeconds.
const (
	DefaultBaseURL         = "https://api.anthropic.com"
	DefaultTimeout         = 120 * time.Second
	DefaultMaxTokens       = 4096
	DefaultThinkingBudget  = 10000
	DefaultTemperature     = 0.7
	minThinkingBudget      = 1024
	emptyToolResultContent = "(no output)"
)

// Config builds a provider.
type Config struct {
	// Model is the model id this provider serves. Required.
	Model string

	// APIKeys are the credentials, in declaration order. Several rotate.
	// Empty falls back to KeyEnv; failing that the provider still builds
	// and every call comes back a clean 401, which is a far easier thing
	// to diagnose than a constructor that refused to exist.
	APIKeys []string

	// BaseURL is the endpoint. Empty takes DefaultBaseURL. It is ALWAYS
	// sent explicitly, so an ambient ANTHROPIC_BASE_URL cannot silently
	// redirect a company's traffic.
	BaseURL string

	// Timeout caps one HTTP attempt. Zero takes DefaultTimeout.
	Timeout time.Duration

	// Cooldowns is the credential bench policy. Zero fields take defaults.
	Cooldowns credential.Policy

	// MaxTokens is the output cap for a request that names none. Zero takes
	// DefaultMaxTokens.
	MaxTokens int

	// Temperature is used for a request that names none (see llm.Request:
	// its zero value cannot be told apart from an unset field).
	Temperature float64

	// Reasoning turns on extended thinking.
	Reasoning bool

	// ThinkingBudget is the thinking allowance. Zero takes
	// DefaultThinkingBudget. Anthropic requires at least 1024 and strictly
	// less than max_tokens; both are enforced here rather than discovered
	// as a 400 on the first turn of a live company.
	ThinkingBudget int

	// HTTPClient overrides the transport. Nil builds one through
	// httpapi.NewHTTPClient, which the provider then owns and closes.
	HTTPClient option.HTTPClient

	// Clock is the pool's monotonic time source. Nil takes the default.
	Clock credential.Clock

	// LookupEnv resolves KeyEnv. Nil takes os.Getenv; the engine passes a
	// secret-store-aware resolver so a rotated secret beats a stale shell.
	LookupEnv func(string) string
}

// Provider is an Anthropic Messages backend.
type Provider struct {
	model       string
	client      sdk.Client
	pool        *credential.Pool
	maxTokens   int64
	temperature float64
	reasoning   bool
	budget      int64
	owned       *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New builds a provider.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("anthropic: Model is required")
	}

	keys := cfg.APIKeys
	if len(keys) == 0 {
		lookup := cfg.LookupEnv
		if lookup == nil {
			lookup = os.Getenv
		}
		if key := strings.TrimSpace(lookup(KeyEnv)); key != "" {
			keys = []string{key}
		}
	}

	baseURL := cfg.BaseURL
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	opts := []option.RequestOption{
		// The SDK otherwise loads ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN,
		// ANTHROPIC_BASE_URL and a profile file of its own. Every one of
		// those is a second credential source the pool does not know about
		// — an ambient auth token would answer every call while the pool
		// dutifully rotated keys nothing was using.
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(baseURL),
		option.WithRequestTimeout(timeout),
		// See the package doc. Not negotiable.
		option.WithMaxRetries(0),
	}
	// A transport the caller supplied stays the caller's to close; one built
	// here is this provider's, and Close reclaims its idle sockets.
	var owned *http.Client
	if cfg.HTTPClient == nil {
		owned = httpapi.NewHTTPClient()
		opts = append(opts, option.WithHTTPClient(owned))
	} else {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}

	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	budget := cfg.ThinkingBudget
	if budget <= 0 {
		budget = DefaultThinkingBudget
	}
	if cfg.Reasoning && budget < minThinkingBudget {
		log.Warn("thinking_budget_raised",
			"model", cfg.Model, "configured", budget, "applied", minThinkingBudget)
		budget = minThinkingBudget
	}
	temperature := cfg.Temperature
	if temperature <= 0 {
		temperature = DefaultTemperature
	}

	return &Provider{
		model:       cfg.Model,
		client:      sdk.NewClient(opts...),
		pool:        credential.New(credential.Options{Keys: keys, Policy: cfg.Cooldowns, Clock: cfg.Clock}),
		maxTokens:   int64(maxTokens),
		temperature: temperature,
		reasoning:   cfg.Reasoning,
		budget:      int64(budget),
		owned:       owned,
	}, nil
}

// Model is the model id this provider answers as.
func (p *Provider) Model() string { return p.model }

// Pool exposes the credential pool's public state for operator surfaces. It
// never yields a key.
func (p *Provider) Pool() *credential.Pool { return p.pool }

// Close releases the idle connections this provider opened. A transport the
// caller supplied is left alone — it may still be serving somebody else.
//
// Nothing breaks if it is never called: the transport reaps its own idle
// connections. It exists so a config swap that replaces a provider does not
// leave the old one's sockets open until that timer fires.
func (p *Provider) Close() {
	if p.owned != nil {
		p.owned.CloseIdleConnections()
	}
}

// Complete calls the Messages API once per live credential until one answers.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	params, err := p.params(req)
	if err != nil {
		return nil, &llm.Error{
			Kind: llm.KindFatal, Provider: providerName, Model: p.model, Err: err,
		}
	}

	msg, err := credential.Rotate(ctx, p.pool,
		credential.Identity{Provider: providerName, Model: p.model},
		p.classify,
		func(key string) (*sdk.Message, error) {
			return p.client.Messages.New(ctx, params, option.WithAPIKey(key))
		})
	if err != nil {
		return nil, err
	}
	return p.completion(msg), nil
}

// classify turns an SDK failure into the contract's error. The errors.As on
// the SDK's own type is the only part a backend can own; see httpapi.
func (p *Provider) classify(err error) *llm.Error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		var header http.Header
		if apiErr.Response != nil {
			header = apiErr.Response.Header
		}
		return httpapi.FromStatus(err, providerName, p.model, apiErr.StatusCode, header)
	}
	return httpapi.FromTransport(err, providerName, p.model)
}

// params renders the neutral request into Anthropic's wire shape.
func (p *Provider) params(req llm.Request) (sdk.MessageNewParams, error) {
	system, rest := splitSystem(req.Messages)
	messages, err := formatMessages(rest)
	if err != nil {
		return sdk.MessageNewParams{}, err
	}
	if len(messages) == 0 {
		// Anthropic requires a non-empty messages array. Refusing here
		// names the actual problem; the API's 400 names a field.
		return sdk.MessageNewParams{}, errors.New(
			"anthropic: request carries no non-system message with content")
	}

	maxTokens := p.maxTokens
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}
	params := sdk.MessageNewParams{
		Model:    p.model,
		Messages: messages,
	}

	if p.reasoning {
		// Anthropic requires max_tokens strictly greater than the thinking
		// budget, and rejects any temperature but 1 while thinking.
		if maxTokens <= p.budget {
			maxTokens = p.budget + maxTokens
		}
		params.Thinking = sdk.ThinkingConfigParamUnion{
			OfEnabled: &sdk.ThinkingConfigEnabledParam{BudgetTokens: p.budget},
		}
		params.Temperature = param.NewOpt(1.0)
	} else {
		// TemperatureOr, not a zero test: an explicit 0.0 is a real request
		// — a judge asking for a reproducible answer — and it must reach
		// the wire, while a request that named nothing takes the
		// provider's configured default.
		params.Temperature = param.NewOpt(req.TemperatureOr(p.temperature))
	}
	params.MaxTokens = maxTokens

	if system != "" {
		params.System = systemBlocks(system)
	}
	if len(req.Tools) > 0 {
		params.Tools = formatTools(req.Tools)
		if choice, ok := toolChoice(req.ToolChoice); ok {
			params.ToolChoice = choice
		}
	}
	return params, nil
}

// cacheBreakpoint marks the (tools + system) prefix cacheable.
//
// That prefix is the large static part of every Plan, Execute and Review
// round: without the breakpoint it is re-billed in full on every round of
// every turn. Anthropic silently ignores a breakpoint on a prefix below the
// cacheable minimum, so setting it is always safe.
func cacheBreakpoint() sdk.CacheControlEphemeralParam {
	return sdk.NewCacheControlEphemeralParam()
}

func systemBlocks(system string) []sdk.TextBlockParam {
	return []sdk.TextBlockParam{{Text: system, CacheControl: cacheBreakpoint()}}
}

// splitSystem lifts system turns out of the conversation: Anthropic carries
// them in a top-level parameter, not as a role.
func splitSystem(messages []llm.Message) (string, []llm.Message) {
	var system []string
	rest := make([]llm.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == llm.RoleSystem {
			if text := strings.TrimSpace(m.Content); text != "" {
				system = append(system, text)
			}
			continue
		}
		rest = append(rest, m)
	}
	return strings.Join(system, "\n"), rest
}

func formatMessages(messages []llm.Message) ([]sdk.MessageParam, error) {
	out := make([]sdk.MessageParam, 0, len(messages))
	for _, m := range messages {
		switch {
		case m.Role == llm.RoleTool:
			content := m.Content
			if strings.TrimSpace(content) == "" {
				// Anthropic rejects an empty content block. A tool that
				// produced nothing is a real outcome, so it is rendered
				// as one rather than dropped: dropping it would leave the
				// preceding tool_use unanswered, which is a 400 about a
				// different message entirely.
				content = emptyToolResultContent
			}
			out = append(out, sdk.NewUserMessage(
				sdk.NewToolResultBlock(m.ToolCallID, content, false)))

		case len(m.ToolCalls) > 0 || len(m.ThinkingBlocks) > 0:
			blocks := make([]sdk.ContentBlockParamUnion, 0,
				len(m.ThinkingBlocks)+len(m.ToolCalls)+1)
			// Thinking blocks go back FIRST and verbatim, signature
			// included: Anthropic validates them against the turn they
			// belong to and rejects a conversation that reordered or
			// paraphrased them.
			for _, tb := range m.ThinkingBlocks {
				if tb.Type == "redacted_thinking" {
					blocks = append(blocks, sdk.NewRedactedThinkingBlock(tb.Data))
					continue
				}
				blocks = append(blocks, sdk.NewThinkingBlock(tb.Signature, tb.Thinking))
			}
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, sdk.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if args == nil {
					args = map[string]any{}
				}
				// Anthropic carries a tool's input as JSON the SDK
				// marshals at request time, so an argument that cannot
				// be JSON — a NaN, an infinity — surfaces from inside
				// the encoder as an error naming no tool at all.
				// Checking here costs one marshal of a small map and
				// buys the same message the OpenAI backend gives, which
				// has to pre-encode anyway.
				if _, err := httpapi.EncodeArgs(args, tc.Name); err != nil {
					return nil, err
				}
				blocks = append(blocks, sdk.NewToolUseBlock(tc.ID, args, tc.Name))
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, sdk.NewAssistantMessage(blocks...))

		case strings.TrimSpace(m.Content) == "":
			// A turn with no content and no tool call carries nothing.
			// Sending it is a guaranteed 400 on an empty text block, so
			// it is dropped — losing an empty message loses nothing.
			continue

		case m.Role == llm.RoleAssistant:
			out = append(out, sdk.NewAssistantMessage(sdk.NewTextBlock(m.Content)))

		default:
			out = append(out, sdk.NewUserMessage(sdk.NewTextBlock(m.Content)))
		}
	}
	return out, nil
}

// formatTools renders the tool array, with a cache breakpoint on the last
// entry so the whole static definition block is cached alongside the system
// prompt.
func formatTools(tools []llm.ToolDef) []sdk.ToolUnionParam {
	out := make([]sdk.ToolUnionParam, 0, len(tools))
	for i, t := range tools {
		tool := &sdk.ToolParam{
			Name:        t.Name,
			InputSchema: toolSchema(t.Parameters),
		}
		if t.Description != "" {
			tool.Description = param.NewOpt(t.Description)
		}
		if i == len(tools)-1 {
			tool.CacheControl = cacheBreakpoint()
		}
		out = append(out, sdk.ToolUnionParam{OfTool: tool})
	}
	return out
}

// toolSchema maps a JSON Schema object onto the SDK's split representation.
//
// The SDK models properties and required as named fields and everything else
// as extras, so a schema carrying $defs, additionalProperties or a description
// keeps them — dropping the remainder would silently weaken the contract the
// tool advertises.
func toolSchema(params map[string]any) sdk.ToolInputSchemaParam {
	schema := sdk.ToolInputSchemaParam{}
	if len(params) == 0 {
		schema.Properties = map[string]any{}
		return schema
	}
	var extras map[string]any
	for key, value := range params {
		switch key {
		case "properties":
			schema.Properties = value
		case "required":
			schema.Required = stringList(value)
		case "type":
			// The SDK pins this to "object", which is the only value a
			// tool input schema may take.
		default:
			if extras == nil {
				extras = map[string]any{}
			}
			extras[key] = value
		}
	}
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}
	schema.ExtraFields = extras
	return schema
}

// stringList coerces a JSON-decoded list into []string. A non-list, or an
// element that is not a string, is not a required-field name and is skipped.
func stringList(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// toolChoice maps the contract's four values onto Anthropic's union. The
// second return is false when nothing should be sent.
func toolChoice(choice string) (sdk.ToolChoiceUnionParam, bool) {
	switch choice {
	case "", "auto":
		return sdk.ToolChoiceUnionParam{OfAuto: &sdk.ToolChoiceAutoParam{}}, true
	case "required":
		// Anthropic spells "you must call one of these" as `any`.
		return sdk.ToolChoiceUnionParam{OfAny: &sdk.ToolChoiceAnyParam{}}, true
	case "none":
		return sdk.ToolChoiceUnionParam{OfNone: &sdk.ToolChoiceNoneParam{}}, true
	default:
		// An unrecognised value is the caller's mistake, and guessing at
		// it would be worse than letting the model decide.
		log.Warn("unknown_tool_choice", "value", choice)
		return sdk.ToolChoiceUnionParam{}, false
	}
}

// completion translates the response.
func (p *Provider) completion(msg *sdk.Message) *llm.Completion {
	// The CONFIGURED model id, not the one the response echoes. A vendor
	// alias resolving to a dated snapshot would otherwise re-key the
	// per-model breakdown the day the alias moves, splitting one model's
	// spend across two names that nothing in the config mentions.
	out := &llm.Completion{Model: p.model, FinishReason: string(msg.StopReason)}
	if out.FinishReason == "" {
		out.FinishReason = "end_turn"
	}

	var content, reasoning strings.Builder
	for _, block := range msg.Content {
		switch block.Type {
		case "thinking":
			reasoning.WriteString(block.Thinking)
			out.ThinkingBlocks = append(out.ThinkingBlocks, llm.ThinkingBlock{
				Type:      "thinking",
				Thinking:  block.Thinking,
				Signature: block.Signature,
			})
		case "redacted_thinking":
			// Carried opaquely and handed back verbatim; there is nothing
			// readable in it to add to the reasoning prose.
			out.ThinkingBlocks = append(out.ThinkingBlocks, llm.ThinkingBlock{
				Type: "redacted_thinking",
				Data: block.Data,
			})
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: httpapi.DecodeArgs(block.Input, block.Name),
			})
		}
	}
	out.Content = content.String()
	out.ReasoningContent = reasoning.String()

	// See the package doc: input_tokens is the uncached remainder, so the
	// full prompt count — the figure a budget is charged — is the sum.
	out.CacheRead = int(msg.Usage.CacheReadInputTokens)
	out.CacheWrite = int(msg.Usage.CacheCreationInputTokens)
	out.InputTokens = int(msg.Usage.InputTokens) + out.CacheRead + out.CacheWrite
	out.OutputTokens = int(msg.Usage.OutputTokens)

	log.Info("llm_complete",
		"model", p.model,
		"input_tokens", out.InputTokens,
		"output_tokens", out.OutputTokens,
		"cache_read_tokens", out.CacheRead,
		"cache_write_tokens", out.CacheWrite,
		"tool_calls", len(out.ToolCalls),
		"stop_reason", out.FinishReason)
	return out
}

// String is the provider's identity in a log line.
func (p *Provider) String() string { return fmt.Sprintf("%s/%s", providerName, p.model) }
