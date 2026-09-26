package objstore

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/objstore/placement"
)

// MapState is the placement map as the coordination store holds it: the map
// every node places by, and what the duty maintaining it has to remember
// between ticks.
//
// # Why the absences ride in the record
//
// A member is taken out of the map after it has been gone for a grace, and
// "gone since when" is a fact about a SERIES of observations. The duty that
// makes them moves between nodes on a lease and carries no memory across the
// move, so a first sighting held in one holder's memory would restart the
// grace on every move — and a fleet whose duty moved more often than the
// grace would never replace a dead member at all. Written beside the map, it
// is read back by whichever node holds the duty next.
//
// Changing an absence changes nothing anybody places by, so it does not move
// [placement.Map.Epoch]: the epoch counts PLACEMENT changes, which is what a
// node comparing two maps needs to know.
type MapState struct {
	Map placement.Map `json:"map"`

	// Absent is when each member was first seen without a presence lease,
	// for the members that still have none.
	Absent map[string]time.Time `json:"absent,omitempty"`
}

// DecodeMapState reads a stored map, refusing one this package cannot place
// by.
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

// Encode is the stored form.
func (s MapState) Encode() ([]byte, error) {
	if err := s.Map.Validate(); err != nil {
		return nil, fmt.Errorf("objstore: refuse to store the placement map: %w", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("objstore: encode the placement map: %w", err)
	}
	return raw, nil
}
