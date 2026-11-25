package googlegrowth_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// fakeVerdicts is the access log's one answer per held call.
type fakeVerdicts struct {
	steps     []growth.Step
	answerers []string
	answered  map[string]bool
}

func (f *fakeVerdicts) Verdict(_ context.Context, step growth.Step, r growth.Record) (bool, error) {
	if f.answered == nil {
		f.answered = map[string]bool{}
	}
	if f.answered[r.HeldCall] {
		return false, nil
	}
	f.answered[r.HeldCall] = true
	f.steps, f.answerers = append(f.steps, step), append(f.answerers, r.Answerer)
	return true, nil
}

func (f *fakeVerdicts) Made(context.Context, growth.Record) (bool, error)    { return true, nil }
func (f *fakeVerdicts) Applied(context.Context, growth.Record) (bool, error) { return true, nil }
func (f *fakeVerdicts) Void(context.Context, growth.Record) (bool, error)    { return true, nil }
func (f *fakeVerdicts) Failed(context.Context, growth.Record) (bool, error)  { return true, nil }

// A HELD CALL ACTS ONLY ON ITS ONE ANSWER, RECORDED FIRST: an allow that
// lands is the call's, a second answer to it is refused, and a node that
// records no answer acts on none.
//
// Mutation: answer nil where the allow was not the first, and a lapsed call
// is made on a late allow.
func TestAHeldCallActsOnlyOnItsOneAnswerRecordedFirst(t *testing.T) {
	t.Parallel()
	v := &fakeVerdicts{}
	c := &googlegrowth.Client{Kind: "x", Audit: v}
	r := growth.Record{HeldCall: "h1", Tool: "x_publish"}
	if err := c.Allow(t.Context(), r, "founder"); err != nil {
		t.Fatalf("the first allow answered %v", err)
	}
	var refused *growth.Refusal
	if err := c.Allow(t.Context(), r, "founder"); !errors.As(err, &refused) || refused.Kind != growth.RefusedForbidden {
		t.Fatalf("a second answer to one call answered %v", err)
	}
	if err := (&googlegrowth.Client{Kind: "x"}).Allow(t.Context(), r, "founder"); !errors.As(err, &refused) {
		t.Fatalf("a node recording no answer allowed: %v", err)
	}
	if len(v.steps) != 1 || v.steps[0] != growth.StepAllowed || v.answerers[0] != "founder" {
		t.Errorf("recorded %v by %v", v.steps, v.answerers)
	}
}

// A DENIAL NAMES ITS PERSON AND A LAPSE NOBODY: the engine's own answer, or
// one from nobody, is the call lapsing unanswered.
func TestADenialNamesItsPersonAndALapseNobody(t *testing.T) {
	t.Parallel()
	v := &fakeVerdicts{}
	c := &googlegrowth.Client{Kind: "x", Audit: v}
	c.Deny(t.Context(), growth.Call{HeldCall: "h1", Approver: "founder"}, growth.Record{HeldCall: "h1"})
	c.Deny(t.Context(), growth.Call{HeldCall: "h2", Approver: "engine"}, growth.Record{HeldCall: "h2"})
	c.Deny(t.Context(), growth.Call{HeldCall: "h3"}, growth.Record{HeldCall: "h3"})
	want := []growth.Step{growth.StepDenied, growth.StepLapsed, growth.StepLapsed}
	for i, s := range want {
		if v.steps[i] != s || (s == growth.StepLapsed) != (v.answerers[i] == "") {
			t.Errorf("answer %d recorded %s by %q", i, v.steps[i], v.answerers[i])
		}
	}
}

// A STEP THAT NEVER LEFT IS NEVER AN UNKNOWN OUTCOME: an answer lost before
// the act's change was sent ([growth.ErrNotSent]) changed nothing, so it is
// timed out where a deadline cut it (its context's, or a transport's own) and
// the vendor's failing otherwise; the same loss once the change was sent may
// have changed anything.
//
// Mutation: answer vendor_unavailable for every unsent loss, and a staged step
// the apply bound cut reads as Google's failing.
func TestAStepThatNeverLeftIsNeverAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	lost := func(err error) error {
		return &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome, Err: fmt.Errorf("googleauth: %w", err)}
	}
	for _, tc := range []struct {
		name string
		err  error
		want growth.Failure
	}{
		{"cut at its context's deadline", growth.NotSent(lost(context.DeadlineExceeded)), growth.FailureTimedOut},
		{"cut at the transport's timeout", growth.NotSent(lost(&net.DNSError{Err: "i/o timeout", IsTimeout: true})),
			growth.FailureTimedOut},
		{"an answer that cannot be read", growth.NotSent(lost(io.ErrUnexpectedEOF)), growth.FailureVendorUnavailable},
		{"sent, its answer cut at its deadline", lost(context.DeadlineExceeded), growth.FailureUnknownOutcome},
	} {
		if got := googlegrowth.StepFailure(tc.err); got != tc.want {
			t.Errorf("a step %s is recorded %q, want %q", tc.name, got, tc.want)
		}
	}
}
