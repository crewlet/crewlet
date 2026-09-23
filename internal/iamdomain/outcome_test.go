package iamdomain_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// silentBroker is an appender that, while `silent` is set, answers nothing for
// every subject containing `on` — an append that may or may not have landed,
// and a probe that cannot say which: the framework's `unknown`.
type silentBroker struct {
	statelog.Appender
	on     string
	silent atomic.Bool

	// asked is set once an append went unanswered: the probe that follows
	// it is what cannot be answered either, while the reads a write makes
	// BEFORE it appends still are.
	asked atomic.Bool
}

var errNoAnswer = errors.New("no responders: the broker did not answer")

func (b *silentBroker) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	if b.silent.Load() && strings.Contains(subject, b.on) {
		b.asked.Store(true)
		return 0, false, errNoAnswer
	}
	return b.Appender.Append(ctx, subject, msgID, expect, body)
}

func (b *silentBroker) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	if b.silent.Load() && b.asked.Load() && strings.Contains(subject, b.on) {
		return 0, false, errNoAnswer
	}
	return b.Appender.LastSeq(ctx, subject)
}

// AN ENROLMENT STOPS AT A STEP NOBODY CAN CONFIRM, AND SAYS UNKNOWN.
//
// An enrolment is claims first and the person last, each step built on the one
// before. A step whose outcome nothing can establish used to come back as a
// zero position and a nil error, so the sequence went on and published the
// person — and the gesture reported success — on a login claim that may never
// have landed. It now stops there and answers `unknown` under the GESTURE's op
// id, which is what a caller retries under: every step's id is derived from
// it, so the retry lands exactly the steps that did not.
//
// Mutation: carry on past an unknown claim and the person record is published
// (and the gesture answers applied).
func TestAnEnrolmentStopsAtAStepNobodyCanConfirm(t *testing.T) {
	t.Parallel()
	var broker *silentBroker
	rig := newWriteRigWith(t, func(inner statelog.Appender) statelog.Appender {
		broker = &silentBroker{Appender: inner, on: ".login."}
		return broker
	})
	broker.silent.Store(true)
	const person = "018f3a9c-0000-7000-8000-00000000e001"
	in := iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Erin", Email: "erin@example.com", Login: "erin.ops",
		OpID: "people:create:erin",
	}
	var result statelog.Result
	if err := rig.draining(func() error {
		var err error
		result, err = rig.writer.Enrol(t.Context(), in)
		return err
	}); err != nil {
		t.Fatalf("the enrolment refused: %v", err)
	}
	if result.Outcome != statelog.OutcomeUnknown || result.OpID != in.OpID {
		t.Fatalf("answered %+v, want unknown under the gesture's own op id %q",
			result, in.OpID)
	}
	var kind string
	err := rig.db.Replicated().SQL().QueryRowContext(t.Context(),
		`SELECT kind FROM iam_people WHERE id = ?`, person).Scan(&kind)
	if err == nil && kind != "" {
		t.Errorf("the person record was published (kind %q) on a login claim "+
			"nobody could confirm", kind)
	}

	// THE RETRY, under the same op id, once the broker answers again.
	broker.silent.Store(false)
	if err := rig.draining(func() error {
		var err error
		result, err = rig.writer.Enrol(t.Context(), in)
		return err
	}); err != nil {
		t.Fatalf("the retry refused: %v", err)
	}
	if result.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the retry answered %+v, want applied", result)
	}
}

// A REPLAY'S REVOCATION LANDS ONCE HOWEVER OFTEN IT IS ASKED FOR.
//
// Every ingress node a replayed cookie reaches asks for it, and so does a node
// whose once-per-lineage dedupe forgot the lineage — and each unconditional
// revocation moved the epoch again, ending the sessions the person had opened
// since the last. RevokePast moves it only while it is still at the replayed
// bearer's, decided in the snapshot it publishes from; past it, nothing is
// published at all.
//
// Mutation: drop the condition and the second ask moves the epoch to 2 and
// appends a record.
func TestAReplaysRevocationLandsOnceHoweverOftenItIsAsked(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	person := enrolForSessions(t, rig)
	epoch := func() string {
		got := rig.column(`SELECT epoch FROM iam_revocation_epochs WHERE person_id = ?`, person)
		if len(got) != 1 {
			return "none"
		}
		return got[0]
	}
	ask := func(opID string) statelog.Result {
		t.Helper()
		var result statelog.Result
		if err := rig.draining(func() error {
			var err error
			result, err = rig.writer.RevokePast(t.Context(), person, 0, opID,
				"a replayed cookie")
			return err
		}); err != nil {
			t.Fatalf("revoke past: %v", err)
		}
		rig.drain()
		return result
	}
	first := ask("session-reuse:node-a")
	if first.Outcome != statelog.OutcomeApplied || first.Position.Seq == 0 {
		t.Fatalf("the first ask answered %+v, want a record applied", first)
	}
	if got := epoch(); got != "1" {
		t.Fatalf("epoch %s after the first ask, want 1", got)
	}
	end, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// ANOTHER NODE, the same replayed bearer, its own op id.
	second := ask("session-reuse:node-b")
	if second.Outcome != statelog.OutcomeApplied || second.Position.Seq != 0 {
		t.Errorf("the second ask answered %+v, want applied with nothing "+
			"published", second)
	}
	if got := epoch(); got != "1" {
		t.Errorf("epoch %s after the second ask, want it still 1 — the "+
			"sessions opened since the first were ended again", got)
	}
	if after, _ := rig.log.End(t.Context()); after != end {
		t.Errorf("the log moved from %d to %d on an ask with nothing to do",
			end, after)
	}

	// THE CONTROL: an unconditional revocation still moves it.
	if err := rig.draining(func() error {
		_, err := rig.writer.Revoke(t.Context(), person, "op-signout", "signed out")
		return err
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rig.drain()
	if got := epoch(); got != "2" {
		t.Errorf("an unconditional revocation left the epoch at %s, want 2", got)
	}
}
