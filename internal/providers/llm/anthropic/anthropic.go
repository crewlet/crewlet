// Package anthropic is the Anthropic Messages backend.
//
// It implements [llm.Provider] over the official anthropic-sdk-go, and it is
// deliberately thin: it translates the neutral request into Anthropic's wire
// shape, makes exactly one HTTP attempt per credential, and translates the
// answer back. What a failure MEANS is the contract's vocabulary (the
// llm.ErrorKind), read from the error type the API names in its body and from
// the status only where it names none ([kindOf] says why); which credential to
// use next is the pool's, and which model to try next is the chain's.
//
// Six details here are the ones worth checking against the vendor rather
// than against intuition:
//
//   - THE REQUEST SHAPE IS THE MODEL'S, read from [claudemodel] once at
//     construction. Thinking is adaptive with a summarized display on every
//     model that has that mode and a budget only on the ones that predate
//     it; effort is the entry's level lowered to the call's ceiling, sent
//     only where the model takes it; a temperature only where the model
//     samples and the call is not thinking; max_tokens is the model's own
//     ceiling. No knob is sent that the model in front of it would answer
//     with a 400, because a 400 is fatal and the chain does not retry it.
//   - AN ASSISTANT TURN GOES BACK AS IT CAME. Every response's content blocks
//     are kept verbatim ([llm.Message.Raw]) and replayed unchanged on every
//     later call, to whichever Claude model is serving it: Opus 5.5, Sonnet
//     5.5 and Fable 5.1 bind each thinking block to the conversation before
//     it, and a turn rebuilt from the neutral view — reordered, its texts
//     joined, its call's input re-encoded — is an edit they refuse. Which
//     model may read which block is the vendor's call, made by dropping what
//     it cannot read; this backend never strips a block for the MODEL in
//     front of it. Only a turn some other backend wrote is rebuilt, without
//     thinking ([formatMessages]).
//   - EXCEPT THE REASONING A CHANGED TOOL SET INVALIDATED. Those models bind
//     a thinking block to the system prompt and the tools of the request
//     that wrote it too, and an executor's tools change mid-conversation —
//     `activate_tool` adds one, a resumed run renders them again. On a model
//     that checks, every turn up to the last one written under another tool
//     set is replayed WITHOUT its thinking: a run shed from the front, which
//     is the one removal the check accepts, and what remains was written
//     under exactly this request's tools (binding.go has the proof).
//   - EVERY CALL STREAMS, whether or not anybody is watching it. max_tokens
//     is the model's ceiling (128K on the current models), and the vendor
//     requires a stream for a response that large: a unary call is bounded
//     only IN TOTAL, so a worker or a judge that thinks at the entry's effort
//     for longer than the timeout dies half-way and the chain pays for the
//     whole call again on its next member. A streamed one is bounded by its
//     SILENCE ([Provider.streamOnce]), and a stream that ends without its
//     `message_stop` is a failure rather than a short answer. The unary route
//     is only the fallback for an endpoint that answered a stream without
//     streaming.
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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/anthropic/claudemodel"
	"github.com/crewlet/crewlet/internal/providers/llm/httpapi"
)

var log = logging.Get("llm.anthropic")

// providerName labels errors and log lines. It is the config's type name.
const providerName = "anthropic"

// Defaults. The timeout matches the config layer's defaultLLMTimeoutSeconds,
// which is what the engine passes; this one serves a Config built without it.
//
// There is deliberately NO default temperature, max_tokens or thinking budget.
// A temperature is a 400 on every current model and the engine's own phases
// never chose one; the output cap is the model's own ceiling (see
// [Provider.params]); and a budget is something a budget-era entry asks for
// rather than something every entry is given.
const (
	DefaultBaseURL         = "https://api.anthropic.com"
	DefaultTimeout         = 600 * time.Second
	emptyToolResultContent = "(no output)"
)

// Config builds a provider.
type Config struct {
	// Model is the model id this provider serves. Required. It is sent as
	// written; the request SHAPE is read from the capability table under
	// [claudemodel.Normalize], so a Bedrock or Vertex spelling of a known
	// model is shaped as that model.
	Model string

	// ClaudeModel names the table row whose request shape this entry uses,
	// when Model is a gateway alias the table cannot read. It must be one
	// of the table's own ids exactly ([claudemodel.Known]), and it is
	// refused beside a Model the table already reads: one entry may not
	// carry two answers to "which model is this". Empty reads the shape
	// from Model, and an id the table does not know gets
	// [claudemodel.Modern].
	ClaudeModel string

	// APIKeys are the credentials, in declaration order. Several rotate.
	// These are THE WHOLE BAG: nothing here reads a variable. Which key an
	// entry that names none runs on is a configuration rule
	// (config.LLMProvider.Keys), decided where the document and the secret
	// store are both in reach. Empty still builds, and every call comes
	// back a clean 401, which is a far easier thing to diagnose than a
	// constructor that refused to exist.
	APIKeys []string

	// BaseURL is the endpoint. Empty takes DefaultBaseURL. It is ALWAYS
	// sent explicitly, so an ambient ANTHROPIC_BASE_URL cannot silently
	// redirect a company's traffic.
	BaseURL string

	// Timeout bounds one HTTP attempt. Zero takes DefaultTimeout.
	//
	// Every call streams, and a streamed call is bounded by its SILENCE —
	// the longest gap with nothing arriving, the wait for the first byte
	// included — and never by its length, because a round that thinks at a
	// high effort writes for many minutes and every one of them is the model
	// working (see [httpapi.IdleWatchdog]). Only on an endpoint that does not
	// stream, where the call falls back to the unary route, is it a bound IN
	// TOTAL, request to last byte.
	Timeout time.Duration

	// Cooldowns is the credential bench policy. Zero fields take defaults.
	Cooldowns credential.Policy

	// Effort is how hard the model thinks on this entry, sent as
	// `output_config.effort`. Empty takes [claudemodel.DefaultEffort].
	// Refused on a model that takes no effort (Sonnet 4.5, Haiku 4.5 and
	// older) and at a level the model does not accept (`xhigh` before Opus
	// 4.7), because either is a 400 on every call. A call lowers it with
	// [llm.Request.Effort] and never raises it.
	//
	// It is the ONLY depth control on a model that thinks adaptively. There
	// is no "off": Opus 5.5, Fable and Mythos refuse it outright, Sonnet
	// 5.5 only at the lower efforts, and Opus 4.8 and 5 with thinking off
	// are documented to write a tool call into their prose instead of
	// making it — the very failure the tool loop's correctives exist for.
	Effort llm.Effort

	// ThinkingBudget is the thinking allowance on a BUDGET-ERA model (Haiku
	// 4.5, Sonnet 4.5, Opus 4.5 and older), the only models that take one.
	// Zero means that model does not think. Refused on an adaptive model,
	// where `budget_tokens` is a 400 or deprecated, and below
	// [claudemodel.MinThinkingBudget] or at or above the model's output
	// cap, where it is a 400 too — refused here rather than silently raised
	// to fit, which is how a configured 10 used to become a 1024 nobody
	// chose.
	ThinkingBudget int

	// HTTPClient overrides the transport. Nil builds one through
	// httpapi.NewHTTPClient, which the provider then owns and closes.
	HTTPClient option.HTTPClient

	// Clock is the pool's monotonic time source. Nil takes the default.
	Clock credential.Clock
}

// Provider is an Anthropic Messages backend.
type Provider struct {
	model  string
	client sdk.Client
	pool   *credential.Pool
	// baseURL is the endpoint every call goes to, kept for the doctor's
	// report: "not served" means nothing until it says by whom.
	baseURL string

	// profile is what this entry's model accepts, decided once at
	// construction: every request is shaped from it.
	profile claudemodel.Profile
	// effort is the entry's level, "" on a model that takes none.
	effort llm.Effort
	// budget is the thinking allowance on a budget-era model, 0 for none.
	budget int64
	// timeout is a streamed call's idle bound, and the total one of a unary
	// call on an endpoint that does not stream.
	timeout time.Duration

	// noStream latches once this endpoint has answered a streaming request
	// without streaming. Atomic because one Provider serves every seat
	// concurrently, and this is written from whichever one discovers it.
	noStream atomic.Bool
}

var _ llm.Provider = (*Provider)(nil)

// New builds a provider. It refuses a configuration that would be a 400 on
// every call, naming the field — a combination the config tier already
// refuses when the model is written literally, and checked again here because
// a model written as a `${VAR}` is only known once it resolves.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("anthropic: Model is required")
	}
	profile, err := claudemodel.Resolve(cfg.Model, cfg.ClaudeModel)
	if err != nil {
		return nil, fmt.Errorf("anthropic: ClaudeModel: %w", err)
	}
	if err := profile.CheckEffort(cfg.Model, claudemodel.Effort(cfg.Effort)); err != nil {
		return nil, fmt.Errorf("anthropic: Effort: %w", err)
	}
	if err := profile.CheckBudget(cfg.Model, cfg.ThinkingBudget); err != nil {
		return nil, fmt.Errorf("anthropic: ThinkingBudget: %w", err)
	}
	// The entry's level: what it named, or the default, on a model that
	// takes one at all — and nothing on one that does not, where any value
	// is a 400.
	effort := cfg.Effort
	switch {
	case len(profile.Efforts) == 0:
		effort = ""
	case effort == "":
		effort = llm.Effort(claudemodel.DefaultEffort)
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

	opts := []option.RequestOption{
		// The SDK otherwise loads ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN,
		// ANTHROPIC_BASE_URL and a profile file of its own. Every one of
		// those is a second credential source the pool does not know about
		// — an ambient auth token would answer every call while the pool
		// dutifully rotated keys nothing was using.
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(baseURL),
		// NO CLIENT-WIDE REQUEST TIMEOUT: every call streams and is bounded
		// by its silence ([Provider.streamOnce]); a total bound here would
		// cut a long round off half-way. The unary fallback sets its own
		// ([Provider.unary]).
		// See the package doc. Not negotiable.
		option.WithMaxRetries(0),
	}
	// A transport the caller supplied is used as given; otherwise the
	// engine's shared one. NOTHING HERE OWNS IT — see [httpapi.NewHTTPClient]
	// for why this provider has no Close.
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	} else {
		opts = append(opts, option.WithHTTPClient(httpapi.NewHTTPClient()))
	}

	if profile.ID == "" {
		// Not a refusal: an id the table has never seen is most likely a
		// model released after it, and Modern is what those accept. Said
		// once per build so an alias for an OLDER model — the one case
		// Modern gets wrong — has a line to find.
		log.Warn("model_profile_unknown",
			"model", cfg.Model,
			"hint", "shaped as the current generation (adaptive thinking, no "+
				"temperature); if this id is a gateway alias for an older Claude "+
				"model, name that model with claude_model")
	}

	return &Provider{
		model:   cfg.Model,
		baseURL: baseURL,
		client:  sdk.NewClient(opts...),
		pool:    credential.New(credential.Options{Keys: keys, Policy: cfg.Cooldowns, Clock: cfg.Clock}),
		profile: profile,
		effort:  effort,
		budget:  int64(cfg.ThinkingBudget),
		timeout: timeout,
	}, nil
}

// Model is the model id this provider answers as.
func (p *Provider) Model() string { return p.model }

// Pool exposes the credential pool's public state for operator surfaces. It
// never yields a key.
func (p *Provider) Pool() *credential.Pool { return p.pool }

// Complete calls the Messages API once per live credential until one answers.
//
// It STREAMS whether or not the request asked to watch the answer arrive
// ([llm.Request.OnDelta]): with no listener the fragments go nowhere, and the
// call is still bounded by its silence rather than its length.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	params, bound, err := p.params(req)
	if err != nil {
		return nil, &llm.Error{
			Kind: llm.KindFatal, Provider: providerName, Model: p.model, Err: err,
		}
	}

	// Per-call local, never a provider field: ONE Provider serves every
	// concurrent caller.
	attempt := 0
	streaming := !p.noStream.Load()
	msg, err := credential.Rotate(ctx, p.pool,
		credential.Identity{Provider: providerName, Model: p.model},
		p.classify,
		func(key string) (*sdk.Message, error) {
			opt := option.WithAPIKey(key)
			if !streaming {
				return p.unary(ctx, params, opt)
			}
			// A ROTATION IS A RESTART: the previous key may have died
			// after streaming half an answer, and appending this attempt
			// to that one would show two half-answers as one.
			attempt++
			if attempt > 1 {
				req.Send(llm.Delta{Restart: true, Model: p.model})
			}
			msg, sErr := p.streamOnce(ctx, req, params, opt)
			if errors.Is(sErr, errNoStream) {
				// This endpoint answered the streaming request without
				// streaming. "OpenAI-compatible" and "Anthropic-compatible"
				// are de-facto standards with real variance — a local shim
				// or a proxy may implement the unary route only — so the
				// capability is NEGOTIATED rather than assumed or pushed
				// onto the operator as a config field they would have to
				// know to set. Latched, so it costs one call per process
				// and never repeats.
				p.noStream.Store(true)
				log.WarnContext(ctx, "provider_does_not_stream",
					"provider", providerName, "model", p.model,
					"hint", "the endpoint answered a streaming request without streaming; "+
						"live phase text will appear per round instead of as it is written, and "+
						"every call is bounded by the timeout in total rather than by its silence")
				return p.unary(ctx, params, opt)
			}
			return msg, sErr
		})
	if err != nil {
		return nil, err
	}
	out := p.completion(msg, bound)
	if err := p.refused(msg, out); err != nil {
		return nil, err
	}
	return out, nil
}

// errNoStream reports an endpoint that accepted a streaming request and
// answered without streaming.
var errNoStream = errors.New("endpoint did not stream")

// errCutShort reports a stream that ended cleanly before its `message_stop`.
// [Provider.classify] reads it as the server's failure.
var errCutShort = errors.New(
	"the response stream ended before message_stop, so the answer is incomplete")

// unary is one attempt on the unary route, which only an endpoint that does
// not stream is sent. Its bound is IN TOTAL — nothing arrives until the answer
// is whole, so there is no silence to measure — and it is explicit, because
// the SDK otherwise REFUSES a unary call whose max_tokens it estimates at over
// ten minutes, and every call here sends the model's own cap.
func (p *Provider) unary(
	ctx context.Context, params sdk.MessageNewParams, opt option.RequestOption,
) (*sdk.Message, error) {
	return p.client.Messages.New(ctx, params, opt, option.WithRequestTimeout(p.timeout))
}

// streamOnce runs one streamed attempt, forwarding fragments as they land.
//
// BOUNDED BY SILENCE, NOT LENGTH: there is no per-attempt deadline, and an
// [httpapi.IdleWatchdog] of the entry's timeout ends the attempt only when
// nothing has arrived for that long. A deadline would cover the whole streamed
// body, and a round that thinks for longer than it — a Fable round, anything
// at xhigh — would die half-way through every time it did its best work.
//
// FINISHED ONLY AT `message_stop`. A gateway or a proxy that closes the
// response in an orderly way mid-answer ends the SDK's stream with no error,
// and what accumulated reads as a round with no stop reason — which
// [stopReason] would take for an ordinary end, handing the loop half an
// answer as the model's last word. So a stream that never sent its terminal
// event is [errCutShort], the server's failure, and the chain tries again.
//
// The SDK accumulates into exactly the [sdk.Message] the unary path returns —
// signatures on thinking blocks included, which must survive verbatim or the
// next round is rejected — so [Provider.completion] reads one shape however
// the response arrived, rather than growing a second interpretation.
func (p *Provider) streamOnce(
	ctx context.Context, req llm.Request,
	params sdk.MessageNewParams, opt option.RequestOption,
) (*sdk.Message, error) {
	ctx, watch := httpapi.WatchIdle(ctx, p.timeout)
	defer watch.Stop()
	stream := p.client.Messages.NewStreaming(ctx, params, opt,
		option.WithMiddleware(watch.Middleware))
	defer func() { _ = stream.Close() }()

	var msg sdk.Message
	events, stopped := 0, false
	for stream.Next() {
		events++
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return nil, err
		}
		if event.Type == "message_stop" {
			stopped = true
		}
		switch d := event.Delta; d.Type {
		case "text_delta":
			req.Send(llm.Delta{Content: d.Text})
		case "thinking_delta":
			req.Send(llm.Delta{Reasoning: d.Thinking})
		}
		// input_json_delta and signature_delta are accumulated and not
		// shown: half a JSON argument is not readable, and a signature is
		// a token for the provider rather than anything for a person.
	}
	if err := stream.Err(); err != nil {
		// A stream that dies MID-BODY is a failure of the call, not a
		// short answer: handing back what accumulated would give the loop
		// a truncated response as though the model had finished. A stream
		// the watchdog ended is reported as the stall it was.
		return nil, watch.Err(err)
	}
	if events == 0 {
		// Not an error of the call — the endpoint simply does not do this.
		// Distinguished from a failure so the caller can fall back rather
		// than fail a phase over a capability.
		return nil, errNoStream
	}
	if !stopped {
		return nil, errCutShort
	}
	return &msg, nil
}

// classify turns an SDK failure into the contract's error. The errors.As on
// the SDK's own type is the only part a backend can own; see httpapi.
func (p *Provider) classify(err error) *llm.Error {
	if errors.Is(err, errCutShort) {
		// The API accepted the request and began answering, so the one
		// thing a cut can never be is a request it refused: the chain may
		// try again, and no key is benched for it.
		return &llm.Error{Kind: llm.KindServer, Provider: providerName, Model: p.model, Err: err}
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		var header http.Header
		if apiErr.Response != nil {
			header = apiErr.Response.Header
		}
		return httpapi.FromKind(err, providerName, p.model, kindOf(apiErr), apiErr.StatusCode, header)
	}
	return httpapi.FromTransport(err, providerName, p.model)
}

// kindOf classifies an API error by the TYPE its body names first and by its
// status only when the body names none this backend knows.
//
// The type first, because the status is not always the API's answer. An
// `error` event inside a stream reaches the SDK after the response opened
// with 200, and the SDK attaches THAT status to it — so an `overloaded_error`
// half-way through a round classified by status is a 200, which is fatal: no
// other member of the chain is tried and the turn fails over a capacity blip
// the next model would have absorbed. The type is the API's own structured
// classification — the body of every Anthropic error carries one, a status
// response's as much as a stream's — so reading it is not prose matching, and
// on a status response the two agree.
//
// The status decides only when the type is absent or new. An unrecognised
// type on a response that had already succeeded is a SERVER failure rather
// than whatever 200 maps to: the API accepted the request, authenticated it
// and began answering, so the one thing the failure cannot be is a request it
// refused.
func kindOf(apiErr *sdk.Error) llm.ErrorKind {
	switch apiErr.Type() {
	case sdk.ErrorTypeRateLimitError, sdk.ErrorTypeBillingError:
		return llm.KindRateLimit
	case sdk.ErrorTypeAuthenticationError, sdk.ErrorTypePermissionError:
		return llm.KindAuth
	case sdk.ErrorTypeOverloadedError, sdk.ErrorTypeAPIError:
		return llm.KindServer
	case sdk.ErrorTypeTimeoutError:
		return llm.KindTimeout
	case sdk.ErrorTypeInvalidRequestError, sdk.ErrorTypeNotFoundError:
		return llm.KindFatal
	}
	if apiErr.StatusCode >= 200 && apiErr.StatusCode < 300 {
		return llm.KindServer
	}
	return llm.KindForStatus(apiErr.StatusCode)
}

// params renders the neutral request into Anthropic's wire shape, and says
// what the turn it answers with is bound to: the request's [binding], or ""
// when the request replays reasoning a model that checks would have shed (see
// binding.go, whose premise such a turn breaks).
func (p *Provider) params(req llm.Request) (sdk.MessageNewParams, string, error) {
	system, rest := splitSystem(req.Messages)
	var tools []sdk.ToolUnionParam
	if len(req.Tools) > 0 {
		tools = formatTools(req.Tools)
	}
	bound, err := binding(system, tools)
	if err != nil {
		return sdk.MessageNewParams{}, "", err
	}
	// The reasoning a changed system prompt or tool set invalidated, oldest
	// first. Shed only where the model checks: elsewhere every block is
	// still valid, and a turn written there while such a block was replayed
	// records no binding, so a checking model sheds it later (binding.go).
	cut, err := shedThrough(rest, bound)
	if err != nil {
		return sdk.MessageNewParams{}, "", err
	}
	shed, writes := 0, bound
	switch {
	case p.profile.PrefixBinding:
		shed = cut
	case cut > 0:
		writes = ""
	}
	if shed > 0 {
		log.Debug("thinking_shed", "model", p.model, "messages", shed,
			"hint", "the tool set or system prompt changed since this reasoning was written, "+
				"and the model refuses reasoning replayed under another")
	}
	messages, err := formatMessages(rest, shed)
	if err != nil {
		return sdk.MessageNewParams{}, "", err
	}
	if len(messages) == 0 {
		// Anthropic requires a non-empty messages array. Refusing here
		// names the actual problem; the API's 400 names a field.
		return sdk.MessageNewParams{}, "", errors.New(
			"anthropic: request carries no non-system message with content")
	}

	if !req.Effort.Valid() {
		return sdk.MessageNewParams{}, "", fmt.Errorf(
			"anthropic: request effort %q is not a level (want low, medium, high, xhigh, max, or empty)",
			req.Effort)
	}
	params := sdk.MessageNewParams{
		Model:    p.model,
		Messages: messages,
	}

	// THE SHAPE IS THE MODEL'S. Every field below is sent only where the
	// profile says the model accepts it, because a field it does not is a
	// 400, and a 400 is fatal to the call — the chain never tries the next
	// member on it.
	thinking := p.profile.Thinks(int(p.budget))
	switch {
	case p.profile.Thinking == claudemodel.ThinkingAdaptive:
		// Explicit, on every call: omitting it means "think" on some of
		// these models and "do not" on others (Opus 4.6–4.8 and Sonnet
		// 4.6). SUMMARIZED, because the default display on every current
		// model is `omitted` — an empty thinking text, so the round's
		// reasoning, the live thinking stream and the dashboard's thinking
		// disclosure would all be blank. A summary is billed the same.
		params.Thinking = sdk.ThinkingConfigParamUnion{OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{
			Display: sdk.ThinkingConfigAdaptiveDisplaySummarized,
		}}
	case thinking:
		params.Thinking = sdk.ThinkingConfigParamUnion{
			OfEnabled: &sdk.ThinkingConfigEnabledParam{BudgetTokens: p.budget},
		}
	}

	// The entry's level lowered to the call's ceiling, and then to the
	// highest level at or below that the model takes: a call asking for
	// `xhigh` on a model whose levels skip it gets `high`, not a 400.
	if effort := fit(p.effort.AtMost(req.Effort), p.profile.Efforts); effort != "" {
		params.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(effort)}
	}

	// A caller's temperature reaches the wire only where it can mean
	// something: a model that takes sampling at all, on a call that is not
	// thinking (the API takes nothing but 1 while it is). Anywhere else it
	// is dropped rather than refused — a model with no sampling parameter
	// cannot honour a 0, and the caller asked for reproducibility, not for
	// a failed call.
	if req.Temperature != nil && p.profile.Sampling && !thinking {
		params.Temperature = param.NewOpt(*req.Temperature)
	}

	// max_tokens is the MODEL'S OWN CEILING. An unused cap costs nothing,
	// and runaway spend is bounded by the token budgets rather than here;
	// a smaller one truncates an executor round mid-call, and on a thinking
	// model the thinking is spent from the same cap as the answer, so a cap
	// sized for a short answer is spent before the answer starts — the
	// empty-answer failure the judge and the knowledge passes describe. A
	// caller's own cap is therefore honoured only on a call that is not
	// thinking, and never above the ceiling.
	params.MaxTokens = int64(p.profile.MaxOutput)
	if req.MaxTokens > 0 && !thinking && req.MaxTokens < p.profile.MaxOutput {
		params.MaxTokens = int64(req.MaxTokens)
	}

	if system != "" {
		params.System = systemBlocks(system)
	}
	// No tool_choice: the API's default with tools present is auto, which
	// is the only choice the contract has (see [llm.Request.Tools]). A
	// forced `any` is a 400 on Opus 5.5, Sonnet 5.5, Fable 5.1 and Mythos
	// 5.1, and on every Claude model while it is thinking.
	if len(tools) > 0 {
		params.Tools = tools
		// THE CONVERSATION IS CACHED TOO, on a call that will be continued.
		// The breakpoints on the system block and the last tool cache the
		// static prefix and nothing after it, so every round of a tool loop
		// re-billed the whole history it had grown so far at the full input
		// price — and by round twenty that history, not the prefix, is most
		// of what a round sends. So the last block of the conversation
		// carries a breakpoint too ([markTail]), which moves forward with
		// it: round N writes what round N+1 reads.
		//
		// ONLY WITH TOOLS, because only then is there a next round: a call
		// that offers tools is a tool loop's, and its answer comes back as
		// this same prefix plus the results. A call with none — a judge, a
		// knowledge or learning pass — is asked once, and caching its tail
		// would pay the write premium on every one of them for a read that
		// never comes.
		markTail(messages)
	}
	return params, writes, nil
}

// fit is effort at or below the highest level the model takes: effort itself
// when the model takes it, the next level down it does take otherwise, and ""
// when it takes no level at or below it — or none at all, or effort is empty.
// accepted is lowest first, as every profile's levels are.
func fit(effort llm.Effort, accepted []claudemodel.Effort) llm.Effort {
	var out llm.Effort
	for _, level := range accepted {
		// level is at or below effort exactly when lowering effort to it
		// gives it back.
		if l := llm.Effort(level); effort != "" && effort.AtMost(l) == l {
			out = l
		}
	}
	return out
}

// cacheBreakpoint is one prompt-cache breakpoint, on the default 5-minute TTL.
//
// Three are set, inside the API's cap of four and all on the one TTL (a
// longer one may not follow a shorter): on the system block and the last
// tool, which cache the (tools + system) prefix — the large static part of
// every executor and reviewer round, re-billed in full on every round without
// them — and, on a call that offers tools, on the conversation's last block
// ([markTail]). Anthropic silently ignores a breakpoint on a prefix below the
// cacheable minimum, so setting one is always safe.
func cacheBreakpoint() sdk.CacheControlEphemeralParam {
	return sdk.NewCacheControlEphemeralParam()
}

// markTail sets a cache breakpoint on the last block of the final message that
// can carry one, so a tool loop's conversation is cached up to where it ends.
//
// EXPLICIT, ON THE BLOCK, and never the request's top-level `cache_control`
// (the API's "automatic" breakpoint, which lands on the same block): the
// legacy Bedrock integration (Opus 4.6 and earlier) answers the top-level
// field with a 400, and this backend reaches Bedrock through any gateway that
// forwards the body — a Bedrock spelling of the model is one it reads. A 400
// is fatal and the chain does not retry it, so the one marker every platform
// accepts is the one written.
//
// The final message is always the user's — a prefill is refused before this
// runs ([ErrPrefill]) — and is built here from text and tool results, both of
// which take a marker. The walk backwards is for a block that does not (one
// that cannot carry `cache_control` has no slot to set), and the automatic
// form does the same walk; a message with no such block is left unmarked
// rather than marked somewhere earlier, where the cache would stop short.
func markTail(messages []sdk.MessageParam) {
	if len(messages) == 0 {
		return
	}
	tail := messages[len(messages)-1].Content
	for i := len(tail) - 1; i >= 0; i-- {
		if marker := tail[i].GetCacheControl(); marker != nil {
			*marker = cacheBreakpoint()
			return
		}
	}
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

// formatMessages renders the conversation. An assistant turn this backend
// wrote is REPLAYED from its own blocks ([replay]) — without its thinking when
// it is one of the first shed messages ([shedThrough]); every other turn is
// built from the neutral view.
func formatMessages(messages []llm.Message, shed int) ([]sdk.MessageParam, error) {
	out := make([]sdk.MessageParam, 0, len(messages))
	for i, m := range messages {
		switch {
		case replayed(m):
			blocks, err := replay(m.Raw, i < shed)
			if err != nil {
				return nil, fmt.Errorf("anthropic: message %d: %w", i, err)
			}
			if len(blocks) == 0 {
				// Nothing but whitespace text, or nothing but shed
				// thinking: see [replay].
				continue
			}
			out = append(out, sdk.NewAssistantMessage(blocks...))

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
			// is_error from the message's own flag: the content says why
			// the call failed, and the flag is the API's structured way of
			// saying THAT it did, which the model reads differently from a
			// tool that ran and returned the same words.
			out = appendUser(out, sdk.NewToolResultBlock(m.ToolCallID, content, m.Failed))

		case len(m.ToolCalls) > 0:
			// A turn another backend wrote — or one parked by a build that
			// kept no blocks — rebuilt from the neutral view, WITHOUT ITS
			// THINKING. Another vendor's reasoning has no Anthropic
			// signature to carry, and a block rebuilt here is not the
			// block that was signed: the order and the text around it are
			// this function's rather than the model's, which is the edit
			// that invalidates it. Leaving it out is never a 400. A turn
			// another vendor wrote never had a block to lose — a turn with
			// no thinking is what every non-Claude turn looks like, and the
			// vendor accepts one anywhere — and the turns a pre-Raw build
			// parked all precede every turn a resumed loop writes, so the
			// blocks they lose are a run dropped from the FRONT, the one
			// removal the vendor's history check accepts.
			blocks := make([]sdk.ContentBlockParamUnion, 0, len(m.ToolCalls)+1)
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
			out = appendUser(out, sdk.NewTextBlock(m.Content))
		}
	}
	// NO PREFILL. A conversation ending on the assistant's turn asks the
	// model to continue it, which every model from Opus 4.6 and Sonnet 4.6
	// on answers with a 400. Nothing in the engine sends one — the tool
	// loop always follows an assistant turn with the results or a user
	// note — so this is the invariant ENFORCED rather than assumed, refused
	// here where it can be named rather than discovered as a fatal 400 that
	// no fallback retries.
	if n := len(out); n > 0 && out[n-1].Role == sdk.MessageParamRoleAssistant {
		return nil, ErrPrefill
	}
	return out, nil
}

// replay is an assistant turn this backend wrote, as the blocks it was written
// in: each one the API's own JSON, sent back as it arrived — never decoded into
// the SDK's param types and re-encoded, which would drop any field this SDK
// version does not model and any block type newer than it, and an edit is an
// edit whether or not it was meant.
//
// One kind of block is left out: a TEXT block holding nothing but whitespace.
// The API refuses one on input, though a model writes them (a newline between
// its thinking and a tool call), and the vendor's history check ignores them by
// rule, so leaving one out changes nothing it compares. A turn that held only
// such blocks comes back empty, and the caller drops it as it drops any turn
// with nothing in it.
//
// With shed set, the turn's THINKING is left out too — every thinking and
// redacted_thinking block, and nothing else: the text and the calls are the
// conversation, and only the reasoning is bound to the tools it was written
// under (see binding.go). A turn that was nothing but reasoning comes back
// empty and is dropped the same way.
//
// A block that is not JSON at all is refused, naming its place: it can only be
// a parked conversation corrupted in storage, and sent as it is the SDK would
// fail to encode the request with an error naming nothing.
func replay(raw []json.RawMessage, shed bool) ([]sdk.ContentBlockParamUnion, error) {
	blocks := make([]sdk.ContentBlockParamUnion, 0, len(raw))
	for i, block := range raw {
		head, err := headOf(block)
		if err != nil {
			return nil, fmt.Errorf("replayed content block %d: %w", i, err)
		}
		if head.Type == "text" && strings.TrimSpace(head.Text) == "" {
			continue
		}
		if shed && isThinking(head.Type) {
			continue
		}
		blocks = append(blocks, param.Override[sdk.ContentBlockParamUnion](block))
	}
	return blocks, nil
}

// ErrPrefill is a request whose conversation ends on the assistant's turn.
var ErrPrefill = errors.New(
	"anthropic: the conversation ends on an assistant turn (a prefill), which " +
		"current Claude models refuse; end it on a user turn or a tool result")

// appendUser adds user-side blocks to the conversation, JOINING the previous
// turn when it is also the user's.
//
// Anthropic's conversation alternates: every tool_use an assistant turn makes
// is answered by a tool_result block in the ONE user turn that follows, and
// those blocks come first in it. The engine's messages do not alternate — each
// tool result is a message of its own, and a person's note to a running turn
// (internal/agent/steer) is a user message sent straight after the results it
// follows. Sent one message each, the API merges consecutive user turns
// server-side, which is a courtesy rather than a contract — so the merge is
// made HERE, where its order is ours: the results in call order, then the
// note.
//
// NEVER A RESULT AFTER TEXT: a tool_result joins a user turn only when that turn
// is made of results so far, because the API requires them first. A result
// that would follow text opens a turn of its own instead, which is the shape
// the conversation had before this merge existed.
func appendUser(out []sdk.MessageParam, block sdk.ContentBlockParamUnion) []sdk.MessageParam {
	n := len(out)
	if n == 0 || out[n-1].Role != sdk.MessageParamRoleUser {
		return append(out, sdk.NewUserMessage(block))
	}
	if prev := out[n-1].Content; block.OfToolResult != nil && len(prev) > 0 &&
		prev[len(prev)-1].OfToolResult == nil {
		return append(out, sdk.NewUserMessage(block))
	}
	out[n-1].Content = append(out[n-1].Content, block)
	return out
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

// completion translates the response, written under the request binding
// bound ([Provider.params]).
func (p *Provider) completion(msg *sdk.Message, bound string) *llm.Completion {
	// The CONFIGURED model id, not the one the response echoes. A vendor
	// alias resolving to a dated snapshot would otherwise re-key the
	// per-model breakdown the day the alias moves, splitting one model's
	// spend across two names that nothing in the config mentions.
	out := &llm.Completion{Model: p.model, Provider: providerName, Binding: bound}

	var content, reasoning strings.Builder
	for _, block := range msg.Content {
		// EVERY block, verbatim and in order, whatever its type — the copy
		// the next call replays ([llm.Message.Raw]). RawJSON is the bytes
		// the API sent; on a streamed response the SDK's accumulator
		// rewrites each block's raw JSON from its deltas when the block
		// stops, so it is the finished block either way.
		out.Raw = append(out.Raw, json.RawMessage(block.RawJSON()))
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
			args, argErr := httpapi.DecodeArgs(block.Input, block.Name)
			call := llm.ToolCall{ID: block.ID, Name: block.Name, Arguments: args}
			if argErr != nil {
				call.ArgumentsError = argErr.Error()
			}
			out.ToolCalls = append(out.ToolCalls, call)
		}
	}
	out.Content = content.String()
	out.ReasoningContent = reasoning.String()
	out.StopReason = stopReason(msg.StopReason, len(out.ToolCalls) > 0)

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
		"stop_reason", string(msg.StopReason))
	return out
}

// stopReason maps the API's stop_reason onto the contract's.
//
// A MISSING ONE is read from the response itself — a tool call means the
// round stopped for its tools, anything else that it ended — because an
// Anthropic-compatible gateway that omits the field has not said the response
// was cut short. An UNKNOWN one is logged and read the same way: a value newer
// than this build is the vendor's, and refusing every round that carries it
// would fail a working seat over a word.
//
// `stop_sequence` is an ordinary end: the engine sets no stop sequences, and a
// caller that did would have asked for exactly that stop.
func stopReason(raw sdk.StopReason, calls bool) llm.StopReason {
	switch raw {
	case sdk.StopReasonEndTurn, sdk.StopReasonStopSequence:
		return llm.StopEnd
	case sdk.StopReasonToolUse:
		return llm.StopToolUse
	case sdk.StopReasonMaxTokens:
		return llm.StopMaxTokens
	case sdk.StopReasonRefusal:
		return llm.StopRefusal
	case sdk.StopReasonModelContextWindowExceeded:
		return llm.StopContextExceeded
	case sdk.StopReasonPauseTurn:
		return llm.StopPaused
	case "":
	default:
		log.Warn("stop_reason_unknown", "stop_reason", string(raw))
	}
	if calls {
		return llm.StopToolUse
	}
	return llm.StopEnd
}

// refused reports a refusal as the classified error the contract asks for,
// or nil when the response was not one. Outside the credential rotation on
// purpose: the call succeeded, the key is healthy, and a refusal benches
// nothing.
func (p *Provider) refused(msg *sdk.Message, out *llm.Completion) error {
	if out.StopReason != llm.StopRefusal {
		return nil
	}
	refusal := &llm.Refusal{
		Category:    string(msg.StopDetails.Category),
		Explanation: msg.StopDetails.Explanation,
		Completion:  out,
	}
	log.Warn("llm_refused", "model", p.model, "category", refusal.Category,
		"input_tokens", out.InputTokens, "output_tokens", out.OutputTokens)
	return llm.Refused(providerName, p.model, refusal)
}

// String is the provider's identity in a log line.
func (p *Provider) String() string { return fmt.Sprintf("%s/%s", providerName, p.model) }
