// Package openai is the OpenAI Chat Completions backend.
//
// It serves both config types that speak this wire format — `openai` and
// `openai-compatible` — because the difference between them is a base URL,
// not a protocol. That is also why the params it sends stay conservative: an
// aggregator, a gateway or a local vLLM has to understand every field.
//
// Three details are worth checking against the vendor rather than intuition:
//
//   - MAX RETRIES IS ZERO, for the reason llm.go gives. The SDK's two default
//     retries fire on 408, 409, 429, every 5xx and every connection error
//     (internal/requestconfig: shouldRetry) — which is exactly the set the
//     credential pool and the fallback chain need to see FIRST, not after the
//     SDK has spent them against the same dead key.
//   - INPUT TOKENS ARE NOT A SUM HERE. usage.prompt_tokens is already the
//     full prompt count and prompt_tokens_details.cached_tokens is a SUBSET
//     of it. This looks like the opposite of the Anthropic backend and is the
//     same invariant: the contract wants the full prompt count, and the two
//     vendors report it differently. Adding the cache figures here would
//     double-bill every cached round.
//   - A REASONING TRACE HAS NO AGREED FIELD NAME. DeepSeek and several MiniMax
//     hosts send reasoning_content, some send a bare reasoning, and OpenAI's
//     own o-series sends neither through this endpoint. Both names are read
//     off the raw JSON, because the typed struct has neither.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/httpapi"
)

var log = logging.Get("llm.openai")

// KeyEnv is the conventional variable consulted when a config names no key.
const KeyEnv = "OPENAI_API_KEY"

// Defaults. The timeout matches the config layer's defaultLLMTimeoutSeconds.
const (
	DefaultBaseURL     = "https://api.openai.com/v1"
	DefaultTimeout     = 120 * time.Second
	DefaultTemperature = 0.7
)

// Config builds a provider.
type Config struct {
	// Model is the model id this provider serves. Required.
	Model string

	// Name labels errors and logs. Empty takes "openai"; an
	// openai-compatible entry passes its own so a chain's telemetry says
	// which endpoint answered.
	Name string

	// APIKeys are the credentials, in declaration order. Several rotate.
	APIKeys []string

	// BaseURL is the endpoint. Empty takes DefaultBaseURL, and it is
	// ALWAYS sent explicitly: the SDK reads OPENAI_BASE_URL from the
	// process environment, and an ambient one would silently redirect a
	// company's traffic to somewhere its operator never configured.
	BaseURL string

	// Timeout caps one HTTP attempt. Zero takes DefaultTimeout.
	Timeout time.Duration

	// Cooldowns is the credential bench policy. Zero fields take defaults.
	Cooldowns credential.Policy

	// MaxTokens caps the output for a request that names none. Zero sends
	// no cap, which is what an openai-compatible endpoint with an unknown
	// context window needs.
	MaxTokens int

	// Temperature is used for a request that names none (see llm.Request:
	// its zero value cannot be told apart from an unset field). Ignored
	// when Reasoning is set — the reasoning models reject it.
	Temperature float64

	// Reasoning turns on the reasoning-effort budget.
	Reasoning bool

	// ReasoningEffort is the budget selector: low, medium, high, max.
	// Empty takes the endpoint's own default.
	ReasoningEffort string

	// HTTPClient overrides the transport. Nil builds one through
	// httpapi.NewHTTPClient, which the provider then owns and closes.
	HTTPClient option.HTTPClient

	// Clock is the pool's monotonic time source. Nil takes the default.
	Clock credential.Clock

	// LookupEnv resolves KeyEnv. Nil takes os.Getenv.
	LookupEnv func(string) string
}

// Provider is an OpenAI-wire-format backend.
type Provider struct {
	name        string
	model       string
	client      sdk.Client
	pool        *credential.Pool
	maxTokens   int64
	temperature float64
	reasoning   bool
	effort      shared.ReasoningEffort
	owned       *http.Client

	// noStream latches once this endpoint has answered a streaming request
	// without streaming. Atomic: one Provider serves every seat
	// concurrently, and this is written by whichever one discovers it.
	noStream atomic.Bool
}

var _ llm.Provider = (*Provider)(nil)

// New builds a provider.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("openai: Model is required")
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
	name := cfg.Name
	if strings.TrimSpace(name) == "" {
		name = "openai"
	}
	temperature := cfg.Temperature
	if temperature <= 0 {
		temperature = DefaultTemperature
	}

	opts := []option.RequestOption{
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

	return &Provider{
		name:        name,
		model:       cfg.Model,
		client:      sdk.NewClient(opts...),
		pool:        credential.New(credential.Options{Keys: keys, Policy: cfg.Cooldowns, Clock: cfg.Clock}),
		maxTokens:   int64(cfg.MaxTokens),
		temperature: temperature,
		reasoning:   cfg.Reasoning,
		effort:      shared.ReasoningEffort(cfg.ReasoningEffort),
		owned:       owned,
	}, nil
}

// Model is the model id this provider answers as.
func (p *Provider) Model() string { return p.model }

// Pool exposes the credential pool's public state for operator surfaces.
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

// Complete calls chat completions once per live credential until one answers.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	params, err := p.params(req)
	if err != nil {
		return nil, &llm.Error{
			Kind: llm.KindFatal, Provider: p.name, Model: p.model, Err: err,
		}
	}

	streaming := req.Streaming() && !p.noStream.Load()
	if streaming {
		// Ask for usage in the stream. Without it the final chunk carries
		// no token counts, and the budget — which is charged from this
		// completion before the round's tools run — would meter every
		// streamed call as free.
		params.StreamOptions.IncludeUsage = param.NewOpt(true)
	}

	// Per-call locals, never provider fields: ONE Provider serves every
	// concurrent caller, so a field here would be a data race between two
	// seats streaming at once.
	attempt := 0
	var streamed string
	resp, err := credential.Rotate(ctx, p.pool,
		credential.Identity{Provider: p.name, Model: p.model},
		p.classify,
		func(key string) (*sdk.ChatCompletion, error) {
			// The key goes on the REQUEST, not the client: one client
			// serves the whole pool, and a per-request option is applied
			// after the client's own, so it wins over the OPENAI_API_KEY
			// the SDK loads from the environment at construction.
			opt := option.WithAPIKey(key)
			if !streaming {
				return p.client.Chat.Completions.New(ctx, params, opt)
			}
			// A ROTATION IS A RESTART. The previous key may have died
			// after streaming half an answer, and without saying so the
			// consumer would append this attempt to that one and show two
			// half-answers as one paragraph.
			attempt++
			if attempt > 1 {
				req.Send(llm.Delta{Restart: true, Model: p.model})
			}
			out, reasoning, sErr := p.streamOnce(ctx, req, params, opt)
			if errors.Is(sErr, errNoStream) {
				// This endpoint accepted `stream: true` and answered
				// without streaming. "OpenAI-compatible" is a de-facto
				// standard with real variance — a local shim or a proxy
				// may implement the unary route only — so the capability
				// is NEGOTIATED rather than assumed, or pushed onto the
				// operator as a config field they would have to know to
				// set. Latched: one call per process, never repeated.
				p.noStream.Store(true)
				log.WarnContext(ctx, "provider_does_not_stream",
					"provider", p.name, "model", p.model,
					"hint", "the endpoint answered a streaming request without streaming; "+
						"live phase text will appear per round instead of as it is written")
				plain := params
				plain.StreamOptions = sdk.ChatCompletionStreamOptionsParam{}
				return p.client.Chat.Completions.New(ctx, plain, opt)
			}
			streamed = reasoning
			return out, sErr
		})
	if err != nil {
		return nil, err
	}
	out, err := p.completion(resp)
	if err != nil {
		return nil, err
	}
	// The assembled message carries no `reasoning_content` — it is not in
	// the schema, so the SDK's accumulator does not keep it — and the
	// streamed text is the only record of it.
	if streamed != "" {
		out.ReasoningContent = streamed
	}
	return out, nil
}

// streamOnce runs one streamed attempt, forwarding fragments as they land and
// returning the same accumulated shape the unary path returns.
//
// The SDK's accumulator rebuilds exactly the [sdk.ChatCompletion] that
// [Provider.completion] already consumes, so the two paths converge on one
// interpretation of a response rather than growing a second.
//
// REASONING IS ACCUMULATED HERE rather than left to the accumulator, because
// `reasoning_content` is not in the OpenAI schema — it is the convention the
// reasoning hosts adopted — so the accumulator neither knows nor keeps it, and
// the assembled message's raw JSON has no trace of it. See [reasoningText].
func (p *Provider) streamOnce(
	ctx context.Context, req llm.Request,
	params sdk.ChatCompletionNewParams, opt option.RequestOption,
) (*sdk.ChatCompletion, string, error) {
	stream := p.client.Chat.Completions.NewStreaming(ctx, params, opt)
	defer func() { _ = stream.Close() }()

	var acc sdk.ChatCompletionAccumulator
	var reasoning strings.Builder
	events := 0
	for stream.Next() {
		events++
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if len(chunk.Choices) == 0 {
			// A usage-only or keep-alive chunk. Accumulated, not shown.
			continue
		}
		delta := chunk.Choices[0].Delta
		thought := reasoningText(delta.RawJSON())
		reasoning.WriteString(thought)
		req.Send(llm.Delta{Content: delta.Content, Reasoning: thought})
	}
	if err := stream.Err(); err != nil {
		// Classified by the caller exactly as a unary failure is. A stream
		// that dies MID-BODY is a failure of the call, not a short answer:
		// returning what accumulated would hand the loop a truncated
		// response as though the model had finished.
		return nil, "", err
	}
	if events == 0 {
		// Not a failure of the call — the endpoint simply does not do
		// this. Distinguished so the caller can fall back rather than fail
		// a phase over a capability.
		return nil, "", errNoStream
	}
	out := acc.ChatCompletion
	return &out, reasoning.String(), nil
}

// errNoStream reports an endpoint that accepted a streaming request and
// answered without streaming.
var errNoStream = errors.New("endpoint did not stream")

// classify turns an SDK failure into the contract's error.
func (p *Provider) classify(err error) *llm.Error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		var header http.Header
		if apiErr.Response != nil {
			header = apiErr.Response.Header
		}
		return httpapi.FromStatus(err, p.name, p.model, apiErr.StatusCode, header)
	}
	return httpapi.FromTransport(err, p.name, p.model)
}

func (p *Provider) params(req llm.Request) (sdk.ChatCompletionNewParams, error) {
	messages, err := formatMessages(req.Messages)
	if err != nil {
		return sdk.ChatCompletionNewParams{}, err
	}
	params := sdk.ChatCompletionNewParams{
		Model:    p.model,
		Messages: messages,
	}

	maxTokens := p.maxTokens
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}

	if p.reasoning {
		if p.effort != "" {
			params.ReasoningEffort = p.effort
		}
		// The reasoning models reject max_tokens outright and reject any
		// temperature but their own default. Sending max_tokens here too
		// 400s every o-series call the moment a caller sets a cap.
		if maxTokens > 0 {
			params.MaxCompletionTokens = param.NewOpt(maxTokens)
		}
	} else {
		// TemperatureOr, not a zero test: an explicit 0.0 is a real request
		// — a judge asking for a reproducible answer — and it must reach
		// the wire, while a request that named nothing takes the
		// provider's configured default.
		params.Temperature = param.NewOpt(req.TemperatureOr(p.temperature))
		// max_tokens rather than max_completion_tokens: the compatible
		// endpoints this backend also serves are years behind the rename.
		if maxTokens > 0 {
			params.MaxTokens = param.NewOpt(maxTokens)
		}
	}

	if len(req.Tools) > 0 {
		params.Tools = formatTools(req.Tools)
		if choice, ok := toolChoice(req.ToolChoice); ok {
			// OfAuto is the SDK's name for the BARE STRING variant of the
			// union, which carries "auto", "required" or "none" — not a
			// field that means auto. The alternatives are the
			// named-tool forms, which nothing here uses.
			params.ToolChoice = sdk.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: param.NewOpt(choice),
			}
		}
	}
	return params, nil
}

func formatMessages(messages []llm.Message) ([]sdk.ChatCompletionMessageParamUnion, error) {
	out := make([]sdk.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case llm.RoleSystem:
			msg := sdk.ChatCompletionSystemMessageParam{}
			msg.Content.OfString = param.NewOpt(m.Content)
			if m.Name != "" {
				msg.Name = param.NewOpt(m.Name)
			}
			out = append(out, sdk.ChatCompletionMessageParamUnion{OfSystem: &msg})

		case llm.RoleTool:
			msg := sdk.ChatCompletionToolMessageParam{ToolCallID: m.ToolCallID}
			msg.Content.OfString = param.NewOpt(m.Content)
			out = append(out, sdk.ChatCompletionMessageParamUnion{OfTool: &msg})

		case llm.RoleAssistant:
			msg := sdk.ChatCompletionAssistantMessageParam{}
			if m.Content != "" {
				msg.Content.OfString = param.NewOpt(m.Content)
			}
			if m.Name != "" {
				msg.Name = param.NewOpt(m.Name)
			}
			// ReasoningContent and ThinkingBlocks are deliberately NOT
			// sent back. This endpoint has no field for either, and the
			// vendors that emit reasoning_content reject it on input.
			for _, tc := range m.ToolCalls {
				args, err := httpapi.EncodeArgs(tc.Arguments, tc.Name)
				if err != nil {
					return nil, err
				}
				msg.ToolCalls = append(msg.ToolCalls,
					sdk.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &sdk.ChatCompletionMessageFunctionToolCallParam{
							ID: tc.ID,
							Function: sdk.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      tc.Name,
								Arguments: args,
							},
						},
					})
			}
			out = append(out, sdk.ChatCompletionMessageParamUnion{OfAssistant: &msg})

		default:
			msg := sdk.ChatCompletionUserMessageParam{}
			msg.Content.OfString = param.NewOpt(m.Content)
			if m.Name != "" {
				msg.Name = param.NewOpt(m.Name)
			}
			out = append(out, sdk.ChatCompletionMessageParamUnion{OfUser: &msg})
		}
	}
	return out, nil
}

func formatTools(tools []llm.ToolDef) []sdk.ChatCompletionToolUnionParam {
	out := make([]sdk.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		fn := shared.FunctionDefinitionParam{
			Name:       t.Name,
			Parameters: toolSchema(t.Parameters),
		}
		if t.Description != "" {
			fn.Description = param.NewOpt(t.Description)
		}
		out = append(out, sdk.ChatCompletionToolUnionParam{
			OfFunction: &sdk.ChatCompletionFunctionToolParam{Function: fn},
		})
	}
	return out
}

// toolSchema passes a JSON Schema object through, normalising the empty case.
//
// OpenAI treats an absent `parameters` as "no arguments", but several
// compatible endpoints reject a function whose schema is missing or has no
// declared type, so the empty case is spelled out rather than omitted.
func toolSchema(params map[string]any) shared.FunctionParameters {
	if len(params) == 0 {
		return shared.FunctionParameters{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	return shared.FunctionParameters(params)
}

// toolChoice maps the contract's values onto the wire strings. The second
// return is false when nothing should be sent.
func toolChoice(choice string) (string, bool) {
	switch choice {
	case "", "auto":
		return "auto", true
	case "required", "none":
		return choice, true
	default:
		log.Warn("unknown_tool_choice", "value", choice)
		return "", false
	}
}

func (p *Provider) completion(resp *sdk.ChatCompletion) (*llm.Completion, error) {
	if len(resp.Choices) == 0 {
		// Returning an empty completion with finish_reason "error" here
		// is the tempting shape, and the tool loop reads it as a clean
		// finish: the phase produces nothing and reports success. An
		// endpoint that returned
		// no choice has malfunctioned, so it is a server failure — the
		// chain may still get an answer from another model, and no
		// credential is benched for it.
		return nil, &llm.Error{
			Kind: llm.KindServer, Provider: p.name, Model: p.model,
			Err: errors.New("response carried no choices"),
		}
	}

	choice := resp.Choices[0]
	out := &llm.Completion{
		// The CONFIGURED model id, not the one the response echoes: an
		// alias resolving to a dated snapshot would re-key the per-model
		// breakdown the day the alias moves.
		Model:            p.model,
		Content:          choice.Message.Content,
		ReasoningContent: reasoningText(choice.Message.RawJSON()),
		FinishReason:     choice.FinishReason,
	}
	if out.FinishReason == "" {
		out.FinishReason = "stop"
	}

	for _, tc := range choice.Message.ToolCalls {
		if tc.Type == "custom" {
			// A custom tool call carries free text, not JSON arguments,
			// and nothing in this engine registers one. Skipping it beats
			// inventing an empty argument map for a call the surface
			// cannot run.
			log.Warn("custom_tool_call_ignored", "model", p.model, "id", tc.ID)
			continue
		}
		out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: httpapi.DecodeArgs([]byte(tc.Function.Arguments), tc.Function.Name),
		})
	}

	// See the package doc: prompt_tokens is ALREADY the full prompt count
	// and the cache figures are a breakdown of it, not an addition.
	out.InputTokens = int(resp.Usage.PromptTokens)
	out.OutputTokens = int(resp.Usage.CompletionTokens)
	out.CacheRead = int(resp.Usage.PromptTokensDetails.CachedTokens)
	out.CacheWrite = int(resp.Usage.PromptTokensDetails.CacheWriteTokens)

	log.Info("llm_complete",
		"model", p.model,
		"input_tokens", out.InputTokens,
		"output_tokens", out.OutputTokens,
		"cache_read_tokens", out.CacheRead,
		"tool_calls", len(out.ToolCalls),
		"finish_reason", out.FinishReason)
	return out, nil
}

// reasoningText pulls a reasoning trace off the raw message JSON.
//
// reasoning_content is preferred when both are present: it is the older and
// more widespread convention, and a host that sends both sends the same text
// twice. A `reasoning` that is an object rather than a string — some hosts
// send a structured summary — yields nothing rather than an error, because a
// missing reasoning trace must never fail a call.
func reasoningText(raw string) string {
	if raw == "" {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return ""
	}
	for _, key := range []string{"reasoning_content", "reasoning"} {
		value, ok := fields[key]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) == nil && text != "" {
			return text
		}
	}
	return ""
}

// String is the provider's identity in a log line.
func (p *Provider) String() string { return fmt.Sprintf("%s/%s", p.name, p.model) }
