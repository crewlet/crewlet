package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// In-sandbox telemetry, without handing the box a secret.
//
// # The problem this exists for
//
// A harness inside a sandbox emits OpenTelemetry, and the obvious wiring
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

// NewOtelTokens builds the minter, or refuses a keyring that cannot sign for
// the fleet ([runtoken.ErrNoKeyring]).
func NewOtelTokens(opts OtelTokenOptions) (*OtelTokens, error) {
	signer, err := runtoken.New(opts)
	if err != nil {
		return nil, fmt.Errorf("sandbox: telemetry tokens: %w", err)
	}
	return &OtelTokens{signer: signer}, nil
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
	host     string
	tokens   *OtelTokens
	upstream string
	headers  map[string]string
	http     *http.Client
}

// OtelReceiverOptions configure [NewOtelReceiver].
type OtelReceiverOptions struct {
	// BaseURL is the externally reachable engine API base the SANDBOX
	// exports to. On a fleet split by node.roles that is an ingress node,
	// which need not be the node that mints. Empty mints no endpoint for a
	// box that is not on this node's host.
	BaseURL string

	// HostURL is where a box on THIS NODE'S OWN HOST — the `direct` cell —
	// exports: the node's own listener that serves the receiver, on
	// loopback, e.g. "http://127.0.0.1:8080". The same address the tool
	// bridge hands such a box (internal/api/mcpbridge's Options.HostURL),
	// derived by the process that bound the listener and PREFERRED to
	// BaseURL for such a box, because loopback never leaves the host. Set
	// only by [BuildOtelReceiver] on a node that forwards — see there.
	HostURL string

	// Tokens mints and verifies. Required.
	Tokens *OtelTokens

	// UpstreamEndpoint is the real OTLP backend. Empty accepts and drops,
	// which is a working configuration rather than a broken one: the
	// engine's own per-turn span still carries the trace, and a deployment
	// with no backend should not have every export fail.
	UpstreamEndpoint string

	// UpstreamHeaders is the backend's ingest credential, applied HERE and
	// never handed to the sandbox. That is the whole point of the hop.
	UpstreamHeaders map[string]string

	HTTP *http.Client
}

// OtelForwardTimeout bounds one forwarded payload.
//
// SHORT. Telemetry is the least important thing in flight, and a slow backend
// must not hold the receiver's handlers open — the exporter inside the box
// retries anyway, so a dropped batch costs a gap in a trace rather than data
// nobody can reconstruct.
const OtelForwardTimeout = 10 * time.Second

// NewOtelReceiver builds the receiver.
func NewOtelReceiver(opts OtelReceiverOptions) (*OtelReceiver, error) {
	if strings.TrimSpace(opts.BaseURL) == "" && strings.TrimSpace(opts.HostURL) == "" {
		return nil, fmt.Errorf(
			"sandbox: the OTLP receiver needs a URL a sandbox can reach it on — " +
				"an endpoint minted against an address the box cannot resolve " +
				"fails silently inside the box")
	}
	if opts.Tokens == nil {
		return nil, fmt.Errorf("sandbox: the OTLP receiver needs a token minter")
	}
	client := opts.HTTP
	if client == nil {
		client = httpx.Client(OtelForwardTimeout)
	}
	headers := make(map[string]string, len(opts.UpstreamHeaders))
	for k, v := range opts.UpstreamHeaders {
		headers[k] = v
	}
	return &OtelReceiver{
		base:     strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		host:     strings.TrimRight(strings.TrimSpace(opts.HostURL), "/"),
		tokens:   opts.Tokens,
		upstream: strings.TrimRight(strings.TrimSpace(opts.UpstreamEndpoint), "/"),
		headers:  headers,
		http:     client,
	}, nil
}

// Reaches is the base URL a box exports to — this node's own listener for a
// box on its host where it has one ([OtelReceiverOptions.HostURL]), and
// [OtelReceiverURLVar]'s address otherwise — or "" where it has none.
//
// THE BRIDGE'S RULE, cell for cell (internal/api/mcpbridge's Bridge.Reaches):
// a `container` box is not on this host's loopback, whatever its runtime
// delivers, and dials the fleet's address alone, which [EngineAddress] then
// writes as that box reaches it.
func (r *OtelReceiver) Reaches(onHost bool) string {
	if onHost && r.host != "" {
		return r.host
	}
	return r.base
}

// EndpointFor mints one run's OTLP endpoint, for a box on this node's host or
// elsewhere ([OtelReceiver.Reaches]), or "" where no address reaches that box.
//
// The token is IN THE PATH, so the box's exporter needs no headers — which is
// what lets OTEL_EXPORTER_OTLP_HEADERS stay empty inside the sandbox.
func (r *OtelReceiver) EndpointFor(traceID string, onHost bool, ttl time.Duration) string {
	base := r.Reaches(onHost)
	if base == "" {
		return ""
	}
	return base + "/otlp/" + r.tokens.Mint(traceID, ttl)
}

// Validate reports the trace a token is scoped to, or empty.
func (r *OtelReceiver) Validate(token string) string { return r.tokens.Validate(token) }

// Forwards reports whether an accepted payload goes anywhere.
//
// The receiver with no upstream is a real configuration — accept and drop —
// so this is what an operator surface asks rather than inferring it from a
// nil.
func (r *OtelReceiver) Forwards() bool { return r.upstream != "" }

// Forward sends a validated payload to the real backend.
//
// FAILURES ARE LOGGED, NEVER RETURNED as something the caller reports to the
// box. An exporter that gets a 5xx retries, and retries against a backend
// that is down turn one outage into two: the box's own traffic
// multiplies while nothing it carries is recoverable anyway.
func (r *OtelReceiver) Forward(ctx context.Context, signal string, body []byte, contentType string) {
	if r.upstream == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.upstream+"/v1/"+signal, bytes.NewReader(body))
	if err != nil {
		log.WarnContext(ctx, "sandbox_otel_forward_failed", "signal", signal, "error", err.Error())
		return
	}
	for k, v := range r.headers {
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

// OtelSignals are the OTLP paths this receiver accepts.
//
// NAMED rather than taking whatever the last path segment says: the segment
// is forwarded into an upstream URL, so an unchecked one is a caller choosing
// part of the address the engine's own credential is sent to.
var OtelSignals = []string{"traces", "metrics", "logs"}

// ValidSignal reports a signal this receiver forwards.
func ValidSignal(signal string) bool { return slices.Contains(OtelSignals, signal) }

// ParseOtelHeaders reads the OTEL_EXPORTER_OTLP_HEADERS `k=v,k2=v2` form.
//
// It is the UPSTREAM backend's auth, added on the trusted side. Parsed here
// rather than passed through as a string so the one place it is read is the
// one place it is understood.
func ParseOtelHeaders(raw string) map[string]string {
	out := map[string]string{}
	for pair := range strings.SplitSeq(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if key = strings.TrimSpace(key); !ok || key == "" {
			continue
		}
		out[key] = strings.TrimSpace(value)
	}
	return out
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
// unset, because an exporter reads it from its ambient environment otherwise,
// and a box's ambient environment is not only what the engine hands it. The
// engine hands a box none of its own (see internal/hostbox), but an image or
// an E2B template may bake in a value of its own, and so may a `-e` an
// operator put in `local.run_args`. An empty value is the difference between
// "no credential" and one this run's exporter was never meant to present —
// the endpoint's token is the whole of what it needs.
//
// The RESOURCE ATTRIBUTES are non-secret routing facts — which turn, which
// seat — so a span from inside a box lands under the turn that started it
// rather than free-floating.
//
// TRACEPARENT is what nests them: the engine's per-turn span is the parent,
// and without it the box's spans form a second, unrelated trace that nobody
// finds.
//
// onHost is whether the run's box is on this node's own host, which picks the
// address it exports to ([OtelReceiver.Reaches]). A box no address reaches
// gets NOTHING, not an environment naming an endpoint it cannot dial: its
// exporter then exports nowhere, which is what a box with no receiver did all
// along.
func (r *OtelReceiver) RunEnv(traceID, spanID, turnID, handle string, onHost bool,
	ttl time.Duration,
) map[string]string {
	if traceID == "" {
		// NO TRACE, NO ENDPOINT. A token scoped to an empty trace would
		// authenticate every run's export as every other run's, which is
		// the one property the scoping exists for.
		return nil
	}
	endpoint := r.EndpointFor(traceID, onHost, ttl)
	if endpoint == "" {
		return nil
	}
	env := map[string]string{
		otelBoxEndpointVar:            endpoint,
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		"OTEL_EXPORTER_OTLP_HEADERS":  "",
		"OTEL_RESOURCE_ATTRIBUTES": strings.Join([]string{
			"crewlet.turn_id=" + turnID,
			"crewlet.agent_handle=" + handle,
		}, ","),
	}
	if spanID != "" {
		// W3C traceparent: version-traceid-spanid-flags, sampled.
		env["TRACEPARENT"] = "00-" + traceID + "-" + spanID + "-01"
	}
	return env
}

// The environment the receiver is configured from.
//
// ENV RATHER THAN TIER A, and deliberately: the receiver's address is a
// property of the DEPLOYMENT's network — which host a sandbox can reach the
// engine on — rather than of the company or of this node's identity, and the
// upstream pair is the standard OTel spelling every collector's own
// documentation uses. An operator wiring a collector should not have to
// translate it into a Crewlet-shaped block.
const (
	// OtelReceiverURLVar is the engine API base a SANDBOX can reach: the
	// fleet's address for the receiver, for a box that is not on its
	// launching node's host. Unset, such a box gets no in-box telemetry,
	// which is the ordinary configuration — and a box on the host gets its
	// node's own listener where the node forwards ([BuildOtelReceiver]).
	OtelReceiverURLVar = "CREWLET_SANDBOX_OTEL_RECEIVER_URL"

	// OtelUpstreamEndpointVar and OtelUpstreamHeadersVar are the real
	// backend, applied on the trusted side only.
	OtelUpstreamEndpointVar = "OTEL_EXPORTER_OTLP_ENDPOINT"
	OtelUpstreamHeadersVar  = "OTEL_EXPORTER_OTLP_HEADERS"
)

// otelBoxEndpointVar is where a box's exporters read the run's endpoint: the
// same standard variable the engine reads its upstream from, set inside the
// box to the receiver instead ([OtelReceiver.RunEnv]).
const otelBoxEndpointVar = "OTEL_EXPORTER_OTLP_ENDPOINT"

// BuildOtelReceiver is THE construction path, called by every engine.
//
// A run's endpoint is minted by the engine that launched it, and the token is
// verified by whichever node the box exports to, which on a fleet need not be
// the same one. So every node builds its receiver from the same environment
// and the same Tier A keyring: a node keyed differently from the one that
// minted answers 401 to every export while its config looks complete, which is
// the failure the signed, stateless token exists to prevent.
//
// hostURL is where a box on this node's own host reaches this node's listener
// on loopback ([OtelReceiverOptions.HostURL]), derived by whoever bound it, ""
// where nothing was bound. It is taken ONLY WHERE THIS NODE FORWARDS — where
// [OtelUpstreamEndpointVar] names a backend — because the node a box on the
// host exports to is this one, and what reaches a node with no upstream is
// accepted and dropped: deriving it there would turn on an export from every
// box on the host that goes nowhere, and would send a box away from a fleet
// address the operator set ([OtelReceiverURLVar]) whose node does forward.
// So the deployment that sets OTEL_EXPORTER_OTLP_ENDPOINT for the engine's
// own spans — the standard variable, which this node already reads — has its
// direct boxes' telemetry beside them, with no address to publish.
//
// A node with neither the variable nor a host address it forwards from builds
// none, and its route is absent rather than refusing.
//
// material is the Tier A keyring, which every process already loads — FROM THE
// KEYRING, the one secret every Crewlet process holds, and never from the
// database, which a token check on an endpoint reachable without other
// credentials must not depend on. One that cannot sign for the fleet is
// refused rather than given a per-process key: that key would validate at the
// minting node and nowhere else, so a box exporting to any other node would be
// refused with its config looking complete.
func BuildOtelReceiver(env func(string) string, material runtoken.Material,
	hostURL string,
) (*OtelReceiver, error) {
	if env == nil {
		return nil, nil
	}
	base := strings.TrimSpace(env(OtelReceiverURLVar))
	upstream := strings.TrimSpace(env(OtelUpstreamEndpointVar))
	host := ""
	if upstream != "" {
		host = strings.TrimSpace(hostURL)
	}
	if base == "" && host == "" {
		return nil, nil
	}
	tokens, err := NewOtelTokens(OtelTokenOptions{
		Domain: OtelKeyDomain, Material: material,
	})
	if err != nil {
		return nil, err
	}
	return NewOtelReceiver(OtelReceiverOptions{
		BaseURL:          base,
		HostURL:          host,
		Tokens:           tokens,
		UpstreamEndpoint: upstream,
		UpstreamHeaders:  ParseOtelHeaders(env(OtelUpstreamHeadersVar)),
	})
}
