package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A DATA NODE ANSWERS ONLY WHAT IT SERVES: the partitions gate 3 says it
// serves ([holdingOf]), never one it does not hold and never one whose holding
// it cannot tell — the router asks another holder for those. A partition it
// holds before its runtime is up is still served, with no halves, so every
// operation on it is answered "not here" and moves on rather than being
// refused as a partition nobody serves.
func TestTheLocalEstateAnswersOnlyWhatThisNodeServes(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{}}
	l := &localEstate{e: e, holding: statelog.ServesOnly(statelog.EstatePartition), now: time.Now,
		read: func(context.Context, *native, statelog.PartitionID) (bool, statelog.ReadRefusal) {
			return true, ""
		},
		verdicts: map[statelog.PartitionID]servingVerdict{}}

	b, ok := l.For(t.Context(), statelog.EstatePartition)
	if !ok || b.Tracker != nil {
		t.Fatalf("a held partition with no runtime = (%+v, %v), want served with no halves", b, ok)
	}
	e.native.Store(&native{trackerReader: &tracker.Reader{}})
	if b, ok = l.For(t.Context(), statelog.EstatePartition); !ok || b.Tracker == nil || b.Answers == nil {
		t.Fatalf("a held partition with a runtime = (%+v, %v), want its halves and its gate", b, ok)
	}
	if _, ok := l.For(t.Context(), statelog.PartitionID{Space: statelog.SpaceTracker, Index: 7}); ok {
		t.Error("a partition this node does not hold was served")
	}
	l.holding = failingHolding{}
	if _, ok := l.For(t.Context(), statelog.EstatePartition); ok {
		t.Error("a partition whose holding could not be told was served")
	}
}

type failingHolding struct{}

func (failingHolding) Serving(statelog.PartitionID) (bool, error) {
	return false, errors.New("the executor could not say")
}

// WHETHER A COPY ANSWERS IS READ AT MOST ONCE A RECHECK, and a read that could
// not reach the broker keeps the verdict before it — while a copy never judged
// answers nothing.
//
// The verdict reads every log's bounds from the broker, so read per request
// it would put three round trips in front of every tool call; and one blip
// must not send every request away from a copy that was answering them.
func TestACopysVerdictIsReadOncePerRecheckAndKeptThroughABlip(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	var reads int
	answer, refusal := true, statelog.ReadRefusal("")
	l := &localEstate{e: &Engine{}, holding: statelog.ServesOnly(statelog.EstatePartition),
		now: func() time.Time { return now },
		read: func(context.Context, *native, statelog.PartitionID) (bool, statelog.ReadRefusal) {
			reads++
			return answer, refusal
		},
		verdicts: map[statelog.PartitionID]servingVerdict{}}
	n := &native{}
	p := statelog.EstatePartition

	// A COPY NEVER JUDGED, whose first read found no broker, answers
	// nothing.
	answer, refusal = false, statelog.RefuseBrokerUnreachable
	if l.answers(t.Context(), n, p) {
		t.Fatal("a copy nobody could measure answered")
	}
	now = now.Add(servingRecheck)
	answer, refusal = true, ""
	if !l.answers(t.Context(), n, p) {
		t.Fatal("a copy measured as answering did not")
	}
	for range 10 {
		l.answers(t.Context(), n, p)
	}
	if reads != 2 {
		t.Fatalf("the verdict was read %d times, want 2 — once per recheck", reads)
	}
	now = now.Add(servingRecheck)
	answer, refusal = false, statelog.RefuseBrokerUnreachable
	if !l.answers(t.Context(), n, p) {
		t.Fatal("a broker blip turned an answering copy away")
	}
	now = now.Add(servingRecheck)
	answer, refusal = false, statelog.RefuseBehind
	if l.answers(t.Context(), n, p) {
		t.Fatal("a copy that fell behind still answers")
	}
}
