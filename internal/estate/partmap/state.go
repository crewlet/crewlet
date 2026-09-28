package partmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
)

// MapState is the estate map as the coordination store holds it: the map every
// node routes and joins by, and what the duty maintaining it has to remember
// between ticks — who is in the map and how that is changing
// ([membership.State], embedded, so its fields sit beside the map in the
// stored record) — and the last balance of the members' shares.
//
// Everything a tick has to remember rides in the record rather than in the
// duty holder's memory, and absences are counted in the maintainer's own
// ticks rather than timed on its clock: internal/membership's package doc has
// why, for this map and the object map alike.
type MapState struct {
	Map Map `json:"map"`

	membership.State

	// Balance is the last balance of the members' shares.
	Balance Balance `json:"balance"`
}

// Balance is the last balance of the members' shares against their weights,
// and the tolerance it was asked for — the finest the layout's partition count
// promises for the fleet it balanced ([placement.Draw.Reachable]).
//
// CONVERGED FALSE IS A MEASUREMENT, NEVER A FAULT: asked for what the count
// can promise a balance converges, and one that did not still answers the
// closest layout it measured — no worse than the one it started from. Nothing
// alarms on it.
type Balance struct {
	Tolerance float64 `json:"tolerance"`
	placement.BalanceReport
}

// Clone is a copy sharing nothing with s, so a function building the next
// state from it never writes into a record a caller still holds.
func (s MapState) Clone() MapState {
	out := s
	out.Map = s.Map.Clone()
	out.State = s.State.Clone()
	return out
}

// DecodeMapState reads a stored map to route and join by, refusing one nobody
// may place by.
//
// FIELDS THIS BUILD DOES NOT KNOW ARE IGNORED: a newer build may add to the
// record during a rolling upgrade, and a reader that stopped routing because
// of it would stop every read of a partition its node does not hold for the
// length of the upgrade. A WRITER must not do the same — see
// [DecodeMapStateForUpdate].
func DecodeMapState(raw []byte) (MapState, error) {
	var s MapState
	if err := json.Unmarshal(raw, &s); err != nil {
		return MapState{}, fmt.Errorf("estate/partmap: decode the estate map: %w", err)
	}
	if err := s.Map.Validate(); err != nil {
		return MapState{}, fmt.Errorf("estate/partmap: the stored estate map: %w", err)
	}
	return s, nil
}

// DecodeMapStateForUpdate reads a stored map this node means to write the next
// version of, refusing — beyond everything [DecodeMapState] refuses — one that
// carries a field this build does not know.
//
// A newer build wrote such a map, and rewritten in this build's shape it would
// silently lose whatever that build added: a holder state it routes by, the
// record of an operator's gesture. Refusing leaves the map as the newer build
// wrote it until a node that can read all of it makes the change, which the
// end of the upgrade guarantees.
func DecodeMapStateForUpdate(raw []byte) (MapState, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s MapState
	if err := dec.Decode(&s); err != nil {
		return MapState{}, fmt.Errorf("estate/partmap: decode the estate map to update it: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return MapState{}, errors.New("estate/partmap: decode the estate map to update it: " +
			"it has data after the record")
	}
	if err := s.Map.Validate(); err != nil {
		return MapState{}, fmt.Errorf("estate/partmap: the stored estate map: %w", err)
	}
	return s, nil
}

// Validate refuses a record whose map nobody may place by, or whose memory of
// the removed disagrees with the map about who is on probation
// ([membership.State.Validate]).
//
// ON THE WRITE ONLY ([MapState.Encode]), for the object map's reason: a reader
// places by the map alone, and the maintainer's next tick derives the
// probation flag from the record again.
func (s MapState) Validate() error {
	if err := s.Map.Validate(); err != nil {
		return err
	}
	return s.State.Validate(s.Map.Members)
}

// Encode is the stored form.
func (s MapState) Encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("estate/partmap: refuse to store the estate map: %w", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("estate/partmap: encode the estate map: %w", err)
	}
	return raw, nil
}

// sameState reports whether two records would be stored as the same bytes —
// the question "is there anything to write", asked of every field at once, so
// a field added later is not one a hand-written comparison forgot.
func sameState(a, b MapState) bool {
	encode := func(s MapState) ([]byte, error) {
		if s.Map.Members == nil {
			s.Map.Members = []placement.Member{}
		}
		return json.Marshal(s)
	}
	ra, errA := encode(a)
	rb, errB := encode(b)
	return errA == nil && errB == nil && bytes.Equal(ra, rb)
}
