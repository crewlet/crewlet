package seataccess_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// EVERY STATUS IS REACHED, AND BY THE RULE THE AGENTS TAB DRAWS: what needs a
// person outranks what is moving, which outranks what is at rest.
func TestEachLaneReadsAsOneStatus(t *testing.T) {
	t.Parallel()
	on := seataccess.Desired{Enabled: true, Level: "a"}
	step := seataccess.Step{Op: seataccess.OpLevel, By: seataccess.ByCrewlet}
	yours := seataccess.Step{Op: seataccess.OpLevel, By: seataccess.ByYou}
	failed := &seataccess.SyncResult{Outcome: seataccess.OutcomeFailed}
	waiting := &seataccess.SyncResult{Outcome: seataccess.OutcomeWaiting}
	cases := []struct {
		name              string
		la                seataccess.LaneAnswer
		syncing, departed bool
		want              seataccess.LaneStatus
	}{
		{"departed", seataccess.LaneAnswer{Desired: on, Sync: failed}, true, true, seataccess.StatusDeparted},
		{"syncing", seataccess.LaneAnswer{Desired: on, Sync: failed, Plan: []seataccess.Step{step}},
			true, false, seataccess.StatusSyncing},
		{"a sync running with nothing to take", seataccess.LaneAnswer{Desired: on}, true, false,
			seataccess.StatusInSync},
		{"refused", seataccess.LaneAnswer{Desired: on, Sync: failed, Plan: []seataccess.Step{step}},
			false, false, seataccess.StatusRefused},
		{"changed at the vendor", seataccess.LaneAnswer{Desired: on, Plan: []seataccess.Step{yours},
			Observed: seataccess.Observed{ChangedByHandAt: time.Unix(1, 0)}}, false, false,
			seataccess.StatusChangedAtVendor},
		{"changed and restored already", seataccess.LaneAnswer{Desired: on,
			Observed: seataccess.Observed{ChangedByHandAt: time.Unix(1, 0)}}, false, false,
			seataccess.StatusInSync},
		{"a step only a person takes", seataccess.LaneAnswer{Desired: on, Plan: []seataccess.Step{step, yours}},
			false, false, seataccess.StatusNeedsYou},
		{"a sync waiting on a person", seataccess.LaneAnswer{Desired: on, Sync: waiting}, false, false,
			seataccess.StatusNeedsYou},
		{"next sync", seataccess.LaneAnswer{Desired: on, Plan: []seataccess.Step{step}}, false, false,
			seataccess.StatusNextSync},
		{"in sync", seataccess.LaneAnswer{Desired: on}, false, false, seataccess.StatusInSync},
		{"off, account kept", seataccess.LaneAnswer{Observed: seataccess.Observed{Exists: true}}, false, false,
			seataccess.StatusOffAccountKept},
		{"off", seataccess.LaneAnswer{}, false, false, seataccess.StatusOff},
	}
	reached := map[seataccess.LaneStatus]bool{}
	for _, c := range cases {
		got := seataccess.StatusOf(c.la, c.syncing, c.departed)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		reached[got] = true
	}
	for _, s := range seataccess.LaneStatuses() {
		if !reached[s] {
			t.Errorf("no case reaches %q, so its rule is untested", s)
		}
		if !s.Valid() || s.Label("GitHub") == string(s) {
			t.Errorf("%q has no words of its own", s)
		}
	}
	if got := seataccess.StatusChangedAtVendor.Label("GitHub"); got != "Changed at GitHub" {
		t.Errorf("changed at the vendor reads %q", got)
	}
	if seataccess.LaneStatus("drafted").Valid() {
		t.Error("a status the engine never sends is valid")
	}
}

// AN ANSWER'S STATUSES ARE SET FROM THE ANSWER WHOLE: its sync in flight and
// each seat's departure.
func TestAnAnswerSetsEveryLanesStatus(t *testing.T) {
	t.Parallel()
	gh := seataccess.LaneGitHub
	answer := seataccess.Answer{Syncing: true, Seats: []seataccess.SeatAnswer{
		{Handle: "sre", Lanes: map[seataccess.Lane]seataccess.LaneAnswer{gh: {
			Desired: seataccess.Desired{Enabled: true},
			Plan:    []seataccess.Step{{Op: seataccess.OpLevel}}}}},
		{Handle: "qa", Departed: true, Lanes: map[seataccess.Lane]seataccess.LaneAnswer{gh: {}}},
	}}
	answer.SetStatuses()
	if got := answer.Seats[0].Lanes[gh].Status; got != seataccess.StatusSyncing {
		t.Errorf("a lane with steps while its sync runs reads %q", got)
	}
	if got := answer.Seats[1].Lanes[gh].Status; got != seataccess.StatusDeparted {
		t.Errorf("a departed seat's lane reads %q", got)
	}
}
