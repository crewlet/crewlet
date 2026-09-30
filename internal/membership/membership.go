// Package membership is who is in a placement map, and how that changes: the
// lifecycle a map's maintainer runs over the data nodes offering to hold what
// the map places, one tick at a time, and the operator's gestures on it.
//
// # One lifecycle for both maps
//
// The object store's map places the bytes of a company's files and the estate
// map places the replicated estate's partitions, and both have to answer the
// same questions about the same data nodes: when a member that went quiet is
// given up on, when one that came back is trusted again, what an operator may
// take out or hold, and what none of that may ever do — remove the last
// member there is to place on. The answers are subtle, each was paid for by a
// failure (below), and two copies of them would drift the way every
// duplicated rule in this tree has (ADR-0008). So they are written once,
// here, as PURE functions over a state value both maps' records embed
// ([State]) and the members a draw is taken over ([placement.Draw]): [Tick]
// is the whole policy of one maintainer tick, and [Out], [In], [HoldFor] and
// [Release] are the gestures. What a map does with the members it is handed —
// balance them, split its groups, converge its holders — is the map's own.
//
// # Why all of it rides in the record
//
// A member is taken out of a map after it has been gone for a grace, and
// "gone for how long" is a fact about a SERIES of observations. The duty that
// makes them moves between nodes on a lease and carries no memory across the
// move, so an absence held in one holder's memory would restart on every move
// — and a fleet whose duty moved more often than the grace would never replace
// a dead member at all. The same is true of every other field of [State]:
// each is read back by whichever node holds the duty next.
//
// # Counted in ticks, never timed
//
// An absence is a count of the maintainer's own ticks ([Absence.Ticks]), not a
// span between two wall-clock readings. The readings would be taken by
// whichever node held the duty at the time, and two nodes' clocks disagree: a
// holder running ahead removes a member early, one running behind late. A tick
// is counted only by the holder that makes it, so a handover gap delays a
// removal — the safe direction — and never skips one ([OutTicks]). The one
// wall-clock instant compared at all is an operator's hold ([Hold.Until]),
// which a person states in wall-clock time and which a few seconds of skew
// cannot matter to.
//
// # What the policy answers
//
// A member gone or failed for a grace is removed — but never onto nothing, so
// a whole tier going dark together leaves the map as it was — and one that
// FLAPS is removed too, because a run of absence is cleared only by a whole
// grace of presence. A removed node seen back is a member again at once but ON
// PROBATION: read from and repaired from, since it may hold the only copy of
// something, and placed on only after the same span of presence, or it would
// be placed on and removed again every cycle ([Removal]). An operator may take
// a member OUT ([Out]) so what it holds moves while it still serves — never
// onto nothing, so an out no other member could rebuild the copies of is
// refused rather than taken as a copy dropped — HOLD the map ([HoldFor])
// through planned maintenance so nothing is removed while nodes restart, and
// BAR a node ([Bar]) — an eviction's record — so it is placed on nothing
// whatever becomes of its membership, until it is put back ([In]).
//
// How many copies a map keeps and which label they are spread across are the
// COMPANY's (ADR-0020), taken from the company configuration stamped with the
// activation it came from ([Company]) and never set back by a holder a
// revision behind — never read off the Tier A of whichever node holds the
// duty, which dropped a fleet to one copy the day a node left at the default
// held it.
//
// # What changes placement
//
// Changing an absence, a hold, or who is remembered as removed changes
// nothing anybody places by: only the members [Tick] returns — who they are,
// their weights, their domains, and whether each is out or on probation — and
// the copies and label do. A map that counts its placement changes (an epoch)
// counts exactly those.
package membership

import (
	"fmt"
	"maps"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/placement"
)

// OutGrace is how long a member may be gone before a map stops placing on it.
//
// TEN MINUTES, the interval Ceph waits before marking a down device out, for
// the same reason: a restart, a reboot and a rolling upgrade's turn at a node
// are all minutes, and removing a member moves its whole share across the
// fleet — traffic that is pure waste for a node about to come back, and that
// competes with everything else the fleet is doing. Past it, the risk that a
// second failure finds something one copy short outweighs the cost of the
// copy. Until then a displaced write keeps its replica count by landing on the
// next member of the ranking, so a short absence costs no durability.
//
// It is never measured as a span of wall-clock time: it is [OutTicks] of the
// maintainer's own ticks.
const OutGrace = 10 * time.Minute

// TickInterval is a maintainer's cadence once its map exists: the caller runs
// one [Tick] per interval while it holds the map's duty, and every tick counts
// one tick of every open absence.
//
// THE RECONCILE INTERVAL, the heartbeat every lease in the fleet is renewed
// on: a node that joins is placed on within one of its own heartbeats, and a
// coarser tick buys nothing but a slower join. The absence arithmetic is
// expressed in ticks derived from it ([OutTicks]), so the grace stays
// [OutGrace] whatever this is.
const TickInterval = coord.ReconcileInterval

// OutTicks is how many ticks must count a member absent before a map removes
// it: [OutGrace] at [TickInterval], forty.
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
// THE GRACE ITSELF, so a map trusts a member back on the same evidence it
// takes to give up on one. A member present for fewer keeps the ticks it has
// accumulated, which is what removes a FLAPPING member: up thirty seconds in
// every few minutes, it is never gone for a whole grace at a stretch, and a
// run cleared by any single sighting kept it placed on for ever while it was
// mostly gone.
const StableTicks = OutTicks

// MaxHold is the longest an operator may hold a map.
//
// A DAY, and a hold always ends. Ceph's noout has no expiry, and its failure
// mode is exactly the one this avoids: a flag set for a maintenance window and
// forgotten pins a dead member in the map — its groups a copy short — until
// someone happens to notice. A day covers any planned maintenance, and a
// longer one is a gesture renewed on purpose.
const MaxHold = 24 * time.Hour

// State is what a map's maintainer has to remember between ticks about who
// is in the map: who is gone and for how long, who it removed and is still
// watching, which activation of the company's configuration set its copies,
// an operator's hold, and who took which member out. A map's stored record
// embeds it beside the map itself — see the package doc for why all of it
// rides there.
type State struct {
	// Absence is each member's current run of absence, for the members
	// with one open.
	Absence map[string]Absence `json:"absence,omitempty"`

	// Removed is each node the map removed for absence and still
	// remembers, so it is not placed on again the moment it next appears —
	// see [Removal]. A removed node seen back is a member again at once,
	// ON PROBATION ([placement.Member.Probation]), and stays here counting
	// its probation until it is trusted: so a node is in both exactly
	// while it is on probation, and [State.Validate] refuses a record that
	// says otherwise.
	Removed map[string]Removal `json:"removed,omitempty"`

	// Config is the company configuration the map's replica count and
	// failure-domain label came from.
	Config ConfigSource `json:"config"`

	// Hold, while it is active ([Hold.Active]), keeps every member that
	// holds a share in the map however long it has been gone — Ceph's
	// noout.
	Hold *Hold `json:"hold,omitempty"`

	// TakenOut records who took each out member out and why. It is the
	// operator's INTENT; [placement.Member.Out] in the map is what nodes
	// place by, and [Tick] derives the one from the other on every tick,
	// so the two never disagree for longer than one.
	//
	// NOT "drained": a drain in this engine is what a node does to itself
	// on its way down, and taking a member out is close to the opposite —
	// the node keeps running and serving while what it holds moves off it.
	TakenOut map[string]Gesture `json:"taken_out,omitempty"`

	// Barred records each node an operator barred from the map ([Bar]) —
	// who, why and when — whether or not it is a member.
	//
	// UNLIKE AN OUT, IT OUTLIVES MEMBERSHIP. An out is about a member, and
	// ends when the member is removed for absence: it did its work, the
	// share has moved. A bar is about the MACHINE — an eviction, the
	// operator's judgement that it is gone and that its copies are fenced
	// off until it is readmitted — and a node an operator evicts is usually
	// one the map has already let go, or soon will. So neither removal nor
	// forgetting lifts it: a barred node seen back joins as a member OUT,
	// placed on nothing, until [In] lifts the bar. Kept only while it was a
	// member, the bar vanished with the removal and a repaired machine
	// restarted under its old id was placed on after its probation, while
	// every log it was placed to serve still gated its writes as evicted.
	Barred map[string]Gesture `json:"barred,omitempty"`
}

// out reports whether node is placed on nothing by an operator's gesture: taken
// out, or barred.
func (s State) out(node string) bool {
	_, taken := s.TakenOut[node]
	_, barred := s.Barred[node]
	return taken || barred
}

// Absence is one member's current run of absence.
//
// A RUN, not a stretch: it is cleared only once the member has been present
// for a whole grace ([Absence.Present] reaching [StableTicks]), and a member
// back for less keeps every tick it has accumulated — so a member up for
// thirty seconds every few minutes is removed in the end, where clearing the
// run on any single sighting kept it placed on for ever while it was mostly
// gone.
type Absence struct {
	// Ticks is how many maintainer ticks have counted this member absent
	// or unhealthy in this run.
	Ticks int `json:"ticks"`

	// Present is how many consecutive ticks have seen it present and
	// healthy since the last one that did not.
	Present int `json:"present,omitempty"`

	// Since is when the run began. FOR DISPLAY ONLY: it was read off
	// whichever node held the duty then, and nothing compares it.
	Since time.Time `json:"since"`

	// Reason is why the latest tick that counted it did, and Detail what
	// the member said about it.
	Reason AbsenceReason `json:"reason"`
	Detail string        `json:"detail,omitempty"`
}

// AbsenceReason is why a tick counted a member absent.
type AbsenceReason string

const (
	// ReasonAbsent is a member with no live membership lease: it is down,
	// or cut off from the coordination store.
	ReasonAbsent AbsenceReason = "absent"

	// ReasonUnhealthy is a member that is up but whose store reports
	// itself failed. It is counted exactly as an absent one, because a
	// node that answers for none of what it holds is, to everybody reading
	// from it, a node that is not there.
	ReasonUnhealthy AbsenceReason = "unhealthy"
)

// Valid reports whether r is a reason this build knows.
func (r AbsenceReason) Valid() bool {
	return r == ReasonAbsent || r == ReasonUnhealthy
}

// Removal is a node a map removed for absence, remembered so that it is not
// placed on again the moment it next appears.
//
// WHY REMEMBER IT AT ALL: a member that flaps — up for a minute in every few —
// is removed once its absences add up, and a map that placed on it again at
// its next sighting would give it a share at once, to be removed again a grace
// later: every cycle re-placing its share twice, and its groups a copy short
// for as long as it was a member, which was nearly always. That is worse than
// never having removed it. So a removed node is placed on again only after it
// has been present and healthy for the span that clears an absence, and is
// forgotten — joining at its next sighting, as any new node would — only
// after it has been continuously gone for a whole grace, which a node that is
// flapping never is.
//
// BUT IT IS A MEMBER AGAIN THE MOMENT IT IS SEEN, on probation: placed on
// nothing, and at the tail of every group's ranking. Readers and repairs find
// copies only through a map's ranking, and a removed node still holds
// whatever it held when it went — for a group whose other holders went with
// it, the only copy there is. Kept out of the map while it proved itself, it
// was a node nobody asked: every object only it held read as lost, and repair
// counted it missing, for the whole span after the node was back and healthy.
// What the rule exists to stop is placing NEW data on a node that may leave
// again, which probation still stops; reading from it never needed stopping.
// A node that leaves while on probation goes back to being removed at once,
// its probation lost, since leaving is exactly what it was being watched for.
type Removal struct {
	// Present is how many consecutive ticks have seen the node present and
	// healthy since the last one that did not — its probation, while it is
	// a member — and Gone is how many consecutive ticks have not. At most
	// one of them is non-zero, and Present is non-zero exactly while the
	// node is a member on probation.
	Present int `json:"present,omitempty"`
	Gone    int `json:"gone,omitempty"`

	// At is when it was last removed — by its absence, or by leaving
	// while on probation — FOR DISPLAY ONLY; Reason and Detail are the
	// absence that removed it.
	At     time.Time     `json:"at"`
	Reason AbsenceReason `json:"reason"`
	Detail string        `json:"detail,omitempty"`
}

// ConfigSource is where a map's replica count and failure-domain label came
// from.
type ConfigSource struct {
	// Epoch is the activation of the company configuration they were read
	// from, and 0 when no company has ever set them. [Tick] takes the
	// values of an activation at least this recent and ignores an older
	// one: a node whose company is a revision behind must not undo what a
	// node on the current one set.
	Epoch uint64 `json:"epoch"`
}

// Copies says where a map's copy count came from, for a refusal that judged
// by it — this package's own and a map's ([placement.Draw.Size] against the
// stored count): the company activation that set it, or none where no
// activation has stamped one yet.
func (c ConfigSource) Copies() string {
	if c.Epoch == 0 {
		return "a copy count no company activation has stamped yet"
	}
	return fmt.Sprintf("the copy count company activation %d set", c.Epoch)
}

// Hold keeps every member in a map, however long it has been gone, until it
// expires or is released: the gesture for planned maintenance, where a node
// going down for twenty minutes is known to be coming back and re-placing its
// share in the meantime is traffic that would have to be undone.
//
// Every member but one ON PROBATION: that one holds no share for a hold to
// save re-placing, and it leaves the map the tick it is not seen, hold or none
// — its probation is the watch for exactly that ([Removal]). When it returns
// it is on probation again at once, as it would have had to be anyway.
type Hold struct {
	// Until is when it ends by itself. A hold always ends — see
	// [MaxHold].
	Until time.Time `json:"until"`

	// By, Reason and At are who placed it, why and when: for the screen
	// an operator reads, never compared.
	By     string    `json:"by"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Active reports whether a hold is in force at now: set, and not yet expired.
// A nil hold is none.
func (h *Hold) Active(now time.Time) bool {
	return h != nil && now.Before(h.Until)
}

// Gesture is who made an operator's change to a map, why, and when — for the
// screen an operator reads, never compared.
type Gesture struct {
	By     string    `json:"by"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Clone is a copy sharing nothing with s, so a function building the next
// state from it never writes into a record a caller still holds.
func (s State) Clone() State {
	out := s
	out.Absence = maps.Clone(s.Absence)
	out.Removed = maps.Clone(s.Removed)
	out.TakenOut = maps.Clone(s.TakenOut)
	out.Barred = maps.Clone(s.Barred)
	if s.Hold != nil {
		hold := *s.Hold
		out.Hold = &hold
	}
	return out
}

// Validate refuses a state that disagrees with a map's members about who is
// on probation: a member is on probation exactly while [State.Removed]
// remembers it. It wraps [placement.ErrInvalid], since a record carrying it
// is a map nobody should write.
//
// FOR A WRITER, never a reader: a reader places by the map alone, and a map
// carrying a probation nothing remembers still places correctly — and the
// maintainer's next tick derives the flag from the record again. What the
// check catches is a WRITER that set one half and not the other, which would
// leave a member placed on nothing with no count ever ending its probation,
// or one placed on with a probation still counting.
func (s State) Validate(members []placement.Member) error {
	for _, m := range members {
		if _, removed := s.Removed[m.Node]; removed != m.Probation {
			return fmt.Errorf("%w: %s is on probation = %v, but the removed nodes "+
				"remembered say %v", placement.ErrInvalid, m.Node, m.Probation, removed)
		}
	}
	return nil
}

// tidy leaves an empty field empty rather than present and blank, so two
// records saying the same thing are one record.
func (s *State) tidy() {
	if len(s.Absence) == 0 {
		s.Absence = nil
	}
	if len(s.Removed) == 0 {
		s.Removed = nil
	}
	if len(s.TakenOut) == 0 {
		s.TakenOut = nil
	}
	if len(s.Barred) == 0 {
		s.Barred = nil
	}
}
