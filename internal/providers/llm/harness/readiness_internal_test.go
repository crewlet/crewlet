package harness

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// signedOutProvider is a fake harness whose every call reports an expired login,
// with a credential file declared (and none written), on a clock the case
// moves.
func signedOutProvider(t *testing.T) (*Provider, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Now()}
	p := fakeProvider(t, map[string]string{
		"FAKE_STDERR": "Error: OAuth token has expired\n", "FAKE_EXIT": "1",
	}, map[string]any{
		"auth_markers":     []any{map[string]any{"sentinel": "OAuth token has expired"}},
		"credential_paths": []any{".fake/creds.json"},
	})
	p.now = clock.read
	return p, clock
}

// answerNext makes the fake's next calls answer.
func answerNext(p *Provider) {
	delete(p.env, "FAKE_STDERR")
	delete(p.env, "FAKE_EXIT")
	p.env["FAKE_STDOUT"] = "ok"
}

// A SPENT PLAN IS KNOWN WITHOUT A CALL, WITH ITS RESET: the entry's sit-out is
// what Unready reports, carrying the instant the entry is tried again, so a
// person can be told when rather than that it failed.
//
// Mutation: drop RetryAt from [Provider.limited], and the reset is zero.
func TestASpentPlanIsUnreadyUntilItsReset(t *testing.T) {
	t.Parallel()
	p, clock := spentProvider(t, 37*time.Minute)
	if err := p.Unready(t.Context()); err != nil {
		t.Fatalf("an entry nothing has called is unready: %v", err)
	}
	if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindRateLimit {
		t.Fatalf("the spent plan answered %v, want a rate limit", err)
	}
	err := p.Unready(t.Context())
	var failure *llm.Error
	if !errors.As(err, &failure) || failure.Kind != llm.KindRateLimit {
		t.Fatalf("a benched entry's Unready = %v, want its rate limit", err)
	}
	want := clock.read().Add(37 * time.Minute)
	if d := failure.RetryAt.Sub(want); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("RetryAt = %v, want the reset the harness named, %v", failure.RetryAt, want)
	}
	if failure.Key != "sub" {
		t.Errorf("Key = %q, want the entry's key", failure.Key)
	}
	clock.advance(38 * time.Minute)
	if err := p.Unready(t.Context()); err != nil {
		t.Errorf("after the reset Unready = %v, want nil", err)
	}
}

// A LOGIN THE HARNESS REFUSED IS REMEMBERED, AND AN ANSWER CLEARS IT: the next
// person is told the model is signed out before they ask, and the first call
// through the entry that answers (a seat's) lifts it.
//
// Mutation: make remember ignore KindAuth, and the first check is nil; make it
// keep the verdict on success, and the last is not.
func TestARefusedLoginIsUnreadyUntilACallAnswers(t *testing.T) {
	t.Parallel()
	p, _ := signedOutProvider(t)
	writeLogin(t, p.ws, `{"token":"expired"}`, "")
	if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindAuth {
		t.Fatalf("the expired login answered %v, want auth", err)
	}
	if err := p.Unready(t.Context()); llm.KindOf(err) != llm.KindAuth {
		t.Fatalf("after a refused login Unready = %v, want auth", err)
	}
	answerNext(p)
	if _, err := ask(t, p, llm.Request{}); err != nil {
		t.Fatalf("the mended harness did not answer: %v", err)
	}
	if err := p.Unready(t.Context()); err != nil {
		t.Errorf("after an answered call Unready = %v, want nil", err)
	}
}

// A LOGIN WRITTEN FROM ANOTHER PROCESS LIFTS THE VERDICT AT ONCE: `crewlet llm
// login` mends the files this process never hears about, and a verdict that
// outlived it would turn the person away from a model that now answers.
//
// Mutation: drop the stamp comparison in Unready, and the verdict holds.
func TestANewLoginLiftsASignedOutVerdict(t *testing.T) {
	t.Parallel()
	p, _ := signedOutProvider(t)
	writeLogin(t, p.ws, `{"token":"expired"}`, "")
	if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindAuth {
		t.Fatalf("the expired login answered %v, want auth", err)
	}
	writeLogin(t, p.ws, `{"token":"fresh and longer than before"}`, "")
	if err := p.Unready(t.Context()); err != nil {
		t.Errorf("after a new login Unready = %v, want nil", err)
	}
}

// NOTHING THAT SIGNS THE HARNESS IN IS SAID UP FRONT, and the verdict cannot lock
// the entry out: past [readinessMemory] it lifts and holds off until a call
// has been tried, since a sign-in the engine cannot see would otherwise keep
// a working model from every caller that checks first.
//
// Mutation: drop the retry flag, and the static verdict comes straight back.
func TestNoSignInIsUnreadyForABoundedTime(t *testing.T) {
	t.Parallel()
	p, clock := signedOutProvider(t)
	if err := p.Unready(t.Context()); llm.KindOf(err) != llm.KindAuth {
		t.Fatalf("an entry with no sign-in is Unready = %v, want auth", err)
	}
	clock.advance(readinessMemory)
	if err := p.Unready(t.Context()); err != nil {
		t.Fatalf("past the memory Unready = %v, want nil: the entry is owed one call", err)
	}
	if err := p.Unready(t.Context()); err != nil {
		t.Errorf("the verdict came back before any call was tried: %v", err)
	}
	if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindAuth {
		t.Fatalf("the call answered %v, want auth", err)
	}
	if err := p.Unready(t.Context()); llm.KindOf(err) != llm.KindAuth {
		t.Errorf("after the tried call failed Unready = %v, want auth again", err)
	}
}

// A PROFILE THAT DECLARES NO SIGN-IN ROUTE IS NOT JUDGED SIGNED OUT: the engine
// cannot see how such a harness signs in, so its configuration says nothing.
func TestAProfileWithNoSignInRouteIsNotJudged(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "ok"}, nil)
	if err := p.Unready(t.Context()); err != nil {
		t.Errorf("Unready = %v, want nil for a profile naming no sign-in route", err)
	}
}

// A HARNESS THAT CANNOT START IS SAID AS SUCH: its failure wraps
// [llm.ErrNotRunnable], which a caller tells a person as a missing program
// rather than a failure, and the entry stays unready until a call answers.
//
// Mutation: drop the ErrNotRunnable wrap at the exec, and errors.Is is false.
func TestAHarnessThatCannotStartIsUnready(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "ok"}, nil)
	p.profile.Binary = "/nonexistent/crewlet-no-cli"
	_, err := ask(t, p, llm.Request{})
	if llm.KindOf(err) != llm.KindFatal || !errors.Is(err, llm.ErrNotRunnable) {
		t.Fatalf("a missing binary answered %v, want a fatal wrapping ErrNotRunnable", err)
	}
	if err := p.Unready(t.Context()); !errors.Is(err, llm.ErrNotRunnable) {
		t.Errorf("Unready = %v, want the harness that cannot start", err)
	}
}
