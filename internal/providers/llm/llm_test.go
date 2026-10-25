package llm_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE STOP VOCABULARY IS CLOSED, and the zero value is not in it: it is a
// backend that reported no reason, which the tool loop reads as an ordinary
// end rather than as one of these.
func TestTheStopReasonSetIsClosedAndEmptyIsUnreported(t *testing.T) {
	t.Parallel()
	for _, r := range []llm.StopReason{llm.StopEnd, llm.StopToolUse} {
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

// fill sets every exported field reachable from v to a non-zero value derived
// from seed, so a field that is not copied cannot pass as an equal zero.
func fill(v reflect.Value, seed string) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(seed)
	case reflect.Int, reflect.Int64:
		v.SetInt(int64(len(seed)))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), seed)
		v.Set(s)
	case reflect.Uint8:
		v.SetUint(uint64(seed[0]))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		val := reflect.New(v.Type().Elem()).Elem()
		fill(val, seed)
		m.SetMapIndex(reflect.ValueOf(seed), val)
		v.Set(m)
	case reflect.Interface:
		v.Set(reflect.ValueOf(seed))
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), seed+"."+v.Type().Field(i).Name)
			}
		}
	}
}

// A COMPLETION'S TURN CARRIES EVERYTHING THE TWO SHARE. The assistant turn a
// conversation grows by is built in one place, and this holds that place to
// the two types: every field of the answer that the turn also has is copied,
// and a field added to EITHER type fails here until it is decided — copied,
// or named below as belonging to one side only.
func TestACompletionsTurnCarriesEverythingItShares(t *testing.T) {
	t.Parallel()
	var c llm.Completion
	fill(reflect.ValueOf(&c).Elem(), "c")
	m := c.Message()

	if m.Role != llm.RoleAssistant {
		t.Errorf("role = %q, want assistant", m.Role)
	}

	// The turn's own fields: a role, and the three that describe a tool
	// RESULT rather than an answer.
	turnOnly := map[string]bool{"Role": true, "ToolCallID": true, "Name": true, "Failed": true}
	// The answer's own: who served it and what it cost and why it stopped
	// — the round's record, not the conversation's.
	answerOnly := map[string]bool{
		"Model": true, "ProviderKey": true, "StopReason": true,
		"InputTokens": true, "OutputTokens": true, "CacheRead": true, "CacheWrite": true,
	}
	cv, mv := reflect.ValueOf(c), reflect.ValueOf(m)
	for i := range mv.NumField() {
		name := mv.Type().Field(i).Name
		if turnOnly[name] {
			continue
		}
		field := cv.FieldByName(name)
		if !field.IsValid() {
			t.Errorf("Message.%s has no counterpart on Completion: copy it in Completion.Message or name it turn-only here", name)
			continue
		}
		if !reflect.DeepEqual(field.Interface(), mv.Field(i).Interface()) {
			t.Errorf("Message.%s = %v, want the completion's %v", name, mv.Field(i), field)
		}
	}
	for i := range cv.NumField() {
		name := cv.Type().Field(i).Name
		if !answerOnly[name] && !mv.FieldByName(name).IsValid() {
			t.Errorf("Completion.%s reaches no turn: give Message the field or name it answer-only here", name)
		}
	}
}
