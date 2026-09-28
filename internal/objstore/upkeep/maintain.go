package upkeep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/placement"
	seatplacement "github.com/crewlet/crewlet/internal/seat/placement"
)

var log = logging.Get("objstore")

// OutGrace is how long a member may be gone before the map stops placing
// objects on it.
//
// TEN MINUTES, the interval Ceph waits before marking a down device out, for
// the same reason: a restart, a reboot and a rolling upgrade's turn at a node
// are all minutes, and removing a member moves its whole share across the
// fleet — traffic that is pure waste for a node about to come back, and that
// competes with everything else the fleet is doing. Past it, the risk that a
// second failure finds a chunk one copy short outweighs the cost of the copy.
// Until then a displaced write keeps its replica count by landing on the next
// member of the ranking, so a short absence costs no durability.
//
// It is never measured as a span of wall-clock time: it is [OutTicks] of the
// maintainer's own ticks.
const OutGrace = 10 * time.Minute

// TickInterval is the maintainer's cadence once a map exists: the caller runs
// [Maintainer.Tick] once per interval while it holds the duty, and every tick
// that finds a map counts one tick of every open absence — which the tick
// itself reports ([TickResult.Mapped]), so the caller paces on it.
//
// THE RECONCILE INTERVAL, the heartbeat every lease in the fleet is renewed
// on: a node that joins is placed on within one of its own heartbeats, and a
// coarser tick buys nothing but a slower join. The absence arithmetic is
// expressed in ticks derived from it ([OutTicks]), so the grace stays
// [OutGrace] whatever this is.
const TickInterval = coord.ReconcileInterval

// OutTicks is how many ticks must count a member absent before the map
// removes it: [OutGrace] at [TickInterval], forty.
//
// TICKS, NOT A DEADLINE. A deadline would be compared against the clock of
// whichever node held the duty — each holder a different clock, so skew would
// stretch or skip the grace — while a tick is counted only by the holder that
// makes it. A gap between holders therefore delays a removal, the safe
// direction, and can never skip one.
const OutTicks = int(OutGrace / TickInterval)

// StableTicks is how many consecutive ticks must see a member present and
// healthy before its absence run is cleared — and how long a node removed for
// absence is on PROBATION once it is seen back: a member again, read from and
// repaired from, but placed on only after this many.
//
// THE GRACE ITSELF, so the map trusts a member back on the same evidence it
// takes to give up on one. A member present for fewer keeps the ticks it has
// accumulated, which is what removes a FLAPPING member: up thirty seconds in
// every few minutes, it is never gone for a whole grace at a stretch, and a
// run cleared by any single sighting kept it placed on for ever while it was
// mostly gone.
const StableTicks = OutTicks

// MaxHold is the longest an operator may hold the map.
//
// A DAY, and a hold always ends. Ceph's noout has no expiry, and its failure
// mode is exactly the one this avoids: a flag set for a maintenance window and
// forgotten pins a dead member in the map — its groups a copy short — until
// someone happens to notice. A day covers any planned maintenance, and a
// longer one is a gesture renewed on purpose.
const MaxHold = 24 * time.Hour

// rebalanceAbove is the measured deviation past which a map whose placement
// changed without a balance — the epoch that split its groups — is balanced.
//
// TWICE THE TOLERANCE a balance aims for, as hysteresis: a balance moves
// groups to buy precision, and a map measured just past the tolerance would
// spend an epoch's worth of movement on a percent of evenness, where one past
// twice it has drifted far enough for a disk to notice.
const rebalanceAbove = 2 * placement.DefaultTolerance

// Presence is one live data node as the maintainer sees it: what its objects
// lease says.
type Presence struct {
	// Node is the node's id.
	Node string

	// Weight is the share of the objects it offers to hold, 1 to
	// [placement.MaxWeight]; a larger one is held to the ceiling, and one
	// below 1 offers nothing and is not a data node at all.
	Weight int

	// Labels are the node's own labels (node.labels), for its value of
	// the map's failure-domain label.
	Labels map[string]string

	// Unhealthy is a node up and holding its lease whose object store
	// reports itself failed, and Detail what it said. The map counts it
	// exactly as an absent one.
	//
	// UNHEALTHY RATHER THAN HEALTHY, so the zero value is the one that
	// removes nobody: a caller that never reported health leaves every
	// node placed on, as before health was reported at all, where the
	// other spelling would count every node absent and empty the map.
	Unhealthy bool
	Detail    string

	// Repair is what its last completed repair pass reported, for the
	// split gate.
	Repair RepairReport
}

// RepairReport is what a data node's last COMPLETED repair pass found.
type RepairReport struct {
	// Epoch is the map epoch the pass ran at, 0 when none has completed —
	// which is no map's epoch, so a node that never finished a pass is
	// never counted clean.
	Epoch uint64

	// Pending is how many chunks the map places on the node that it still
	// did not hold when the pass ended.
	Pending int
}

// Company is what the company configuration says about the object store,
// read afresh every tick.
//
// THE COMPANY'S, not the node's: the copies every chunk has are a decision
// about the company's data, and a replica count read off whichever node held
// the map duty — its own stream.replicas — dropped every group to one copy
// the day a node left at the default held it, and the collectors deleted the
// rest.
type Company struct {
	// Epoch is the activation this came from: newer activations are
	// larger. 0 is no company at all, and the zero Company is therefore
	// none — the maintainer applies nothing from it.
	Epoch uint64

	// Replicas is the copies each chunk should have, already resolved
	// from the config's default: 1..[placement.MaxReplicas].
	Replicas int

	// FailureDomain is the node label whose values copies are spread
	// across, and empty for none.
	FailureDomain string
}

// Validate refuses a company the maintainer may not apply, naming what is
// wrong with it.
func (c Company) Validate() error {
	switch {
	case c.Epoch == 0:
		return errors.New("objstore/upkeep: the company names no activation epoch")
	case c.Replicas < 1 || c.Replicas > placement.MaxReplicas:
		return fmt.Errorf("objstore/upkeep: objects.replicas resolved to %d, want 1..%d",
			c.Replicas, placement.MaxReplicas)
	}
	if c.FailureDomain != "" {
		if err := seatplacement.CheckLabelKey(c.FailureDomain); err != nil {
			return fmt.Errorf("objstore/upkeep: objects.failure_domain: %w", err)
		}
	}
	return nil
}

// Next is the map that should follow state, given who is live now and what
// the company says. It is one TICK: every open absence is counted once.
//
// PURE — but for the generation it mints for a first map, which is random by
// design — so every rule is tested without a store. The rules run in order,
// each a step below:
//
//  1. Config: a company at least as recent as the one the map was set by
//     sets the replica count and the failure-domain label; an older one, or
//     none, changes neither. A first map is written only from a company.
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
//  6. Out: a member is out exactly while an operator's gesture says so.
//  7. Placement: a changed placement is balanced and moves the epoch. An
//     unchanged one never measured since it last changed is measured, and
//     balanced if it has drifted past twice the tolerance. Otherwise, a
//     clean fleet whose group count is short of its target splits one bit —
//     an epoch that changes nothing else.
//
// changed reports whether there is anything to write at all: absences,
// removals remembered, holds and measurements included.
func Next(state objstore.MapState, live []Presence, company Company, now time.Time) (objstore.MapState, bool) {
	first := state.Map.Generation == uuid.Nil
	if first && company.Validate() != nil {
		// NO FIRST MAP WITHOUT A COMPANY: it would have to invent the
		// copies every chunk keeps, and the first file stored would be
		// stored at a count nobody chose.
		return state, false
	}
	t := newTicking(state, live, now)
	t.config(company)
	t.returning()
	t.membership()
	t.absence()
	t.removal()
	t.out()
	if first && len(t.members) == 0 {
		// NO MAP FOR A FLEET WITH NO MEMBER: an empty one says what
		// none says, at a write per tick.
		return state, false
	}
	t.assemble()
	t.placement(first)
	return t.next, !sameState(state, t.next)
}

// ticking is one tick's work in progress.
type ticking struct {
	before objstore.MapState
	next   objstore.MapState

	// members is the next map's members by node, until assembled.
	members map[string]placement.Member

	// live is every node offering to hold objects, by node.
	live map[string]Presence

	// counted is the members this tick counted absent.
	counted map[string]bool

	now time.Time
}

func newTicking(state objstore.MapState, live []Presence, now time.Time) *ticking {
	t := &ticking{
		before:  state,
		next:    state.Clone(),
		members: make(map[string]placement.Member, len(state.Map.Members)),
		live:    byNode(live),
		counted: map[string]bool{},
		// UTC, so one instant is one stored spelling and a record
		// rewritten with nothing changed is the same bytes.
		now: now.UTC(),
	}
	for _, m := range state.Map.Members {
		t.members[m.Node] = m
	}
	if t.next.Absence == nil {
		t.next.Absence = map[string]objstore.Absence{}
	}
	if t.next.Removed == nil {
		t.next.Removed = map[string]objstore.Removal{}
	}
	if t.next.TakenOut == nil {
		t.next.TakenOut = map[string]objstore.Gesture{}
	}
	return t
}

// byNode is every node offering to hold objects, by node: a presence with no
// node or offering no share is none, and a node listed twice is taken at its
// first.
func byNode(live []Presence) map[string]Presence {
	out := make(map[string]Presence, len(live))
	for _, p := range live {
		if p.Node == "" || p.Weight < 1 {
			continue
		}
		if _, twice := out[p.Node]; !twice {
			out[p.Node] = p
		}
	}
	return out
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
	t.next.Map.Replicas = c.Replicas
	t.next.Map.FailureDomain = c.FailureDomain
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
			r.At, r.Reason, r.Detail = t.now, objstore.ReasonAbsent, ""
			if p, live := t.live[node]; live {
				r.Reason, r.Detail = objstore.ReasonUnhealthy, p.Detail
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
	domain := t.next.Map.FailureDomain
	relabelled := domain != t.before.Map.FailureDomain
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
			// THE PREVIOUS MAP'S RATE, for every newcomer alike, so
			// two joining at once start where either would alone.
			Share: t.before.Map.Draw().ShareFor(weight),
			// A REMOVED NODE SEEN BACK joins at once, on probation:
			// read from and repaired from, placed on nothing until
			// returning trusts it. See [objstore.Removal].
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
			run = objstore.Absence{Since: t.now}
		}
		run.Ticks++
		run.Present = 0
		run.Reason, run.Detail = objstore.ReasonAbsent, ""
		if live {
			run.Reason, run.Detail = objstore.ReasonUnhealthy, p.Detail
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
	isOut := func(node string) bool {
		_, out := t.next.TakenOut[node]
		return out
	}
	present := 0
	for node, m := range t.members {
		if !isOut(node) && !m.Probation && !t.counted[node] {
			present++
		}
	}
	for _, node := range due {
		if present == 0 && !isOut(node) {
			continue
		}
		run := t.next.Absence[node]
		delete(t.members, node)
		delete(t.next.Absence, node)
		delete(t.next.TakenOut, node)
		t.next.Removed[node] = objstore.Removal{At: t.now, Reason: run.Reason, Detail: run.Detail}
	}
}

// out is step 6.
func (t *ticking) out() {
	for node := range t.next.TakenOut {
		if _, member := t.members[node]; !member {
			delete(t.next.TakenOut, node)
		}
	}
	for node, m := range t.members {
		_, m.Out = t.next.TakenOut[node]
		t.members[node] = m
	}
}

// assemble writes the members back into the map in node order.
func (t *ticking) assemble() {
	t.next.Map.Members = make([]placement.Member, 0, len(t.members))
	for _, node := range slices.Sorted(maps.Keys(t.members)) {
		t.next.Map.Members = append(t.next.Map.Members, t.members[node])
	}
	tidy(&t.next)
}

// tidy leaves an empty record field empty rather than present and blank, so
// two records saying the same thing are one record.
func tidy(s *objstore.MapState) {
	if len(s.Absence) == 0 {
		s.Absence = nil
	}
	if len(s.Removed) == 0 {
		s.Removed = nil
	}
	if len(s.TakenOut) == 0 {
		s.TakenOut = nil
	}
}

// placement is step 7.
//
// A SPLIT IS AN EPOCH OF ITS OWN. Raising the group bits re-places half the
// data — every upper child is drawn afresh — and it is the only change that
// keeps every lower child's holders exactly, which it can do only when the
// shares are the parent's. So the epoch that splits changes nothing else, and
// the balance the split calls for comes on the next tick, as its own epoch:
// each moving as little as it can. And it splits only a CLEAN fleet — every
// placeable member present, healthy and finished repairing at this epoch with
// nothing pending — one bit per epoch, so a fleet never re-places half its
// data while it is still moving the last change's.
func (t *ticking) placement(first bool) {
	switch {
	case first:
		t.next.Map.Generation = uuid.New()
		t.next.Map.PGBits = objplacement.TargetPGBits(len(t.next.Map.Placeable()), t.next.Map.Size())
		rebalance(&t.next, 0)
	case !samePlacement(t.before.Map, t.next.Map):
		rebalance(&t.next, t.before.Map.Epoch)
	case t.next.Balance.Epoch != t.next.Map.Epoch:
		t.measure()
	case t.clean():
		t.next.Map.PGBits++
		t.next.Map.Epoch++
	}
}

// measure measures a placement nothing has measured since it changed, and
// balances it when it has drifted past [rebalanceAbove].
func (t *ticking) measure() {
	m := t.next.Map
	if dev := m.Layout().Deviation(); dev <= rebalanceAbove {
		t.next.Balance = objstore.Balance{Epoch: m.Epoch, BalanceReport: placement.BalanceReport{
			Deviation: dev, Converged: dev <= placement.DefaultTolerance}}
		return
	}
	balanced, report := placement.Balance(m.Draw(), placement.BalanceOptions{})
	if slices.Equal(balanced.Members, m.Members) {
		// Nothing better to be had: record that, so the next tick does
		// not measure it again.
		t.next.Balance = objstore.Balance{Epoch: m.Epoch, BalanceReport: report}
		return
	}
	t.next.Map.Members = balanced.Members
	t.next.Map.Epoch = m.Epoch + 1
	t.next.Balance = objstore.Balance{Epoch: t.next.Map.Epoch, BalanceReport: report}
}

// clean reports whether the fleet may split its groups now: short of the
// count it should have, with no absence run open and no member on probation,
// and [Settled].
func (t *ticking) clean() bool {
	m := t.next.Map
	// ANY OPEN RUN REFUSES, a member back and still proving itself included
	// — deliberately not the [absentNow] question every other gate asks.
	// This one is about STABILITY rather than presence: a split re-places
	// half the data, and a member that was gone within the last grace is
	// one that may leave again halfway through copying its new share. A
	// member on PROBATION refuses for the same reason from the other side:
	// the tick that trusts it is a placement change within a grace, and a
	// split taken now would re-place half the data only for the balance
	// that follows it to move a share again.
	if m.PGBits >= objplacement.MaxPGBits || len(t.next.Absence) > 0 ||
		slices.ContainsFunc(m.Members, func(x placement.Member) bool { return x.Probation }) {
		return false
	}
	if objplacement.TargetPGBits(len(m.Placeable()), m.Size()) <= m.PGBits {
		return false
	}
	return settled(m, t.live)
}

// Settled reports whether a map's repair has FINISHED across the fleet: every
// member it places on is present in live, healthy, and reports a completed
// repair at the map's own epoch with nothing pending. A map that places on
// nobody is not settled — nothing holds what it names.
//
// ONE DEFINITION FOR BOTH OF ITS READERS: the maintainer splits groups only
// on a settled fleet, and a data node collects the copies an epoch moved away
// from it once the fleet has settled at that epoch — the moment every member
// the map places them on can vouch for its own. Two spellings of "the fleet
// has caught up" would drift. It only ever says WHEN to look: what a
// collection may delete is still decided copy by copy, by the members'
// verified answers ([Node.Collect]).
func Settled(m objplacement.Map, live []Presence) bool {
	return settled(m, byNode(live))
}

func settled(m objplacement.Map, live map[string]Presence) bool {
	placeable := m.Placeable()
	if len(placeable) == 0 {
		return false
	}
	for _, member := range placeable {
		p, ok := live[member.Node]
		if !ok || p.Unhealthy || p.Repair.Epoch != m.Epoch || p.Repair.Pending != 0 {
			return false
		}
	}
	return true
}

// rebalance finishes a change to what a map places: shares balanced from the
// ones it has, and the epoch moved past from.
func rebalance(s *objstore.MapState, from uint64) {
	balanced, report := placement.Balance(s.Map.Draw(), placement.BalanceOptions{})
	s.Map.Members = balanced.Members
	s.Map.Epoch = from + 1
	s.Balance = objstore.Balance{Epoch: s.Map.Epoch, BalanceReport: report}
}

// samePlacement reports whether two maps place every chunk on the same
// members: everything but the epoch and the generation.
func samePlacement(a, b objplacement.Map) bool {
	return a.Replicas == b.Replicas && a.PGBits == b.PGBits &&
		a.FailureDomain == b.FailureDomain && slices.Equal(a.Members, b.Members)
}

// sameState reports whether two records would be stored as the same bytes —
// which is the question "is there anything to write", asked of every field
// at once, so a field added later is not one a hand-written comparison
// forgot.
func sameState(a, b objstore.MapState) bool {
	encode := func(s objstore.MapState) ([]byte, error) {
		if s.Map.Members == nil {
			s.Map.Members = []placement.Member{}
		}
		return json.Marshal(s)
	}
	ra, errA := encode(a)
	rb, errB := encode(b)
	return errA == nil && errB == nil && bytes.Equal(ra, rb)
}

// Gesture refusals, each naming what the operator asked for that cannot be
// done.
var (
	// ErrNoMap is a gesture on a fleet that has no placement map yet.
	ErrNoMap = errors.New("objstore/upkeep: there is no placement map yet")

	// ErrUnknownMember names a node the map neither holds nor remembers.
	ErrUnknownMember = errors.New("objstore/upkeep: no such member of the placement map")

	// ErrRemovedMember is taking out a node the map removed for absence
	// and has not seen back: it is not a member and places nothing, so
	// there is nothing to take out. It IS an unknown member to the
	// gesture, and wraps [ErrUnknownMember] so a caller that renders only
	// that still answers truthfully.
	//
	// A STATEMENT OF WHAT IS, with the node named last as every refusal
	// here names it, and no remedy in it: the text is the detail of every
	// surface's refusal — the API's, the CLI's, the dashboard's — and each
	// words the remedy for its own reader in its hint (`crewlet objects
	// in`, a "Put back" button), so a command name here would be the wrong
	// one on two of the three.
	ErrRemovedMember = fmt.Errorf("%w: the map removed it for absence, so it places "+
		"nothing to take out", ErrUnknownMember)

	// ErrNothingPlaceable is taking out the last member copies could be
	// placed on.
	ErrNothingPlaceable = errors.New("objstore/upkeep: taking it out would leave no " +
		"present member to place copies on")

	// ErrHoldRange is a hold of no length, or one past [MaxHold].
	ErrHoldRange = errors.New("objstore/upkeep: a hold lasts more than nothing and at most a day")
)

// Out takes a member out: the map places nothing on it, so its share moves to
// the others while it keeps serving what it holds — the gesture that makes a
// planned removal a copy rather than a recovery, since every chunk it holds
// has a live source until the others have it. The member stays until [In]
// puts it back or it is gone for the grace. A member on probation may be
// taken out too, and then stays out once its probation ends.
//
// TAKING OUT A MEMBER ALREADY OUT CHANGES NOTHING, the record of who took it
// out, why and when included: the answer is the state it was given, so a
// caller that writes only what changed writes nothing, and an operator
// re-sending a gesture whose answer was lost — the thing a lost answer tells
// them to do — neither moves the record's version under a maintainer's tick
// nor overwrites the first operator's reason with a retry's.
//
// It refuses to take out the last member present to place copies on: every
// write would then have nowhere to land. Present is what the latest tick saw
// ([absentNow]), so a member back from a missed tick counts. And it refuses a
// node the map removed and has not seen back ([ErrRemovedMember]): that one
// places nothing already.
//
// Pure over the record: the caller reads it, applies this and writes the
// result with a compare-and-set.
func Out(state objstore.MapState, node, by, reason string, now time.Time) (objstore.MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	member, ok := state.Map.Member(node)
	switch {
	case !ok:
		if _, removed := state.Removed[node]; removed {
			return state, fmt.Errorf("%w: %q", ErrRemovedMember, node)
		}
		return state, fmt.Errorf("%w: %q", ErrUnknownMember, node)
	case member.Out:
		return state, nil
	}
	if member.Placeable() {
		others := 0
		for _, m := range state.Map.Placeable() {
			if m.Node != node && !absentNow(state, m.Node) {
				others++
			}
		}
		if others == 0 {
			return state, fmt.Errorf("%w: %s", ErrNothingPlaceable, node)
		}
	}
	next := state.Clone()
	if next.TakenOut == nil {
		next.TakenOut = map[string]objstore.Gesture{}
	}
	next.TakenOut[node] = objstore.Gesture{By: by, Reason: reason, At: now.UTC()}
	setMember(&next.Map, node, func(m *placement.Member) { m.Out = true })
	rebalance(&next, state.Map.Epoch)
	return next, nil
}

// In puts a member back: the map places on it again, and its share moves back.
//
// IT VOUCHES FOR THE NODE, whatever is keeping it off the map: an operator's
// out, a probation the maintainer is counting, or a removal the map
// remembers. A member out or on probation is placed on at once; a node
// removed and not seen since is FORGOTTEN, so it joins — placeable — the next
// time it is seen present and healthy, rather than after it has proven itself
// stable: the operator vouching for it in place of the ticks.
//
// PUTTING BACK A MEMBER ALREADY PLACED ON CHANGES NOTHING: the answer is the
// state it was given, so a re-sent `in` writes nothing.
func In(state objstore.MapState, node string) (objstore.MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	member, isMember := state.Map.Member(node)
	_, removed := state.Removed[node]
	switch {
	case !isMember && !removed:
		return state, fmt.Errorf("%w: %q", ErrUnknownMember, node)
	case isMember && member.Placeable():
		return state, nil
	}
	next := state.Clone()
	delete(next.Removed, node)
	delete(next.TakenOut, node)
	tidy(&next)
	if !isMember {
		return next, nil
	}
	setMember(&next.Map, node, func(m *placement.Member) { m.Out, m.Probation = false, false })
	rebalance(&next, state.Map.Epoch)
	return next, nil
}

// absentNow reports whether the latest tick counted node absent: its run is
// open AND no tick has seen it present since the one that last counted it.
//
// AN OPEN RUN IS NOT AN ABSENCE. A member back from one missed tick keeps its
// run open for a whole [StableTicks] while it proves itself stable — placed
// on, holding its lease, serving every chunk — so reading the run alone as
// "gone" called a member absent for up to ten minutes after it had returned,
// and refused taking out a second member beside it for all of that time. The
// tick that counts a member absent zeroes [objstore.Absence.Present], and the
// first that sees it back raises it, so Present is exactly the latest tick's
// verdict.
func absentNow(state objstore.MapState, node string) bool {
	run, open := state.Absence[node]
	return open && run.Present == 0
}

// setMember changes one member of a map in place.
func setMember(m *objplacement.Map, node string, change func(*placement.Member)) {
	for i := range m.Members {
		if m.Members[i].Node == node {
			change(&m.Members[i])
		}
	}
}

// HoldFor holds the map for d FROM NOW: no member is removed however long it
// is gone, until the hold expires or is released — while its absence keeps
// being counted, so a member still gone when the hold ends is removed on the
// next tick. It changes no objplacement. A member on probation is the exception
// ([objstore.Hold]): it has no share for the hold to keep, and leaves the map
// the tick it is not seen.
//
// IT ALWAYS WRITES A NEW HOLD, ending d after the call — replacing any hold
// already placed, and its who, why and when with it. So a hold sent again
// EXTENDS the one in force to d past the resend: an operator retrying a hold
// whose answer was lost holds the map until d after the retry, not d after
// the first send. That is deliberate rather than an accident of a retry: a
// hold is the operator stating, now, how much longer the maintenance needs,
// and the newest statement is the one that should stand — a shorter one
// included, which ends the hold sooner. Unlike [Out] and [In], a repeat is
// therefore never a no-op, and a surface telling an operator a re-send is
// harmless must say that it restarts the hold.
func HoldFor(state objstore.MapState, d time.Duration, by, reason string, now time.Time) (objstore.MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	if d <= 0 || d > MaxHold {
		return state, fmt.Errorf("%w: asked for %s", ErrHoldRange, d)
	}
	now = now.UTC()
	next := state.Clone()
	next.Hold = &objstore.Hold{Until: now.Add(d), By: by, Reason: reason, At: now}
	return next, nil
}

// Release ends a hold, if there is one. It changes no placement, and a map
// with no hold is answered as it was given, so a re-sent release writes
// nothing.
func Release(state objstore.MapState) objstore.MapState {
	if state.Hold == nil {
		return state
	}
	next := state.Clone()
	next.Hold = nil
	return next
}

// MapStore is what the maintainer needs from the coordination store.
type MapStore interface {
	ObjectMap(ctx context.Context) (coord.ObjectMapRecord, bool, error)
	CreateObjectMap(ctx context.Context, value []byte) (coord.ObjectMapRecord, bool, error)
	UpdateObjectMap(ctx context.Context, value []byte, version uint64) (coord.ObjectMapRecord, bool, error)
}

// Observer is told every map the maintainer wrote, so its own node places by
// it at once.
type Observer interface {
	Observe(state objstore.MapState, version uint64)
}

// MaintainerOptions are a maintainer's dependencies.
type MaintainerOptions struct {
	Store MapStore

	// Live answers every live data node offering to hold objects.
	Live func(ctx context.Context) ([]Presence, error)

	// Company answers what the company configuration says, and false
	// while this node has none — then a tick sets neither the replica
	// count nor the failure domain, and writes no first map.
	Company func() (Company, bool)

	// Observer, when set, is told every map written.
	Observer Observer

	// Now stamps what the record shows an operator — when an absence
	// began, when a member was removed — and is what a hold expires
	// against. Injected for tests.
	Now func() time.Time
}

// Maintainer keeps the stored map in step with the fleet. A FLEET SINGLETON:
// the caller runs [Maintainer.Tick] only while it holds the duty, once per
// [TickInterval] after a tick that found a map, and every write is a
// compare-and-set, so a tick that lost the duty mid-write loses the race
// rather than overwriting its successor.
type Maintainer struct {
	opts MaintainerOptions
}

// NewMaintainer builds a maintainer.
func NewMaintainer(opts MaintainerOptions) (*Maintainer, error) {
	if opts.Store == nil || opts.Live == nil || opts.Company == nil {
		return nil, errors.New("objstore/upkeep: a maintainer needs a store, a roster and the company")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Maintainer{opts: opts}, nil
}

// errNewerMap is a stored map this build cannot rewrite, which it must not
// overwrite: a newer build wrote it, and a map rewritten in an older shape
// would drop whatever that build added.
var errNewerMap = errors.New("objstore/upkeep: the stored map is one this build cannot rewrite")

// TickResult is what one tick found, for the caller to pace the next by.
type TickResult struct {
	// Mapped is whether the stored map existed when the tick read it —
	// the STORE's answer, never a cache's — and so whether the tick counted
	// one tick of every open absence against it. The next tick must then
	// come no sooner than [TickInterval], whether or not this one's write
	// landed: a write that answered an error may still have.
	//
	// False for a tick that found no map, which counted nothing, and for
	// one that failed before it read the store, which counted nothing
	// either; its error says which.
	Mapped bool
}

// Tick brings the stored map up to date, once — and counts one tick of every
// open absence, which is why a tick that found a map ([TickResult.Mapped])
// must be followed by the next only after [TickInterval]. A tick that found
// none has nothing to count, so the caller may tick again sooner, and should:
// until there is a map no node can store a file.
//
// THE CALLER PACES ON THIS RESULT AND ON NOTHING ELSE. Its node's cache of the
// map is a different reading — refreshed on its own interval, and free to lag
// or refuse a map this tick found — and a pace keyed to it would count every
// absence at the no-map poll for as long as the two disagreed: fifteen times
// too fast, a member removed forty seconds after it went quiet.
func (m *Maintainer) Tick(ctx context.Context) (TickResult, error) {
	live, err := m.opts.Live(ctx)
	if err != nil {
		return TickResult{}, fmt.Errorf("objstore/upkeep: read the live data nodes: %w", err)
	}
	company := m.company(ctx)
	rec, found, err := m.opts.Store.ObjectMap(ctx)
	if err != nil {
		return TickResult{}, fmt.Errorf("objstore/upkeep: read the placement map: %w", err)
	}
	res := TickResult{Mapped: found}
	var state objstore.MapState
	if found {
		if state, err = objstore.DecodeMapStateForUpdate(rec.Value); err != nil {
			return res, fmt.Errorf("%w: %w", errNewerMap, err)
		}
		if m.opts.Observer != nil {
			m.opts.Observer.Observe(state, rec.Version)
		}
	}
	next, changed := Next(state, live, company, m.opts.Now().UTC())
	if !changed {
		return res, nil
	}
	raw, err := next.Encode()
	if err != nil {
		return res, err
	}
	var wrote coord.ObjectMapRecord
	var won bool
	if found {
		wrote, won, err = m.opts.Store.UpdateObjectMap(ctx, raw, rec.Version)
	} else {
		wrote, won, err = m.opts.Store.CreateObjectMap(ctx, raw)
	}
	if err != nil {
		return res, fmt.Errorf("objstore/upkeep: write the placement map: %w", err)
	}
	if !won {
		// A LOST RACE: another holder wrote first. The next tick reads
		// what it wrote and starts from there — this tick's counts are
		// lost with it, which delays a removal by a tick and never
		// hastens one.
		return res, nil
	}
	logChanges(ctx, state, next)
	if m.opts.Observer != nil {
		m.opts.Observer.Observe(next, wrote.Version)
	}
	return res, nil
}

// company is the company to apply this tick: none when this node has none, or
// when what it has cannot be applied — which is logged, because it is a
// configuration somebody has to fix.
func (m *Maintainer) company(ctx context.Context) Company {
	c, ok := m.opts.Company()
	if !ok {
		return Company{}
	}
	if err := c.Validate(); err != nil {
		log.WarnContext(ctx, "object_map_company_refused", "error", err,
			"detail", "the placement map keeps the replica count and failure domain it has")
		return Company{}
	}
	return c
}

// logChanges says what a written tick changed.
func logChanges(ctx context.Context, before, after objstore.MapState) {
	for _, m := range after.Map.Members {
		was, member := before.Map.Member(m.Node)
		switch {
		case !member && m.Probation:
			log.InfoContext(ctx, "object_member_probation", "node", m.Node, "weight", m.Weight,
				"domain", m.Domain, "placed_after_ticks", StableTicks-after.Removed[m.Node].Present,
				"detail", "a node removed for absence is back: readers and repairs find what "+
					"it holds, and the map places on it once it has stayed present and healthy")
		case !member:
			log.InfoContext(ctx, "object_member_added", "node", m.Node, "weight", m.Weight,
				"domain", m.Domain)
		case was.Probation && !m.Probation:
			log.InfoContext(ctx, "object_member_trusted", "node", m.Node,
				"detail", "back from removal and present long enough: the map places on it again")
		}
	}
	for _, m := range before.Map.Members {
		if after.Map.Holds(m.Node) {
			continue
		}
		removal := after.Removed[m.Node]
		if m.Probation {
			log.WarnContext(ctx, "object_member_removed", "node", m.Node,
				"reason", string(removal.Reason), "detail", removal.Detail, "on_probation", true)
			continue
		}
		log.WarnContext(ctx, "object_member_removed", "node", m.Node,
			"reason", string(removal.Reason), "detail", removal.Detail,
			"absent_ticks", before.Absence[m.Node].Ticks+1)
	}
	for _, node := range slices.Sorted(maps.Keys(after.Absence)) {
		if _, was := before.Absence[node]; !was {
			run := after.Absence[node]
			log.WarnContext(ctx, "object_member_absent", "node", node,
				"reason", string(run.Reason), "detail", run.Detail, "out_after_ticks", OutTicks)
		}
	}
	for _, node := range slices.Sorted(maps.Keys(before.Absence)) {
		if _, still := after.Absence[node]; !still && after.Map.Holds(node) {
			log.InfoContext(ctx, "object_member_returned", "node", node,
				"present_ticks", StableTicks)
		}
	}
	m := after.Map
	switch {
	case m.Epoch != before.Map.Epoch:
		attrs := []any{"epoch", m.Epoch, "generation", m.Generation.String(),
			"members", len(m.Members), "placeable", len(m.Placeable()),
			"replicas", m.Replicas, "copies", m.Size(), "pg_bits", m.PGBits,
			"failure_domain", m.FailureDomain, "domain_limited", m.DomainLimited()}
		if after.Balance.Epoch == m.Epoch {
			attrs = append(attrs, "balance_rounds", after.Balance.Rounds,
				"balance_deviation", after.Balance.Deviation,
				"balance_converged", after.Balance.Converged)
		}
		log.InfoContext(ctx, "object_map_changed", attrs...)
	case after.Balance != before.Balance:
		log.InfoContext(ctx, "object_map_measured", "epoch", m.Epoch,
			"deviation", after.Balance.Deviation, "converged", after.Balance.Converged)
	}
}
