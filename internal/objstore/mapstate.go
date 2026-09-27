package objstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/objstore/placement"
)

// MapState is the placement map as the coordination store holds it: the map
// every node places by, and what the duty maintaining it has to remember
// between ticks — who is gone and for how long, which activation of the
// company's configuration the map's copies were set by, an operator's hold,
// and who took which member out.
//
// # Why all of it rides in the record
//
// A member is taken out of the map after it has been gone for a grace, and
// "gone for how long" is a fact about a SERIES of observations. The duty that
// makes them moves between nodes on a lease and carries no memory across the
// move, so an absence held in one holder's memory would restart on every move
// — and a fleet whose duty moved more often than the grace would never replace
// a dead member at all. The same is true of every other field here: each is
// read back by whichever node holds the duty next.
//
// # Counted in ticks, never timed
//
// An absence is a count of the maintainer's own ticks ([Absence.Ticks]), not a
// span between two wall-clock readings. The readings would be taken by
// whichever node held the duty at the time, and two nodes' clocks disagree: a
// holder running ahead removes a member early, one running behind late. A tick
// is counted only by the holder that makes it, so a handover gap delays a
// removal — the safe direction — and never skips one. The one wall-clock
// instant compared at all is an operator's hold ([Hold.Until]), which a person
// states in wall-clock time and which a few seconds of skew cannot matter to.
//
// # What moves the epoch
//
// Changing an absence, a hold, a measurement or who is remembered as removed
// changes nothing anybody places by, so none of them moves
// [placement.Map.Epoch]: the epoch counts PLACEMENT changes, which is what a
// node comparing two maps needs to know.
type MapState struct {
	Map placement.Map `json:"map"`

	// Absence is each member's current run of absence, for the members
	// with one open.
	Absence map[string]Absence `json:"absence,omitempty"`

	// Removed is each node the map removed for absence and still
	// remembers, so it is not placed on again the moment it next appears —
	// see [Removal]. A removed node seen back is a member again at once,
	// ON PROBATION ([placement.Member.Probation]), and stays here counting
	// its probation until it is trusted: so a node is in both exactly
	// while it is on probation, and [MapState.Validate] refuses a record
	// that says otherwise.
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
	// place by, and the maintainer derives the one from the other on every
	// tick, so the two never disagree for longer than one.
	//
	// NOT "drained": a drain in this engine is what a node does to itself
	// on its way down, and taking a member out is close to the opposite —
	// the node keeps running and serving while its data moves off it.
	TakenOut map[string]Gesture `json:"taken_out,omitempty"`

	// Balance is the last measurement of the members' shares against
	// their weights.
	Balance Balance `json:"balance"`
}

// Absence is one member's current run of absence.
//
// A RUN, not a stretch: it is cleared only once the member has been present
// for a whole grace ([Absence.Present] reaching the maintainer's stable span),
// and a member back for less keeps every tick it has accumulated — so a member
// up for thirty seconds every few minutes is removed in the end, where
// clearing the run on any single sighting kept it placed on for ever while it
// was mostly gone.
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
	// ReasonAbsent is a member with no live objects lease: it is down, or
	// cut off from the coordination store.
	ReasonAbsent AbsenceReason = "absent"

	// ReasonUnhealthy is a member that is up but whose object store
	// reports itself failed. It is counted exactly as an absent one,
	// because a node that answers for none of its chunks is, to everybody
	// reading them, a node that is not there.
	ReasonUnhealthy AbsenceReason = "unhealthy"
)

// Valid reports whether r is a reason this build knows.
func (r AbsenceReason) Valid() bool {
	return r == ReasonAbsent || r == ReasonUnhealthy
}

// Removal is a node the map removed for absence, remembered so that it is not
// placed on again the moment it next appears.
//
// WHY REMEMBER IT AT ALL: a member that flaps — up for a minute in every few —
// is removed once its absences add up, and a map that placed on it again at
// its next sighting would give it a share of the data at once, to be removed
// again a grace later: every cycle re-placing its share twice, and its groups
// a copy short for as long as it was a member, which was nearly always. That
// is worse than never having removed it. So a removed node is placed on again
// only after it has been present and healthy for the span that clears an
// absence, and is forgotten — joining at its next sighting, as any new node
// would — only after it has been continuously gone for a whole grace, which a
// node that is flapping never is.
//
// BUT IT IS A MEMBER AGAIN THE MOMENT IT IS SEEN, on probation: placed on
// nothing, and at the tail of every group's ranking. Readers and repairs find
// copies only through the map's ranking, and a removed node still holds
// whatever it held when it went — for a group whose other holders went with
// it, the only copy there is. Kept out of the map while it proved itself, it
// was a node nobody asked: every chunk only it held read as lost, and repair
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

// ConfigSource is where the map's replica count and failure-domain label came
// from.
type ConfigSource struct {
	// Epoch is the activation of the company configuration they were read
	// from, and 0 when no company has ever set them. The maintainer takes
	// the values of an activation at least this recent and ignores an
	// older one: a node whose company is a revision behind must not undo
	// what a node on the current one set.
	Epoch uint64 `json:"epoch"`
}

// Hold keeps every member in the map, however long it has been gone, until it
// expires or is released: the gesture for planned maintenance, where a node
// going down for twenty minutes is known to be coming back and re-placing its
// share in the meantime is traffic that would have to be undone.
//
// Every member but one ON PROBATION: that one holds no share for a hold to
// save re-placing, and it leaves the map the tick it is not seen, hold or none
// — its probation is the watch for exactly that ([Removal]). When it returns
// it is on probation again at once, as it would have had to be anyway.
type Hold struct {
	// Until is when it ends by itself. A hold always ends — see the
	// maintainer's ceiling on one.
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

// Gesture is who made an operator's change to the map, why, and when — for the
// screen an operator reads, never compared.
type Gesture struct {
	By     string    `json:"by"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Balance is the last measurement of the members' shares against their
// weights — by a balance that set them ([placement.Balance]), or by a layout
// that found them close enough to leave alone.
type Balance struct {
	// Epoch is the map epoch whose placement was measured. A map whose own
	// epoch differs has changed placement without being measured — the
	// epoch that split its groups, which deliberately does not balance —
	// and the maintainer measures it on its next tick.
	Epoch uint64 `json:"epoch"`

	placement.BalanceReport
}

// Clone is a copy sharing nothing with s, so a function building the next
// state from it never writes into a record a caller still holds.
func (s MapState) Clone() MapState {
	out := s
	out.Map.Members = slices.Clone(s.Map.Members)
	out.Absence = maps.Clone(s.Absence)
	out.Removed = maps.Clone(s.Removed)
	out.TakenOut = maps.Clone(s.TakenOut)
	if s.Hold != nil {
		hold := *s.Hold
		out.Hold = &hold
	}
	return out
}

// DecodeMapState reads a stored map to place by, refusing one this package
// cannot place by.
//
// FIELDS THIS BUILD DOES NOT KNOW ARE IGNORED: a newer build may add to the
// record during a rolling upgrade, and a reader that stopped placing because
// of it would stop every upload on its node for the length of the upgrade. A
// WRITER must not do the same — see [DecodeMapStateForUpdate].
func DecodeMapState(raw []byte) (MapState, error) {
	var s MapState
	if err := json.Unmarshal(raw, &s); err != nil {
		return MapState{}, fmt.Errorf("objstore: decode the placement map: %w", err)
	}
	if err := s.Map.Validate(); err != nil {
		return MapState{}, fmt.Errorf("objstore: the stored placement map: %w", err)
	}
	return s, nil
}

// DecodeMapStateForUpdate reads a stored map this node means to write the next
// version of, refusing — beyond everything [DecodeMapState] refuses — one that
// carries a field this build does not know.
//
// A newer build wrote such a map, and rewritten in this build's shape it would
// silently lose whatever that build added: a flag it places by, the record of
// an operator's gesture. Refusing leaves the map as the newer build wrote it
// until a node that can read all of it makes the change, which the end of the
// upgrade guarantees.
func DecodeMapStateForUpdate(raw []byte) (MapState, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s MapState
	if err := dec.Decode(&s); err != nil {
		return MapState{}, fmt.Errorf("objstore: decode the placement map to update it: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return MapState{}, errors.New("objstore: decode the placement map to update it: " +
			"it has data after the record")
	}
	if err := s.Map.Validate(); err != nil {
		return MapState{}, fmt.Errorf("objstore: the stored placement map: %w", err)
	}
	return s, nil
}

// Validate refuses a record whose map cannot place, or whose memory of the
// removed disagrees with the map about who is on probation: a member is on
// probation exactly while [MapState.Removed] remembers it.
//
// ON THE WRITE ONLY ([MapState.Encode]), never on a read: a reader places by
// the map alone, and a map carrying a probation nothing remembers still
// places correctly — and the maintainer's next tick derives the flag from the
// record again. What the check catches is a WRITER that set one half and not
// the other, which would leave a member placed on nothing with no count ever
// ending its probation, or one placed on with a probation still counting.
func (s MapState) Validate() error {
	if err := s.Map.Validate(); err != nil {
		return err
	}
	for _, m := range s.Map.Members {
		if _, removed := s.Removed[m.Node]; removed != m.Probation {
			return fmt.Errorf("%w: %s is on probation = %v, but the removed nodes "+
				"remembered say %v", placement.ErrInvalid, m.Node, m.Probation, removed)
		}
	}
	return nil
}

// Encode is the stored form.
func (s MapState) Encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("objstore: refuse to store the placement map: %w", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("objstore: encode the placement map: %w", err)
	}
	return raw, nil
}
