package sandbox

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/crewlet/crewlet/internal/events"
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
// auth on the trusted side, and the box holds nothing worth stealing. It
// forwards TRACES and nothing else — what the runners declare a box exports
// (see otelwire.go).
//
// # Why the token is in the PATH and signed rather than stored
//
// The box needs to authenticate itself, and every header it could carry is a
// header a leaked box carries too. So the credential is a per-run token in
// the URL — which means the exporter needs no header configuration at all —
// minted for one run, scoped to the TRACE that run was launched in, and
// expiring a day after it was minted ([OtelTokenLifetime]).
//
// # What the scope holds, and what it does not
//
// The token names the trace the run was launched in, and the receiver holds
// every export to it ([OtelReceiver.Accept]): a span in any other trace is
// refused and never forwarded, and so is every metric and log record, which
// the receiver does not serve at all. So a token copied out of a box cannot
// put a span into any other trace.
//
// THE SCOPE IS THE TRACE, NOT THE RUN. A trace can hold other seats' turns
// and coding runs beside this one — an A2A ask and the turn that answers it,
// every seat one webhook delivery woke — and a token admits spans anywhere in
// it, those runs' parts included. Per trace is the finest scope this verifier
// can hold: telling this run's spans from another's in the same trace needs
// their ancestry, which a stateless check on whichever node a box reaches
// does not have across exports; and minting each run a trace of its own would
// undo the nesting under the launching turn that the receiver exists to give.
//
// So what a token is worth, for its day, is spans in its run's trace —
// junk in the company's backend at worst, through the receiver, never the
// backend's credential — and that is the exposure the lifetime bounds.
//
// The token itself is [runtoken], which is where the format, the signature
// and the expiry live — the MCP bridge carries the same credential shape for
// the same reason, and two copies would have to agree about five decisions
// nothing compares.

// OtelTokens mints and validates per-run OTLP tokens.
//
// A thin type over the shared signer, kept because the SUBJECT is this
// receiver's own: an OTLP token is scoped to a TRACE — the receiver refuses
// what it carries from any other ([OtelReceiver.Accept]) — and naming that at
// the type boundary is what stops a caller passing a run id and getting a
// token whose every span is refused.
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

// OtelReceiver mints a run's endpoint and forwards the traces the box exports.
//
// It holds a CONNECTION to a gRPC upstream once one has forwarded, for the
// rest of its life — a forward is one call on it, not a dial and a TLS
// handshake per batch — so whoever built it closes it ([OtelReceiver.Close]).
type OtelReceiver struct {
	base     string
	tokens   *OtelTokens
	upstream otlpenv.Destination
	// forward is nil when the upstream does not export.
	forward otelForwarder
	// budget is what every export decoding at once may hold together.
	budget *otelBudget
}

// OtelReceiverOptions configure [NewOtelReceiver].
type OtelReceiverOptions struct {
	// BaseURL is the externally reachable engine API base the SANDBOX
	// exports to. On a fleet split by node.roles that is an ingress node,
	// which need not be the node that mints.
	BaseURL string

	// Tokens mints and verifies. Required.
	Tokens *OtelTokens

	// Upstream is where a box's traces go: the traces destination as
	// [otlpenv] resolved it — the one the engine's own span exporter
	// resolves, which is what makes a box's spans land beside the turn that
	// started them. Its headers are the backend's ingest credential,
	// applied HERE and never handed to the sandbox; that is the whole point
	// of the hop.
	//
	// One that does not export ACCEPTS AND DROPS, which is a working
	// configuration rather than a broken one: the engine's own per-turn
	// span still carries the trace, and a deployment with no backend should
	// not have every export fail.
	Upstream otlpenv.Destination
}

// OtelForwardTimeout bounds one forwarded payload.
//
// SHORT. Telemetry is the least important thing in flight, and a slow backend
// must not hold the receiver's handlers open — the exporter inside the box
// retries anyway, so a dropped batch costs a gap in a trace rather than data
// nobody can reconstruct.
//
// ITS OWN, and not the timeout [otlpenv] resolves into [OtelReceiverOptions]'s
// Upstream from OTEL_EXPORTER_OTLP_TIMEOUT: that bounds the engine's own
// exports, while a forward is waited on synchronously by a box whose exporter
// is told how long to wait ([OtelAnswerWithin]) from this value, at a launch
// that cannot know what a later forward will be held to. So the receiver
// resolves the timeout alike, as the agreement test holds, and does not honour
// it.
const OtelForwardTimeout = 10 * time.Second

// OtelAnswerWithin is how long a box's exporter is told to wait for the
// receiver to answer one export ([otlpenv.TimeoutVar]), and what a runner derives
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
// the payload off the box's connection, decoding it, and the reply — two
// seconds, which carries the largest payload the route accepts (four
// mebibytes) at sixteen megabits a second, far below any network a box shares
// with the engine that mints its endpoint — and waiting for the decoding
// budget, which is [otelBudgetWait]'s one second.
const otelAnswerMargin = 2*time.Second + otelBudgetWait

// NewOtelReceiver builds the receiver, and its forwarder when the upstream
// exports.
//
// A FORWARDER SPEAKS ITS DESTINATION'S PROTOCOL, compression and TLS — the
// engine's own exporter's, since both resolve through [otlpenv] — so a box's
// export reaches a gRPC collector as a gRPC Export call and an https one
// verified against the certificate the environment names. A gRPC connection
// is made on the first forward, so building a receiver dials nothing and
// starts nothing.
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
	if s := opts.Upstream.Signal; s != "" && s != otlpenv.Traces {
		return nil, fmt.Errorf("sandbox: the OTLP receiver forwards traces, and was "+
			"handed the %s destination", s)
	}
	upstream := opts.Upstream
	// COPIED, headers included, so a caller that goes on to change its
	// map cannot change where — or with what credential — this forwards.
	upstream.Headers = maps.Clone(upstream.Headers)
	r := &OtelReceiver{
		base:     strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		tokens:   opts.Tokens,
		upstream: upstream,
		budget:   newOtelBudget(otelBudgetBytes),
	}
	if upstream.Exports() {
		f, err := newOtelForwarder(upstream)
		if err != nil {
			return nil, fmt.Errorf("sandbox: the OTLP receiver's upstream: %w", err)
		}
		r.forward = f
	}
	return r, nil
}

// Close releases the receiver's upstream connections. Exports accepted after
// it are answered as ever and forwarded nowhere, with a warning each.
func (r *OtelReceiver) Close() error {
	if r.forward == nil {
		return nil
	}
	return r.forward.close()
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

// Upstream is where this receiver forwards a box's traces: the destination it
// was built with. A copy, so a caller cannot change what the receiver sends.
func (r *OtelReceiver) Upstream() otlpenv.Destination {
	d := r.upstream
	d.Headers = maps.Clone(d.Headers)
	return d
}

// Forwards reports whether an accepted export goes anywhere.
//
// The receiver with no upstream is a real configuration — accept and drop —
// so this is what an operator surface asks rather than inferring it from a
// nil.
func (r *OtelReceiver) Forwards() bool { return r.upstream.Exports() }

// Accept takes one traces export a box made under the token for trace,
// forwards it, and answers what the route sends the box.
//
// DECODED FIRST, so an export the receiver cannot read is refused as what it
// is — [ErrOtelUnsupported], [ErrOtelMalformed], [ErrOtelTooLarge] — which an
// exporter takes as final rather than retrying. See otelwire.go for why the
// receiver decodes at all.
//
// AND WEIGHED BEFORE IT IS DECODED: what decoding it would hold is measured
// first, refused past [MaxOtelDecoded], and reserved from the receiver's
// budget until the forward is done — or answered [ErrOtelBusy] when the
// budget is held by other exports for longer than the receiver can wait. See
// otelcost.go.
//
// TO THE TRACES DESTINATION, which is the endpoint the engine's span exporter
// sends to — OTEL_EXPORTER_OTLP_TRACES_ENDPOINT when it is set, the base with
// `/v1/traces` otherwise. Forwarding to the base with the path appended,
// whatever else was set, is what dropped every box's spans on a deployment
// that set only the traces endpoint.
//
// A FORWARD'S FAILURE IS LOGGED, NEVER RETURNED as something the caller
// reports to the box. An exporter that gets a 5xx retries, and retries against
// a backend that is down turn one outage into two: the coding run's own
// traffic multiplies while nothing it carries is recoverable anyway. So an
// export that decodes is answered as accepted whatever the backend did.
//
// AND HELD TO ITS TOKEN'S TRACE. A span in any other trace is removed before
// the forward and reported back to the box as a partial success, which an
// exporter neither retries nor fails its batch for. The token is minted for
// the trace the run was launched in, and the box is handed that trace's
// TRACEPARENT, so every span a coding agent records descends from it
// (measured: every Claude Code span of a run, tool calls included); what names
// another trace was written by something in the box other than the agent.
// The scope is that TRACE, which other seats' turns and runs can share — see
// [OtelTokens] and the package doc.
func (r *OtelReceiver) Accept(ctx context.Context, trace string, e OtelExport) (OtelReply, error) {
	hold := r.budget.hold()
	defer hold.release()
	decoded, err := decodeExport(ctx, e, hold)
	if err != nil {
		return OtelReply{}, err
	}
	resp := &coltracepb.ExportTraceServiceResponse{}
	removed, kept := scopeToTrace(decoded.request, traceBytes(trace))
	if removed > 0 {
		why := fmt.Sprintf("%d span(s) are in a trace other than the one this "+
			"endpoint was minted for, and were not forwarded", removed)
		resp = outOfScope(removed, why)
		log.WarnContext(ctx, "sandbox_otel_out_of_scope", "trace_id", trace, "removed", removed)
	}
	if kept > 0 {
		r.forwardTo(ctx, decoded.request)
	}
	return encodeReply(resp, decoded.mediaType)
}

// traceBytes is a token's trace as the id OTLP carries, or nil for a subject
// that is not one — which then scopes every record out, rather than none.
func traceBytes(trace string) []byte {
	id, err := hex.DecodeString(trace)
	if err != nil || len(id) != 16 {
		return nil
	}
	return id
}

// forwardTo sends a decoded export upstream, bounded by [OtelForwardTimeout].
func (r *OtelReceiver) forwardTo(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) {
	if r.forward == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, OtelForwardTimeout)
	defer cancel()
	resp, err := r.forward.send(ctx, req)
	if refused := (otelRefused{}); errors.As(err, &refused) {
		log.WarnContext(ctx, "sandbox_otel_forward_refused", "status", refused.status)
		return
	}
	if err != nil {
		log.WarnContext(ctx, "sandbox_otel_forward_failed", "error", err.Error())
		return
	}
	// A PARTIAL SUCCESS is the backend saying it dropped part of what it
	// was sent and why, on a 200 — the one refusal an exporter never
	// retries and so never reports twice.
	if p := resp.GetPartialSuccess(); p.GetRejectedSpans() > 0 || p.GetErrorMessage() != "" {
		log.WarnContext(ctx, "sandbox_otel_forward_partial",
			"rejected", p.GetRejectedSpans(), "reason", p.GetErrorMessage())
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
// engine with every export.
//
// The PROTOCOL is http/protobuf, the one the receiver's route speaks, and the
// TIMEOUT is [OtelAnswerWithin], so the exporter waits as long as the receiver
// may take to answer — see there.
//
// EACH SIGNAL'S OWN FORM OF THOSE FOUR IS WRITTEN EMPTY, for every signal the
// specification names ([otlpenv.Specified]). An exporter reads
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT — and the signal's _HEADERS, _PROTOCOL
// and _TIMEOUT — BEFORE the base variable, so one an image bakes in decides
// where a box's spans go and what they carry whatever the base says. Measured
// against Claude Code 2.1.300 with only the base variables written: a baked
// traces endpoint sent every span to the image's collector and none to the
// receiver, a baked traces header reached the engine on every export, a baked
// `grpc` lost every export in silence against a route that speaks HTTP, and a
// baked timeout gave up before the receiver could answer.
//
// EMPTY, AND NOT THE MINTED VALUES. The specification reads an empty variable
// as unset, and so does every reader involved — the Go SDK, OpenTelemetry JS,
// Claude Code's own protocol read, [otlpenv] — so each signal falls back to the
// base variable: the engine's, or the one an operator's role.sandbox.env writes
// over it. Written with the minted values instead, they would outrank an
// operator who points a seat at their own collector with the BASE variable,
// which is the documented way to. Every signal and not just traces, because an
// operator may switch a box's metrics or logs back on, and they would then go
// wherever the image said.
//
// ALL OF THIS IS AN ENVIRONMENT, and an environment is not all an agent reads:
// one that reads a settings file above it can be pointed elsewhere by a file
// the image carries. What a runner can do about that is its own
// ([Runner.Telemetry]).
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
	if trace.TraceID == "" || trace.SpanID == "" {
		// NO PARENT, NO ENDPOINT. A token scoped to an empty trace would
		// authenticate every run's export as every other run's, which is
		// the one property the scoping exists for; and a run with a trace
		// but no span to nest under is handed no TRACEPARENT, so its
		// agent would root traces of its own — every span of which the
		// receiver refuses as out of the token's scope.
		return nil
	}
	env := map[string]string{
		otlpenv.EndpointVar: r.EndpointFor(trace.TraceID, ttl),
		otlpenv.ProtocolVar: string(otlpenv.HTTPProtobuf),
		otlpenv.HeadersVar:  "",
		otlpenv.TimeoutVar:  strconv.FormatInt(OtelAnswerWithin.Milliseconds(), 10),
		"OTEL_RESOURCE_ATTRIBUTES": strings.Join([]string{
			"crewlet.turn_id=" + turnID,
			"crewlet.agent_handle=" + handle,
		}, ","),
	}
	for _, s := range otlpenv.Specified {
		for _, name := range []string{s.EndpointVar(), s.HeadersVar(), s.ProtocolVar(), s.TimeoutVar()} {
			env[name] = ""
		}
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
// THE UPSTREAM IS RESOLVED BY [otlpenv.Resolve], for traces — the signal and
// the resolver the engine's span exporter uses — so a value that can never
// work (an endpoint that is not an http(s) URL, a protocol or an exporter
// this build does not have) is refused here by name, as the engine's own
// exporter refuses it, rather than accepted and forwarded nowhere.
//
// keyMaterial is the Tier A keyring, which every process already loads. Nil
// or empty takes a per-process key: correct for a single process, and warned
// about because it cannot work across two. ctx carries the boot's trace onto
// what this logs.
func BuildOtelReceiver(ctx context.Context, env func(string) string, keyMaterial []string) (*OtelReceiver, error) {
	if env == nil {
		return nil, nil
	}
	base := strings.TrimSpace(env(OtelReceiverURLVar))
	if base == "" {
		return nil, nil
	}
	upstream, err := otlpenv.Resolve(env, otlpenv.Traces)
	if err != nil {
		return nil, fmt.Errorf("sandbox: the OTLP receiver's upstream: %w", err)
	}
	otlpenv.ReportDropped(ctx, log, otlpenv.ReaderSandboxForwarder, upstream)
	return NewOtelReceiver(OtelReceiverOptions{
		BaseURL:  base,
		Tokens:   NewOtelTokens(OtelTokenOptions{Key: OtelSigningKey(keyMaterial)}),
		Upstream: upstream,
	})
}
