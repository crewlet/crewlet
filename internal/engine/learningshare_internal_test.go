package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// countingProvider answers every completion with a fixed spend, counting calls.
type countingProvider struct {
	calls int
}

func (p *countingProvider) Model() string { return "aux" }

func (p *countingProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	p.calls++
	return &llm.Completion{InputTokens: 40, OutputTokens: 10}, nil
}

// A SEAT'S LEARNING WAITS ONCE IT HAS TAKEN ITS SHARE OF THE DAY: a learning
// call under the share runs and is counted on the seat's learning counter, one
// at or past it is not made and says why, and the next company day starts it
// again. Work that is not learning is never held.
func TestASeatsLearningWaitsAtItsShareOfTheDay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	day := time.Date(2026, 3, 6, 9, 0, 0, 0, time.UTC)
	seatScope, learningScope := coord.AgentScope("a-1"), coord.LearningScope("a-1")
	charge := func(scope string, tokens int) {
		t.Helper()
		if _, err := fleet.PostChargeSeat(ctx, scope, tokens, coord.WindowsAt(day, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	charge(seatScope, 1000)
	charge(learningScope, 90)
	share := &learningShare{budgets: fleet, seat: seatScope, learning: learningScope, zone: time.UTC, max: 0.15}
	inner := &countingProvider{}
	call := func(purpose types.AuxPurpose, at time.Time) error {
		p := recordedProvider{inner: inner, use: auxspend.Use{Stage: types.AuxStageReflection, Purpose: purpose},
			now: func() time.Time { return at }, share: share}
		if !purpose.Learning() {
			p.share = nil
		}
		_, err := p.Complete(ctx, llm.Request{})
		return err
	}
	if err := call(types.AuxPersistDecider, day); err != nil {
		t.Fatalf("a learning call at 9%% of the day was held: %v", err)
	}
	learned, err := fleet.Used(ctx, learningScope, coord.WindowsAt(day, time.UTC))
	if err != nil || learned.In(period.Day).Used != 140 {
		t.Fatalf("the learning counter reads %+v (%v), want the call's 50 tokens on top of 90", learned, err)
	}
	charge(learningScope, 20)
	before := inner.calls
	if err := call(types.AuxSkillSynthesizer, day); !errors.Is(err, learning.ErrLearningShare) || inner.calls != before {
		t.Fatalf("a learning call at 16%% of the day answered %v after %d calls, want held, unmade",
			err, inner.calls-before)
	}
	if err := call(types.AuxMemoryFilter, day); err != nil {
		t.Errorf("work that is not learning was held: %v", err)
	}
	if err := call(types.AuxPersistDecider, day.Add(24*time.Hour)); err != nil {
		t.Errorf("the next company day's learning call was held: %v", err)
	}
}
