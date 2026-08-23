// Package llm is the contract every language-model backend implements, and
// the vocabulary a turn speaks to one.
//
// It is deliberately small and deliberately dumb about failure. Two things it
// does NOT do, each because the Python engine learned it the expensive way:
//
//   - It does not retry. Every backend sets its SDK's retry count to zero,
//     because retrying inside a provider hides the one signal the layers above
//     need — which credential failed and why. Rotation belongs to the
//     credential pool, fallback to the chain, and neither can act on an error
//     the SDK already swallowed and re-tried into a timeout.
//   - It does not decide what a failure MEANS beyond a coarse kind. A backend
//     classifies; the pool and the chain decide. Putting the policy in the
//     backend gave three backends three subtly different ideas of "exhausted".
package llm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Role names a message's author. Strings rather than an enum because they go
// on the wire to providers that each accept their own set, and a backend
// translates.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one turn of a conversation.
//
// ReasoningContent and ThinkingBlocks are separate because they are different
// things: the first is prose a model emitted as its reasoning, the second is
// the provider's own structured blocks, which must be handed BACK verbatim on
// the next call or the provider rejects the conversation. Flattening them
// loses the round trip.
type Message struct {
	Role             string
	Content          string
	ReasoningContent string
	ThinkingBlocks   []ThinkingBlock
	ToolCalls        []ToolCall
	ToolCallID       string
	Name             string
}

// ThinkingBlock is a provider's structured reasoning block, carried opaquely.
type ThinkingBlock struct {
	Type      string
	Thinking  string
	Signature string
	Data      string
}

// ToolDef is a tool offered to the model. Parameters is a JSON Schema object.
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is the model asking for a tool to run.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// Completion is one model response.
//
// InputTokens is ALWAYS the full prompt token count for the call, cache reads
// and writes included, so it stays a correct budget figure whatever the cache
// did. CacheRead and CacheWrite break that total down for cost reporting only
// — a reader that adds them to InputTokens is double-counting.
type Completion struct {
	Content          string
	ReasoningContent string
	ThinkingBlocks   []ThinkingBlock
	ToolCalls        []ToolCall
	FinishReason     string

	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
}

// TotalTokens is the figure a budget is charged.
func (c Completion) TotalTokens() int { return c.InputTokens + c.OutputTokens }

// Request is one call's inputs. A struct rather than a parameter list because
// it grows: every provider feature added to the Python signature over time
// became another positional argument at forty call sites.
type Request struct {
	Messages    []Message
	Tools       []ToolDef
	Temperature float64
	MaxTokens   int

	// ToolChoice is "", "auto", "required" or "none". "required" is what
	// the turn engine's corrective re-prompt sets when a phase must end in
	// a tool call and the model answered with prose.
	ToolChoice string
}

// Provider is a language-model backend.
//
// Model is a method rather than a field so a chain wrapper can answer for
// whichever member actually served the call — telemetry reads it directly, and
// a chain reporting its own name instead of the model that answered makes the
// per-model token breakdown wrong.
type Provider interface {
	Model() string
	Complete(ctx context.Context, req Request) (*Completion, error)
}

// --- failure classification ------------------------------------------------

// ErrorKind is how a provider failure is classified. The backend classifies;
// the credential pool and the fallback chain decide what to do about it.
type ErrorKind int

const (
	// KindFatal is any non-retryable failure: a malformed request, a
	// content-policy refusal, a 404. The chain does NOT try another
	// provider — the next one will refuse it identically, and trying is
	// two more seconds and another log line saying the same thing.
	KindFatal ErrorKind = iota

	// KindRateLimit is 429 or 402 — quota exhausted or a billing problem.
	// The credential pool puts the key into a rate-limit cooldown.
	KindRateLimit

	// KindAuth is 401 or 403. A shorter cooldown than rate-limit, because
	// a token refresh may rescue it. Repeated auth failures on one key with
	// no success in between back off exponentially, so a permanently bad
	// key stops thrashing the provider; one success resets it.
	KindAuth

	// KindTimeout is a network or context timeout, or 408/425. Retryable
	// across the chain, and it NEVER marks the credential exhausted: a
	// timeout is a transport fact, not a key fact.
	KindTimeout

	// KindServer is 5xx. Same treatment as a timeout.
	KindServer
)

func (k ErrorKind) String() string {
	switch k {
	case KindRateLimit:
		return "rate_limit"
	case KindAuth:
		return "auth"
	case KindTimeout:
		return "timeout"
	case KindServer:
		return "server"
	default:
		return "fatal"
	}
}

// Retryable reports whether another member of a fallback chain is worth
// trying. Fatal is the only kind that is not.
func (k ErrorKind) Retryable() bool { return k != KindFatal }

// ExhaustsCredential reports whether this kind should cool the credential
// down. Transport failures must not: cooling a healthy key on a network blip
// is how a fleet talks itself out of every key it has.
func (k ErrorKind) ExhaustsCredential() bool {
	return k == KindRateLimit || k == KindAuth
}

// Error is a classified provider failure.
//
// RetryAfter is carried rather than applied because the layer that knows how
// long to wait is not the layer that decides whether to wait at all.
type Error struct {
	Kind       ErrorKind
	Provider   string
	Model      string
	Status     int
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	return fmt.Sprintf("llm %s/%s: %s: %v", e.Provider, e.Model, e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the classification of an error, or KindFatal for anything
// this package did not classify. Fatal is the safe default for an UNRECOGNISED
// error: treating an unknown failure as retryable means a chain walks every
// provider it has for a request none of them can serve, turning one clear
// failure into N slow ones.
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	return KindFatal
}

// KindForStatus maps an HTTP status onto a kind. Shared by every backend so
// two SDKs cannot drift into disagreeing about what a 402 means.
func KindForStatus(status int) ErrorKind {
	switch {
	case status == 401 || status == 403:
		return KindAuth
	case status == 402 || status == 429:
		return KindRateLimit
	case status == 408 || status == 425:
		return KindTimeout
	case status >= 500 && status < 600:
		return KindServer
	default:
		return KindFatal
	}
}

// ParseRetryAfter parses one Retry-After header value in either RFC 9110 form
// — delta-seconds, or an HTTP-date — and reports whether it parsed.
//
// Both forms, because providers send both, and a client that understands only
// the integer form silently falls back to its own guess exactly when the
// server has told it the answer. Negative and absurd values are clamped: a
// date already in the past means "now", and a cooldown longer than a day is a
// clock disagreement rather than an instruction.
func ParseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		return clampRetryAfter(time.Duration(secs * float64(time.Second))), true
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if when, err := time.Parse(layout, raw); err == nil {
			return clampRetryAfter(when.Sub(now)), true
		}
	}
	return 0, false
}

// maxRetryAfter bounds an honoured cooldown. A server asking for longer than a
// day is telling us about its clock, not about our quota, and parking a seat
// for that long on a header is worse than retrying and being refused.
const maxRetryAfter = 24 * time.Hour

func clampRetryAfter(d time.Duration) time.Duration {
	return max(0, min(d, maxRetryAfter))
}
