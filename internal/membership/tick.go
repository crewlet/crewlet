package membership

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/placement"
	seatplacement "github.com/crewlet/crewlet/internal/seat/placement"
)

// Presence is one live data node as a maintainer sees it: what its membership
// lease says. A map whose own lease says more — the object map's repair
// report — carries this inside its own presence and hands [Tick] this part.
type Presence struct {
	// Node is the node's id.
	Node string

	// Weight is the share it offers to hold, 1 to [placement.MaxWeight]; a
	// larger one is held to the ceiling, and one below 1 offers nothing and
	// is not a data node at all.
	Weight int

	// Labels are the node's own labels (node.labels), for its value of
	// the map's failure-domain label.
	Labels map[string]string

	// Unhealthy is a node up and holding its lease whose store reports
	// itself failed, and Detail what it said. A map counts it exactly as
	// an absent one.
	//
	// UNHEALTHY RATHER THAN HEALTHY, so the zero value is the one that
	// removes nobody: a caller that never reported health leaves every
	// node placed on, as before health was reported at all, where the
	// other spelling would count every node absent and empty the map.
	Unhealthy bool
	Detail    string
}

// ByNode is every node offering a share, by node, from a list of whatever a
// map's own presences are — of says which part of one is its [Presence]: a
// presence with no node or offering no share is none, and a node listed twice
// is taken at its first.
//
// ONE RULE for which presences a tick counts, read by [Tick] and by any rule
// of a map's own that has to see the same nodes — the object map's split and
// collection gates, which read the repair report beside it.
func ByNode[P any](live []P, of func(P) Presence) map[string]P {
	out := make(map[string]P, len(live))
	for _, p := range live {
		mp := of(p)
		if mp.Node == "" || mp.Weight < 1 {
			continue
		}
		if _, twice := out[mp.Node]; !twice {
			out[mp.Node] = p
		}
	}
	return out
}

// Company is what the company configuration says about a map, read afresh
// every tick.
//
// THE COMPANY'S, not the node's: the copies a map keeps are a decision about
// the company's data, and a replica count read off whichever node held the
// map's duty — its own stream.replicas — dropped every group of the object map
// to one copy the day a node left at the default held it, and the collectors
// deleted the rest (ADR-0027).
type Company struct {
	// Epoch is the activation this came from: newer activations are
	// larger. 0 is no company at all, and the zero Company is therefore
	// none — [Tick] applies nothing from it.
	Epoch uint64

	// Replicas is the copies each group should have, already resolved
	// from the config's default: 1..[placement.MaxReplicas].
	Replicas int

	// FailureDomain is the node label whose values copies are spread
	// across, and empty for none.
	FailureDomain string

	// Block is the Tier B block these came from — `objects`, `estate` —
	// which a refusal names, so an operator is told the field to change.
	Block string
}

// Validate refuses a company a maintainer may not apply, naming the field
// that is wrong. Like the gestures' refusals it carries no package prefix: the
// maintainer that logs it names its map.
func (c Company) Validate() error {
	switch {
	case c.Epoch == 0:
		return errors.New("the company names no activation epoch")
	case c.Replicas < 1 || c.Replicas > placement.MaxReplicas:
		return fmt.Errorf("%s resolved to %d, want 1..%d", c.field("replicas"),
			c.Replicas, placement.MaxReplicas)
	}
	if c.FailureDomain != "" {
		if err := seatplacement.CheckLabelKey(c.FailureDomain); err != nil {
			return fmt.Errorf("%s: %w", c.field("failure_domain"), err)
		}
	}
	return nil
}

// field is a field of the company's block, as a refusal names it.
func (c Company) field(name string) string {
	if c.Block == "" {
		return name
	}
	return c.Block + "." + name
}

// Tick is what should follow state and the members a map's draw is taken
// over, given who is live now and what the company says. It is one TICK:
// every open absence is counted once. It answers the next state and the draw
// with its copies, label and members brought up to date — the members sorted
// by node, each carrying the Out and Probation flags placement reads — and
// leaves the draw's salt and groups as they were.
//
// PURE, so every rule is tested without a store. The rules run in order, each
// a step below:
//
//  1. Config: a company at least as recent as the one the map was set by
//     sets the replica count and the failure-domain label; an older one, or
//     none, changes neither.
//  2. Returning: a node removed for absence that is present and healthy
//     counts a tick of probation, and is trusted — placed on again — after
//     [StableTicks] of them; one that is not loses its probation, is no
//     longer a member if it was one, and is forgotten after [OutTicks]
//     consecutive ticks gone.
//  3. Membership: every live data node is weighed and labelled; a healthy one
//     that is not a member joins, at the fleet's share for its weight — on
//     probation if it is remembered as removed, placed on nothing until it
//     is trusted.
//  4. Absence: a member not live, or live and unhealthy, is counted one more
//     tick absent; one present with a run open counts a present tick, and
//     the run is cleared after [StableTicks] of them.
//  5. Removal: a member counted absent on this tick whose run has reached
//     [OutTicks] is removed — unless a hold is active, or it is one that
//     takes copies and no member that takes copies is present and healthy
//     on this tick to take its share.
//  6. Out: a member is out exactly while an operator's gesture says so — taken
//     out, or barred, which a removal does not lift ([State.Barred]).
//
// What a map does next with a change to its members — balance it, move an
// epoch — is the map's.
func Tick(s State, d placement.Draw, live []Presence, co Company, now time.Time) (State, placement.Draw) {
	t := newTicking(s, d, live, now)
	t.config(co)
	t.returning()
	t.membership()
	t.absence()
	t.removal()
	t.out()
	t.assemble()
	return t.next, t.draw
}

// ticking is one tick's work in progress.
type ticking struct {
	before     State
	beforeDraw placement.Draw
	next       State
	draw       placement.Draw

	// members is the next draw's members by node, until assembled.
	members map[string]placement.Member

	// live is every node offering a share, by node.
	live map[string]Presence

	// counted is the members this tick counted absent.
	counted map[string]bool

	now time.Time
}

func newTicking(s State, d placement.Draw, live []Presence, now time.Time) *ticking {
	t := &ticking{
		before:     s,
		beforeDraw: d,
		next:       s.Clone(),
		draw:       d,
		members:    make(map[string]placement.Member, len(d.Members)),
		live:       ByNode(live, func(p Presence) Presence { return p }),
		counted:    map[string]bool{},
		// UTC, so one instant is one stored spelling and a record
		// rewritten with nothing changed is the same bytes.
		now: now.UTC(),
	}
	for _, m := range d.Members {
		t.members[m.Node] = m
	}
	if t.next.Absence == nil {
		t.next.Absence = map[string]Absence{}
	}
	if t.next.Removed == nil {
		t.next.Removed = map[string]Removal{}
	}
	if t.next.TakenOut == nil {
		t.next.TakenOut = map[string]Gesture{}
	}
	return t
}

// healthy is a node's presence when it is live and its store is not failed.
func (t *ticking) healthy(node string) (Presence, bool) {
	p, ok := t.live[node]
	return p, ok && !p.Unhealthy
}

// config is step 1.
//
// AN OLDER ACTIVATION NEVER OVERWRITES A NEWER ONE: the duty moves between
// nodes, and a node that has not yet applied the latest revision would
// otherwise set the replica count back to the one before it — and back again
// when the duty moved on, each flip re-placing a share of the data.
func (t *ticking) config(c Company) {
	if c.Validate() != nil || c.Epoch < t.next.Config.Epoch {
		return
	}
	t.draw.Replicas = c.Replicas
	t.draw.FailureDomain = c.FailureDomain
	t.next.Config.Epoch = c.Epoch
}

// returning is step 2: every node remembered as removed counts one more tick
// present or gone — a tick of probation, or one toward being forgotten — and
// is trusted again, sent back to removed, or forgotten.
//
// A MEMBER ON PROBATION THAT IS NOT PRESENT AND HEALTHY LEAVES THE MAP ON THIS
// TICK, with no grace of its own: it was being watched for exactly that, and
// it places nothing, so there is no share to move and nothing a second grace
// would save. What it held is where it was, for when it is next seen.
func (t *ticking) returning() {
	for node, r := range t.next.Removed {
		if _, ok := t.healthy(node); ok {
			r.Present++
			r.Gone = 0
			if r.Present >= StableTicks {
				// Trusted again: membership places on it.
				delete(t.next.Removed, node)
				continue
			}
			t.next.Removed[node] = r
			continue
		}
		if _, member := t.members[node]; member {
			delete(t.members, node)
			r.At, r.Reason, r.Detail = t.now, ReasonAbsent, ""
			if p, live := t.live[node]; live {
				r.Reason, r.Detail = ReasonUnhealthy, p.Detail
			}
		}
		r.Gone++
		r.Present = 0
		if r.Gone >= OutTicks {
			// Gone a whole grace at a stretch, which no flapping node
			// is: when it returns it is a node like any other.
			delete(t.next.Removed, node)
			continue
		}
		t.next.Removed[node] = r
	}
}

// membership is step 3.
//
// PROBATION IS DERIVED, never carried: a member is on it exactly while the
// record remembers it as removed, the way a member is out exactly while the
// record holds an operator's gesture — so the flag the map places by and the
// count that ends it can never disagree past the tick.
func (t *ticking) membership() {
	domain := t.draw.FailureDomain
	relabelled := domain != t.beforeDraw.FailureDomain
	for node, m := range t.members {
		_, m.Probation = t.next.Removed[node]
		p, live := t.live[node]
		switch {
		case domain == "":
			m.Domain = ""
		case live:
			m.Domain = p.Labels[domain]
		case relabelled:
			// Its value of the NEW label is unknown until it is
			// seen again, and its value of the old one names
			// nothing: no domain, which collides with nobody.
			m.Domain = ""
		}
		if live {
			if w := min(p.Weight, placement.MaxWeight); w != m.Weight {
				m.Share = rescale(m.Share, m.Weight, w)
				m.Weight = w
			}
		}
		t.members[node] = m
	}
	for node, p := range t.live {
		if _, member := t.members[node]; member || p.Unhealthy {
			continue
		}
		_, removed := t.next.Removed[node]
		weight := min(p.Weight, placement.MaxWeight)
		m := placement.Member{Node: node, Weight: weight,
			// THE PREVIOUS DRAW'S RATE, for every newcomer alike, so
			// two joining at once start where either would alone.
			Share: t.beforeDraw.ShareFor(weight),
			// A REMOVED NODE SEEN BACK joins at once, on probation:
			// read from and repaired from, placed on nothing until
			// returning trusts it. See [Removal].
			Probation: removed}
		if domain != "" {
			m.Domain = p.Labels[domain]
		}
		t.members[node] = m
	}
}

// rescale is a member's share after its weight moved, as the balance's
// starting point: the same share per unit of weight as before.
func rescale(share uint32, from, to int) uint32 {
	if from < 1 {
		return placement.DefaultShare(to)
	}
	s := uint64(share) * uint64(to) / uint64(from)
	return uint32(min(max(s, 1), math.MaxUint32))
}

// absence is step 4.
func (t *ticking) absence() {
	for node := range t.members {
		p, live := t.live[node]
		run, open := t.next.Absence[node]
		if live && !p.Unhealthy {
			if !open {
				continue
			}
			run.Present++
			if run.Present >= StableTicks {
				delete(t.next.Absence, node)
				continue
			}
			t.next.Absence[node] = run
			continue
		}
		if !open {
			run = Absence{Since: t.now}
		}
		run.Ticks++
		run.Present = 0
		run.Reason, run.Detail = ReasonAbsent, ""
		if live {
			run.Reason, run.Detail = ReasonUnhealthy, p.Detail
		}
		t.next.Absence[node] = run
		t.counted[node] = true
	}
	for node := range t.next.Absence {
		if _, member := t.members[node]; !member {
			delete(t.next.Absence, node)
		}
	}
}

// removal is step 5.
//
// ONLY A MEMBER COUNTED ABSENT ON THIS TICK is removed, never one present now
// whose run is still open: a member back is placed on while it proves itself
// stable, and one that leaves again reaches the grace on the tick it does.
//
// AND NEVER ONTO NOTHING. Removing a member re-places its share on the others,
// so it is removed only while some member that takes copies is present and
// healthy on THIS tick to take it — the question [Out] asks. Counting every
// member that is merely not due yet was the wrong question: in a whole-tier
// outage whose leases lapse over a few ticks, the first to reach the grace
// counted the others — as dead as it was, just later — as somewhere to go,
// and each was removed onto the next until one was left; when power came back
// the map placed on that one for a whole probation while the rest proved
// themselves, every write kept one copy and every group the one had not held
// read as missing. With none present nothing that takes copies is removed:
// the map that works the moment the fleet returns is the one it has. A member
// on probation takes no copies, so it is no place for a share to go, and an
// out member places nothing, so removing one never leaves the map with less
// to place on.
func (t *ticking) removal() {
	if t.next.Hold != nil && !t.next.Hold.Active(t.now) {
		t.next.Hold = nil
	}
	if t.next.Hold.Active(t.now) {
		return
	}
	var due []string
	for node := range t.counted {
		if t.next.Absence[node].Ticks >= OutTicks {
			due = append(due, node)
		}
	}
	if len(due) == 0 {
		return
	}
	present := 0
	for node, m := range t.members {
		if !t.next.out(node) && !m.Probation && !t.counted[node] {
			present++
		}
	}
	for _, node := range due {
		if present == 0 && !t.next.out(node) {
			continue
		}
		run := t.next.Absence[node]
		delete(t.members, node)
		delete(t.next.Absence, node)
		delete(t.next.TakenOut, node)
		t.next.Removed[node] = Removal{At: t.now, Reason: run.Reason, Detail: run.Detail}
	}
}

// out is step 6.
//
// AN OUT ENDS WITH MEMBERSHIP AND A BAR DOES NOT: the operator's out did its
// work once the member's share moved and it was removed, while a bar is kept
// for the node it names whether or not the map holds it ([State.Barred]).
func (t *ticking) out() {
	for node := range t.next.TakenOut {
		if _, member := t.members[node]; !member {
			delete(t.next.TakenOut, node)
		}
	}
	for node, m := range t.members {
		m.Out = t.next.out(node)
		t.members[node] = m
	}
}

// assemble writes the members back into the draw in node order.
func (t *ticking) assemble() {
	t.draw.Members = make([]placement.Member, 0, len(t.members))
	for _, node := range slices.Sorted(maps.Keys(t.members)) {
		t.draw.Members = append(t.draw.Members, t.members[node])
	}
	t.next.tidy()
}
