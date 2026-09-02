package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/runtoken"
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
	upstream string
	headers  map[string]string
	http     *http.Client
}

// OtelReceiverOptions configure [NewOtelReceiver].
type OtelReceiverOptions struct {
	// BaseURL is the externally reachable engine API base the SANDBOX
	// exports to. In a split deployment that is the API process, which is
	// not the process that mints.
	BaseURL string

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
		client = &http.Client{Timeout: OtelForwardTimeout}
	}
	headers := make(map[string]string, len(opts.UpstreamHeaders))
	for k, v := range opts.UpstreamHeaders {
		headers[k] = v
	}
	return &OtelReceiver{
		base:     strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		tokens:   opts.Tokens,
		upstream: strings.TrimRight(strings.TrimSpace(opts.UpstreamEndpoint), "/"),
		headers:  headers,
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
// that is down turn one outage into two: the coding run's own traffic
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
func ValidSignal(signal string) bool {
	for _, known := range OtelSignals {
		if signal == known {
			return true
		}
	}
	return false
}

// ParseOtelHeaders reads the OTEL_EXPORTER_OTLP_HEADERS `k=v,k2=v2` form.
//
// It is the UPSTREAM backend's auth, added on the trusted side. Parsed here
// rather than passed through as a string so the one place it is read is the
// one place it is understood.
func ParseOtelHeaders(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if key = strings.TrimSpace(key); !ok || key == "" {
			continue
		}
		out[key] = strings.TrimSpace(value)
	}
	return out
}

// OtelSigningKey derives the token key every process must share.
//
// FROM THE KEYRING, which is the one secret every Crewlet process already
// loads — and never from the database, which a token check must not depend
// on: the check runs on the request path of an endpoint that is deliberately
// reachable without other credentials.
//
// With no keyring the key is RANDOM PER PROCESS. A single-process deployment
// is unaffected, because the process that mints also verifies. A split one
// gets a loud warning rather than a deterministic key invented from
// non-secret material, which would let anyone who can reach the endpoint
// forge one.
func OtelSigningKey(material []string) []byte {
	if len(material) == 0 {
		log.Warn("sandbox_otel_signing_key_ephemeral",
			"detail", "no Tier A secrets.keys, so OTLP tokens are signed with "+
				"a per-process key — a split deployment cannot verify tokens "+
				"the other process minted. `crewlet secrets keygen` fixes it")
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
// unset, because the exporter reads it from the ambient environment
// otherwise, and a box inherits whatever the engine host exported. An empty
// value is the difference between "no credential" and "the engine's".
//
// The RESOURCE ATTRIBUTES are non-secret routing facts — which turn, which
// seat — so a span from inside a box lands under the turn that started it
// rather than free-floating.
//
// TRACEPARENT is what nests them: the engine's per-turn span is the parent,
// and without it the box's spans form a second, unrelated trace that nobody
// finds.
func (r *OtelReceiver) RunEnv(traceID, spanID, turnID, handle string, ttl time.Duration) map[string]string {
	if traceID == "" {
		// NO TRACE, NO ENDPOINT. A token scoped to an empty trace would
		// authenticate every run's export as every other run's, which is
		// the one property the scoping exists for.
		return nil
	}
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": r.EndpointFor(traceID, ttl),
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
	// OtelReceiverURLVar is the engine API base a SANDBOX can reach. Unset
	// means no in-box telemetry, which is the ordinary configuration.
	OtelReceiverURLVar = "CREWLET_SANDBOX_OTEL_RECEIVER_URL"

	// OtelUpstreamEndpointVar and OtelUpstreamHeadersVar are the real
	// backend, applied on the trusted side only.
	OtelUpstreamEndpointVar = "OTEL_EXPORTER_OTLP_ENDPOINT"
	OtelUpstreamHeadersVar  = "OTEL_EXPORTER_OTLP_HEADERS"
)

// BuildOtelReceiver is THE construction path, called by the engine and by a
// standalone API alike.
//
// Both need one and for different halves: the engine MINTS a run's endpoint,
// and whichever process is externally reachable VERIFIES the token. A
// deployment where only one of them built a receiver answered 401 to every
// export while its config looked complete — which is the failure the signed,
// stateless token exists to prevent, and it cannot prevent it if one side
// never constructs the verifier.
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
	return NewOtelReceiver(OtelReceiverOptions{
		BaseURL:          base,
		Tokens:           NewOtelTokens(OtelTokenOptions{Key: OtelSigningKey(keyMaterial)}),
		UpstreamEndpoint: strings.TrimSpace(env(OtelUpstreamEndpointVar)),
		UpstreamHeaders:  ParseOtelHeaders(env(OtelUpstreamHeadersVar)),
	})
}
