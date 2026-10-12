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
//   - AN ASSISTANT TURN GOES BACK TO THE ENDPOINT THAT WROTE IT AS IT CAME.
//     A reasoning model's trace has no field in OpenAI's schema, so each host
//     added its own — reasoning_content, reasoning, OpenRouter's
//     reasoning_details, a thought signature on a tool call, a content that is
//     a list of chunks — and documented that the next request must carry it
//     back: Kimi K3 is trained to read it on every earlier message, and
//     DeepSeek and Gemini 3 answer a tool loop without it with a 400. Every
//     such field is kept verbatim ([llm.Message.Raw]), an empty one included,
//     and sent back in the shape it arrived in, to the same endpoint only
//     ([llm.Message.Binding]), because a field one host emits is one another
//     may refuse. Once a turn going back carries reasoning_content, every
//     other turn carries an empty one, because a missing one is what Moonshot
//     refuses. replay.go has the vendors' rules and the stream's merge.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// binding is this endpoint's identity, which a turn's kept reasoning is
	// bound to and replayed only under — see [endpointBinding].
	binding string

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
		binding:   endpointBinding(baseURL),
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
	var streamed *streamTurn
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
			out, turn, unary, sErr := p.streamOnce(ctx, req, params, opt)
			if unary || errors.Is(sErr, errNoStream) {
				// This endpoint accepted `stream: true` and answered
				// without streaming. "OpenAI-compatible" is a de-facto
				// standard with real variance — a local shim or a proxy
				// may implement the unary route only — so the capability
				// is NEGOTIATED rather than assumed, or pushed onto the
				// operator as a config field they would have to know to
				// set. Latched: found out once per process, never again.
				p.noStream.Store(true)
				log.WarnContext(ctx, "provider_does_not_stream",
					"provider", p.name, "model", p.model,
					"hint", "the endpoint answered a streaming request without streaming; "+
						"live phase text will appear per round instead of as it is written")
				if unary {
					// THE ANSWER IT GAVE IS THE ANSWER. It was processed
					// and billed, and asking again would pay twice for a
					// round whose first answer no counter ever heard of.
					// Its reasoning, if any, is on the message itself,
					// where the unary path reads it.
					streamed = nil
					return out, nil
				}
				// Answered with nothing a completion could be read from,
				// so there is no answer to keep: asked again, unary.
				plain := params
				plain.StreamOptions = sdk.ChatCompletionStreamOptionsParam{}
				streamed = nil
				return p.client.Chat.Completions.New(ctx, plain, opt)
			}
			streamed = turn
			return out, sErr
		})
	if err != nil {
		return nil, err
	}
	// The assembled message carries none of what a host sends beside
	// OpenAI's schema — the SDK's accumulator does not keep it — so a streamed
	// answer's reasoning is read from what the stream itself kept.
	return p.completion(resp, streamed)
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
// none of the fields a host carries it in is in the OpenAI schema, so the
// accumulator neither knows nor keeps them, and the assembled message's raw
// JSON has no trace of them. The [streamTurn] returned holds them, merged the
// way a whole response would have carried them (replay.go).
//
// unary reports an endpoint that answered with a whole completion instead of a
// stream ([httpapi.UnaryAnswer]): that completion is the answer, decoded
// exactly as the unary path decodes one, and no fragment was forwarded.
func (p *Provider) streamOnce(
	ctx context.Context, req llm.Request,
	params sdk.ChatCompletionNewParams, opt option.RequestOption,
) (out *sdk.ChatCompletion, turn *streamTurn, unary bool, err error) {
	ctx, watch := httpapi.WatchIdle(ctx, p.timeout)
	defer watch.Stop()
	var done doneWatch
	var answer sdk.ChatCompletion
	whole := httpapi.NewUnaryAnswer(func(body []byte) bool {
		var c sdk.ChatCompletion
		// A completion carries its choices; an error envelope a gateway
		// sent with a 2xx status, or an empty body, carries none.
		if json.Unmarshal(body, &c) != nil || len(c.Choices) == 0 {
			return false
		}
		answer = c
		return true
	})
	// The watchdog and the sentinel watch INSIDE the unary reader, so a
	// whole body read to find out whether it is a completion is bounded by
	// its silence as a stream is, and a stream it hands back has already
	// been seen for its `[DONE]`.
	stream := p.client.Chat.Completions.NewStreaming(ctx, params, opt,
		option.WithRequestTimeout(0),
		option.WithMiddleware(whole.Middleware, watch.Middleware, done.Middleware))
	defer func() { _ = stream.Close() }()

	var acc sdk.ChatCompletionAccumulator
	turn = &streamTurn{}
	events := 0
	finished := false
	for stream.Next() {
		events++
		chunk := stream.Current()
		if !acc.AddChunk(chunk) {
			// THE ANSWER CANNOT HOLD THIS CHUNK. The accumulator refuses
			// one naming another completion id than the stream's first,
			// or an index outside its bounds, and leaves the answer
			// without it — so reading on hands the loop an answer short
			// of what the live view showed, or a round whose usage never
			// reached the budget, as though it were whole. The host's
			// failure, retried like a stream cut short; and nothing of
			// the chunk is forwarded or kept, so the reasoning record
			// never holds what the answer does not.
			return nil, nil, false, errStreamUnassembled
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				finished = true
			}
		}
		if len(chunk.Choices) == 0 {
			// A usage-only or keep-alive chunk. Accumulated, not shown.
			continue
		}
		// The text is read off the delta's own JSON, never the typed
		// Content: a host that sends its content as a list of chunks
		// arrives there as the list's JSON text, which is not what the
		// model wrote.
		content, thought := turn.add(chunk.Choices[0].Delta)
		req.Send(llm.Delta{Content: content, Reasoning: thought})
	}
	if err := stream.Err(); err != nil {
		// Classified by the caller exactly as a unary failure is. A stream
		// that dies MID-BODY is a failure of the call, not a short answer:
		// returning what accumulated would hand the loop a truncated
		// response as though the model had finished. A stream the watchdog
		// ended is reported as the stall it was.
		return nil, nil, false, watch.Err(err)
	}
	if events == 0 {
		if whole.Taken() {
			return &answer, nil, true, nil
		}
		// Not a failure of the call — the endpoint simply does not do
		// this. Distinguished so the caller can fall back rather than fail
		// a phase over a capability.
		return nil, nil, false, errNoStream
	}
	if !finished && !done.seen.Load() {
		// A BODY THAT ENDED CLEANLY IS NOT A FINISHED ANSWER. The SDK's
		// decoder reports an orderly EOF — a gateway or proxy closing the
		// response mid-answer — as no error at all, and the stop-reason
		// mapping reads an absent finish_reason as an ordinary end, so a
		// round cut after its first deltas was taken as the whole answer.
		// What says the model finished is the stream's own terminal
		// evidence: a finish_reason on some choice, or the `[DONE]`
		// sentinel (a host that names no reason still ends its stream).
		// Neither means the call failed, and it is retried like the
		// server failure it is. The ""-is-an-end reading stays for unary
		// responses, which arrive whole or not at all.
		return nil, nil, false, errStreamCut
	}
	assembled := acc.ChatCompletion
	return &assembled, turn, false, nil
}

// errNoStream reports an endpoint that accepted a streaming request and
// answered with neither a stream nor a completion.
var errNoStream = errors.New("endpoint did not stream")

// errStreamCut reports a stream whose body ended before its terminal event:
// no choice carried a finish_reason and no `[DONE]` arrived.
var errStreamCut = errors.New("stream ended before its terminal event (no finish_reason and no [DONE])")

// errStreamUnassembled reports a stream that sent a chunk its answer cannot
// hold (see [sdk.ChatCompletionAccumulator.AddChunk]).
var errStreamUnassembled = errors.New("stream sent a chunk its answer cannot hold " +
	"(another completion id than its first, or an index out of bounds)")

// doneWatch notes whether a streamed body carried the `[DONE]` sentinel.
//
// The SDK consumes the sentinel and reports it exactly as it reports a body
// that simply stopped — Next is false and Err is nil either way — so it is
// read off the bytes on their way in, through the same middleware hook the
// idle watchdog uses.
type doneWatch struct{ seen atomic.Bool }

// Middleware wraps the response body in a [doneBody].
func (d *doneWatch) Middleware(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	resp, err := next(req)
	if err == nil && resp != nil && resp.Body != nil {
		resp.Body = &doneBody{ReadCloser: resp.Body, watch: d, tail: []byte("\n")}
	}
	return resp, err
}

// doneBody scans an SSE body for a `data: [DONE]` line.
//
// Matched at the START OF A LINE — a newline before `data:` — and never as a
// bare `[DONE]`, which a model may write inside its own content; a JSON
// payload cannot carry a raw newline, so a line start is the event's own
// field. A read may split the marker, so the last bytes of each read are kept
// and prefixed to the next.
type doneBody struct {
	io.ReadCloser
	watch *doneWatch
	tail  []byte
}

// doneMarkers are the two spellings SSE allows: a space after the colon is
// optional, and the SDK accepts both.
var doneMarkers = [][]byte{[]byte("\ndata: [DONE]"), []byte("\ndata:[DONE]")}

// doneKeep is how many trailing bytes a read carries into the next: one fewer
// than the longest marker, so a marker split anywhere is still seen whole.
const doneKeep = len("\ndata: [DONE]") - 1

func (b *doneBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.watch.seen.Load() {
		b.tail = append(b.tail, p[:n]...)
		for _, marker := range doneMarkers {
			if bytes.Contains(b.tail, marker) {
				b.watch.seen.Store(true)
				break
			}
		}
		if len(b.tail) > doneKeep {
			b.tail = append(b.tail[:0], b.tail[len(b.tail)-doneKeep:]...)
		}
	}
	return n, err
}

// classify turns an SDK failure into the contract's error.
//
// WITH WHAT THE ENDPOINT SAID ([Detail]): the SDK's error names the status
// alone, and the status is all the classification needs — but a reader of the
// error needs the endpoint's reason, which only its fields still carry.
func (p *Provider) classify(err error) *llm.Error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		var header http.Header
		if apiErr.Response != nil {
			header = apiErr.Response.Header
		}
		classified := httpapi.FromStatus(err, p.name, p.model, apiErr.StatusCode, header)
		classified.Detail = Detail(apiErr)
		return classified
	}
	if errors.Is(err, errStreamCut) || errors.Is(err, errStreamUnassembled) {
		// The response opened with 200 and stopped short, or sent what
		// no answer can be assembled from: the server's failure, retried
		// and handed down the chain like one.
		return &llm.Error{Kind: llm.KindServer, Provider: p.name, Model: p.model, Err: err}
	}
	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		// WITH WHAT THE ENDPOINT SAID, as an API error is, and never the
		// SDK's text: its StreamError pastes the error chunk raw, which is
		// the endpoint's own words unredacted.
		return &llm.Error{Kind: streamErrorKind(streamErr), Provider: p.name, Model: p.model,
			Err: &streamFailure{err: err}, Detail: streamDetail(streamErr)}
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

// streamFailure is an error chunk inside a stream as this engine shows it: a
// line of its own, with the SDK's error behind it for errors.Is and errors.As
// and never printed — the chunk's own words travel as [llm.Error.Detail],
// redacted ([streamDetail]).
type streamFailure struct{ err error }

func (e *streamFailure) Error() string { return "the response stream carried an error" }

func (e *streamFailure) Unwrap() error { return e.err }

// streamDetail is what an error chunk SAID — its message and the fields it
// filed it under — as one redacted, bounded line, read the way [Detail] reads
// an API error's body: the OpenAI envelope first, and otherwise whatever the
// chunk's data can honestly yield.
func streamDetail(se *ssestream.StreamError) string {
	var body struct {
		Error struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
			Param   string          `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(se.Event.Data, &body) == nil {
		code := strings.Trim(string(body.Error.Code), `"`)
		if code == "null" {
			code = ""
		}
		if said := httpapi.Said(body.Error.Message,
			httpapi.Filed{Name: "type", Value: body.Error.Type},
			httpapi.Filed{Name: "code", Value: code},
			httpapi.Filed{Name: "param", Value: body.Error.Param}); said != "" {
			return said
		}
	}
	return httpapi.SaidBody("application/json", se.Event.Data)
}

func (p *Provider) params(req llm.Request) (sdk.ChatCompletionNewParams, error) {
	if !req.Effort.Valid() {
		return sdk.ChatCompletionNewParams{}, fmt.Errorf("request effort %q is not a level (want one of low, "+
			"medium, high, xhigh, max, or empty)", req.Effort)
	}
	messages, err := formatMessages(req.Messages, p.binding)
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

// formatMessages renders the conversation for the endpoint whose identity is
// binding. Every turn is built from the neutral view; an assistant turn this
// endpoint wrote also carries back, in the host's own fields, the reasoning it
// came with ([replay]) — and a turn any other endpoint or backend wrote
// carries none, because its fields are not this host's to read. Once a turn
// going back carries reasoning_content, every assistant turn does, the empty
// string where it has none ([speaksReasoningContent]).
func formatMessages(messages []llm.Message, binding string) ([]sdk.ChatCompletionMessageParamUnion, error) {
	records, err := replays(messages, binding)
	if err != nil {
		return nil, err
	}
	emptyReasoning := speaksReasoningContent(records)
	out := make([]sdk.ChatCompletionMessageParamUnion, 0, len(messages))
	for i, m := range messages {
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
			// The reasoning goes back only as the host sent it, and only to
			// the host that sent it — never rebuilt from ReasoningContent or
			// ThinkingBlocks, which are the engine's reading of it and in no
			// host's field. See replay.go.
			extra := map[string]any{}
			if r, ok := records[i]; ok {
				r.into(&msg, extra)
			}
			if _, carried := extra[reasoningContentField]; emptyReasoning && !carried {
				extra[reasoningContentField] = ""
			}
			if len(extra) > 0 {
				msg.SetExtraFields(extra)
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

// completion reads a response into the contract's answer. streamed is what a
// stream kept beside the SDK's accumulator, nil for a response that arrived
// whole — whose message carries it in its own raw JSON.
func (p *Provider) completion(resp *sdk.ChatCompletion, streamed *streamTurn) (*llm.Completion, error) {
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
	var extra kept
	var err error
	if streamed != nil {
		extra, err = streamed.kept(choice.Message)
	} else {
		extra, err = keptFromMessage(choice.Message)
	}
	if err != nil {
		// Only a value this process decoded and could not encode again:
		// the call answered, and a turn that cannot keep its reasoning
		// would go back without it — which is the failure this exists to
		// remove, so it is not handed on as an answer.
		return nil, &llm.Error{Kind: llm.KindServer, Provider: p.name, Model: p.model, Err: err}
	}
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
		ReasoningContent: extra.reasoning,
	}
	if extra.listed {
		// The typed message holds the chunk list's own JSON text here.
		out.Content = extra.text
	}
	if extra.record != nil {
		out.Raw = []json.RawMessage{extra.record}
		out.Binding = p.binding
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
