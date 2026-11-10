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
// call just under the share runs and is counted on the seat's learning counter,
// one at it or past it is not made and says why, and the next company day
// starts it again. Work that is not learning is never held, and neither is the
// compaction the raw cap asks for, which keeps the seat's rows bounded.
func TestASeatsLearningWaitsAtItsShareOfTheDay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	day := time.Date(2026, 3, 6, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		learned int
		purpose types.AuxPurpose
		at      time.Time
		held    bool
	}{
		{"just under the share", 149, types.AuxPersistDecider, day, false},
		{"at the share exactly", 150, types.AuxPersistDecider, day, true},
		{"past the share", 160, types.AuxSkillSynthesizer, day, true},
		{"the raw cap's compaction past the share", 160, types.AuxEpisodeCompaction, day, false},
		{"work that is not learning past the share", 160, types.AuxMemoryFilter, day, false},
		{"the next company day", 160, types.AuxPersistDecider, day.Add(24 * time.Hour), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fleet := coordmem.NewFleet()
			seatScope, learningScope := coord.AgentScope("a-1"), coord.LearningScope("a-1")
			for scope, tokens := range map[string]int{seatScope: 1000, learningScope: c.learned} {
				if _, err := fleet.PostChargeSeat(ctx, scope, tokens, coord.WindowsAt(day, time.UTC)); err != nil {
					t.Fatal(err)
				}
			}
			inner := &countingProvider{}
			p := recordedProvider{inner: inner, use: auxspend.Use{Stage: types.AuxStageReflection, Purpose: c.purpose},
				now:   func() time.Time { return c.at },
				share: &learningShare{budgets: fleet, seat: seatScope, learning: learningScope, zone: time.UTC, max: 0.15}}
			if !c.purpose.Learning() {
				p.share = nil
			}
			_, err := p.Complete(ctx, llm.Request{})
			if held := errors.Is(err, learning.ErrLearningShare); held != c.held || (held && inner.calls != 0) {
				t.Fatalf("held %v (%v) after %d calls, want held %v", held, err, inner.calls, c.held)
			}
			if c.held || !c.purpose.Learning() {
				return
			}
			learned, err := fleet.Used(ctx, learningScope, coord.WindowsAt(c.at, time.UTC))
			if err != nil || learned.In(period.Day).Used < 50 {
				t.Errorf("the learning counter reads %+v (%v), want the call's 50 tokens counted", learned, err)
			}
		})
	}
}
