// Package openai is the OpenAI Chat Completions backend.
//
// It serves both config types that speak this wire format — `openai` and
// `openai-compatible` — because the difference between them is a base URL,
// not a protocol. That is also why the params it sends stay conservative: an
// aggregator, a gateway or a local vLLM has to understand every field.
//
// Four details are worth checking against the vendor rather than intuition:
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
//   - NOTHING IS READ FROM THE PROCESS ENVIRONMENT. The SDK loads seven
//     OPENAI_* variables at construction — an admin key and organization and
//     project headers among them — and sends them to whatever endpoint an
//     entry names; [WithoutAmbientEnvironment] undoes each, and the
//     embeddings provider builds its client with the same options.
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
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/httpapi"
)

var log = logging.Get("llm.openai")

// wireName is this backend's type: the default label for its errors, and what
// a completion records as its [llm.Origin.Provider] whatever the label.
const wireName = "openai"

// Defaults. The timeout matches the config layer's defaultLLMTimeoutSeconds,
// which is what the engine passes; this one serves a Config built without it.
//
// There is deliberately NO default temperature. The engine's phases never
// chose one, so a provider-side default was a number nobody picked, sent on
// every round in place of the vendor's own; a call that wants one names it on
// the request.
const (
	DefaultBaseURL = "https://api.openai.com/v1"
	DefaultTimeout = 600 * time.Second
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
	// They are THE WHOLE BAG: nothing here reads a variable. Which key an
	// entry that names none runs on is a configuration rule
	// (config.LLMProvider.Keys), decided where the document and the secret
	// store are both in reach.
	APIKeys []string

	// BaseURL is the endpoint. Empty takes DefaultBaseURL, and it is
	// ALWAYS sent explicitly: the SDK reads OPENAI_BASE_URL from the
	// process environment, and an ambient one would silently redirect a
	// company's traffic to somewhere its operator never configured.
	BaseURL string

	// Timeout bounds one HTTP attempt. Zero takes DefaultTimeout. A unary
	// call is bounded in total; a streamed one by its SILENCE, never its
	// length — see [httpapi.IdleWatchdog] for why the two differ.
	Timeout time.Duration

	// Cooldowns is the credential bench policy. Zero fields take defaults.
	Cooldowns credential.Policy

	// MaxTokens caps the output for a request that names none. Zero sends
	// no cap, which is what an openai-compatible endpoint with an unknown
	// context window needs.
	MaxTokens int

	// Reasoning turns on the reasoning-effort budget.
	Reasoning bool

	// ReasoningEffort is the budget selector: low, medium, high, max.
	// Empty takes the endpoint's own default. A request's
	// [llm.Request.Effort] lowers it for that call, and never raises it.
	ReasoningEffort string

	// HTTPClient overrides the transport. Nil builds one through
	// httpapi.NewHTTPClient, which the provider then owns and closes.
	HTTPClient option.HTTPClient

	// Clock is the pool's monotonic time source. Nil takes the default.
	Clock credential.Clock
}

// Provider is an OpenAI-wire-format backend.
type Provider struct {
	name      string
	model     string
	client    sdk.Client
	pool      *credential.Pool
	maxTokens int64
	reasoning bool
	effort    shared.ReasoningEffort
	// timeout is a unary call's total bound and a streamed call's idle one.
	timeout time.Duration

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
		name = wireName
	}

	// NOTHING FROM THE PROCESS ENVIRONMENT, first — see
	// [WithoutAmbientEnvironment]. The key is set per request, after these.
	opts := append(WithoutAmbientEnvironment(),
		option.WithBaseURL(baseURL),
		// A unary call's total bound. A streamed attempt replaces it with
		// none and bounds its silence instead (see [Provider.streamOnce]).
		option.WithRequestTimeout(timeout),
		// See the package doc. Not negotiable.
		option.WithMaxRetries(0),
	)
	// A transport the caller supplied is used as given; otherwise the
	// engine's shared one. NOTHING HERE OWNS IT — see [httpapi.NewHTTPClient]
	// for why this provider has no Close.
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	} else {
		opts = append(opts, option.WithHTTPClient(httpapi.NewHTTPClient()))
	}

	return &Provider{
		name:      name,
		model:     cfg.Model,
		client:    sdk.NewClient(opts...),
		pool:      credential.New(credential.Options{Keys: keys, Policy: cfg.Cooldowns, Clock: cfg.Clock}),
		maxTokens: int64(cfg.MaxTokens),
		reasoning: cfg.Reasoning,
		effort:    shared.ReasoningEffort(cfg.ReasoningEffort),
		timeout:   timeout,
	}, nil
}

// Model is the model id this provider answers as.
func (p *Provider) Model() string { return p.model }

// Pool exposes the credential pool's public state for operator surfaces.
func (p *Provider) Pool() *credential.Pool { return p.pool }

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
// BOUNDED BY SILENCE, NOT LENGTH: the client's per-attempt timeout is lifted
// for this request and an [httpapi.IdleWatchdog] of the same duration ends it
// only when nothing has arrived for that long, because the per-attempt deadline
// would cover the whole body and kill a long reasoning round half-way through.
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
	ctx, watch := httpapi.WatchIdle(ctx, p.timeout)
	defer watch.Stop()
	stream := p.client.Chat.Completions.NewStreaming(ctx, params, opt,
		option.WithRequestTimeout(0), option.WithMiddleware(watch.Middleware))
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
		// response as though the model had finished. A stream the watchdog
		// ended is reported as the stall it was.
		return nil, "", watch.Err(err)
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
	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		return &llm.Error{Kind: streamErrorKind(streamErr), Provider: p.name, Model: p.model, Err: err}
	}
	return httpapi.FromTransport(err, p.name, p.model)
}

// streamErrorKind classifies an `{"error":…}` chunk that ended a stream already
// under way.
//
// The SDK raises it as a [ssestream.StreamError] carrying no status — the
// response opened with 200 — so it is neither an API error nor a transport
// one, and handed to [httpapi.FromTransport] it was fatal: an OpenAI
// server_error half-way through a round stopped the fallback chain dead.
//
// A failure on a response that had already begun is the SERVER's: the
// endpoint accepted the request, authenticated it and started answering, so
// it cannot be a request it refused or a key it rejected. The one structured
// fact some hosts add is an HTTP-shaped `code` — vLLM's error body carries
// the status it would have sent — and where that is present it decides,
// because it is the endpoint's own classification rather than this backend's
// guess. OpenAI's own `code` is a string or null and does not.
func streamErrorKind(se *ssestream.StreamError) llm.ErrorKind {
	var body struct {
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	var status int
	if json.Unmarshal(se.Event.Data, &body) == nil &&
		json.Unmarshal(body.Error.Code, &status) == nil && status >= 400 && status < 600 {
		return llm.KindForStatus(status)
	}
	return llm.KindServer
}

func (p *Provider) params(req llm.Request) (sdk.ChatCompletionNewParams, error) {
	if !req.Effort.Valid() {
		return sdk.ChatCompletionNewParams{}, fmt.Errorf("request effort %q is not a level (want one of low, "+
			"medium, high, xhigh, max, or empty)", req.Effort)
	}
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
		// THE LOWER of the entry's level and the call's ceiling. An entry
		// with no level sends none, ceiling or not: the endpoint's default
		// is not a level this can compare, and some models default BELOW
		// low (`none` on GPT-5.1), so sending the ceiling could raise the
		// effort it exists to bound.
		if effort := llm.Effort(p.effort).AtMost(req.Effort); effort != "" {
			params.ReasoningEffort = shared.ReasoningEffort(effort)
		}
		// The reasoning models reject max_tokens outright and reject any
		// temperature but their own default. Sending max_tokens here too
		// 400s every o-series call the moment a caller sets a cap.
		//
		// And the CALLER'S cap is not sent at all: it sizes an answer
		// (llm.Request.MaxTokens), and max_completion_tokens bounds the
		// reasoning as well, so a cap sized for a one-line answer is spent
		// reasoning and the call comes back empty. Only the entry's own
		// cap applies here; the call's effort is what keeps it short.
		if p.maxTokens > 0 {
			params.MaxCompletionTokens = param.NewOpt(p.maxTokens)
		}
	} else {
		// Only a temperature the CALL named, and then exactly — an explicit
		// 0.0 is a real request, a judge asking for a reproducible answer.
		// A call that named none sends none and runs at the endpoint's own
		// default: a substitute chosen here would be a number nobody picked,
		// and every compatible host has a default of its own to apply.
		if req.Temperature != nil {
			params.Temperature = param.NewOpt(*req.Temperature)
		}
		// max_tokens rather than max_completion_tokens: the compatible
		// endpoints this backend also serves are years behind the rename.
		if maxTokens > 0 {
			params.MaxTokens = param.NewOpt(maxTokens)
		}
	}

	// No tool_choice: with tools present the API's default is auto, which
	// is the only choice the contract has (see [llm.Request.Tools]) — and
	// leaving it out is also what every compatible endpoint this backend
	// serves understands, where an explicit value is one more field an
	// older server can refuse.
	if len(req.Tools) > 0 {
		params.Tools = formatTools(req.Tools)
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
			if strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
				// A turn with neither content nor a tool call carries
				// nothing, and this endpoint requires one of the two —
				// an assistant message with both absent is a 400, not
				// an empty turn. Dropped rather than padded with "",
				// matching anthropic's own guard: losing an empty
				// message loses nothing, and no tool result can be
				// orphaned by it because there were no calls to pair.
				//
				// It is REACHABLE: the tool loop appends the round it
				// is about to correct before re-calling, and a model
				// that thought and stopped produces exactly this.
				continue
			}
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
		Model: p.model,
		// The wire format, never p.name: an openai-compatible entry
		// relabels its errors with its key, and a key may be any word —
		// `anthropic` included — while a turn's origin has to say which
		// backend's shape it is in. See [llm.Origin].
		Provider:         wireName,
		Content:          choice.Message.Content,
		ReasoningContent: reasoningText(choice.Message.RawJSON()),
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
		args, argErr := httpapi.DecodeArgs([]byte(tc.Function.Arguments), tc.Function.Name)
		call := llm.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args}
		if argErr != nil {
			call.ArgumentsError = argErr.Error()
		}
		out.ToolCalls = append(out.ToolCalls, call)
	}
	out.StopReason = stopReason(choice.FinishReason, len(out.ToolCalls) > 0)
	if choice.Message.Refusal != "" {
		// The model's own refusal message, which the API puts beside the
		// content rather than in it — a refusal whatever finish_reason
		// says, and the one place OpenAI gives an account of it.
		out.StopReason = llm.StopRefusal
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
		"finish_reason", choice.FinishReason)
	if out.StopReason == llm.StopRefusal {
		// A refusal is an error, not an answer — see [llm.StopRefusal] —
		// and it is returned here, after the credential rotation, because
		// the call succeeded on a healthy key. OpenAI names no policy
		// category; the refusal message, where there is one, is its
		// account of why.
		log.Warn("llm_refused", "model", p.model,
			"input_tokens", out.InputTokens, "output_tokens", out.OutputTokens)
		return nil, llm.Refused(p.name, p.model, &llm.Refusal{
			Explanation: choice.Message.Refusal,
			Completion:  out,
		})
	}
	return out, nil
}

// stopReason maps a finish_reason onto the contract's.
//
// `length` is the output cap and `content_filter` the host's policy filter,
// which is a refusal whatever produced it. A MISSING one is read from the
// response — a tool call means it stopped for its tools — because a compatible
// host that omits the field has not said the response was cut short; an
// UNKNOWN one is logged and read the same way, since a value newer than this
// build is the host's and failing every round over a word fails a working seat.
// `function_call` is the deprecated spelling of `tool_calls`.
func stopReason(raw string, calls bool) llm.StopReason {
	switch raw {
	case "stop":
		return llm.StopEnd
	case "tool_calls", "function_call":
		return llm.StopToolUse
	case "length":
		return llm.StopMaxTokens
	case "content_filter":
		return llm.StopRefusal
	case "":
	default:
		log.Warn("finish_reason_unknown", "finish_reason", raw)
	}
	if calls {
		return llm.StopToolUse
	}
	return llm.StopEnd
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
