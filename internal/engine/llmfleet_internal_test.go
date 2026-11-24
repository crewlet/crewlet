package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// fleetEvents collects the fleet events r's queue carries.
type fleetEvents struct {
	mu   sync.Mutex
	full []types.LLMFleetFull
	room []types.LLMFleetRoom
}

func watchFleet(t *testing.T, r *limitedRig) *fleetEvents {
	t.Helper()
	w := &fleetEvents{}
	for _, typ := range []string{"llm_fleet_full", "llm_fleet_room"} {
		if err := r.q.Subscribe(t.Context(), topics.Event(typ), "test-"+typ,
			func(_ context.Context, ev *events.Event) queue.Result {
				w.mu.Lock()
				defer w.mu.Unlock()
				if p, ok := events.DataAs[*types.LLMFleetFull](ev); ok && p != nil {
					w.full = append(w.full, *p)
				}
				if p, ok := events.DataAs[*types.LLMFleetRoom](ev); ok && p != nil {
					w.room = append(w.room, *p)
				}
				return queue.Ack()
			}); err != nil {
			t.Fatalf("Subscribe %s: %v", typ, err)
		}
	}
	return w
}

func (w *fleetEvents) seen() ([]types.LLMFleetFull, []types.LLMFleetRoom) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.full), slices.Clone(w.room)
}

// A SEAT ON A FLEET WITH NO MEMBER WAITS, AND A MEMBER JOINING RELEASES IT:
// its inbox is held with no probe to arm, since no call can end the wait,
// nothing is published as a park (the seat reads the fleet's "No members
// yet"), and the apply that adds a member lifts the hold, so the held mail
// runs once.
//
// Mutation: drop the dispatcher's empty-fleet branch, and the delivery is
// NAKed into the same refusal instead of held.
func TestASeatOnAnEmptyFleetWaitsUntilAMemberJoins(t *testing.T) {
	t.Parallel()
	model := &limitedModel{}
	r := newLimitedRig(t, chain.Member{Key: "default", Provider: model})
	model.now = r.clock
	empty := true
	r.parkRig.mu.Lock()
	r.turnFn = func() (turn.Result, error) {
		if empty {
			return turn.Result{}, fmt.Errorf("runner: review: %w", &phase.EmptyFleetError{Fleet: "default-fleet"})
		}
		return turn.Result{}, nil
	}
	r.parkRig.mu.Unlock()

	first := r.send(t)
	if !slices.Contains(r.holds(), pauseReasonProvider) {
		t.Fatalf("holds = %v, want the seat held on its inbox", r.holds())
	}
	if got := r.alarms.live(); len(got) != 0 {
		t.Errorf("%d alarms armed, want none: no call can answer an empty fleet", len(got))
	}
	if refusal, held := r.e.providerParkRefusal("lead"); !held || !refusal.Until.IsZero() ||
		refusal.Reason != emptyFleetReason("default-fleet") {
		t.Errorf("resume refusal = %+v (%v), want the empty fleet's, with no instant", refusal, held)
	}

	// The renew that resumes the quiesced attachment hands it nothing while held.
	r.renew(t)
	if got := r.runs(); got != 1 {
		t.Fatalf("%d turns ran during the hold, want the one that met the empty fleet", got)
	}
	r.parkRig.mu.Lock()
	empty = false
	r.parkRig.mu.Unlock()
	next := leadCompany(t, "100000000", "0")
	next.Models = newEmptyRegistry(t)
	r.e.epoch.current.Store(next)
	r.e.reconcileProviderParks(t.Context(), next)
	if slices.Contains(r.holds(), pauseReasonProvider) {
		t.Fatalf("the apply that added a member left the hold: %v", r.holds())
	}
	if got := r.workedIDs(); !slices.Equal(got, []uuid.UUID{first, first}) {
		t.Errorf("turns ran on %v, want the held delivery again, once", got)
	}
	if got := r.parks(); len(got) != 0 {
		t.Errorf("published %d parks, want none for an empty fleet", len(got))
	}
	if got := r.releases(); len(got) != 0 {
		t.Errorf("published %d releases, want none for an empty fleet", len(got))
	}
}

// EVERY MEMBER OF A FLEET AT ITS LIMIT IS ONE WAIT: the seat parks on the
// fleet's walk with one probe at the earliest room, llm_fleet_full names who
// waits and the member with the first room, and the probe's answer publishes
// llm_fleet_room naming the member that answered and the seats it releases.
func TestAFleetAtItsLimitSaysWhoWaitsAndWhenItHasRoom(t *testing.T) {
	t.Parallel()
	soon, later := parkedAt.Add(20*time.Minute), parkedAt.Add(3*time.Hour)
	a, b := &limitedModel{reset: later}, &limitedModel{reset: soon}
	r := newLimitedRig(t, chain.Member{Key: "a", Provider: a}, chain.Member{Key: "b", Provider: b})
	a.now, b.now = r.clock, r.clock
	w := watchFleet(t, r)
	ledger := fleet.NewLedger(r.clock)
	picks := ledger.Walk(fleet.Fleet{Key: "opus", Strategy: fleet.Balanced, Threshold: 5, Members: []fleet.Member{
		{Key: "b", Plan: true, State: fleet.Limit, Until: soon},
		{Key: "a", Plan: true, State: fleet.Limit, Until: later},
	}}, fleet.Job{Fleet: "opus", Subject: "lead", Job: "execute"})
	walk, err := chain.New([]chain.Member{{Key: "b", Provider: b}, {Key: "a", Provider: a}},
		chain.Options{Fleet: picks})
	if err != nil {
		t.Fatalf("chain.New: %v", err)
	}
	r.parkRig.mu.Lock()
	r.turnFn = func() (turn.Result, error) {
		_, err := walk.Complete(context.Background(), llm.Request{})
		return turn.Result{}, err
	}
	r.parkRig.mu.Unlock()

	r.send(t)
	alarms := r.alarms.live()
	if len(alarms) != 1 || alarms[0].wait != soon.Sub(parkedAt) {
		t.Fatalf("alarms = %+v, want one probe at the earliest room, 20 minutes away", alarms)
	}
	full, _ := w.seen()
	if len(full) != 1 || full[0].Fleet != "opus" || !slices.Equal(full[0].Waiting, []string{"lead"}) ||
		full[0].FirstRoomMember != "b" || full[0].FirstRoomAt == nil || !full[0].FirstRoomAt.Equal(soon) {
		t.Fatalf("llm_fleet_full = %+v, want opus with lead waiting and b's room at %v", full, soon)
	}

	r.at(soon.Add(time.Second))
	alarms[0].fire()
	_, room := w.seen()
	if len(room) != 1 || room[0].Fleet != "opus" || room[0].Member != "b" ||
		!slices.Equal(room[0].Released, []string{"lead"}) {
		t.Fatalf("llm_fleet_room = %+v, want b answering for opus and releasing lead", room)
	}
	if slices.Contains(r.holds(), pauseReasonProvider) {
		t.Errorf("the hold outlived the room: %v", r.holds())
	}
}

// A HELPER ON A FLEET RUNS ON ITS PICK: Ask Crewlet set to `fleet:<key>` takes
// the balancer's pick among the members this node builds a helper's run of,
// records it as the helper's home under the key the Fleets answer reads, and
// carries the pick's decision record; a read takes nothing, and a fleet with
// no member refuses as the seat's would.
func TestAHelperOnAFleetRunsOnItsPick(t *testing.T) {
	t.Parallel()
	now := parkedAt
	ledger := fleet.NewLedger(func() time.Time { return now })
	used := map[string]float64{"a": 0.40, "b": 0.10}
	entries := []phase.Entry{{Key: "a", Provider: &limitedModel{now: time.Now}},
		{Key: "b", Provider: &limitedModel{now: time.Now}}}
	reg, err := phase.NewFleetRegistry(entries, map[string]phase.Fleet{
		"opus":  {Strategy: fleet.Balanced, Threshold: 5, Members: []string{"a", "b"}},
		"empty": {Strategy: fleet.Balanced, Threshold: 5},
	}, phase.Balance{Ledger: ledger, Signals: func(key string) fleet.Signals {
		u := used[key]
		return fleet.Signals{Kind: fleet.KindPlan, Readings: []fleet.Reading{{
			LimitReading: llm.LimitReading{Window: llm.LimitSession, State: llm.LimitOK, Used: &u}, Seen: now}}}
	}})
	if err != nil {
		t.Fatalf("NewFleetRegistry: %v", err)
	}
	c := &Company{Models: reg, Helpers: map[string]llm.Provider{"a": entries[0].Provider, "b": entries[1].Provider}}
	job := fleet.Job{Fleet: "opus", Subject: "assistant", Job: "assistant"}

	read, err := helperPick(c, "assistant.model", "fleet:opus", "assistant", false)
	if err != nil || read.key != "b" || ledger.Home(job) != "" {
		t.Fatalf("a read = %q, %v, home %q; want b, the least used, and nothing taken", read.key, err, ledger.Home(job))
	}
	run, err := helperPick(c, "assistant.model", "fleet:opus", "assistant", true)
	if err != nil || run.key != "b" || ledger.Home(job) != "b" {
		t.Fatalf("a turn's pick = %q, %v, home %q; want b taken as the helper's home", run.key, err, ledger.Home(job))
	}
	if run.fleet.Fleet != "opus" || run.fleet.FleetReason != string(fleet.ReasonLowestShare) ||
		run.fleet.ShareAtPick == nil || *run.fleet.ShareAtPick != 10 {
		t.Errorf("decision = %+v, want opus, lowest share, at 10", run.fleet)
	}
	if pinned, err := helperPick(c, "assistant.model", "a", "assistant", true); err != nil ||
		pinned.key != "a" || pinned.fleet.Fleet != "" {
		t.Errorf("a pinned helper = %+v, %v; want a with no decision", pinned, err)
	}
	var empty *phase.EmptyFleetError
	if _, err := helperPick(c, "assistant.model", "fleet:empty", "assistant", true); !errors.As(err, &empty) {
		t.Errorf("a helper on an empty fleet = %v, want its EmptyFleetError", err)
	}
}
