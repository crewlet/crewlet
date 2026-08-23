// Package httpapi is what the HTTP-speaking LLM backends share: how a status
// and a set of response headers become the contract's classified error, and
// how a tool call's arguments survive the JSON round trip.
//
// It exists because the alternative is two copies. llm.go records what two
// copies cost the Python engine — "three backends three subtly different ideas
// of exhausted" — and the drift is not hypothetical here either: the Anthropic
// and OpenAI SDKs raise structurally identical errors from two different
// internal packages, so the only thing a backend can honestly own is the
// errors.As that names its own SDK type. Everything after that is this
// package.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

var log = logging.Get("providers.llm")

// idleConnsPerHost is how many warm connections a backend keeps to one
// endpoint.
//
// Go's default is 2 (http.DefaultMaxIdleConnsPerHost). Over HTTP/2 — what the
// vendor endpoints negotiate — that barely matters, because one connection
// multiplexes every concurrent request. It matters a great deal for the
// endpoints this same code also serves: a self-hosted vLLM or a gateway
// speaking HTTP/1.1 gives one request per connection, so a node running more
// than two concurrent turns pays a fresh TCP and TLS handshake on every call
// past the second, on the hot path of every round of every phase.
//
// 32 keeps a warm connection for every plausibly concurrent turn on one node
// — the cli-agent backend caps itself at 4 processes and an HTTP node runs
// tens of seats, not hundreds — costs at most 32 sockets per provider, and
// idle ones are still reaped by the transport's 90-second IdleConnTimeout.
const idleConnsPerHost = 32

// NewHTTPClient builds the transport an LLM backend talks through.
//
// It CLONES http.DefaultTransport rather than building one, so the proxy, TLS
// and HTTP/2 settings the process was started with survive: a hand-built
// transport silently drops ProxyFromEnvironment, which is how a deployment
// behind a corporate proxy stops being able to reach anything, with no error
// that says so.
//
// No client-level Timeout is set. The per-call deadline belongs to the SDK's
// own request option, which is the one that produces a context.DeadlineExceeded
// this package can classify as KindTimeout rather than as an unknown failure.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = idleConnsPerHost
	return &http.Client{Transport: transport}
}

// FromStatus classifies an API failure that carried an HTTP status.
//
// The status decides the kind through [llm.KindForStatus] — shared so two SDKs
// cannot drift into disagreeing about what a 402 means — and the headers are
// read for a server-supplied cooldown, which the credential pool prefers over
// its configured TTL.
func FromStatus(err error, provider, model string, status int, h http.Header) *llm.Error {
	e := &llm.Error{
		Kind:     llm.KindForStatus(status),
		Provider: provider,
		Model:    model,
		Status:   status,
		Err:      err,
	}
	// Only a benching kind has anywhere to put the hint. Reading it for a
	// 500 would be harmless but misleading: nothing consumes it, and a
	// populated RetryAfter on a server error invites a reader to think
	// something waits.
	if e.Kind.ExhaustsCredential() {
		if d, ok := RetryHint(h, time.Now()); ok {
			e.RetryAfter = d
		}
	}
	return e
}

// FromTransport classifies a failure that never reached a status: a dial
// error, a reset connection, a deadline, a cancelled context.
//
// A transport failure is NEVER a credential failure. [llm.ErrorKind] carries
// that rule in ExhaustsCredential, and this is the function that must not
// hand it the wrong kind: benching a healthy key on a network blip is how a
// fleet talks itself out of every credential it has.
func FromTransport(err error, provider, model string) *llm.Error {
	kind := llm.KindFatal
	switch {
	case errors.Is(err, context.Canceled):
		// A cancelled call is the caller's own doing — a seat fence, a
		// shutdown — not a provider verdict. Fatal is the classification
		// that gets this right in both directions: no other member of the
		// chain is tried (they would be cancelled too) and no credential
		// is benched. The word in the log is the only thing it costs, and
		// errors.Is(err, context.Canceled) still answers through Unwrap.
		kind = llm.KindFatal
	case errors.Is(err, context.DeadlineExceeded), isNetworkError(err):
		kind = llm.KindTimeout
	}
	return &llm.Error{Kind: kind, Provider: provider, Model: model, Err: err}
}

// isNetworkError reports whether err came from the network rather than from
// the service. *url.Error and *net.OpError both satisfy net.Error, which
// covers everything an http.Client surfaces.
func isNetworkError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr)
}

// resetHeaders are the vendor-specific "your limit clears at" headers, in the
// order they are consulted.
//
// Two shapes, because the vendors chose differently: OpenAI sends a duration
// ("6m0s", "1.5s", sometimes a bare number of seconds) and Anthropic sends an
// RFC 3339 instant. The Python engine read only the OpenAI names and only the
// bare-number form, so "6m0s" — the value OpenAI actually sends once a limit
// has properly tripped — parsed as nothing and the static TTL applied.
var resetHeaders = []string{
	"x-ratelimit-reset-requests",
	"x-ratelimit-reset-tokens",
	"anthropic-ratelimit-requests-reset",
	"anthropic-ratelimit-tokens-reset",
	"anthropic-ratelimit-input-tokens-reset",
	"anthropic-ratelimit-output-tokens-reset",
}

// RetryHint reads a server-supplied cooldown off a response's headers.
//
// Retry-After wins, in either RFC 9110 form, through [llm.ParseRetryAfter] —
// there is exactly one parser for that grammar and this is not a second one.
// Failing that, the vendor reset headers are consulted in order.
//
// A non-positive value is NOT a hint and the scan keeps going. That rule is
// deliberate: a zero cooldown is not a cooldown, it is a key with no record
// that it just failed, which the next lease picks straight back up because it
// is the least loaded. The header's job is to SHORTEN the bench, not to remove
// it, and "x-ratelimit-reset-requests: 0s" on a 429 means the request bucket
// is not the one that tripped.
//
// now is the instant an HTTP-date is measured against — wall clock, correctly:
// it is being compared with a wall-clock instant the server chose. The
// DURATION it yields is then held on the pool's monotonic clock.
func RetryHint(h http.Header, now time.Time) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	if d, ok := llm.ParseRetryAfter(h.Get("Retry-After"), now); ok && d > 0 {
		return d, true
	}
	for _, name := range resetHeaders {
		raw := strings.TrimSpace(h.Get(name))
		if raw == "" {
			continue
		}
		if d, ok := parseReset(raw, now); ok && d > 0 {
			return d, true
		}
	}
	return 0, false
}

// parseReset reads one reset header value in any of the three shapes vendors
// send: a Go-style duration ("6m0s", "1.5s"), a bare number of seconds ("20"),
// or an RFC 3339 instant.
func parseReset(raw string, now time.Time) (time.Duration, bool) {
	if d, err := time.ParseDuration(raw); err == nil {
		return d, true
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(secs * float64(time.Second)), true
	}
	if when, err := time.Parse(time.RFC3339, raw); err == nil {
		return when.Sub(now), true
	}
	return 0, false
}

// DecodeArgs turns a tool call's raw JSON arguments into the contract's map.
//
// Unparseable arguments produce an EMPTY map rather than an error, because the
// alternative kills a phase over something the model can fix: the tool sees no
// arguments, fails, and its failure goes back to the model as an ordinary tool
// result — which is exactly the loop's design.
//
// The partially-decoded map is discarded, and that is the whole point of not
// writing this inline twice. encoding/json populates what it managed before it
// failed, so `{"n": 1e1000}` yields both an error AND a map holding +Inf — a
// value that marshals back out as nothing at all. Keeping it would put a
// round-trip landmine in the conversation: the call looks fine this round, and
// the NEXT request fails to serialise the history, on a round that has nothing
// to do with the tool that caused it.
func DecodeArgs(raw []byte, tool string) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	args := map[string]any{}
	if err := json.Unmarshal(raw, &args); err != nil {
		log.Warn("tool_arguments_unparseable",
			"tool", tool, "bytes", len(raw), "error", err.Error())
		return map[string]any{}
	}
	if args == nil {
		// A literal `null` unmarshals into a nil map without error.
		return map[string]any{}
	}
	return args
}

// EncodeArgs renders arguments for a wire format that carries them as a JSON
// string (OpenAI's function calls).
//
// A nil map must render "{}" and not "null": encoding/json spells a nil map
// null, and an assistant turn replaying `"arguments": "null"` is a message the
// model wrote turning into one it did not.
//
// A marshal failure is an error rather than a silent "{}". By this point the
// map came from the caller, not from a provider response — a value that cannot
// be JSON (a NaN, an infinity) is a plumbing fault, and swapping it for empty
// arguments would rewrite the conversation and blame the model.
func EncodeArgs(args map[string]any, tool string) (string, error) {
	if len(args) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("tool %q arguments are not JSON: %w", tool, err)
	}
	return string(raw), nil
}
