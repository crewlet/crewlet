package upkeep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/objstore"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/placement"
)

var log = logging.Get("objstore")

// rebalanceAbove is the measured deviation past which a map whose placement
// changed without a balance — the epoch that split its groups — is balanced.
//
// TWICE THE TOLERANCE a balance aims for, as hysteresis: a balance moves
// groups to buy precision, and a map measured just past the tolerance would
// spend an epoch's worth of movement on a percent of evenness, where one past
// twice it has drifted far enough for a disk to notice.
const rebalanceAbove = 2 * placement.DefaultTolerance

// Presence is one live data node as the maintainer sees it: what its objects
// lease says — the membership half every map's lease carries, and the repair
// report only this map reads.
type Presence struct {
	membership.Presence

	// Repair is what its last completed repair pass reported, for the
	// split gate.
	Repair RepairReport
}

// memberships is the membership half of every presence, for the tick.
func memberships(live []Presence) []membership.Presence {
	out := make([]membership.Presence, len(live))
	for i, p := range live {
		out[i] = p.Presence
	}
	return out
}

// byNode is every node offering to hold objects, by node, as the tick counts
// them ([membership.ByNode]) — with each one's repair report.
func byNode(live []Presence) map[string]Presence {
	return membership.ByNode(live, func(p Presence) membership.Presence { return p.Presence })
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

// Next is the map that should follow state, given who is live now and what
// the company says. It is one TICK: every open absence is counted once.
//
// PURE — but for the generation it mints for a first map, which is random by
// design — so every rule is tested without a store. Who is in the map is
// internal/membership's ([membership.Tick]: the company's copies and label,
// returning nodes, membership, absence, removal and out, in that order), and
// this map adds what it places:
//
//   - A first map is written only from a company, and only for a fleet with
//     a member: it would otherwise have to invent the copies every chunk
//     keeps, and an empty one says what none says, at a write per tick. It
//     starts at the group count its fleet needs, since it places no data
//     yet.
//   - Placement: a changed placement is balanced and moves the epoch. An
//     unchanged one never measured since it last changed is measured, and
//     balanced if it has drifted past twice the tolerance. Otherwise, a
//     clean fleet whose group count is short of its target splits one bit —
//     an epoch that changes nothing else.
//
// changed reports whether there is anything to write at all: absences,
// removals remembered, holds and measurements included.
func Next(state objstore.MapState, live []Presence, company membership.Company, now time.Time) (objstore.MapState, bool) {
	first := state.Map.Generation == uuid.Nil
	if first && company.Validate() != nil {
		// NO FIRST MAP WITHOUT A COMPANY: it would have to invent the
		// copies every chunk keeps, and the first file stored would be
		// stored at a count nobody chose.
		return state, false
	}
	ms, drawn := membership.Tick(state.State, state.Map.Draw(), memberships(live), company, now)
	if first && len(drawn.Members) == 0 {
		// NO MAP FOR A FLEET WITH NO MEMBER: an empty one says what
		// none says, at a write per tick.
		return state, false
	}
	t := &placing{before: state, next: state, live: byNode(live)}
	t.next.State = ms
	t.next.Map.Replicas = drawn.Replicas
	t.next.Map.FailureDomain = drawn.FailureDomain
	t.next.Map.Members = drawn.Members
	t.placement(first)
	return t.next, !sameState(state, t.next)
}

// placing is the placement step of one tick, once membership has run.
type placing struct {
	before objstore.MapState
	next   objstore.MapState

	// live is every node offering to hold objects, by node.
	live map[string]Presence
}

// placement is what a changed membership means for what the map places.
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
func (t *placing) placement(first bool) {
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
func (t *placing) measure() {
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
func (t *placing) clean() bool {
	m := t.next.Map
	// ANY OPEN RUN REFUSES, a member back and still proving itself included
	// — deliberately not the question [membership.Out] asks, whether a
	// member is absent NOW. This one is about STABILITY rather than
	// presence: a split re-places half the data, and a member that was gone
	// within the last grace is one that may leave again halfway through
	// copying its new share. A member on PROBATION refuses for the same
	// reason from the other side: the tick that trusts it is a placement
	// change within a grace, and a split taken now would re-place half the
	// data only for the balance that follows it to move a share again.
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

// ErrNoMap is a gesture on a fleet that has no placement map yet. Every other
// refusal is membership's — [membership.ErrUnknownMember],
// [membership.ErrRemovedMember], [membership.ErrNothingPlaceable] and
// [membership.ErrHoldRange] — wrapped in this map's name ([refused]).
var ErrNoMap = errors.New("objstore/upkeep: there is no placement map yet")

// refused is a membership refusal as this map answers it: named for the map,
// so the detail every surface shows says which map refused.
func refused(err error) error { return fmt.Errorf("objstore/upkeep: %w", err) }

// Out takes a member out of the placement map ([membership.Out]): the map
// places nothing on it, so its share moves to the others while it keeps
// serving what it holds — balanced in the epoch the gesture moves. Taking out
// a member already out answers the record it was given.
//
// Pure over the record: the caller reads it, applies this and writes the
// result with a compare-and-set.
func Out(state objstore.MapState, node, by, reason string, now time.Time) (objstore.MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	return gesture(state, func(s membership.State, d placement.Draw) (membership.State, placement.Draw, error) {
		return membership.Out(s, d, node, by, reason, now)
	})
}

// In puts a member back ([membership.In]): the map places on it again, and its
// share moves back in the epoch the gesture moves. A node removed and not
// seen since is forgotten, which changes no placement and moves no epoch.
// Putting back a member already placed on answers the record it was given.
func In(state objstore.MapState, node string) (objstore.MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	return gesture(state, func(s membership.State, d placement.Draw) (membership.State, placement.Draw, error) {
		return membership.In(s, d, node)
	})
}

// gesture applies a membership gesture to the record, and finishes a change
// to what the map places the way every such change is finished here: balanced
// from the shares the map has, in an epoch of its own. A gesture that changed
// nothing placed moves no epoch, and one that changed nothing at all answers
// the record as it was given.
func gesture(state objstore.MapState,
	change func(membership.State, placement.Draw) (membership.State, placement.Draw, error)) (objstore.MapState, error) {

	s, d, err := change(state.State, state.Map.Draw())
	if err != nil {
		return state, refused(err)
	}
	next := state
	next.State = s
	next.Map.Members = d.Members
	if !samePlacement(state.Map, next.Map) {
		rebalance(&next, state.Map.Epoch)
	}
	return next, nil
}

// HoldFor holds the map for d FROM NOW ([membership.HoldFor]): no member is
// removed however long it is gone, until the hold expires or is released. It
// changes no placement. A hold sent again EXTENDS the one in force to d past
// the resend — so, unlike [Out] and [In], a repeat is never a no-op.
func HoldFor(state objstore.MapState, d time.Duration, by, reason string, now time.Time) (objstore.MapState, error) {
	if state.Map.Generation == uuid.Nil {
		return state, ErrNoMap
	}
	s, err := membership.HoldFor(state.State, d, by, reason, now)
	if err != nil {
		return state, refused(err)
	}
	next := state
	next.State = s
	return next, nil
}

// Release ends a hold, if there is one ([membership.Release]). It changes no
// placement, and a map with no hold is answered as it was given, so a re-sent
// release writes nothing.
func Release(state objstore.MapState) objstore.MapState {
	next := state
	next.State = membership.Release(state.State)
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
	Company func() (membership.Company, bool)

	// Observer, when set, is told every map written.
	Observer Observer

	// Now stamps what the record shows an operator — when an absence
	// began, when a member was removed — and is what a hold expires
	// against. Injected for tests.
	Now func() time.Time
}

// Maintainer keeps the stored map in step with the fleet. A FLEET SINGLETON:
// the caller runs [Maintainer.Tick] only while it holds the duty, once per
// [membership.TickInterval] after a tick that found a map, and every write is a
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
	// come no sooner than [membership.TickInterval], whether or not this one's write
	// landed: a write that answered an error may still have.
	//
	// False for a tick that found no map, which counted nothing, and for
	// one that failed before it read the store, which counted nothing
	// either; its error says which.
	Mapped bool
}

// Tick brings the stored map up to date, once — and counts one tick of every
// open absence, which is why a tick that found a map ([TickResult.Mapped])
// must be followed by the next only after [membership.TickInterval]. A tick that found
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
func (m *Maintainer) company(ctx context.Context) membership.Company {
	c, ok := m.opts.Company()
	if !ok {
		return membership.Company{}
	}
	if err := c.Validate(); err != nil {
		log.WarnContext(ctx, "object_map_company_refused", "error", refused(err),
			"detail", "the placement map keeps the replica count and failure domain it has")
		return membership.Company{}
	}
	return c
}

// logChanges says what a written tick changed: who arrived and left, whose
// absence opened and closed ([membership.Diff]), and what the map places.
func logChanges(ctx context.Context, before, after objstore.MapState) {
	changes := membership.Diff(before.State, before.Map.Members, after.State, after.Map.Members)
	for _, a := range changes.Admitted {
		m := a.Member
		switch a.How {
		case membership.OnProbation:
			log.InfoContext(ctx, "object_member_probation", "node", m.Node, "weight", m.Weight,
				"domain", m.Domain, "placed_after_ticks", a.PlacedAfterTicks,
				"detail", "a node removed for absence is back: readers and repairs find what "+
					"it holds, and the map places on it once it has stayed present and healthy")
		case membership.Joined:
			log.InfoContext(ctx, "object_member_added", "node", m.Node, "weight", m.Weight,
				"domain", m.Domain)
		case membership.Trusted:
			log.InfoContext(ctx, "object_member_trusted", "node", m.Node,
				"detail", "back from removal and present long enough: the map places on it again")
		}
	}
	for _, d := range changes.Removed {
		if d.OnProbation {
			log.WarnContext(ctx, "object_member_removed", "node", d.Node,
				"reason", string(d.Removal.Reason), "detail", d.Removal.Detail, "on_probation", true)
			continue
		}
		log.WarnContext(ctx, "object_member_removed", "node", d.Node,
			"reason", string(d.Removal.Reason), "detail", d.Removal.Detail,
			"absent_ticks", d.AbsentTicks)
	}
	for _, o := range changes.Absent {
		log.WarnContext(ctx, "object_member_absent", "node", o.Node,
			"reason", string(o.Absence.Reason), "detail", o.Absence.Detail,
			"out_after_ticks", membership.OutTicks)
	}
	for _, node := range changes.Returned {
		log.InfoContext(ctx, "object_member_returned", "node", node,
			"present_ticks", membership.StableTicks)
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
