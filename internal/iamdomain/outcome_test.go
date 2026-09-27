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
