package upkeep

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/objstore"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
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
func company(epoch uint64, replicas int, domain string) membership.Company {
	return membership.Company{Epoch: epoch, Replicas: replicas, FailureDomain: domain}
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

// repaired is live with every node reporting a completed repair at epoch that
// left pending chunks unheld.
func repaired(live []Presence, epoch uint64, pending int) []Presence {
	out := slices.Clone(live)
	for i := range out {
		out[i].Repair = RepairReport{Epoch: epoch, Pending: pending}
	}
	return out
}

// first is the first map a maintainer writes for this fleet and company.
func first(t *testing.T, live []Presence, c membership.Company) objstore.MapState {
	t.Helper()
	state, changed := Next(objstore.MapState{}, live, c, t0)
	if !changed {
		t.Fatal("no first map was made")
	}
	if err := state.Map.Validate(); err != nil {
		t.Fatalf("the first map does not validate: %v", err)
	}
	return state
}

// ticks runs n ticks against the same fleet.
func ticks(t *testing.T, state objstore.MapState, live []Presence, c membership.Company, n int) objstore.MapState {
	t.Helper()
	for range n {
		state = tick(t, state, live, c)
	}
	return state
}

// tick runs one tick, checking what every tick must hold: a record that can
// be stored.
func tick(t *testing.T, state objstore.MapState, live []Presence, c membership.Company) objstore.MapState {
	t.Helper()
	next, _ := Next(state, live, c, t0)
	if _, err := next.Encode(); err != nil {
		t.Fatalf("a tick made a map that cannot be stored: %v", err)
	}
	return next
}

// nodes is the map's members' names.
func nodes(m objplacement.Map) []string {
	var out []string
	for _, member := range m.Members {
		out = append(out, member.Node)
	}
	return out
}

// A FIRST MAP COMES FROM THE COMPANY AND A LIVE DATA NODE, and from nothing
// less: a map written without a company would have invented the copies every
// chunk keeps, and an empty one says what none says, at a write per tick. It
// starts at the group count its fleet needs, since it places no data yet.
func TestAFirstMapComesFromTheCompany(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		live    []Presence
		company membership.Company
	}{
		"no company":              {roster(2), membership.Company{}},
		"a company that is wrong": {roster(2), company(7, 0, "")},
		"no data node":            {nil, company(7, 3, "")},
		"a node offering nothing": {[]Presence{up("agent-1", 0)}, company(7, 3, "")},
		"only a failed store":     {[]Presence{sick("data-a", "disk gone")}, company(7, 3, "")},
	} {
		if next, changed := Next(objstore.MapState{}, c.live, c.company, t0); changed ||
			len(next.Map.Members) > 0 {
			t.Errorf("%s: a first map was made: %+v", name, next)
		}
	}

	state := first(t, []Presence{up("data-a", 1), up("data-b", 2)}, company(7, 3, ""))
	m := state.Map
	switch {
	case m.Epoch != 1 || state.Config.Epoch != 7:
		t.Errorf("epoch %d from activation %d, want 1 from 7", m.Epoch, state.Config.Epoch)
	case m.Replicas != 3 || m.Size() != 2:
		t.Errorf("replicas %d placing %d, want 3 placing 2", m.Replicas, m.Size())
	case !slices.Equal(nodes(m), []string{"data-a", "data-b"}) || m.Members[1].Weight != 2:
		t.Errorf("members %+v", m.Members)
	case state.Balance.Epoch != m.Epoch:
		t.Errorf("the first map was not balanced: %+v", state.Balance)
	}
	if big := first(t, roster(8), company(1, 3, "")); big.Map.PGBits != objplacement.MinPGBits+1 ||
		objplacement.TargetPGBits(8, 3) != objplacement.MinPGBits+1 {
		t.Errorf("eight members at three copies start at %d group bits, want the target %d",
			big.Map.PGBits, objplacement.TargetPGBits(8, 3))
	}
	if again := first(t, roster(1), company(1, 1, "")); again.Map.Generation == m.Generation {
		t.Error("two first maps share a generation — a recreated key would read as the old one")
	}
}

// AN OLDER ACTIVATION NEVER OVERWRITES A NEWER ONE, and a company that cannot
// be applied is not: the duty moves between nodes, and one a revision behind
// must not set the replica count back until the duty moves on.
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
		company      membership.Company
		wantReplicas int
		wantDomain   string
		wantConfig   uint64
		wantEpoch    uint64
		wantChanged  bool
	}{
		{"an older activation is ignored", company(9, 1, "zone"), 3, "", 10, 1, false},
		{"no company is ignored", membership.Company{}, 3, "", 10, 1, false},
		{"the same activation changes nothing", company(10, 3, ""), 3, "", 10, 1, false},
		{"a newer activation sets the copies", company(11, 2, ""), 2, "", 11, 2, true},
		{"a newer activation sets the domain", company(11, 3, "zone"), 3, "zone", 11, 2, true},
		{"a newer activation saying the same moves no epoch", company(11, 3, ""), 3, "", 11, 1, true},
		{"too many copies is refused", company(12, placement.MaxReplicas+1, ""), 3, "", 10, 1, false},
		{"a label that is no label is refused", company(12, 3, "my zone"), 3, "", 10, 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			next, changed := Next(base, live, c.company, t0)
			m := next.Map
			if m.Replicas != c.wantReplicas || m.FailureDomain != c.wantDomain ||
				next.Config.Epoch != c.wantConfig || m.Epoch != c.wantEpoch || changed != c.wantChanged {
				t.Fatalf("replicas %d, domain %q, from activation %d, epoch %d, changed %v; "+
					"want %d, %q, %d, %d, %v", m.Replicas, m.FailureDomain, next.Config.Epoch,
					m.Epoch, changed, c.wantReplicas, c.wantDomain, c.wantConfig, c.wantEpoch,
					c.wantChanged)
			}
			if c.wantDomain == "zone" {
				want := []string{"eu-1", "eu-2", ""}
				for i, member := range m.Members {
					if member.Domain != want[i] {
						t.Errorf("%s is in domain %q, want %q", member.Node, member.Domain, want[i])
					}
				}
			}
		})
	}
}

// THE FLEET KEEPS ITS MEMBERSHIP WITHOUT A COMPANY: the company says how many
// copies, never who holds them, so a node whose company has not arrived still
// adds a data node that joins — at the copies the map already has.
func TestAJoinerIsAddedWithoutACompany(t *testing.T) {
	t.Parallel()
	base := first(t, roster(2), company(10, 3, ""))
	next, changed := Next(base, roster(3), membership.Company{}, t0)
	if !changed || len(next.Map.Members) != 3 || next.Map.Replicas != 3 || next.Map.Epoch != 2 {
		t.Fatalf("a joiner with no company gave %+v (changed %v)", next.Map, changed)
	}
}

// A STEADY FLEET WRITES NOTHING: every tick of a map with nothing to count, to
// measure or to change is a write saved.
func TestASteadyFleetWritesNothing(t *testing.T) {
	t.Parallel()
	c := company(1, 3, "")
	state := first(t, roster(4), c)
	if next, changed := Next(state, roster(4), c, t0.Add(time.Hour)); changed {
		t.Fatalf("a steady fleet wrote %+v", next)
	}
}

// AN ABSENCE MOVES NO EPOCH AND A REMOVAL MOVES IT BY ONE. Who is absent and
// for how long is the membership's to count (internal/membership holds every
// rule of it, in ticks); what the object map adds is its epoch, which counts
// PLACEMENT changes — so every tick that counts the absence writes the record
// at the same epoch, the member still placed on, and the tick that removes it
// is a placement change, balanced in an epoch of its own.
func TestAnAbsenceMovesNoEpochAndARemovalMovesOne(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	gone := without(live, "data-02")
	start := first(t, live, c)
	state := start
	for i := 1; i <= membership.OutTicks; i++ {
		next, changed := Next(state, gone, c, t0)
		if !changed {
			t.Fatalf("tick %d wrote nothing", i)
		}
		if i < membership.OutTicks && (next.Map.Epoch != start.Map.Epoch || !next.Map.Holds("data-02")) {
			t.Fatalf("tick %d of the absence: epoch %d → %d, the member held %v", i,
				start.Map.Epoch, next.Map.Epoch, next.Map.Holds("data-02"))
		}
		state = next
	}
	if state.Map.Holds("data-02") || state.Map.Epoch != start.Map.Epoch+1 || state.Absence != nil ||
		state.Balance.Epoch != state.Map.Epoch {
		t.Fatalf("the removal: held %v, epoch %d → %d, absences %v, balance %+v",
			state.Map.Holds("data-02"), start.Map.Epoch, state.Map.Epoch, state.Absence, state.Balance)
	}
}

// A REMOVED NODE IS NOT PLACED ON AGAIN UNTIL IT IS STABLE: placed on at its
// next sighting, a flapping node would take a share and be removed again every
// cycle. But it is a MEMBER again at that sighting, on probation — at the tail
// of every ranking, where a reader and a repair look for the copies only it
// holds — because keeping it out of the map while it proved itself kept
// everyone from reading what it held. Leaving while on probation removes it
// again at once. And one gone for a whole grace at a stretch — which no
// flapping node is — is forgotten, and joins as any node does.
func TestARemovedNodeIsPlacedOnOnlyOnceStable(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	gone := without(live, "data-02")
	removed := ticks(t, first(t, live, c), gone, c, membership.OutTicks)
	if removed.Map.Holds("data-02") {
		t.Fatal("setup: not removed")
	}
	placeable := func(s objstore.MapState) bool {
		m, ok := s.Map.Member("data-02")
		return ok && m.Placeable()
	}

	t.Run("seen back, it is a member on probation at once", func(t *testing.T) {
		t.Parallel()
		back := tick(t, removed, live, c)
		m, ok := back.Map.Member("data-02")
		if !ok || !m.Probation || m.Out || back.Removed["data-02"].Present != 1 {
			t.Fatalf("seen back once: member %+v (%v), removal %+v", m, ok, back.Removed)
		}
		if back.Map.Epoch != removed.Map.Epoch+1 {
			t.Fatalf("a member joining on probation moved epoch %d → %d; a reader needs the "+
				"map that ranks it", removed.Map.Epoch, back.Map.Epoch)
		}
		if back.Map.Size() != removed.Map.Size() || len(back.Map.Placeable()) != 2 {
			t.Fatalf("on probation it is placed on: size %d, placeable %v", back.Map.Size(),
				nodes(objplacement.Map{Members: back.Map.Placeable()}))
		}
		l := back.Map.Layout()
		for pg := range back.Map.Groups() {
			if slices.Contains(l.Up(pg), "data-02") {
				t.Fatalf("group %d is placed on the member on probation: %v", pg, l.Up(pg))
			}
			if ranked := back.Map.Ranked(pg); ranked[len(ranked)-1] != "data-02" {
				t.Fatalf("group %d: the member on probation is not where a reader looks: %v",
					pg, ranked)
			}
		}
	})

	t.Run("flapping, it is never placed on", func(t *testing.T) {
		t.Parallel()
		state := removed
		for i := 1; i <= 10*membership.StableTicks; i++ {
			fleet := gone
			if i%10 >= 8 {
				fleet = live
			}
			state = tick(t, state, fleet, c)
			if placeable(state) {
				t.Fatalf("a flapping node was placed on again at tick %d", i)
			}
			if held := state.Map.Holds("data-02"); held != (i%10 >= 8) {
				t.Fatalf("tick %d: a member %v while it is %s", i, held,
					map[bool]string{true: "back", false: "gone"}[i%10 >= 8])
			}
		}
	})

	t.Run("leaving on probation removes it again at once", func(t *testing.T) {
		t.Parallel()
		state := ticks(t, removed, live, c, membership.StableTicks-1)
		state = tick(t, state, append(without(live, "data-02"), sick("data-02", "fsync: EIO")), c)
		if state.Map.Holds("data-02") {
			t.Fatal("a member on probation that failed was kept")
		}
		if r := state.Removed["data-02"]; r.Present != 0 || r.Gone != 1 ||
			r.Reason != membership.ReasonUnhealthy || r.Detail != "fsync: EIO" {
			t.Fatalf("sent back to removed as %+v", r)
		}
		// Its probation starts over: a whole span again.
		state = ticks(t, state, live, c, membership.StableTicks-1)
		if placeable(state) || state.Removed["data-02"].Present != membership.StableTicks-1 {
			t.Fatalf("back again, trusted early: %+v", state.Removed)
		}
	})

	// A HOLD KEEPS NO MEMBER ON PROBATION: it has no share for the hold to
	// save re-placing, and leaving is what it is watched for.
	t.Run("a hold keeps no member on probation", func(t *testing.T) {
		t.Parallel()
		held, err := HoldFor(tick(t, removed, live, c), time.Hour, "ops", "maintenance", t0)
		if err != nil {
			t.Fatal(err)
		}
		if state := tick(t, held, gone, c); state.Map.Holds("data-02") {
			t.Fatal("a member on probation that left was kept by a hold")
		}
	})

	t.Run("stable, it is placed on", func(t *testing.T) {
		t.Parallel()
		state := ticks(t, removed, live, c, membership.StableTicks-1)
		if placeable(state) || state.Removed["data-02"].Present != membership.StableTicks-1 {
			t.Fatalf("placed on early: %+v", state.Removed)
		}
		before := state.Map.Epoch
		state = tick(t, state, live, c)
		if !placeable(state) || state.Removed != nil || state.Map.Epoch != before+1 {
			t.Fatalf("not placed on after %d stable ticks: %+v at epoch %d", membership.StableTicks,
				state.Removed, state.Map.Epoch)
		}
	})

	t.Run("gone a whole grace, it is forgotten", func(t *testing.T) {
		t.Parallel()
		state := ticks(t, removed, gone, c, membership.OutTicks-1)
		if _, remembered := state.Removed["data-02"]; !remembered {
			t.Fatal("forgotten early")
		}
		state = tick(t, state, gone, c)
		if state.Removed != nil {
			t.Fatalf("still remembered after %d ticks gone: %+v", membership.OutTicks, state.Removed)
		}
		if state = tick(t, state, live, c); !placeable(state) {
			t.Fatal("a forgotten node did not join at its next sighting")
		}
	})

	t.Run("an operator can vouch for it", func(t *testing.T) {
		t.Parallel()
		state, err := In(removed, "data-02")
		if err != nil {
			t.Fatal(err)
		}
		if state = tick(t, state, live, c); !placeable(state) {
			t.Fatal("put back in, the node was not placed on at its next sighting")
		}
		// And on probation, it is placed on at once.
		probation := tick(t, removed, live, c)
		in, err := In(probation, "data-02")
		if err != nil {
			t.Fatal(err)
		}
		if !placeable(in) || in.Removed != nil || in.Map.Epoch != probation.Map.Epoch+1 ||
			in.Balance.Epoch != in.Map.Epoch {
			t.Fatalf("vouched for on probation: %+v, removed %+v, epoch %d", in.Map.Members,
				in.Removed, in.Map.Epoch)
		}
		if _, err := in.Encode(); err != nil {
			t.Fatalf("the vouched-for record cannot be stored: %v", err)
		}
	})
}

// OUT IS THE OPERATOR'S GESTURE AND NOTHING ELSE: every tick derives the flag
// the map places by from the recorded gesture, and forgets a gesture about a
// node the map no longer holds.
func TestAMemberIsOutExactlyWhileTakenOut(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	state := first(t, live, c)
	state.TakenOut = map[string]membership.Gesture{"data-01": {By: "ops"}, "data-09": {By: "ops"}}
	state.Map.Members[2].Out = true // a flag no gesture stands behind
	next, _ := Next(state, live, c, t0)
	for _, m := range next.Map.Members {
		if m.Out != (m.Node == "data-01") {
			t.Errorf("%s out = %v", m.Node, m.Out)
		}
	}
	if _, kept := next.TakenOut["data-09"]; kept {
		t.Error("a gesture about a node the map does not hold was kept")
	}
	if next.Map.Epoch != state.Map.Epoch+1 {
		t.Errorf("a change of who is out moved epoch %d → %d", state.Map.Epoch, next.Map.Epoch)
	}
}

// NEXT NEVER WRITES INTO THE RECORD IT WAS GIVEN: the caller may be holding it
// — its node places by it — and a tick that lost its race must leave nothing
// of itself behind.
func TestNextLeavesItsInputAlone(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	state := ticks(t, first(t, roster(3), c), roster(2), c, 3)
	state.TakenOut = map[string]membership.Gesture{"data-00": {By: "ops"}}
	before, err := state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		Next(state, roster(1), c, t0)
	}
	if after, _ := state.Encode(); string(before) != string(after) {
		t.Fatalf("Next changed its input:\n%s\n%s", before, after)
	}
}

// splittable is a fleet whose group count is short of its target: seven
// members at three copies want 256 groups and the map is written at that,
// then an eighth joins — which wants 512.
func splittable(t *testing.T) (objstore.MapState, []Presence, membership.Company) {
	t.Helper()
	c := company(1, 3, "")
	state := first(t, roster(7), c)
	live := roster(8)
	state = tick(t, state, live, c)
	if state.Map.PGBits != objplacement.MinPGBits ||
		objplacement.TargetPGBits(8, 3) != objplacement.MinPGBits+1 {
		t.Fatalf("setup: pg_bits %d, target %d", state.Map.PGBits, objplacement.TargetPGBits(8, 3))
	}
	return state, live, c
}

// A FLEET SPLITS ITS GROUPS ONLY WHEN IT IS CLEAN — every placeable member
// present, healthy and finished repairing at this epoch with nothing pending,
// and no absence open — because a split re-places half the data, and doing it
// while the last change is still moving would stack the two.
func TestASplitWaitsForACleanFleet(t *testing.T) {
	t.Parallel()
	state, live, c := splittable(t)
	epoch := state.Map.Epoch
	for _, tc := range []struct {
		name string
		live []Presence
	}{
		{"a member has not finished repairing at this epoch", func() []Presence {
			l := repaired(live, epoch, 0)
			l[3].Repair.Epoch = epoch - 1
			return l
		}()},
		{"a member never finished a repair", func() []Presence {
			l := repaired(live, epoch, 0)
			l[3].Repair = RepairReport{}
			return l
		}()},
		{"a member has chunks pending", repaired(live, epoch, 1)},
		{"a member is absent", repaired(without(live, "data-05"), epoch, 0)},
		{"a member's store failed", func() []Presence {
			l := repaired(live, epoch, 0)
			l[5].Unhealthy = true
			return l
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if next, _ := Next(state, tc.live, c, t0); next.Map.PGBits != state.Map.PGBits {
				t.Fatalf("split to %d bits on a fleet that is not clean", next.Map.PGBits)
			}
		})
	}

	t.Run("a member with an absence run still open", func(t *testing.T) {
		back := tick(t, state, without(live, "data-05"), c)
		if next, _ := Next(back, repaired(live, epoch, 0), c, t0); next.Map.PGBits != state.Map.PGBits {
			t.Fatal("split while a member's absence run was still open")
		}
	})

	// A MEMBER ON PROBATION is a placement change coming within a grace:
	// the tick that trusts it moves a share, so a split now would stack
	// two moves.
	t.Run("a member on probation", func(t *testing.T) {
		probation := state.Clone()
		probation.Map.Members = append(probation.Map.Members, placement.Member{Node: "data-99",
			Weight: 1, Share: placement.DefaultShare(1), Probation: true})
		probation.Removed = map[string]membership.Removal{"data-99": {Present: 1, At: t0,
			Reason: membership.ReasonAbsent}}
		next, _ := Next(probation, repaired(append(slices.Clone(live), up("data-99", 1)), epoch, 0),
			c, t0)
		if next.Map.PGBits != state.Map.PGBits || next.Removed["data-99"].Present != 2 {
			t.Fatalf("split to %d bits beside a member on probation (%+v)", next.Map.PGBits,
				next.Removed)
		}
	})

	t.Run("a change to placement on the same tick", func(t *testing.T) {
		next, _ := Next(state, repaired(append(slices.Clone(live), up("data-99", 1)), epoch, 0), c, t0)
		if next.Map.PGBits != state.Map.PGBits || !next.Map.Holds("data-99") {
			t.Fatal("a split shared its epoch with a join")
		}
	})

	t.Run("the control: a clean fleet splits", func(t *testing.T) {
		if next, _ := Next(state, repaired(live, epoch, 0), c, t0); next.Map.PGBits != state.Map.PGBits+1 {
			t.Fatal("a clean fleet short of its group count did not split")
		}
	})
}

// A SPLIT EPOCH CHANGES THE GROUP BITS AND NOTHING ELSE — one bit, the shares
// untouched, so every lower child keeps its parent's holders exactly and only
// the upper children move — and the balance it calls for comes on the next
// tick, as a step of its own.
func TestASplitIsAnEpochOfItsOwn(t *testing.T) {
	t.Parallel()
	state, live, c := splittable(t)
	split, changed := Next(state, repaired(live, state.Map.Epoch, 0), c, t0)
	if !changed || split.Map.PGBits != state.Map.PGBits+1 || split.Map.Epoch != state.Map.Epoch+1 {
		t.Fatalf("pg_bits %d → %d, epoch %d → %d", state.Map.PGBits, split.Map.PGBits,
			state.Map.Epoch, split.Map.Epoch)
	}
	if !slices.Equal(split.Map.Members, state.Map.Members) || split.Balance != state.Balance {
		t.Fatal("the split epoch changed the shares, or claims a balance of its own")
	}
	before, after := state.Map.Layout(), split.Map.Layout()
	for pg := range state.Map.Groups() {
		if !slices.Equal(after.Up(2*pg), before.Up(pg)) {
			t.Fatalf("group %d's lower child moved: %v → %v", pg, before.Up(pg), after.Up(2*pg))
		}
	}

	measured, changed := Next(split, repaired(live, split.Map.Epoch, 0), c, t0)
	if !changed || measured.Balance.Epoch != measured.Map.Epoch ||
		measured.Map.PGBits != split.Map.PGBits {
		t.Fatalf("after the split: pg_bits %d, balance %+v at epoch %d",
			measured.Map.PGBits, measured.Balance, measured.Map.Epoch)
	}
}

// ONE BIT PER EPOCH, even when the target is two away: the second waits for
// the first to be measured and for the fleet to be clean at its epoch.
func TestASplitIsOneBitAtATime(t *testing.T) {
	t.Parallel()
	c := company(1, 3, "")
	state := first(t, roster(7), c)
	live := roster(16)
	state = tick(t, state, live, c)
	if target := objplacement.TargetPGBits(16, 3); target != state.Map.PGBits+2 {
		t.Fatalf("setup: target %d from %d", target, state.Map.PGBits)
	}
	bits := []int{state.Map.PGBits}
	for range 6 {
		next := tick(t, state, repaired(live, state.Map.Epoch, 0), c)
		if next.Map.PGBits != state.Map.PGBits {
			if next.Map.PGBits != state.Map.PGBits+1 {
				t.Fatalf("split %d bits in one tick", next.Map.PGBits-state.Map.PGBits)
			}
			if state.Balance.Epoch != state.Map.Epoch {
				t.Fatal("split a map whose last split was never measured")
			}
			bits = append(bits, next.Map.PGBits)
		}
		state = next
	}
	if want := []int{8, 9, 10}; !slices.Equal(bits, want) {
		t.Fatalf("the group bits went %v, want %v", bits, want)
	}
}

// A PLACEMENT NOBODY MEASURED IS MEASURED ONCE: balanced when it has drifted
// past twice the tolerance, and otherwise recorded as it is — never measured
// again every tick, and never re-balanced for the noise a balance leaves.
func TestAnUnmeasuredPlacementIsMeasuredOnce(t *testing.T) {
	t.Parallel()
	c := company(1, 3, "")
	live := roster(10)
	state := first(t, live, c)
	if state.Balance.Deviation > rebalanceAbove {
		t.Fatalf("setup: the first map is off by %.3f", state.Balance.Deviation)
	}

	even := state.Clone()
	even.Balance = objstore.Balance{}
	next, changed := Next(even, live, c, t0)
	if !changed || next.Map.Epoch != state.Map.Epoch || next.Balance.Epoch != state.Map.Epoch ||
		next.Balance.Rounds != 0 || !slices.Equal(next.Map.Members, state.Map.Members) {
		t.Fatalf("a balanced map, measured: epoch %d, %+v", next.Map.Epoch, next.Balance)
	}
	if _, again := Next(next, live, c, t0); again {
		t.Fatal("a measured map was measured again")
	}

	skewed := state.Clone()
	skewed.Balance = objstore.Balance{}
	skewed.Map.Members[0].Share *= 4
	next, _ = Next(skewed, live, c, t0)
	if next.Map.Epoch != state.Map.Epoch+1 || next.Balance.Epoch != next.Map.Epoch ||
		next.Balance.Rounds == 0 || next.Balance.Deviation > rebalanceAbove {
		t.Fatalf("a skewed map, measured: epoch %d, %+v", next.Map.Epoch, next.Balance)
	}
}

// A CHANGE TO PLACEMENT IS BALANCED IN ITS OWN EPOCH: a joiner's copies come
// out near its weight's fraction, rather than at whatever straw2 gave the
// share it joined at — and the record says how near, measuring the map it
// stored.
func TestAChangeIsBalancedInItsOwnEpoch(t *testing.T) {
	t.Parallel()
	c := company(1, 3, "")
	state := first(t, roster(10), c)
	live := append(roster(10), up("data-heavy", 4))

	joined := state.Map
	_, drawn := membership.Tick(state.State, state.Map.Draw(), memberships(live), membership.Company{}, t0)
	joined.Members = drawn.Members
	unbalanced := joined.Layout().Deviation()

	next, _ := Next(state, live, c, t0)
	if next.Map.Epoch != state.Map.Epoch+1 || next.Balance.Epoch != next.Map.Epoch ||
		next.Balance.Rounds == 0 {
		t.Fatalf("the join was not balanced in its epoch: epoch %d, %+v", next.Map.Epoch, next.Balance)
	}
	if got := next.Map.Layout().Deviation(); got != next.Balance.Deviation ||
		got >= unbalanced || got > rebalanceAbove {
		t.Fatalf("joined at %.4f off, the record says %.4f and the map measures %.4f",
			unbalanced, next.Balance.Deviation, got)
	}
}

// THE GESTURES: out and in move the epoch and balance in it; a hold and its
// release move nothing anybody places by. Each refuses what cannot be done, by
// a sentinel the caller can name to an operator.
func TestTheGestures(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	state := first(t, roster(3), c)

	t.Run("out and in", func(t *testing.T) {
		t.Parallel()
		out, err := Out(state, "data-01", "ops@example.com", "replacing the disk", t0)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := out.Map.Member("data-01")
		if !m.Out || out.Map.Epoch != state.Map.Epoch+1 || out.Balance.Epoch != out.Map.Epoch ||
			out.TakenOut["data-01"] != (membership.Gesture{By: "ops@example.com",
				Reason: "replacing the disk", At: t0}) {
			t.Fatalf("taken out: %+v, epoch %d, %+v", m, out.Map.Epoch, out.TakenOut)
		}
		if orig, _ := state.Map.Member("data-01"); orig.Out || state.TakenOut != nil {
			t.Fatal("Out wrote into the record it was given")
		}
		// A tick keeps it out and changes nothing.
		if next, changed := Next(out, roster(3), c, t0); changed {
			t.Fatalf("a tick after taking a member out changed %+v", next)
		}
		in, err := In(out, "data-01")
		if err != nil {
			t.Fatal(err)
		}
		if m, _ := in.Map.Member("data-01"); m.Out || in.TakenOut != nil ||
			in.Map.Epoch != out.Map.Epoch+1 || in.Balance.Epoch != in.Map.Epoch {
			t.Fatalf("put back: %+v, %+v, epoch %d", m, in.TakenOut, in.Map.Epoch)
		}
	})

	// A GESTURE THE MAP ALREADY SAYS WRITES NOTHING: out of a member already
	// out keeps the first gesture's record — a retry of a gesture whose answer
	// was lost neither overwrites who took it out and why nor moves the
	// record's version under a maintainer's tick — and in of a member already
	// placed on, or a release with no hold, is the record it was given.
	t.Run("a repeat writes nothing", func(t *testing.T) {
		t.Parallel()
		out, err := Out(state, "data-01", "ops@example.com", "replacing the disk", t0)
		if err != nil {
			t.Fatal(err)
		}
		for name, repeat := range map[string]func() (objstore.MapState, objstore.MapState, error){
			"out of a member already out": func() (objstore.MapState, objstore.MapState, error) {
				again, err := Out(out, "data-01", "someone-else", "a retry", t0.Add(time.Hour))
				return out, again, err
			},
			"in of a member not out": func() (objstore.MapState, objstore.MapState, error) {
				again, err := In(state, "data-00")
				return state, again, err
			},
			"a release with no hold": func() (objstore.MapState, objstore.MapState, error) {
				return state, Release(state), nil
			},
		} {
			given, answered, err := repeat()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			want, _ := given.Encode()
			if got, _ := answered.Encode(); string(got) != string(want) {
				t.Errorf("%s wrote:\n%s\nwas\n%s", name, got, want)
			}
		}
	})

	t.Run("refusals", func(t *testing.T) {
		t.Parallel()
		if _, err := Out(state, "data-09", "ops", "", t0); !errors.Is(err, membership.ErrUnknownMember) ||
			errors.Is(err, membership.ErrRemovedMember) {
			t.Errorf("out of an unknown node = %v", err)
		}
		// A node removed and not seen back places nothing already: out is
		// refused by a sentinel of its own, still an unknown member.
		removed := ticks(t, state, roster(2), c, membership.OutTicks)
		if _, err := Out(removed, "data-02", "ops", "", t0); !errors.Is(err, membership.ErrRemovedMember) ||
			!errors.Is(err, membership.ErrUnknownMember) {
			t.Errorf("out of a removed node = %v, want membership.ErrRemovedMember", err)
		}
		if _, err := In(state, "data-09"); !errors.Is(err, membership.ErrUnknownMember) {
			t.Errorf("in of an unknown node = %v", err)
		}
		_, outErr := Out(objstore.MapState{}, "data-00", "ops", "", t0)
		_, inErr := In(objstore.MapState{}, "data-00")
		_, holdErr := HoldFor(objstore.MapState{}, time.Hour, "ops", "", t0)
		for name, err := range map[string]error{"out": outErr, "in": inErr, "hold for": holdErr} {
			if !errors.Is(err, ErrNoMap) {
				t.Errorf("%s with no map = %v, want ErrNoMap", name, err)
			}
		}
		two, err := Out(state, "data-00", "ops", "", t0)
		if err != nil {
			t.Fatal(err)
		}
		// data-01 goes quiet: data-02 is the last member present to
		// place on.
		quiet := tick(t, two, without(roster(3), "data-01"), c)
		if _, err := Out(quiet, "data-02", "ops", "", t0); !errors.Is(err, membership.ErrNothingPlaceable) {
			t.Errorf("taking out the last present placeable member = %v", err)
		}
		if _, err := Out(quiet, "data-01", "ops", "", t0); err != nil {
			t.Errorf("taking out a quiet member beside a present one = %v", err)
		}
		one := first(t, roster(1), c)
		if _, err := Out(one, "data-00", "ops", "", t0); !errors.Is(err, membership.ErrNothingPlaceable) {
			t.Errorf("taking out the only member = %v", err)
		}
	})

	t.Run("hold and release", func(t *testing.T) {
		t.Parallel()
		for _, d := range []time.Duration{0, -time.Minute, membership.MaxHold + time.Second} {
			if _, err := HoldFor(state, d, "ops", "", t0); !errors.Is(err, membership.ErrHoldRange) {
				t.Errorf("a hold of %s = %v, want membership.ErrHoldRange", d, err)
			}
		}
		held, err := HoldFor(state, membership.MaxHold, "ops", "datacentre move", t0)
		if err != nil {
			t.Fatal(err)
		}
		want := membership.Hold{Until: t0.Add(membership.MaxHold), By: "ops", Reason: "datacentre move", At: t0}
		if held.Hold == nil || *held.Hold != want || held.Map.Epoch != state.Map.Epoch || state.Hold != nil {
			t.Fatalf("held: %+v at epoch %d", held.Hold, held.Map.Epoch)
		}
		if !held.Hold.Active(t0.Add(membership.MaxHold-time.Second)) || held.Hold.Active(t0.Add(membership.MaxHold)) {
			t.Fatal("the hold is not active exactly until it ends")
		}
		released := Release(held)
		if released.Hold != nil || released.Map.Epoch != state.Map.Epoch || held.Hold == nil {
			t.Fatalf("released: %+v at epoch %d", released.Hold, released.Map.Epoch)
		}
	})

	// A HOLD SENT AGAIN EXTENDS: it always writes a new hold ending its
	// length after THIS call, who and why included — the operator stating
	// now how much longer the maintenance needs. A retry is therefore never
	// a no-op, and the surfaces say so.
	t.Run("a hold sent again extends", func(t *testing.T) {
		t.Parallel()
		held, err := HoldFor(state, time.Hour, "ops", "rolling upgrade", t0)
		if err != nil {
			t.Fatal(err)
		}
		later := t0.Add(20 * time.Minute)
		again, err := HoldFor(held, time.Hour, "ops-2", "still upgrading", later)
		if err != nil {
			t.Fatal(err)
		}
		want := membership.Hold{Until: later.Add(time.Hour), By: "ops-2", Reason: "still upgrading",
			At: later}
		if again.Hold == nil || *again.Hold != want || held.Hold.Until != t0.Add(time.Hour) {
			t.Fatalf("held again: %+v, want %+v", again.Hold, want)
		}
	})

	// PROBATION AND OUT ARE SEPARATE: a member on probation may be taken
	// out, and stays out once its probation ends — the operator's decision
	// outlives the maintainer's.
	t.Run("out on probation", func(t *testing.T) {
		t.Parallel()
		live := roster(3)
		probation := tick(t, ticks(t, state, roster(2), c, membership.OutTicks), live, c)
		out, err := Out(probation, "data-02", "ops", "retire it", t0)
		if err != nil {
			t.Fatal(err)
		}
		if m, _ := out.Map.Member("data-02"); !m.Out || !m.Probation {
			t.Fatalf("taken out on probation: %+v", m)
		}
		trusted := ticks(t, out, live, c, membership.StableTicks)
		if m, _ := trusted.Map.Member("data-02"); !m.Out || m.Probation || m.Placeable() {
			t.Fatalf("after its probation: %+v, want out and not placeable", m)
		}
	})
}

// SETTLED IS EVERY MEMBER THE MAP PLACES ON present, healthy and finished
// repairing at the map's own epoch with nothing pending — the moment the
// copies an epoch moved away can all be confirmed. A member taken out places
// nothing and is not asked; a map that places on nobody is never settled,
// since nothing holds what it names.
func TestSettledIsEveryPlaceableMemberRepairedAtThisEpoch(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	state, err := Out(first(t, roster(3), c), "data-02", "ops", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	m := state.Map
	// data-02 is out: it reports nothing, and is not asked.
	done := repaired(roster(2), m.Epoch, 0)
	nobody := m
	nobody.Members = slices.Clone(m.Members)
	for i := range nobody.Members {
		nobody.Members[i].Out = true
	}
	for name, tc := range map[string]struct {
		m    objplacement.Map
		live []Presence
		want bool
	}{
		"the control: every placeable member done": {m, done, true},
		"a placeable member holds no lease":        {m, done[:1], false},
		"a placeable member's store failed": {m, func() []Presence {
			l := slices.Clone(done)
			l[1].Unhealthy = true
			return l
		}(), false},
		"a member finished at an older epoch": {m, func() []Presence {
			l := slices.Clone(done)
			l[1].Repair.Epoch = m.Epoch - 1
			return l
		}(), false},
		"a member has chunks pending": {m, repaired(roster(2), m.Epoch, 3), false},
		"a member never completed a pass": {m, func() []Presence {
			l := slices.Clone(done)
			l[0].Repair = RepairReport{}
			return l
		}(), false},
		"a map that places on nobody": {nobody, done, false},
	} {
		if got := Settled(tc.m, tc.live); got != tc.want {
			t.Errorf("%s: settled = %v, want %v", name, got, tc.want)
		}
	}
}

// A TICK SAYS WHETHER IT FOUND A MAP IN THE STORE — which is what its caller
// paces the next tick by — because a tick that found one counted every open
// absence against it. One that found none, the tick writing the first map
// included, counted nothing; so did one that failed before it read the map.
// And one that found a map it may not rewrite still found one.
func TestATickSaysWhetherItFoundAMap(t *testing.T) {
	t.Parallel()
	store := memory.NewFleet()
	live := roster(2)
	m, err := NewMaintainer(MaintainerOptions{Store: store,
		Live:    func(context.Context) ([]Presence, error) { return live, nil },
		Company: func() (membership.Company, bool) { return company(1, 2, ""), true },
		Now:     func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := m.Tick(t.Context()); err != nil || res.Mapped {
		t.Fatalf("the tick that wrote the first map = %+v, %v; want no map found", res, err)
	}
	if state := stored(t, store); len(state.Map.Members) != 2 {
		t.Fatalf("setup: the first map holds %v", nodes(state.Map))
	}
	live = live[:1]
	if res, err := m.Tick(t.Context()); err != nil || !res.Mapped {
		t.Fatalf("a tick over a stored map = %+v, %v; want a map found", res, err)
	}
	if run := stored(t, store).Absence["data-01"]; run.Ticks != 1 {
		t.Fatalf("the tick that found a map counted %d ticks of the absence, want 1", run.Ticks)
	}

	for name, opts := range map[string]MaintainerOptions{
		"the store did not answer": {Store: unreadable{memory.NewFleet()},
			Live: func(context.Context) ([]Presence, error) { return live, nil }},
		"the roster did not answer": {Store: store,
			Live: func(context.Context) ([]Presence, error) { return nil, errors.New("no leases") }},
	} {
		opts.Company = func() (membership.Company, bool) { return company(1, 2, ""), true }
		failing, err := NewMaintainer(opts)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := failing.Tick(t.Context()); err == nil || res.Mapped {
			t.Errorf("%s: a tick = %+v, %v; want an error and no map found", name, res, err)
		}
	}
}

type unreadable struct{ *memory.Fleet }

func (unreadable) ObjectMap(context.Context) (coord.ObjectMapRecord, bool, error) {
	return coord.ObjectMapRecord{}, false, errors.New("the store did not answer")
}

// THE ABSENCE OUTLIVES THE DUTY HOLDER: a run started by one maintainer is
// finished by the next, because it is written into the stored record rather
// than held in anybody's memory.
func TestAnAbsenceStartedByOneHolderIsFinishedByTheNext(t *testing.T) {
	t.Parallel()
	store := memory.NewFleet()
	live := roster(3)
	holder := func() *Maintainer {
		m, err := NewMaintainer(MaintainerOptions{Store: store,
			Live:    func(context.Context) ([]Presence, error) { return live, nil },
			Company: func() (membership.Company, bool) { return company(1, 2, ""), true },
			Now:     func() time.Time { return t0 }})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if _, err := holder().Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	live = live[:2]
	for range membership.OutTicks {
		if _, err := holder().Tick(t.Context()); err != nil { // a new holder every tick
			t.Fatal(err)
		}
	}
	state := stored(t, store)
	if state.Map.Holds("data-02") || len(state.Map.Members) != 2 {
		t.Fatalf("after %d ticks the map holds %v, want data-00 and data-01", membership.OutTicks, nodes(state.Map))
	}
}

// stored is the map in the store.
func stored(t *testing.T, store *memory.Fleet) objstore.MapState {
	t.Helper()
	rec, found, err := store.ObjectMap(t.Context())
	if err != nil || !found {
		t.Fatalf("no map stored: %v", err)
	}
	state, err := objstore.DecodeMapState(rec.Value)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// NO MAP WITHOUT A MEMBER OR A COMPANY, through the store as well — and a
// maintainer is refused one it could never have.
func TestNoFirstMapWithoutAMemberAndACompany(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]MaintainerOptions{
		"no member": {
			Live:    func(context.Context) ([]Presence, error) { return []Presence{up("agent-1", 0)}, nil },
			Company: func() (membership.Company, bool) { return company(1, 3, ""), true }},
		"no company": {
			Live:    func(context.Context) ([]Presence, error) { return roster(1), nil },
			Company: func() (membership.Company, bool) { return membership.Company{}, false }},
		"a company that cannot be applied": {
			Live:    func(context.Context) ([]Presence, error) { return roster(1), nil },
			Company: func() (membership.Company, bool) { return company(1, 0, ""), true }},
	} {
		store := memory.NewFleet()
		opts.Store = store
		m, err := NewMaintainer(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Tick(t.Context()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, found, _ := store.ObjectMap(t.Context()); found {
			t.Errorf("%s: a map was written", name)
		}
	}
	if _, err := NewMaintainer(MaintainerOptions{Store: memory.NewFleet(),
		Live: func(context.Context) ([]Presence, error) { return nil, nil }}); err == nil {
		t.Error("a maintainer with no company source was built")
	}
}

// A MAP THIS BUILD CANNOT REWRITE IS NEVER OVERWRITTEN — one it cannot read,
// and one it can read but that carries a field it does not know: a newer
// build wrote either, and rewriting it in this build's shape would drop what
// that build added. The second is still PLACED BY, since a reader refusing it
// would stop every upload for the length of an upgrade.
func TestAMapThisBuildCannotRewriteIsNotOverwritten(t *testing.T) {
	t.Parallel()
	valid, err := first(t, roster(1), company(1, 1, "")).Encode()
	if err != nil {
		t.Fatal(err)
	}
	newer := append([]byte(`{"from_a_newer_build":true,`), valid[1:]...)
	if _, err := objstore.DecodeMapState(newer); err != nil {
		t.Fatalf("a reader refused a map with a field it does not know: %v", err)
	}
	for name, raw := range map[string][]byte{
		"unreadable":         []byte(`{"map":{"epoch":9,"replicas":0}}`),
		"from a newer build": newer,
	} {
		store := memory.NewFleet()
		if _, _, err := store.CreateObjectMap(t.Context(), raw); err != nil {
			t.Fatal(err)
		}
		m, err := NewMaintainer(MaintainerOptions{Store: store,
			Live:    func(context.Context) ([]Presence, error) { return roster(2), nil },
			Company: func() (membership.Company, bool) { return company(2, 1, ""), true }})
		if err != nil {
			t.Fatal(err)
		}
		// FOUND ALL THE SAME: the caller paces on it as on any map.
		if res, err := m.Tick(t.Context()); !errors.Is(err, errNewerMap) || !res.Mapped {
			t.Errorf("%s: Tick = %+v, %v; want a map found and errNewerMap", name, res, err)
		}
		if rec, _, _ := store.ObjectMap(t.Context()); string(rec.Value) != string(raw) {
			t.Errorf("%s: overwritten with %s", name, rec.Value)
		}
	}
}

// A LOST RACE IS NOT AN ERROR AND WRITES NOTHING: the next tick starts from
// what the winner wrote.
func TestALostRaceWritesNothing(t *testing.T) {
	t.Parallel()
	store := &racing{Fleet: memory.NewFleet()}
	observed := &observer{}
	m, err := NewMaintainer(MaintainerOptions{Store: store, Observer: observed,
		Live:    func(context.Context) ([]Presence, error) { return roster(1), nil },
		Company: func() (membership.Company, bool) { return company(1, 1, ""), true }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Tick(t.Context()); err != nil {
		t.Fatalf("a lost race = %v, want nil", err)
	}
	if observed.n != 0 {
		t.Fatal("a map this holder lost the race to write was installed as though written")
	}
}

type racing struct{ *memory.Fleet }

func (r *racing) CreateObjectMap(context.Context, []byte) (coord.ObjectMapRecord, bool, error) {
	return coord.ObjectMapRecord{}, false, nil
}

type observer struct{ n int }

func (o *observer) Observe(objstore.MapState, uint64) { o.n++ }
