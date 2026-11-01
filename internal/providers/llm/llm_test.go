package llm_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE STOP VOCABULARY IS CLOSED, and the zero value is not in it: it is a
// backend that reported no reason, which the tool loop reads as an ordinary
// end rather than as one of these.
func TestTheStopReasonSetIsClosedAndEmptyIsUnreported(t *testing.T) {
	t.Parallel()
	for _, r := range []llm.StopReason{llm.StopEnd, llm.StopToolUse, llm.StopCap} {
		if !r.Valid() {
			t.Errorf("%q: Valid() = false, want true", r)
		}
	}
	for _, r := range []llm.StopReason{"", "end_turn", "length", "stop", "tool_calls", "max_tokens"} {
		if r.Valid() {
			t.Errorf("%q: Valid() = true — a vendor's own spelling must be mapped, never passed through", r)
		}
	}
}

// ONLY A FATAL STOPS THE CHAIN. Retryable is what the fallback chain reads:
// a fatal is a call the engine or its host cannot make, and every other kind —
// a spent plan, a login, a slow, silent or failed CLI run — is one the next
// member may well answer.
func TestOnlyAFatalStopsTheChain(t *testing.T) {
	t.Parallel()
	for kind, retryable := range map[llm.ErrorKind]bool{
		llm.KindFatal: false, llm.KindRateLimit: true, llm.KindAuth: true,
		llm.KindTimeout: true, llm.KindServer: true,
	} {
		if kind.Retryable() != retryable {
			t.Errorf("%s: Retryable() = %v, want %v", kind, kind.Retryable(), retryable)
		}
	}
}

// AN UNCLASSIFIED FAILURE IS FATAL, a deadline is a timeout, and a classified
// one keeps its kind through any wrapping — the three answers [llm.KindOf]
// gives the chain.
func TestKindOfReadsThroughWrappingAndDefaultsToFatal(t *testing.T) {
	t.Parallel()
	classified := &llm.Error{Kind: llm.KindRateLimit, Provider: "cli-agent/claude-code", Model: "sonnet",
		Err: errors.New("Usage limit reached")}
	for _, tc := range []struct {
		name string
		err  error
		want llm.ErrorKind
	}{
		{"classified", classified, llm.KindRateLimit},
		{"wrapped", fmt.Errorf("toolloop: %w", classified), llm.KindRateLimit},
		{"a deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), llm.KindTimeout},
		{"anything else", errors.New("boom"), llm.KindFatal},
	} {
		if got := llm.KindOf(tc.err); got != tc.want {
			t.Errorf("%s: KindOf = %s, want %s", tc.name, got, tc.want)
		}
	}
	if msg := classified.Error(); !strings.Contains(msg, "rate_limit") ||
		!strings.Contains(msg, "Usage limit reached") || !strings.Contains(msg, "cli-agent/claude-code") {
		t.Errorf("Error() = %q, want the backend, the kind and what the CLI said", msg)
	}
}

// AN EMPTY FRAGMENT IS NOT SENT. A keep-alive carries no text, and a
// callback that published a frame for each one spent a running seat's whole
// socket queue on nothing; a restart is sent although it carries none,
// because it is news.
func TestSendDropsEmptyFragmentsAndKeepsRestarts(t *testing.T) {
	t.Parallel()
	var got []llm.Delta
	req := llm.Request{OnDelta: func(d llm.Delta) { got = append(got, d) }}
	req.Send(llm.Delta{})
	req.Send(llm.Delta{Content: "hi"})
	req.Send(llm.Delta{Restart: true, Model: "sonnet"})
	if len(got) != 2 || got[0].Content != "hi" || !got[1].Restart {
		t.Errorf("sent %+v, want the text and the restart only", got)
	}
	llm.Request{}.Send(llm.Delta{Content: "nobody asked"}) // no callback: no panic
}

// WHAT WAS SHOWN NEVER QUIETLY DIFFERS FROM WHAT IS TRUE. Reconcile appends
// only when the shown text is a prefix of the answer, and otherwise starts
// the reader over with the whole answer, so applying its result to the shown
// text always yields the answer exactly.
func TestReconcileAlwaysLandsOnTheAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		shown, want, tail string
		restart           bool
	}{
		{"", "", "", false},
		{"", "The sea", "The sea", false},
		{"The sea", "The sea", "", false},
		{"The sea", "The sea stays", " stays", false},
		{"The sea st", "The tide", "The tide", true},
		{"The sea stays", "The sea", "The sea", true},
	}
	for _, c := range cases {
		tail, restart := llm.Reconcile(c.shown, c.want)
		if tail != c.tail || restart != c.restart {
			t.Errorf("Reconcile(%q, %q) = (%q, %v), want (%q, %v)",
				c.shown, c.want, tail, restart, c.tail, c.restart)
		}
		landed := c.shown + tail
		if restart {
			landed = tail
		}
		if landed != c.want {
			t.Errorf("Reconcile(%q, %q) lands the reader on %q", c.shown, c.want, landed)
		}
	}
}

// A LIMIT'S WINDOW IS ONE OF A CLOSED SET. A reader that draws one (the Models
// screen) is held to LimitWindows, so what Valid admits and what the set lists
// are one list: a window added to one and not the other fails here.
func TestALimitWindowIsOneOfTheClosedSet(t *testing.T) {
	t.Parallel()
	for _, w := range llm.LimitWindows {
		if !w.Valid() {
			t.Errorf("%q is in LimitWindows and not Valid", w)
		}
	}
	for _, w := range []llm.LimitWindow{"", "five_hour", "seven_day", "monthly"} {
		if w.Valid() {
			t.Errorf("%q is Valid and not in LimitWindows", w)
		}
	}
	if len(llm.LimitWindows) != 4 {
		t.Errorf("LimitWindows has %d windows, want session, weekly, model and unknown", len(llm.LimitWindows))
	}
}
