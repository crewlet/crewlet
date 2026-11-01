package assistant_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// modelFailure is a classified failure of the entry `assistant`.
func modelFailure(kind llm.ErrorKind, err error, retryAt time.Time) error {
	return &llm.Error{Kind: kind, Key: "assistant", RetryAt: retryAt, Err: err}
}

// EVERY MODEL FAILURE IS TOLD IN ITS OWN WORDS, from the backend's
// classification and never its prose: a signed-out model, a spent plan with
// its reset, a harness that ran out of time, a harness that cannot start, and anything
// else under the generic code. An administrator is told where to fix it and
// anybody else whom to ask; Try again is offered only where asking again can
// help.
//
// Mutation: map KindAuth to the generic code, and the signed-out rows fail;
// drop the admin branch of a hint, and the admin rows fail.
func TestEveryModelFailureIsToldInItsOwnWords(t *testing.T) {
	t.Parallel()
	reset := evals.Epoch.Add(30 * time.Minute)
	cases := map[string]struct {
		err       error
		admin     bool
		code      wire.ErrorCode
		message   string
		hint      string
		retryable bool
		wait      int
	}{
		"signed out": {err: modelFailure(llm.KindAuth, errors.New("OAuth token has expired"), time.Time{}),
			code: wire.ErrorModelSignedOut, message: "signed out",
			hint: "Ask an administrator to sign Ask Crewlet's model in again."},
		"signed out, to an administrator": {err: modelFailure(llm.KindAuth, errors.New("expired"), time.Time{}),
			admin: true, code: wire.ErrorModelSignedOut, message: "signed out",
			hint: "`crewlet llm login assistant`"},
		"a spent plan": {err: modelFailure(llm.KindRateLimit, errors.New("Usage limit reached"), reset),
			code: wire.ErrorModelBusy, message: "usage limit", hint: "once the limit resets",
			retryable: true, wait: 1800},
		"a spent plan, to an administrator": {err: modelFailure(llm.KindRateLimit, errors.New("spent"), reset),
			admin: true, code: wire.ErrorModelBusy, message: "usage limit", hint: "assistant.llm",
			retryable: true, wait: 1800},
		"a timeout": {err: modelFailure(llm.KindTimeout, errors.New("ran out of time"), time.Time{}),
			code: wire.ErrorModelTimeout, message: "took too long", hint: "narrower question",
			retryable: true},
		"a timeout, to an administrator": {err: modelFailure(llm.KindTimeout, errors.New("slow"), time.Time{}),
			admin: true, code: wire.ErrorModelTimeout, message: "took too long",
			hint: "providers.llm.assistant.timeout_seconds", retryable: true},
		"a harness that cannot start": {err: modelFailure(llm.KindFatal, llm.ErrNotRunnable, time.Time{}),
			code: wire.ErrorModelUnavailable, message: "cannot be started",
			hint: "Ask an administrator to check Ask Crewlet's model."},
		"a harness that cannot start, to an administrator": {err: modelFailure(llm.KindFatal, llm.ErrNotRunnable, time.Time{}),
			admin: true, code: wire.ErrorModelUnavailable, message: "cannot be started",
			hint: "`crewlet llm doctor assistant`"},
		"anything else": {err: modelFailure(llm.KindServer, errors.New("exit 1"), time.Time{}),
			code: wire.ErrorModelUnavailable, message: "did not answer", retryable: true, wait: 10},
		"anything else, to an administrator": {err: modelFailure(llm.KindServer, errors.New("exit 1"), time.Time{}),
			admin: true, code: wire.ErrorModelUnavailable, message: "did not answer",
			hint: "`crewlet llm doctor assistant`", retryable: true, wait: 10},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := evals.RigOptions{Rounds: []evals.Round{{Err: tc.err}}}
			if tc.admin {
				admin := evals.Person("grace", "grace", iam.GrantAdmin)
				o.Person = &admin
			}
			f := assistanttest.NewRig(t, o)
			accepted, err := f.Ask("model", "Who works here?", nil)
			if err != nil {
				t.Fatal(err)
			}
			frame := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventError)
			if frame == nil {
				t.Fatal("the turn ended with no error frame")
			}
			got := frame.Error
			if got.Code != tc.code || !strings.Contains(got.Message, tc.message) {
				t.Errorf("error = %s %q, want %s naming %q", got.Code, got.Message, tc.code, tc.message)
			}
			if tc.hint == "" && got.Hint != "" || !strings.Contains(got.Hint, tc.hint) {
				t.Errorf("hint = %q, want one naming %q", got.Hint, tc.hint)
			}
			if !tc.admin && strings.Contains(got.Hint, "crewlet llm") {
				t.Errorf("a person who is not an administrator was handed a command: %q", got.Hint)
			}
			if got.Retryable != tc.retryable || got.RetryAfterSeconds != tc.wait {
				t.Errorf("retryable = %v after %ds, want %v after %ds", got.Retryable,
					got.RetryAfterSeconds, tc.retryable, tc.wait)
			}
			for _, text := range []string{got.Message, got.Hint} {
				if strings.ContainsAny(text, "\u2013\u2014") {
					t.Errorf("%q carries an en or em dash", text)
				}
			}
			row, _ := f.Store.Turn(t.Context(), f.Person.ID, accepted.TurnID)
			if row.ErrorCode != string(tc.code) {
				t.Errorf("the turn closed with %q, want %q", row.ErrorCode, tc.code)
			}
		})
	}
}

// A MODEL KNOWN UNREADY REFUSES THE QUESTION BEFORE ANY ROUND: nothing is sent
// to the model and no turn is recorded, and the refusal says what a failed
// round would have said. The same reading is what `/viewer` tells the person
// before they ask, and it clears when the model is ready again.
//
// Mutation: drop the refusal from Start, and the model is asked a round.
func TestAModelKnownUnreadyRefusesTheQuestionBeforeAnyRound(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{evals.Say("Hello.")}})
	f.Model.SetUnready(modelFailure(llm.KindAuth, errors.New("no sign-in"), time.Time{}))

	notice := f.Svc.ModelNotice(t.Context(), f.Person)
	if notice == nil || notice.Code != wire.ErrorModelSignedOut {
		t.Fatalf("the notice before asking = %+v, want model_signed_out", notice)
	}
	_, err := f.Ask("refused", "Who works here?", nil)
	notReady, ok := errorAs[*assistant.ModelNotReady](err)
	if !ok || notReady.Notice.Code != wire.ErrorModelSignedOut || !errors.Is(err, assistant.ErrUnavailable) {
		t.Fatalf("Start = %v, want a ModelNotReady naming model_signed_out", err)
	}
	if notReady.Notice.Message != notice.Message || notReady.RetryAfter != 0 {
		t.Errorf("the refusal %+v is not the notice %+v, or invites a wait", notReady.Notice, notice)
	}
	if n := len(f.Model.Requests()); n != 0 {
		t.Errorf("the model was asked %d rounds on a chain known unready", n)
	}
	if _, err := f.Store.Turn(t.Context(), f.Person.ID, assistant.TurnID(evals.Key("refused"))); err == nil {
		t.Error("a refused question was recorded as a turn")
	}

	f.Model.SetUnready(nil)
	if notice := f.Svc.ModelNotice(t.Context(), f.Person); notice != nil {
		t.Errorf("a ready model still carries a notice: %+v", notice)
	}
	if _, err := f.Ask("ready", "Who works here?", nil); err != nil {
		t.Errorf("a ready model refused the question: %v", err)
	}
}

// A SPENT PLAN'S REFUSAL CARRIES ITS RESET, so the person is told when, and a
// client waits that long rather than retrying at once.
func TestASpentPlansRefusalCarriesItsReset(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{})
	reset := f.Clock.Now().Add(20 * time.Minute)
	f.Model.SetUnready(modelFailure(llm.KindRateLimit, errors.New("spent"), reset))
	_, err := f.Ask("spent", "Who works here?", nil)
	notReady, ok := errorAs[*assistant.ModelNotReady](err)
	if !ok || notReady.Notice.Code != wire.ErrorModelBusy {
		t.Fatalf("Start = %v, want model_busy", err)
	}
	if notReady.RetryAfter != 20*time.Minute || notReady.Notice.RetryAt == nil ||
		!notReady.Notice.RetryAt.Equal(reset) {
		t.Errorf("the refusal waits %v until %v, want 20m until %v", notReady.RetryAfter,
			notReady.Notice.RetryAt, reset)
	}
}
