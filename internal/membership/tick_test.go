package membership

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/placement"
)

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// up is a live, healthy data node.
func up(node string, weight int) Presence { return Presence{Node: node, Weight: weight} }

// labelled is a live, healthy data node carrying labels.
func labelled(node string, weight int, labels map[string]string) Presence {
	return Presence{Node: node, Weight: weight, Labels: labels}
}

// sick is a live data node whose store reports itself failed.
func sick(node, detail string) Presence {
	return Presence{Node: node, Weight: 1, Unhealthy: true, Detail: detail}
}

// company is an activation.
func company(epoch uint64, replicas int, domain string) Company {
	return Company{Epoch: epoch, Replicas: replicas, FailureDomain: domain}
}

// roster is n equal, live, healthy data nodes: data-00, data-01, …
func roster(n int) []Presence {
	out := make([]Presence, n)
	for i := range out {
		out[i] = up(fmt.Sprintf("data-%02d", i), 1)
	}
	return out
}

// without is live less the named nodes.
func without(live []Presence, nodes ...string) []Presence {
	return slices.DeleteFunc(slices.Clone(live), func(p Presence) bool {
		return slices.Contains(nodes, p.Node)
	})
}

// fixed is a map's groups as a tick sees them: never drawn over, since
// membership decides who is in a map and never where anything goes.
type fixed int

func (f fixed) Count() int       { return int(f) }
func (fixed) Seed(pg int) uint64 { return uint64(pg) }

// record is one map's membership: the state it remembers and the draw its
// members are taken over.
type record struct {
	s State
	d placement.Draw
}

// empty is a map with nobody in it yet.
func empty() record {
	return record{d: placement.Draw{Salt: placement.EstateSalt, Groups: fixed(64)}}
}

// first is the membership the first tick makes for this fleet and company.
func first(t *testing.T, live []Presence, c Company) record {
	t.Helper()
	r := tick(t, empty(), live, c)
	if len(r.d.Members) == 0 {
		t.Fatal("the first tick admitted nobody")
	}
	return r
}

// ticks runs n ticks against the same fleet.
func ticks(t *testing.T, r record, live []Presence, c Company, n int) record {
	t.Helper()
	for range n {
		r = tick(t, r, live, c)
	}
	return r
}

// tick runs one tick, checking what every tick must hold: a draw that can
// place and a state that agrees with it about who is on probation.
func tick(t *testing.T, r record, live []Presence, c Company) record {
	t.Helper()
	return tickAt(t, r, live, c, t0)
}

// tickAt runs one tick at now.
func tickAt(t *testing.T, r record, live []Presence, c Company, now time.Time) record {
	t.Helper()
	s, d := Tick(r.s, r.d, live, c, now)
	if err := d.Validate(); err != nil {
		t.Fatalf("a tick made a draw that cannot place: %v", err)
	}
	if err := s.Validate(d.Members); err != nil {
		t.Fatalf("a tick made a state a writer would refuse: %v", err)
	}
	return record{s: s, d: d}
}

// nodes is the draw's members' names.
func nodes(d placement.Draw) []string {
	var out []string
	for _, member := range d.Members {
		out = append(out, member.Node)
	}
	return out
}

// placeable reports whether node is a member the draw places on.
func placeable(r record, node string) bool {
	m, ok := r.d.Member(node)
	return ok && m.Placeable()
}

// A FIRST TICK ADMITS EVERY LIVE, HEALTHY DATA NODE, at the copies and label
// the company says — and a node offering no share, or one whose store has
// failed, is no data node at all.
func TestAFirstTickAdmitsTheFleet(t *testing.T) {
	t.Parallel()
	r := first(t, []Presence{up("data-a", 1), up("data-b", 2), up("agent-1", 0),
		sick("data-c", "disk gone")}, company(7, 3, ""))
	switch {
	case !slices.Equal(nodes(r.d), []string{"data-a", "data-b"}) || r.d.Members[1].Weight != 2:
		t.Errorf("members %+v", r.d.Members)
	case r.d.Replicas != 3 || r.s.Config.Epoch != 7:
		t.Errorf("replicas %d from activation %d, want 3 from 7", r.d.Replicas, r.s.Config.Epoch)
	case r.d.Members[0].Share != placement.DefaultShare(1) || r.d.Members[1].Share != placement.DefaultShare(2):
		t.Errorf("shares %+v, want the defaults of a draw with nobody in it", r.d.Members)
	case r.d.Salt != placement.EstateSalt || r.d.Groups.Count() != 64:
		t.Errorf("the tick changed the draw's salt or groups: %+v", r.d)
	}
}

// AN OLDER ACTIVATION NEVER OVERWRITES A NEWER ONE, and a company that cannot
// be applied is not: the duty moves between nodes, and one a revision behind
// must not set the replica count back until the duty moves on. A newer one
// relabels every member it can see.
func TestOnlyTheNewestCompanySetsTheCopies(t *testing.T) {
	t.Parallel()
	live := []Presence{
		labelled("data-a", 1, map[string]string{"zone": "eu-1"}),
		labelled("data-b", 1, map[string]string{"zone": "eu-2"}),
		up("data-c", 1),
	}
	base := first(t, live, company(10, 3, ""))
	for _, c := range []struct {
		name         string
		company      Company
		wantReplicas int
		wantDomain   string
		wantConfig   uint64
	}{
		{"an older activation is ignored", company(9, 1, "zone"), 3, "", 10},
		{"no company is ignored", Company{}, 3, "", 10},
		{"the same activation changes nothing", company(10, 3, ""), 3, "", 10},
		{"a newer activation sets the copies", company(11, 2, ""), 2, "", 11},
		{"a newer activation sets the domain", company(11, 3, "zone"), 3, "zone", 11},
		{"too many copies is refused", company(12, placement.MaxReplicas+1, ""), 3, "", 10},
		{"a label that is no label is refused", company(12, 3, "my zone"), 3, "", 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			next := tick(t, base, live, c.company)
			if next.d.Replicas != c.wantReplicas || next.d.FailureDomain != c.wantDomain ||
				next.s.Config.Epoch != c.wantConfig {
				t.Fatalf("replicas %d, domain %q, from activation %d; want %d, %q, %d",
					next.d.Replicas, next.d.FailureDomain, next.s.Config.Epoch,
					c.wantReplicas, c.wantDomain, c.wantConfig)
			}
			want := []string{"", "", ""}
			if c.wantDomain == "zone" {
				want = []string{"eu-1", "eu-2", ""}
			}
			for i, member := range next.d.Members {
				if member.Domain != want[i] {
					t.Errorf("%s is in domain %q, want %q", member.Node, member.Domain, want[i])
				}
			}
		})
	}
}

// THE FLEET KEEPS ITS MEMBERSHIP WITHOUT A COMPANY: the company says how many
// copies, never who holds them, so a node whose company has not arrived still
// admits a data node that joins — at the copies the map already has.
func TestAJoinerIsAdmittedWithoutACompany(t *testing.T) {
	t.Parallel()
	base := first(t, roster(2), company(10, 3, ""))
	next := tick(t, base, roster(3), Company{})
	if len(next.d.Members) != 3 || next.d.Replicas != 3 || next.s.Config.Epoch != 10 {
		t.Fatalf("a joiner with no company gave %+v from activation %d", next.d, next.s.Config.Epoch)
	}
}

// MEMBERSHIP, one rule each: who joins, at what share and weight, in which
// domain, and who does not.
func TestWhoIsAMember(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "zone")
	base := first(t, []Presence{
		labelled("data-a", 1, map[string]string{"zone": "eu-1"}),
		labelled("data-b", 1, map[string]string{"zone": "eu-2"}),
	}, c)
	base.d.Members[0].Share = 3 << 16 // the fleet runs at 2.0 per unit of weight
	base.d.Members[1].Share = 1 << 16
	for _, tc := range []struct {
		name  string
		live  []Presence
		check func(t *testing.T, members map[string]placement.Member)
	}{
		{
			name: "a joiner starts at the fleet's rate for its weight",
			live: []Presence{up("data-a", 1), up("data-b", 1),
				labelled("data-c", 3, map[string]string{"zone": "eu-3"})},
			check: func(t *testing.T, members map[string]placement.Member) {
				if c := members["data-c"]; c.Share != 6<<16 || c.Weight != 3 || c.Domain != "eu-3" {
					t.Fatalf("the joiner is %+v, want weight 3 at share %d in eu-3", c, 6<<16)
				}
			},
		},
		{
			name: "a changed weight keeps its share per unit of weight",
			live: []Presence{up("data-a", 3), up("data-b", 1)},
			check: func(t *testing.T, members map[string]placement.Member) {
				if a := members["data-a"]; a.Weight != 3 || a.Share != 9<<16 {
					t.Fatalf("data-a is %+v, want weight 3 at share %d", a, 9<<16)
				}
			},
		},
		{
			name: "a weight past the ceiling is held to it",
			live: []Presence{up("data-a", placement.MaxWeight*2), up("data-b", 1)},
			check: func(t *testing.T, members map[string]placement.Member) {
				if a := members["data-a"]; a.Weight != placement.MaxWeight {
					t.Fatalf("data-a weighs %d, want %d", a.Weight, placement.MaxWeight)
				}
			},
		},
		{
			name: "a node offering nothing and one whose store failed do not join",
			live: []Presence{up("data-a", 1), up("data-b", 1), up("agent-1", 0), sick("data-c", "io")},
			check: func(t *testing.T, members map[string]placement.Member) {
				if len(members) != 2 {
					t.Fatalf("members %v, want data-a and data-b", members)
				}
			},
		},
		{
			name: "a node's domain is its value of the label, and none without it",
			live: []Presence{labelled("data-a", 1, map[string]string{"zone": "eu-9"}), up("data-b", 1)},
			check: func(t *testing.T, members map[string]placement.Member) {
				if members["data-a"].Domain != "eu-9" || members["data-b"].Domain != "" {
					t.Fatalf("domains %q and %q, want eu-9 and none",
						members["data-a"].Domain, members["data-b"].Domain)
				}
			},
		},
		{
			name: "an absent member keeps its domain while the label is unchanged",
			live: []Presence{up("data-a", 1)},
			check: func(t *testing.T, members map[string]placement.Member) {
				if members["data-b"].Domain != "eu-2" {
					t.Fatalf("the absent member's domain became %q", members["data-b"].Domain)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tk := newTicking(base.s, base.d, tc.live, t0)
			tk.config(c)
			tk.returning()
			tk.membership()
			tc.check(t, tk.members)
		})
	}

	// A CHANGED LABEL LEAVES AN ABSENT MEMBER IN NO DOMAIN: its value of
	// the old label names nothing, and its value of the new one is unknown
	// until it is seen.
	tk := newTicking(base.s, base.d, []Presence{labelled("data-a", 1, map[string]string{"rack": "r1"})}, t0)
	tk.config(company(2, 2, "rack"))
	tk.membership()
	if tk.members["data-a"].Domain != "r1" || tk.members["data-b"].Domain != "" {
		t.Fatalf("after the label changed: %+v", tk.members)
	}
}

// A STEADY FLEET CHANGES NOTHING: every tick of a map with nothing to count is
// a write saved, so a tick that sees what the last one saw answers what it
// was given.
func TestASteadyFleetChangesNothing(t *testing.T) {
	t.Parallel()
	c := company(1, 3, "")
	r := first(t, roster(4), c)
	if next := tickAt(t, r, roster(4), c, t0.Add(time.Hour)); !reflect.DeepEqual(next, r) {
		t.Fatalf("a steady fleet became %+v", next)
	}
}

// AN ABSENCE IS COUNTED IN TICKS, and changes no member: the member is still
// placed on, and a displaced write lands on the next member of its ranking.
func TestAnAbsenceIsCountedInTicks(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	r := first(t, live, c)
	gone := without(live, "data-02")

	next := tick(t, r, gone, c)
	run := next.s.Absence["data-02"]
	if run.Ticks != 1 || run.Present != 0 || !run.Since.Equal(t0) ||
		run.Reason != ReasonAbsent || !reflect.DeepEqual(next.d, r.d) {
		t.Fatalf("one tick absent: %+v, the draw %+v → %+v", run, r.d, next.d)
	}

	// Since is the run's first tick and stays there.
	later := tickAt(t, next, gone, c, t0.Add(time.Hour))
	if run := later.s.Absence["data-02"]; run.Ticks != 2 || !run.Since.Equal(t0) {
		t.Fatalf("two ticks absent: %+v", run)
	}

	// A member back counts present ticks and keeps its absent ones.
	back := tick(t, later, live, c)
	if run := back.s.Absence["data-02"]; run.Ticks != 2 || run.Present != 1 {
		t.Fatalf("back one tick: %+v", run)
	}
	// Gone again: the present count starts over, the absent one does not.
	again := tick(t, back, gone, c)
	if run := again.s.Absence["data-02"]; run.Ticks != 3 || run.Present != 0 {
		t.Fatalf("gone again: %+v", run)
	}
}

// AN UNHEALTHY MEMBER IS AN ABSENT ONE: up and holding its lease, but its
// store answers for none of what it holds — so it accumulates ticks exactly
// as a member that is down does, and is removed at the same grace.
func TestAnUnhealthyMemberIsCountedAbsent(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	r := first(t, live, c)
	failing := append(without(live, "data-02"), sick("data-02", "fsync: input/output error"))
	r = ticks(t, r, failing, c, OutTicks-1)
	run := r.s.Absence["data-02"]
	if run.Ticks != OutTicks-1 || run.Reason != ReasonUnhealthy ||
		run.Detail != "fsync: input/output error" || !r.d.Holds("data-02") {
		t.Fatalf("after %d unhealthy ticks: %+v", OutTicks-1, run)
	}
	r = tick(t, r, failing, c)
	if r.d.Holds("data-02") {
		t.Fatalf("an unhealthy member was kept past %d ticks", OutTicks)
	}
	if removal := r.s.Removed["data-02"]; removal.Reason != ReasonUnhealthy {
		t.Fatalf("the removal says %+v", removal)
	}
}

// A MEMBER IS REMOVED ON THE TICK ITS RUN REACHES THE GRACE, and on no other —
// counted in ticks, so the removal happens after exactly OutTicks of them
// whatever the clock says: forty ticks with the clock frozen remove a member,
// and thirty-nine a day apart do not.
func TestRemovalCountsTicksNotTime(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	gone := without(live, "data-02")
	start := first(t, live, c)

	r := start
	for i := 1; i <= OutTicks; i++ {
		r = tick(t, r, gone, c) // the clock never moves
		if held := r.d.Holds("data-02"); held != (i < OutTicks) {
			t.Fatalf("after %d ticks the member held = %v", i, held)
		}
		if i < OutTicks && r.s.Absence["data-02"].Ticks != i {
			t.Fatalf("after %d ticks the run is %+v", i, r.s.Absence["data-02"])
		}
	}
	if r.s.Absence != nil || !slices.Equal(nodes(r.d), []string{"data-00", "data-01"}) {
		t.Fatalf("the removal left members %v and absences %v", nodes(r.d), r.s.Absence)
	}
	if removal, ok := r.s.Removed["data-02"]; !ok || removal.Reason != ReasonAbsent ||
		!removal.At.Equal(t0) {
		t.Fatalf("the removal is remembered as %+v (%v)", removal, ok)
	}

	r = start
	for i := range OutTicks - 1 {
		r = tickAt(t, r, gone, c, t0.Add(time.Duration(i)*24*time.Hour))
	}
	if !r.d.Holds("data-02") {
		t.Fatal("a member was removed on elapsed time rather than on ticks")
	}
}

// THE SCENARIOS THE RULES ARE FOR, each a sequence of ticks.
func TestAbsenceScenarios(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	gone := without(live, "data-02")

	// A FLAPPING MEMBER — up two ticks in every ten — is removed once its
	// absent ticks add up, where clearing the run on any sighting kept it
	// for ever.
	t.Run("a flapping member is removed", func(t *testing.T) {
		t.Parallel()
		r := first(t, live, c)
		removedAt := 0
		for i := 1; i <= 10*OutTicks && removedAt == 0; i++ {
			fleet := gone
			if i%10 >= 8 {
				fleet = live
			}
			r = tick(t, r, fleet, c)
			if !r.d.Holds("data-02") {
				removedAt = i
			}
		}
		// Seven absent ticks, then eight in every ten: the fortieth is
		// tick 50.
		if removedAt != 50 {
			t.Fatalf("the flapping member was removed at tick %d, want 50", removedAt)
		}
	})

	t.Run("absent 39, present 39, absent 1 is removed", func(t *testing.T) {
		t.Parallel()
		r := first(t, live, c)
		r = ticks(t, r, gone, c, OutTicks-1)
		r = ticks(t, r, live, c, StableTicks-1)
		if run := r.s.Absence["data-02"]; run.Ticks != OutTicks-1 || run.Present != StableTicks-1 {
			t.Fatalf("the run is %+v", run)
		}
		r = tick(t, r, gone, c)
		if r.d.Holds("data-02") {
			t.Fatal("a member present for less than the stable span lost its accumulated ticks")
		}
	})

	t.Run("absent, then present for the stable span, is cleared", func(t *testing.T) {
		t.Parallel()
		r := first(t, live, c)
		r = ticks(t, r, gone, c, OutTicks-1)
		r = ticks(t, r, live, c, StableTicks)
		if _, open := r.s.Absence["data-02"]; open {
			t.Fatalf("the run survived %d present ticks: %+v", StableTicks, r.s.Absence)
		}
		r = ticks(t, r, gone, c, OutTicks-1)
		if !r.d.Holds("data-02") {
			t.Fatal("a cleared run was still counted")
		}
	})

	// A HOLD KEEPS AN ABSENT MEMBER PAST THE GRACE, still counting, and
	// its expiry lets the next tick remove it.
	t.Run("a hold keeps an absent member until it expires", func(t *testing.T) {
		t.Parallel()
		r := first(t, live, c)
		held, err := HoldFor(r.s, time.Hour, "ops", "rolling kernel upgrade", t0)
		if err != nil {
			t.Fatal(err)
		}
		r.s = held
		for range OutTicks + 5 {
			r = tickAt(t, r, gone, c, t0.Add(30*time.Minute))
		}
		if !r.d.Holds("data-02") || r.s.Absence["data-02"].Ticks != OutTicks+5 {
			t.Fatalf("under the hold: member held %v, run %+v", r.d.Holds("data-02"),
				r.s.Absence["data-02"])
		}
		r = tickAt(t, r, gone, c, t0.Add(time.Hour))
		if r.d.Holds("data-02") || r.s.Hold != nil {
			t.Fatalf("after the hold expired: member held %v, hold %+v",
				r.d.Holds("data-02"), r.s.Hold)
		}
	})

	// A MEMBER BACK WITH ITS RUN PAST THE GRACE — gone through a hold — is
	// not removed while it is present, and is removed the tick it leaves.
	t.Run("a present member is never removed", func(t *testing.T) {
		t.Parallel()
		r := first(t, live, c)
		held, err := HoldFor(r.s, time.Hour, "ops", "", t0)
		if err != nil {
			t.Fatal(err)
		}
		r.s = held
		r = ticks(t, r, gone, c, OutTicks+1)
		r.s = Release(r.s)
		r = tick(t, r, live, c)
		if !r.d.Holds("data-02") {
			t.Fatal("a member present on this tick was removed")
		}
		r = tick(t, r, gone, c)
		if r.d.Holds("data-02") {
			t.Fatal("a member past the grace that left again was kept")
		}
	})
}

// A REMOVED NODE IS NOT PLACED ON AGAIN UNTIL IT IS STABLE: placed on at its
// next sighting, a flapping node would take a share and be removed again every
// cycle. But it is a MEMBER again at that sighting, on probation — at the tail
// of every ranking, where a reader and a repair look for what only it holds —
// because keeping it out of the map while it proved itself kept everyone from
// reading what it held. Leaving while on probation removes it again at once.
// And one gone for a whole grace at a stretch — which no flapping node is — is
// forgotten, and joins as any node does.
func TestARemovedNodeIsPlacedOnOnlyOnceStable(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	gone := without(live, "data-02")
	removed := ticks(t, first(t, live, c), gone, c, OutTicks)
	if removed.d.Holds("data-02") {
		t.Fatal("setup: not removed")
	}

	t.Run("seen back, it is a member on probation at once", func(t *testing.T) {
		t.Parallel()
		back := tick(t, removed, live, c)
		m, ok := back.d.Member("data-02")
		if !ok || !m.Probation || m.Out || back.s.Removed["data-02"].Present != 1 {
			t.Fatalf("seen back once: member %+v (%v), removal %+v", m, ok, back.s.Removed)
		}
		if back.d.Size() != removed.d.Size() || len(back.d.Placeable()) != 2 {
			t.Fatalf("on probation it is placed on: size %d, placeable %v", back.d.Size(),
				nodes(placement.Draw{Members: back.d.Placeable()}))
		}
		// ITS SHARE IS THE FLEET'S RATE: nobody else's, and a balance's
		// starting point for the day it is trusted.
		if want := removed.d.ShareFor(1); m.Share != want {
			t.Fatalf("it came back at share %d, want the fleet's rate %d", m.Share, want)
		}
	})

	t.Run("flapping, it is never placed on", func(t *testing.T) {
		t.Parallel()
		r := removed
		for i := 1; i <= 10*StableTicks; i++ {
			fleet := gone
			if i%10 >= 8 {
				fleet = live
			}
			r = tick(t, r, fleet, c)
			if placeable(r, "data-02") {
				t.Fatalf("a flapping node was placed on again at tick %d", i)
			}
			if held := r.d.Holds("data-02"); held != (i%10 >= 8) {
				t.Fatalf("tick %d: a member %v while it is %s", i, held,
					map[bool]string{true: "back", false: "gone"}[i%10 >= 8])
			}
		}
	})

	t.Run("leaving on probation removes it again at once", func(t *testing.T) {
		t.Parallel()
		r := ticks(t, removed, live, c, StableTicks-1)
		r = tick(t, r, append(without(live, "data-02"), sick("data-02", "fsync: EIO")), c)
		if r.d.Holds("data-02") {
			t.Fatal("a member on probation that failed was kept")
		}
		if removal := r.s.Removed["data-02"]; removal.Present != 0 || removal.Gone != 1 ||
			removal.Reason != ReasonUnhealthy || removal.Detail != "fsync: EIO" {
			t.Fatalf("sent back to removed as %+v", removal)
		}
		// Its probation starts over: a whole span again.
		r = ticks(t, r, live, c, StableTicks-1)
		if placeable(r, "data-02") || r.s.Removed["data-02"].Present != StableTicks-1 {
			t.Fatalf("back again, trusted early: %+v", r.s.Removed)
		}
	})

	// A HOLD KEEPS NO MEMBER ON PROBATION: it has no share for the hold to
	// save re-placing, and leaving is what it is watched for.
	t.Run("a hold keeps no member on probation", func(t *testing.T) {
		t.Parallel()
		r := tick(t, removed, live, c)
		held, err := HoldFor(r.s, time.Hour, "ops", "maintenance", t0)
		if err != nil {
			t.Fatal(err)
		}
		r.s = held
		if r = tick(t, r, gone, c); r.d.Holds("data-02") {
			t.Fatal("a member on probation that left was kept by a hold")
		}
	})

	t.Run("stable, it is placed on", func(t *testing.T) {
		t.Parallel()
		r := ticks(t, removed, live, c, StableTicks-1)
		if placeable(r, "data-02") || r.s.Removed["data-02"].Present != StableTicks-1 {
			t.Fatalf("placed on early: %+v", r.s.Removed)
		}
		r = tick(t, r, live, c)
		if !placeable(r, "data-02") || r.s.Removed != nil {
			t.Fatalf("not placed on after %d stable ticks: %+v", StableTicks, r.s.Removed)
		}
	})

	t.Run("gone a whole grace, it is forgotten", func(t *testing.T) {
		t.Parallel()
		r := ticks(t, removed, gone, c, OutTicks-1)
		if _, remembered := r.s.Removed["data-02"]; !remembered {
			t.Fatal("forgotten early")
		}
		r = tick(t, r, gone, c)
		if r.s.Removed != nil {
			t.Fatalf("still remembered after %d ticks gone: %+v", OutTicks, r.s.Removed)
		}
		if r = tick(t, r, live, c); !placeable(r, "data-02") {
			t.Fatal("a forgotten node did not join at its next sighting")
		}
	})

	t.Run("an operator can vouch for it", func(t *testing.T) {
		t.Parallel()
		s, d, err := In(removed.s, removed.d, "data-02")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(d, removed.d) || s.Removed != nil {
			t.Fatalf("vouching for a node not yet back changed the members or kept its "+
				"removal: %+v, %+v", d, s.Removed)
		}
		if r := tick(t, record{s, d}, live, c); !placeable(r, "data-02") {
			t.Fatal("put back in, the node was not placed on at its next sighting")
		}
		// And on probation, it is placed on at once.
		probation := tick(t, removed, live, c)
		s, d, err = In(probation.s, probation.d, "data-02")
		if err != nil {
			t.Fatal(err)
		}
		if in := (record{s, d}); !placeable(in, "data-02") || in.s.Removed != nil {
			t.Fatalf("vouched for on probation: %+v, removed %+v", in.d.Members, in.s.Removed)
		}
		if err := s.Validate(d.Members); err != nil {
			t.Fatalf("the vouched-for state is one a writer would refuse: %v", err)
		}
	})
}

// THE LAST MEMBER THAT CAN BE PLACED ON IS NEVER REMOVED: there is nowhere to
// re-place its share, and a map left placing on nothing would refuse every
// write after the node returned, until it had proved itself stable. An out
// member places nothing, so it is removed beside it all the same.
func TestTheLastPlaceableMemberIsNeverRemoved(t *testing.T) {
	t.Parallel()
	c := company(1, 3, "")
	r := first(t, roster(1), c)
	r = ticks(t, r, nil, c, 2*OutTicks)
	if !r.d.Holds("data-00") {
		t.Fatal("the only member was removed")
	}
	if r = tick(t, r, roster(1), c); r.s.Absence["data-00"].Present != 1 {
		t.Fatalf("back: %+v", r.s.Absence)
	}

	two := first(t, roster(2), c)
	s, d, err := Out(two.s, two.d, "data-01", "ops", "decommission", t0)
	if err != nil {
		t.Fatal(err)
	}
	two = ticks(t, record{s, d}, nil, c, OutTicks)
	if !two.d.Holds("data-00") {
		t.Fatal("the last placeable member was removed beside an out one")
	}
	if two.d.Holds("data-01") || two.s.TakenOut != nil {
		t.Fatalf("the out member gone for the grace was kept: %v, %v", nodes(two.d), two.s.TakenOut)
	}
}

// A WHOLE TIER GOING DARK REMOVES NOBODY, however its leases lapse. Removing a
// member re-places its share, which is only worth doing onto a member present
// and healthy NOW: counting the members merely not due yet, the first to reach
// the grace was removed onto others as dead as it was, each in turn onto the
// next, until one was left — and when power came back the map placed on that
// one alone while the rest proved themselves, every write keeping one copy
// and most groups reading as missing from four healthy nodes that held them.
func TestAWholeTierLapsingOverSeveralTicksRemovesNobody(t *testing.T) {
	t.Parallel()
	c := company(1, 3, "")
	live := roster(5)
	r := first(t, live, c)
	// data-00 and data-01 lapse first, data-02 and data-03 a tick later,
	// data-04 a tick after that — and the fleet stays dark past every grace.
	r = tick(t, r, without(live, "data-00", "data-01"), c)
	r = tick(t, r, without(live, "data-00", "data-01", "data-02", "data-03"), c)
	r = ticks(t, r, nil, c, 2*OutTicks)
	if len(r.d.Members) != 5 || r.s.Removed != nil {
		t.Fatalf("a dark tier lost members: %v, removed %v", nodes(r.d), r.s.Removed)
	}
	if run := r.s.Absence["data-00"]; run.Ticks != 2*OutTicks+2 {
		t.Fatalf("the absence stopped being counted: %+v", run)
	}
	back := tick(t, r, live, c)
	if got := len(back.d.Placeable()); got != 5 || back.d.Size() != 3 {
		t.Fatalf("the first tick back places on %d members at %d copies, want 5 at 3",
			got, back.d.Size())
	}

	// A MEMBER ON PROBATION IS NO PLACE FOR A SHARE: it takes no copies,
	// so the two placeable members going dark beside it remove nobody.
	three := roster(3)
	removedOne := ticks(t, first(t, three, c), without(three, "data-02"), c, OutTicks)
	// Everything dark for a few ticks, then data-02 back — on probation —
	// while the other two stay dark past their grace.
	dark := ticks(t, ticks(t, removedOne, nil, c, 5), three[2:], c, OutTicks-3)
	if m, _ := dark.d.Member("data-02"); !m.Probation {
		t.Fatalf("setup: data-02 is %+v, want on probation", m)
	}
	if run := dark.s.Absence["data-00"]; run.Ticks <= OutTicks {
		t.Fatalf("setup: data-00's run is %+v, want past the grace", run)
	}
	if !dark.d.Holds("data-00") || !dark.d.Holds("data-01") {
		t.Fatalf("the placeable members were removed onto one on probation: %v", nodes(dark.d))
	}

	// AND ONE PRESENT MEMBER IS ENOUGH: the others are removed onto it.
	lit := ticks(t, first(t, live, c), live[4:], c, OutTicks)
	if got := nodes(lit.d); !slices.Equal(got, []string{"data-04"}) {
		t.Fatalf("with one member present the map holds %v, want only it", got)
	}
}

// OUT IS THE OPERATOR'S GESTURE AND NOTHING ELSE: every tick derives the flag
// the map places by from the recorded gesture, and forgets a gesture about a
// node the map no longer holds.
func TestAMemberIsOutExactlyWhileTakenOut(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	r := first(t, live, c)
	r.s.TakenOut = map[string]Gesture{"data-01": {By: "ops"}, "data-09": {By: "ops"}}
	r.d.Members[2].Out = true // a flag no gesture stands behind
	next := tick(t, r, live, c)
	for _, m := range next.d.Members {
		if m.Out != (m.Node == "data-01") {
			t.Errorf("%s out = %v", m.Node, m.Out)
		}
	}
	if _, kept := next.s.TakenOut["data-09"]; kept {
		t.Error("a gesture about a node the map does not hold was kept")
	}
}

// A TICK NEVER WRITES INTO WHAT IT WAS GIVEN: the caller may be holding it —
// its node places by it — and a tick that lost its race must leave nothing of
// itself behind.
func TestATickLeavesItsInputAlone(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	r := ticks(t, first(t, roster(3), c), roster(2), c, 3)
	r.s.TakenOut = map[string]Gesture{"data-00": {By: "ops"}}
	held, err := HoldFor(r.s, time.Hour, "ops", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	r.s = held
	state, members := r.s.Clone(), slices.Clone(r.d.Members)
	for range 5 {
		Tick(r.s, r.d, roster(1), company(2, 1, ""), t0.Add(2*time.Hour))
	}
	if !reflect.DeepEqual(r.s, state) || !slices.Equal(r.d.Members, members) {
		t.Fatalf("Tick changed its input:\n%+v %+v\n%+v %+v", state, members, r.s, r.d.Members)
	}
}

// THE TICK ARITHMETIC IS THE GRACE: forty ticks at the reconcile interval are
// ten minutes, and the span that clears a run is the span that removes one.
func TestTheTickArithmetic(t *testing.T) {
	t.Parallel()
	if got := time.Duration(OutTicks) * TickInterval; got != OutGrace || OutTicks != 40 {
		t.Fatalf("%d ticks of %s = %s, want %s in 40", OutTicks, TickInterval, got, OutGrace)
	}
	if StableTicks != OutTicks {
		t.Fatalf("stable after %d ticks, removed after %d", StableTicks, OutTicks)
	}
}

// A PRESENCE COUNTS ONCE, and only when it offers a share: a node listed twice
// is taken at its first, and one naming no node or offering nothing is none.
func TestAPresenceCountsOnce(t *testing.T) {
	t.Parallel()
	type rich struct {
		p    Presence
		note string
	}
	got := ByNode([]rich{
		{up("data-a", 1), "first"}, {up("data-a", 3), "second"},
		{up("", 1), "no node"}, {up("agent-1", 0), "no share"}, {sick("data-b", "io"), "sick"},
	}, func(r rich) Presence { return r.p })
	if len(got) != 2 || got["data-a"].note != "first" || got["data-b"].note != "sick" {
		t.Fatalf("indexed %+v", got)
	}
}
