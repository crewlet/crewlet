package objstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/crewlet/crewlet/internal/membership"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/placement"
)

// MapState is the placement map as the coordination store holds it: the map
// every node places by, and what the duty maintaining it has to remember
// between ticks — who is in the map and how that is changing
// ([membership.State], embedded, so its fields sit beside the map in the
// stored record exactly as they always have), and the last measurement of the
// balance.
//
// Everything a tick has to remember rides in the record rather than in the
// duty holder's memory, and absences are counted in the maintainer's own
// ticks rather than timed on its clock: internal/membership's package doc has
// why, for this map and the estate map alike.
//
// # What moves the epoch
//
// Changing an absence, a hold, a measurement or who is remembered as removed
// changes nothing anybody places by, so none of them moves
// [objplacement.Map.Epoch]: the epoch counts PLACEMENT changes, which is what
// a node comparing two maps needs to know.
type MapState struct {
	Map objplacement.Map `json:"map"`

	membership.State

	// Balance is the last measurement of the members' shares against
	// their weights.
	Balance Balance `json:"balance"`
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
	out.State = s.State.Clone()
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
// removed disagrees with the map about who is on probation
// ([membership.State.Validate]).
//
// ON THE WRITE ONLY ([MapState.Encode]), never on a read: a reader places by
// the map alone, and a map carrying a probation nothing remembers still
// places correctly — and the maintainer's next tick derives the flag from the
// record again.
func (s MapState) Validate() error {
	if err := s.Map.Validate(); err != nil {
		return err
	}
	return s.State.Validate(s.Map.Members)
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
