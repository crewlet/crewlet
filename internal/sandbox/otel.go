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

// In-sandbox telemetry without handing the box a secret. Pointing a box at the
// real backend with its long-lived ingest credential in
// OTEL_EXPORTER_OTLP_HEADERS puts that credential in the least trusted thing
// in the deployment, so the box exports to the ENGINE, which adds the real
// auth on the trusted side and forwards.
//
// The box authenticates with a per-run, trace-scoped, short-lived token in the
// URL PATH: any header it could carry a leaked box carries too, and a path
// needs no exporter header configuration. The token is [runtoken]'s, shared
// with the MCP bridge so one format, signature and expiry serve both.

// OtelTokens mints and validates per-run OTLP tokens.
//
// A thin type over the shared signer because its SUBJECT is a TRACE: naming
// that at the type boundary stops a caller minting, from a run id, a token the
// receiver would validate into the wrong thing.
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

// Mint returns a token scoped to one trace, valid for ttl, or refuses a trace
// id no token can carry redacted (runtoken.ErrSubject).
func (t *OtelTokens) Mint(traceID string, ttl time.Duration) (string, error) {
	token, err := t.signer.Mint(traceID, ttl)
	if err != nil {
		return "", fmt.Errorf("sandbox: a telemetry token for trace %q: %w", traceID, err)
	}
	return token, nil
}

// Validate returns the token's trace id, or empty for one that is forged,
// malformed or expired.
func (t *OtelTokens) Validate(token string) string { return t.signer.Validate(token) }

// OtelTokenTTL is how long a run's telemetry endpoint stays valid from its
// job's start: FOUR HOURS, a judgment between two costs. The receiver checks a
// token against its signature and expiry alone (#449), so this lifetime is all
// that bounds a leaked one, which writes into its one trace and reads nothing.
// Every job start, a resume's included, mints afresh ([withTelemetry]), so it
// need outlast one job's working stretch, past which the run's tail is lost.
const OtelTokenTTL = 4 * time.Hour

// OtelReceiver mints a run's endpoint and forwards what the box exports.
type OtelReceiver struct {
	base     string
	host     string
	tokens   *OtelTokens
	tokenTTL time.Duration
	upstream string
	headers  map[string]string
	http     *http.Client
}

// OtelReceiverOptions configure [NewOtelReceiver].
type OtelReceiverOptions struct {
	// BaseURL is the externally reachable engine API base the SANDBOX
	// exports to. On a fleet split by node.roles that is an ingress node,
	// which need not be the node that mints. Empty mints no endpoint for a
	// box that does not share this node's loopback.
	BaseURL string

	// HostURL is where a box that SHARES THIS NODE'S LOOPBACK (the `direct`
	// cell) exports: the node's own listener on loopback, e.g.
	// "http://127.0.0.1:8080", as internal/api/mcpbridge's Options.HostURL.
	// PREFERRED to BaseURL for such a box, since loopback never leaves the
	// host. Set only by [BuildOtelReceiver], on a node that forwards.
	HostURL string

	// Tokens mints and verifies. Required.
	Tokens *OtelTokens

	// TokenTTL is how long a minted endpoint stays valid: [OtelTokenTTL] for
	// every engine's receiver ([BuildOtelReceiver]). Required.
	TokenTTL time.Duration

	// UpstreamEndpoint is the real OTLP backend. Empty accepts and drops, a
	// working configuration: the engine's per-turn span still carries the
	// trace, and a deployment with no backend should not fail every export.
	UpstreamEndpoint string

	// UpstreamHeaders is the backend's ingest credential, applied HERE and
	// never handed to the sandbox: the point of the hop.
	UpstreamHeaders map[string]string

	HTTP *http.Client
}

// OtelForwardTimeout bounds one forwarded payload. SHORT: a slow backend must
// not hold the receiver's handlers open, and the box's exporter retries, so a
// dropped batch costs only a gap in a trace.
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
	if opts.TokenTTL <= 0 {
		return nil, fmt.Errorf("sandbox: the OTLP receiver needs a positive token TTL; got %v",
			opts.TokenTTL)
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
		tokenTTL: opts.TokenTTL,
		upstream: strings.TrimRight(strings.TrimSpace(opts.UpstreamEndpoint), "/"),
		headers:  headers,
		http:     client,
	}, nil
}

// Reaches is the base URL a box exports to: this node's own listener for a
// box sharing its loopback where it has one ([OtelReceiverOptions.HostURL]), else
// [OtelReceiverURLVar]'s address, or "" for neither. The rule is
// internal/api/mcpbridge's Bridge.Reaches, cell for cell: a `container` box is
// not on this host's loopback whatever its runtime delivers, so it dials the
// fleet's address alone, which [EngineAddress] rewrites as that box reaches it.
func (r *OtelReceiver) Reaches(onHost bool) string {
	if onHost && r.host != "" {
		return r.host
	}
	return r.base
}

// EndpointFor mints one run's OTLP endpoint for a box sharing this node's
// loopback or not ([OtelReceiver.Reaches]), "" where no address reaches that box, or
// an error where its token cannot be minted ([OtelTokens.Mint]). The token is
// IN THE PATH, so OTEL_EXPORTER_OTLP_HEADERS stays empty inside the box, and
// lives [OtelReceiverOptions.TokenTTL] from now.
func (r *OtelReceiver) EndpointFor(traceID string, onHost bool) (string, error) {
	base := r.Reaches(onHost)
	if base == "" {
		return "", nil
	}
	token, err := r.tokens.Mint(traceID, r.tokenTTL)
	if err != nil {
		return "", err
	}
	return base + otelTokenPath + token, nil
}

// otelTokenPath is what precedes a run's own export token in its endpoint
// ([OtelReceiver.EndpointFor]): what follows it is the token, a credential of
// the run's that a capture of its box's session scrubs out ([handedSecrets]).
const otelTokenPath = "/otlp/"

// Validate reports the trace a token is scoped to, or empty.
func (r *OtelReceiver) Validate(token string) string { return r.tokens.Validate(token) }

// Forwards reports whether an accepted payload goes anywhere: no upstream
// (accept and drop) is a real configuration an operator surface asks about.
func (r *OtelReceiver) Forwards() bool { return r.upstream != "" }

// Forward sends a validated payload to the real backend.
//
// FAILURES ARE LOGGED, NEVER RETURNED for the box: its exporter retries a 5xx,
// multiplying traffic against a backend that is down for nothing recoverable.
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

// OtelSignals are the OTLP paths this receiver accepts. NAMED, because the
// segment goes into the upstream URL: unchecked, a caller chooses part of the
// address the engine's credential is sent to.
var OtelSignals = []string{"traces", "metrics", "logs"}

// ValidSignal reports a signal this receiver forwards.
func ValidSignal(signal string) bool { return slices.Contains(OtelSignals, signal) }

// ParseOtelHeaders reads the OTEL_EXPORTER_OTLP_HEADERS `k=v,k2=v2` form: the
// UPSTREAM backend's auth, added on the trusted side.
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

// OtelKeyDomain separates this endpoint's tokens from the tool bridge's: both
// are HMACs over the same fleet key, so without it one would validate at the
// other.
const OtelKeyDomain = "crewlet.otlp.v1"

// RunEnv is the OTel environment one run's box receives. The endpoint carries
// the run's token, so OTEL_EXPORTER_OTLP_HEADERS is set EMPTY, not left unset:
// an image or a `-e` in `local.run_args` may bake in a value
// the exporter would otherwise present (the engine hands a box none of its
// own; internal/hostbox). The resource attributes (turn, seat) file a box's
// spans under its turn, and TRACEPARENT nests them under the engine's per-turn
// span rather than in an unrelated trace.
//
// onHost picks the address ([OtelReceiver.Reaches]); a box no address reaches
// gets NO environment rather than an endpoint it cannot dial. A trace id no
// token can carry redacted is an error ([OtelTokens.Mint]) that fails the
// launch.
func (r *OtelReceiver) RunEnv(traceID, spanID, turnID, handle string, onHost bool) (map[string]string, error) {
	if traceID == "" {
		// NO TRACE, NO ENDPOINT. A token scoped to an empty trace would
		// authenticate every run's export as every other run's, which is
		// the one property the scoping exists for.
		return nil, nil
	}
	endpoint, err := r.EndpointFor(traceID, onHost)
	if err != nil || endpoint == "" {
		return nil, err
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
	return env, nil
}

// The environment the receiver is configured from. ENV RATHER THAN TIER A:
// the receiver's address is a property of the DEPLOYMENT's network (which host
// a sandbox reaches the engine on), not of the company or the node, and the
// upstream pair is the standard OTel spelling every collector documents.
const (
	// OtelReceiverURLVar is the fleet's engine API base for a SANDBOX that
	// does not share its launching node's loopback (a container box, or any
	// box of a node that forwards nothing). Unset, the ordinary configuration,
	// such a box gets no in-box telemetry (a direct box still gets its node's
	// listener where the node forwards: [BuildOtelReceiver]). Set, it is an
	// absolute http or https URL with a host ([ParseBoxURL]), or the node
	// refuses to boot.
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

// BuildOtelReceiver is THE construction path, called by every engine. The
// launching node mints and whichever node the box exports to verifies, so all
// build from the same environment and Tier A keyring (material; never the
// database, which an endpoint reachable without other credentials must not
// depend on). A keyring that cannot sign for the fleet is refused: a
// per-process key would answer 401 at every other node, config looking whole.
//
// hostURL (loopback, [OtelReceiverOptions.HostURL]; "" for none) is taken ONLY
// WHERE THIS NODE FORWARDS ([OtelUpstreamEndpointVar] set): on a node that
// drops, it would turn on exports that go nowhere and pull boxes off a fleet
// address ([OtelReceiverURLVar]) that forwards. With neither, it builds none.
// [ParseBoxURL] refuses an undialable address. Its endpoints live [OtelTokenTTL].
func BuildOtelReceiver(env func(string) string, material runtoken.Material,
	hostURL string,
) (*OtelReceiver, error) {
	if env == nil {
		return nil, nil
	}
	base, err := ParseBoxURL(OtelReceiverURLVar, env(OtelReceiverURLVar))
	if err != nil {
		return nil, err
	}
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
		TokenTTL:         OtelTokenTTL,
		UpstreamEndpoint: upstream,
		UpstreamHeaders:  ParseOtelHeaders(env(OtelUpstreamHeadersVar)),
	})
}
