package statelog_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A TRACKER HANDS ON EVERY TRANSITION ONCE.
//
// An instance raised is `fired` and one gone is `cleared`, each ONCE however
// many evaluations it stands through. A per-log alarm is one
// transition per log, as its log lines are: a log joining a kind already up is
// a fire of its own and one leaving it a clear of its own. Each carries the
// node, the alarm, the log, the measurement, when the instance was raised and
// when it changed, under the id statelog.TransitionID derives from them, so no
// two transitions share an id and the same transition is the same id however
// often it is derived. A tracker nobody asked to be told hands on nothing and
// observes as it always did.
//
// Mutation: hand on the firing set of every evaluation rather than its
// changes, or derive an id that leaves out the change, and the case is red.
func TestATrackerHandsOnEveryTransitionOnce(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	clock := start
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })
	var got []statelog.AlarmTransition
	tracker.OnTransition("node-a", func(tr statelog.AlarmTransition) { got = append(got, tr) })
	silent := statelog.NewTracker(nil, func() time.Time { return clock })

	backup := statelog.Alarm{Kind: statelog.KindBackupAge, Detail: "no backup for 3d"}
	lag := func(detail string) statelog.Alarm {
		return statelog.Alarm{Kind: statelog.KindApplyLag, Detail: detail}
	}
	trim := func(logs ...string) statelog.Alarm {
		a := statelog.Alarm{Kind: statelog.KindTrimBlocked, Detail: "a trim is held"}
		for _, l := range logs {
			a.Domains = append(a.Domains, statelog.AlarmDomain{Domains: []string{l}, Detail: "held on " + l})
		}
		return a
	}
	for i, round := range []struct {
		alarms []statelog.Alarm
		want   []string
	}{
		{[]statelog.Alarm{backup}, []string{"fired backup_age"}},
		{[]statelog.Alarm{backup}, nil},
		{[]statelog.Alarm{backup, lag("4m behind"), trim("tracker")},
			[]string{"fired apply_lag", "fired trim_blocked/tracker"}},
		{[]statelog.Alarm{lag("5m behind"), trim("tracker", "pages")},
			[]string{"fired trim_blocked/pages", "cleared backup_age"}},
		{[]statelog.Alarm{trim("pages")}, []string{"cleared apply_lag", "cleared trim_blocked/tracker"}},
		{nil, []string{"cleared trim_blocked/pages"}},
	} {
		before := len(got)
		tracker.Observe(t.Context(), round.alarms)
		silent.Observe(t.Context(), round.alarms)
		var said []string
		for _, tr := range got[before:] {
			name := string(tr.Change) + " " + string(tr.Kind)
			if tr.Domain != "" {
				name += "/" + tr.Domain
			}
			said = append(said, name)
			if !tr.At.Equal(clock) || tr.Node != "node-a" {
				t.Errorf("round %d: %s is stamped %s at %v, want node-a at %v", i, name, tr.Node,
					tr.At, clock)
			}
		}
		if !slices.Equal(said, round.want) {
			t.Errorf("round %d handed on %v, want %v", i, said, round.want)
		}
		clock = clock.Add(statelog.AlarmInterval)
	}

	byName := map[string]statelog.AlarmTransition{}
	ids := map[string]bool{}
	for _, tr := range got {
		if ids[tr.ID] {
			t.Errorf("two transitions share the id %s", tr.ID)
		}
		ids[tr.ID] = true
		if want := statelog.TransitionID(tr.Node, tr.Kind, tr.Domain, tr.Entry, tr.Change, tr.Since); tr.ID != want {
			t.Errorf("%s %s carries the id %s, want the derived %s", tr.Change, tr.Kind, tr.ID, want)
		}
		byName[fmt.Sprintf("%s %s/%s", tr.Change, tr.Kind, tr.Domain)] = tr
	}
	beat := func(n int) time.Time { return start.Add(time.Duration(n) * statelog.AlarmInterval) }
	for name, want := range map[string]struct {
		since  time.Time
		detail string
	}{
		"fired backup_age/":            {beat(0), "no backup for 3d"},
		"cleared backup_age/":          {beat(0), "no backup for 3d"},
		"fired apply_lag/":             {beat(2), "4m behind"},
		"cleared apply_lag/":           {beat(2), "5m behind"},
		"fired trim_blocked/pages":     {beat(3), "held on pages"},
		"cleared trim_blocked/tracker": {beat(2), "held on tracker"},
		"cleared trim_blocked/pages":   {beat(3), "held on pages"},
		"fired trim_blocked/tracker":   {beat(2), "held on tracker"},
	} {
		tr, ok := byName[name]
		if !ok {
			t.Errorf("no transition %q was handed on", name)
			continue
		}
		if !tr.Since.Equal(want.since) || tr.Detail != want.detail {
			t.Errorf("%s: since %v with %q, want since %v with %q", name, tr.Since, tr.Detail,
				want.since, want.detail)
		}
	}
	if kinds, observed := silent.Standing(); !observed || len(kinds) != 0 {
		t.Errorf("a tracker nobody asked to be told stands at %v (observed %v), want nothing standing",
			kinds, observed)
	}
	for _, change := range statelog.TransitionChanges() {
		if !change.Valid() {
			t.Errorf("%s is in the closed set and not valid", change)
		}
	}
	if statelog.TransitionChange("escalated").Valid() {
		t.Error("a change this build does not make reads as valid")
	}
}

// TWO ENTRIES OF ONE PER-ENTRY KIND THAT RISE TOGETHER ARE TWO TRANSITIONS,
// each naming its entry under an id of its own, because a wake is keyed on the
// id and two servers raised in one evaluation share the node, the kind, the
// change and the instant. Their clears are told apart the same way, so one
// server recovering while the other stays down is not the alarm clearing.
//
// Mutation: leave the entry out of the transition or out of [TransitionID], and
// the two carry no entry, or one id.
func TestTwoEntriesRisingTogetherAreTwoTransitions(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })
	var got []statelog.AlarmTransition
	tracker.OnTransition("node-a", func(tr statelog.AlarmTransition) { got = append(got, tr) })
	down := func(servers ...string) []statelog.Alarm {
		a := statelog.Alarm{Kind: statelog.KindMCPServerFailing, Detail: "servers are down"}
		for _, s := range servers {
			a.Entries = append(a.Entries, statelog.AlarmEntry{Key: s, Detail: s + " is down"})
		}
		return []statelog.Alarm{a}
	}
	tracker.Observe(t.Context(), nil)
	tracker.Observe(t.Context(), down("github", "linear"))
	clock = clock.Add(statelog.AlarmInterval)
	tracker.Observe(t.Context(), down("linear"))

	var said []string
	ids := map[string]bool{}
	for _, tr := range got {
		said = append(said, string(tr.Change)+" "+tr.Entry+": "+tr.Detail)
		if ids[tr.ID] {
			t.Errorf("%s %s shares the id %s with another transition", tr.Change, tr.Entry, tr.ID)
		}
		ids[tr.ID] = true
		if want := statelog.TransitionID(tr.Node, tr.Kind, tr.Domain, tr.Entry, tr.Change, tr.Since); tr.ID != want {
			t.Errorf("%s %s carries the id %s, want the derived %s", tr.Change, tr.Entry, tr.ID, want)
		}
	}
	want := []string{"fired github: github is down", "fired linear: linear is down",
		"cleared github: github is down"}
	if !slices.Equal(said, want) {
		t.Errorf("the tracker handed on %q, want %q", said, want)
	}
}
