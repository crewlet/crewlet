package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/otlpenv"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tracing"
)

// In-sandbox telemetry, without handing the box a secret.
//
// # The problem this exists for
//
// A coding agent inside a sandbox emits OpenTelemetry, and the obvious wiring
// — point OTEL_EXPORTER_OTLP_ENDPOINT at the real backend and set
// OTEL_EXPORTER_OTLP_HEADERS to its ingest credential — puts that credential
// inside a box running generated code. It is the company's telemetry backend,
// the header is long-lived, and the box is the least trusted thing in the
// deployment.
//
// So the box exports to the ENGINE, which forwards. The engine adds the real
// auth on the trusted side, and the box holds nothing worth stealing.
//
// # Why the token is in the PATH and signed rather than stored
//
// The box needs to authenticate itself, and every header it could carry is a
// header a leaked box carries too. So the credential is a per-run,
// trace-scoped, short-lived token in the URL — which means the exporter needs
// no header configuration at all.
//
// The token itself is [runtoken], which is where the format, the signature
// and the expiry live — the MCP bridge carries the same credential shape for
// the same reason, and two copies would have to agree about five decisions
// nothing compares.

// OtelTokens mints and validates per-run OTLP tokens.
//
// A thin type over the shared signer, kept because the SUBJECT is this
// receiver's own: an OTLP token is scoped to a TRACE, and naming that at the
// type boundary is what stops a caller passing a run id and getting a token
// the receiver will happily validate into the wrong thing.
type OtelTokens struct{ signer *runtoken.Signer }

// OtelTokenOptions configure [NewOtelTokens]. See [runtoken.Options].
type OtelTokenOptions = runtoken.Options

// NewOtelTokens builds the minter.
func NewOtelTokens(opts OtelTokenOptions) *OtelTokens {
	return &OtelTokens{signer: runtoken.New(opts)}
}

// Mint returns a token scoped to one trace, valid for ttl.
func (t *OtelTokens) Mint(traceID string, ttl time.Duration) string {
	return t.signer.Mint(traceID, ttl)
}

// Validate returns the token's trace id, or empty for one that is forged,
// malformed or expired.
func (t *OtelTokens) Validate(token string) string { return t.signer.Validate(token) }

// OtelReceiver mints a run's endpoint and forwards what the box exports.
type OtelReceiver struct {
	base     string
	tokens   *OtelTokens
	upstream map[otlpenv.Signal]otlpenv.Destination
	http     *http.Client
}

// OtelReceiverOptions configure [NewOtelReceiver].
type OtelReceiverOptions struct {
	// BaseURL is the externally reachable engine API base the SANDBOX
	// exports to. On a fleet split by node.roles that is an ingress node,
	// which need not be the node that mints.
	BaseURL string

	// Tokens mints and verifies. Required.
	Tokens *OtelTokens

	// Upstream is the real OTLP backend, PER SIGNAL, as [otlpenv] resolved
	// it — the same destinations the engine's own exporters resolve, which
	// is what makes a box's spans land beside the turn that started them.
	// Its headers are the backend's ingest credential, applied HERE and
	// never handed to the sandbox; that is the whole point of the hop.
	//
	// A signal with no destination, or one that does not export, ACCEPTS
	// AND DROPS, which is a working configuration rather than a broken one:
	// the engine's own per-turn span still carries the trace, and a
	// deployment with no backend should not have every export fail.
	Upstream map[otlpenv.Signal]otlpenv.Destination

	HTTP *http.Client
}

// OtelForwardTimeout bounds one forwarded payload.
//
// SHORT. Telemetry is the least important thing in flight, and a slow backend
// must not hold the receiver's handlers open — the exporter inside the box
// retries anyway, so a dropped batch costs a gap in a trace rather than data
// nobody can reconstruct.
const OtelForwardTimeout = 10 * time.Second

// OtelAnswerWithin is how long a box's exporter is told to wait for the
// receiver to answer one export ([OtelTimeoutVar]), and what a runner derives
// its agent's own export waits from.
//
// TIED TO [OtelForwardTimeout] rather than picked, because the receiver
// answers only once it has forwarded. An exporter that gives up sooner
// abandons an export the receiver is still forwarding, and its disconnect
// cancels the forward — the batch is lost for having been slow rather than
// refused. The specification's default exporter timeout is ten seconds, the
// forward's own bound, so which of the two gave up first was a race.
const OtelAnswerWithin = OtelForwardTimeout + otelAnswerMargin

// otelAnswerMargin is what an answer takes beyond the forward itself: reading
// the payload off the box's connection, decoding it, and the reply. Two
// seconds carries the largest payload the route accepts (four mebibytes) at
// sixteen megabits a second, far below any network a box shares with the
// engine that mints its endpoint.
const otelAnswerMargin = 2 * time.Second

// OtelTimeoutVar is the standard variable an OTLP exporter reads its
// per-export wait from, in milliseconds. Written into a box's environment
// ([OtelReceiver.RunEnv]); the engine's own exporters take theirs from the
// SDK, which reads the same variable from this process's environment.
const OtelTimeoutVar = "OTEL_EXPORTER_OTLP_TIMEOUT"

// NewOtelReceiver builds the receiver.
func NewOtelReceiver(opts OtelReceiverOptions) (*OtelReceiver, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, fmt.Errorf(
			"sandbox: the OTLP receiver needs the base URL a sandbox can " +
				"reach it on — an endpoint minted against an address the box " +
				"cannot resolve fails silently inside the box")
	}
	if opts.Tokens == nil {
		return nil, fmt.Errorf("sandbox: the OTLP receiver needs a token minter")
	}
	client := opts.HTTP
	if client == nil {
		client = httpx.Client(OtelForwardTimeout)
	}
	// COPIED, headers included, so a caller that goes on to change its
	// map cannot change where — or with what credential — this forwards.
	upstream := make(map[otlpenv.Signal]otlpenv.Destination, len(opts.Upstream))
	for s, d := range opts.Upstream {
		d.Headers = maps.Clone(d.Headers)
		upstream[s] = d
	}
	return &OtelReceiver{
		base:     strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		tokens:   opts.Tokens,
		upstream: upstream,
		http:     client,
	}, nil
}

// EndpointFor mints one run's OTLP endpoint.
//
// The token is IN THE PATH, so the box's exporter needs no headers — which is
// what lets OTEL_EXPORTER_OTLP_HEADERS stay empty inside the sandbox.
func (r *OtelReceiver) EndpointFor(traceID string, ttl time.Duration) string {
	return r.base + "/otlp/" + r.tokens.Mint(traceID, ttl)
}

// Validate reports the trace a token is scoped to, or empty.
func (r *OtelReceiver) Validate(token string) string { return r.tokens.Validate(token) }

// Upstream is where this receiver forwards one signal: the destination it was
// built with, or the zero one — which exports nothing — for a signal it was
// not. A copy, so a caller cannot change what the receiver sends.
func (r *OtelReceiver) Upstream(signal otlpenv.Signal) otlpenv.Destination {
	d := r.upstream[signal]
	d.Headers = maps.Clone(d.Headers)
	return d
}

// Forwards reports whether an accepted payload of this signal goes anywhere.
//
// The receiver with no upstream is a real configuration — accept and drop —
// so this is what an operator surface asks rather than inferring it from a
// nil.
func (r *OtelReceiver) Forwards(signal otlpenv.Signal) bool {
	return r.upstream[signal].Exports()
}

// Forwarding is every signal an accepted payload goes somewhere for, in
// [otlpenv.Signals] order.
func (r *OtelReceiver) Forwarding() []otlpenv.Signal {
	var out []otlpenv.Signal
	for _, s := range otlpenv.Signals {
		if r.Forwards(s) {
			out = append(out, s)
		}
	}
	return out
}

// Forward sends a validated payload to the real backend.
//
// TO THE SIGNAL'S OWN DESTINATION, which is the endpoint the engine's exporter
// would send that signal to — its OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT when
// one is set, the base with the signal path otherwise. Forwarding everything
// to the base with the path appended is what dropped every box's spans on a
// deployment that set only the traces endpoint.
//
// FAILURES ARE LOGGED, NEVER RETURNED as something the caller reports to the
// box. An exporter that gets a 5xx retries, and retries against a backend
// that is down turn one outage into two: the coding run's own traffic
// multiplies while nothing it carries is recoverable anyway.
func (r *OtelReceiver) Forward(ctx context.Context, signal otlpenv.Signal, body []byte, contentType string) {
	upstream := r.upstream[signal]
	if !upstream.Exports() {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		upstream.Endpoint, bytes.NewReader(body))
	if err != nil {
		log.WarnContext(ctx, "sandbox_otel_forward_failed", "signal", signal, "error", err.Error())
		return
	}
	for k, v := range upstream.Headers {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := r.http.Do(req)
	if err != nil {
		log.WarnContext(ctx, "sandbox_otel_forward_failed", "signal", signal, "error", err.Error())
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		log.WarnContext(ctx, "sandbox_otel_forward_refused",
			"signal", signal, "status", resp.StatusCode)
	}
}

// OtelSigningKey derives the token key every process must share.
//
// FROM THE KEYRING, which is the one secret every Crewlet process already
// loads — and never from the database, which a token check must not depend
// on: the check runs on the request path of an endpoint that is deliberately
// reachable without other credentials.
//
// With no keyring the key is RANDOM PER PROCESS. A single node is unaffected,
// because the node that mints also verifies. A fleet gets a loud warning
// rather than a deterministic key invented from non-secret material, which
// would let anyone who can reach the endpoint forge one.
func OtelSigningKey(material []string) []byte {
	if len(material) == 0 {
		log.Warn("sandbox_otel_signing_key_ephemeral",
			"detail", "no Tier A secrets.keys, so OTLP tokens are signed with "+
				"a per-process key: on a fleet, a box that exports to any node "+
				"but the one that minted its token is refused. `crewlet secrets "+
				"keygen` fixes it")
		return nil
	}
	return runtoken.KeyFrom(OtelKeyDomain, material)
}

// OtelKeyDomain separates this endpoint's tokens from the tool bridge's.
//
// Without a domain, a token minted for one would validate at the other: both
// are HMACs over the same fleet key, and the subject is just a string.
const OtelKeyDomain = "crewlet.otlp.v1"

// RunEnv is the OTel environment one run's box receives.
//
// # What is in it, and what deliberately is not
//
// The ENDPOINT carries the run's own token in its path, so
// OTEL_EXPORTER_OTLP_HEADERS stays EMPTY — set explicitly rather than left
// unset, because an exporter reads it from the box's own environment
// otherwise, and a box image or template can bake one in (a Dockerfile's ENV,
// an E2B template's environment): a value written there would be sent to the
// engine with every export. An empty value is the difference between "no
// credential" and somebody else's.
//
// The PROTOCOL is http/protobuf, the one the receiver's route speaks, and the
// TIMEOUT is [OtelAnswerWithin], so the exporter waits as long as the receiver
// may take to answer — see there.
//
// The RESOURCE ATTRIBUTES are non-secret routing facts — which turn, which
// seat — so a span from inside a box lands under the turn that started it
// rather than free-floating.
//
// TRACEPARENT is what nests them: the engine's per-turn span is the parent,
// and without it the box's spans form a second, unrelated trace that nobody
// finds. Its FLAG is the turn's own sampling decision, rendered by the
// propagator ([tracing.Traceparent]) — `-00` for a run launched from an
// unsampled turn, which a box's SDK reads as "record nothing", exactly as the
// engine's own spans of that trace record nothing. It used to be `-01` for
// every run, so a trace the operator's sampler had decided against was
// exported from inside the box with no parent above it.
//
// What switches an agent's export ON is not here: that is the runner's to
// declare ([Runner.Telemetry]), and the launch adds it only beside an
// endpoint this minted.
func (r *OtelReceiver) RunEnv(ctx context.Context, trace events.TraceContext,
	turnID, handle string, ttl time.Duration,
) map[string]string {
	if trace.TraceID == "" {
		// NO TRACE, NO ENDPOINT. A token scoped to an empty trace would
		// authenticate every run's export as every other run's, which is
		// the one property the scoping exists for.
		return nil
	}
	env := map[string]string{
		otlpenv.EndpointVar: r.EndpointFor(trace.TraceID, ttl),
		otlpenv.ProtocolVar: string(otlpenv.HTTPProtobuf),
		otlpenv.HeadersVar:  "",
		OtelTimeoutVar:      strconv.FormatInt(OtelAnswerWithin.Milliseconds(), 10),
		"OTEL_RESOURCE_ATTRIBUTES": strings.Join([]string{
			"crewlet.turn_id=" + turnID,
			"crewlet.agent_handle=" + handle,
		}, ","),
	}
	if parent := tracing.Traceparent(ctx, trace); parent != "" {
		env[tracing.TraceparentVar] = parent
	}
	return env
}

// The environment the receiver is configured from.
//
// ENV RATHER THAN TIER A, and deliberately: the receiver's address is a
// property of the DEPLOYMENT's network — which host a sandbox can reach the
// engine on — rather than of the company or of this node's identity, and the
// upstream is the standard OTel spelling every collector's own documentation
// uses, resolved by [otlpenv] exactly as the engine's own exporters resolve
// it. An operator wiring a collector should not have to translate it into a
// Crewlet-shaped block.
const (
	// OtelReceiverURLVar is the engine API base a SANDBOX can reach. Unset
	// means no in-box telemetry, which is the ordinary configuration.
	OtelReceiverURLVar = "CREWLET_SANDBOX_OTEL_RECEIVER_URL"
)

// BuildOtelReceiver is THE construction path, called by every engine.
//
// A run's endpoint is minted by the engine that launched it, and the token is
// verified by whichever node the box exports to, which on a fleet need not be
// the same one. So every node builds its receiver from the same environment
// and the same Tier A keyring: a node keyed differently from the one that
// minted answers 401 to every export while its config looks complete, which is
// the failure the signed, stateless token exists to prevent. A node with the
// variable unset builds none, and its route is absent rather than refusing.
//
// THE UPSTREAM IS RESOLVED BY [otlpenv.ResolveAll], the resolver the engine's
// exporters use, for every signal — so a value that can never work (an
// endpoint that is not an http(s) URL, a protocol or an exporter this build
// does not have) is refused here by name, as the engine's own exporter refuses
// it, rather than accepted and forwarded nowhere.
//
// keyMaterial is the Tier A keyring, which every process already loads. Nil
// or empty takes a per-process key: correct for a single process, and warned
// about because it cannot work across two.
func BuildOtelReceiver(env func(string) string, keyMaterial []string) (*OtelReceiver, error) {
	if env == nil {
		return nil, nil
	}
	base := strings.TrimSpace(env(OtelReceiverURLVar))
	if base == "" {
		return nil, nil
	}
	upstream, err := otlpenv.ResolveAll(env)
	if err != nil {
		return nil, fmt.Errorf("sandbox: the OTLP receiver's upstream: %w", err)
	}
	var resolved []otlpenv.Destination
	for _, s := range otlpenv.Signals {
		resolved = append(resolved, upstream[s])
	}
	for _, d := range otlpenv.DroppedOf(resolved...) {
		log.Warn("sandbox_otel_header_dropped", "var", d.Var, "entry", d.Entry,
			"header", d.Header, "reason", d.Reason)
	}
	return NewOtelReceiver(OtelReceiverOptions{
		BaseURL:  base,
		Tokens:   NewOtelTokens(OtelTokenOptions{Key: OtelSigningKey(keyMaterial)}),
		Upstream: upstream,
	})
}
