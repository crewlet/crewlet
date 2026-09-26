package sandbox

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
)

// # A member this build does not know is carried, at every depth
//
// A run's row is one value in the fleet's coordination store, and every write
// to it is a read-modify-write of the whole: decoded into this build's
// structs, changed, encoded again. A member a newer peer wrote that this build
// has no field for would be erased by the first write this build makes — and
// not only at the top of the row: a member added INSIDE an object the row
// holds (the held answer, one of older builds' bridged calls, the reference to
// a held answer's parts) is decoded into this build's struct for that object
// and gone on the way back out just the same.
//
// So EVERY OBJECT ON THE ROW CARRIES WHAT IT DOES NOT KNOW, through one
// mechanism: its type keeps those members in a field named Extra, tagged
// `json:"-"`, and its MarshalJSON and UnmarshalJSON go through
// [encodeCarrying] and [decodeCarrying]. [PendingRun], [HeldAnswer],
// [HeldParts] and [BridgeCall] do. AN OBJECT ADDED TO THE ROW DOES THE SAME,
// and TestEveryObjectOnARunsRowCarriesWhatItDoesNotKnow walks [PendingRun]'s
// type and fails on a struct on the row that does not: one of this package's
// at any depth, and another package's where the row holds it.

// decodeCarrying decodes raw into fields and returns every member of raw that
// fields' type has no field for, or nil when there is none.
//
// T is the object's FIELDS TYPE — the object's own type redeclared without its
// methods — so that decoding into it does not call the UnmarshalJSON this is
// called from. A nested object on it is still decoded through its own.
func decodeCarrying[T any](raw []byte, fields *T) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(raw, fields); err != nil {
		return nil, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	known := knownMembers(reflect.TypeFor[T]())
	for key := range members {
		if known[strings.ToLower(key)] {
			delete(members, key)
		}
	}
	if len(members) == 0 {
		return nil, nil
	}
	return members, nil
}

// encodeCarrying encodes fields with every member of extra its type has no
// field for.
//
// A MEMBER THE TYPE KNOWS IS NEVER TAKEN FROM EXTRA, even one its own value
// leaves out of the encoding: a field this build cleared — an answer a launch
// dropped — is written absent, and a copy carried beside it would undo the
// clear. [decodeCarrying] files no known member there; this holds whatever a
// caller put in the map.
func encodeCarrying[T any](fields T, extra map[string]json.RawMessage) ([]byte, error) {
	raw, err := json.Marshal(fields)
	if err != nil || len(extra) == 0 {
		return raw, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(raw, &merged); err != nil {
		return nil, err
	}
	known := knownMembers(reflect.TypeFor[T]())
	for key, value := range extra {
		if !known[strings.ToLower(key)] {
			merged[key] = value
		}
	}
	return json.Marshal(merged)
}

// memberSets caches [knownMembers] per type: each set is read off the type
// once and never changes.
var memberSets sync.Map

// knownMembers is the member name of every field a struct type encodes,
// folded to lower case.
//
// CASE-FOLDED, as encoding/json matches a member to a field when it decodes: a
// member it decoded into a field is that field's, and carried beside it as
// well it would be written twice.
//
// READ OFF THE TYPE'S OWN TAGS rather than listed beside it, because the list
// and the struct are one fact: a field added to the struct and missing from a
// list would be carried in Extra as well as in its field, while the tags are
// what the encoder itself writes.
func knownMembers(t reflect.Type) map[string]bool {
	if known, ok := memberSets.Load(t); ok {
		return known.(map[string]bool)
	}
	known := map[string]bool{}
	collectMembers(t, known)
	memberSets.Store(t, known)
	return known
}

func collectMembers(t reflect.Type, into map[string]bool) {
	for i := range t.NumField() {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch {
		case name == "-":
			continue
		case field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct:
			// Promoted, as encoding/json promotes an untagged embedded
			// struct's fields into its parent's object.
			collectMembers(field.Type, into)
			continue
		case !field.IsExported():
			continue
		case name == "":
			name = field.Name
		}
		into[strings.ToLower(name)] = true
	}
}

// pendingRunFields is [PendingRun] without its methods, for the carry.
type pendingRunFields PendingRun

// MarshalJSON encodes the run with every member this build does not know
// ([PendingRun.Extra]).
func (r PendingRun) MarshalJSON() ([]byte, error) {
	return encodeCarrying(pendingRunFields(r), r.Extra)
}

// UnmarshalJSON decodes a run, keeping every member this build does not know
// in [PendingRun.Extra].
func (r *PendingRun) UnmarshalJSON(raw []byte) error {
	var fields pendingRunFields
	extra, err := decodeCarrying(raw, &fields)
	if err != nil {
		return err
	}
	*r = PendingRun(fields)
	r.Extra = extra
	return nil
}

// heldAnswerFields is [HeldAnswer] without its methods, for the carry.
type heldAnswerFields HeldAnswer

// MarshalJSON encodes the held answer with every member this build does not
// know ([HeldAnswer.Extra]).
func (h HeldAnswer) MarshalJSON() ([]byte, error) {
	return encodeCarrying(heldAnswerFields(h), h.Extra)
}

// UnmarshalJSON decodes a held answer, keeping every member this build does
// not know in [HeldAnswer.Extra].
func (h *HeldAnswer) UnmarshalJSON(raw []byte) error {
	var fields heldAnswerFields
	extra, err := decodeCarrying(raw, &fields)
	if err != nil {
		return err
	}
	*h = HeldAnswer(fields)
	h.Extra = extra
	return nil
}

// heldSpendFields is [HeldSpend] without its methods, for the carry.
type heldSpendFields HeldSpend

// MarshalJSON encodes the held spend with every member this build does not
// know ([HeldSpend.Extra]).
func (h HeldSpend) MarshalJSON() ([]byte, error) {
	return encodeCarrying(heldSpendFields(h), h.Extra)
}

// UnmarshalJSON decodes a held spend, keeping every member this build does not
// know in [HeldSpend.Extra].
func (h *HeldSpend) UnmarshalJSON(raw []byte) error {
	var fields heldSpendFields
	extra, err := decodeCarrying(raw, &fields)
	if err != nil {
		return err
	}
	*h = HeldSpend(fields)
	h.Extra = extra
	return nil
}

// heldModelFields is [HeldModel] without its methods, for the carry.
type heldModelFields HeldModel

// MarshalJSON encodes the part with every member this build does not know
// ([HeldModel.Extra]).
func (m HeldModel) MarshalJSON() ([]byte, error) {
	return encodeCarrying(heldModelFields(m), m.Extra)
}

// UnmarshalJSON decodes a part, keeping every member this build does not know
// in [HeldModel.Extra].
func (m *HeldModel) UnmarshalJSON(raw []byte) error {
	var fields heldModelFields
	extra, err := decodeCarrying(raw, &fields)
	if err != nil {
		return err
	}
	*m = HeldModel(fields)
	m.Extra = extra
	return nil
}

// heldPartsFields is [HeldParts] without its methods, for the carry.
type heldPartsFields HeldParts

// MarshalJSON encodes the reference with every member this build does not
// know ([HeldParts.Extra]).
func (p HeldParts) MarshalJSON() ([]byte, error) {
	return encodeCarrying(heldPartsFields(p), p.Extra)
}

// UnmarshalJSON decodes a reference, keeping every member this build does not
// know in [HeldParts.Extra].
func (p *HeldParts) UnmarshalJSON(raw []byte) error {
	var fields heldPartsFields
	extra, err := decodeCarrying(raw, &fields)
	if err != nil {
		return err
	}
	*p = HeldParts(fields)
	p.Extra = extra
	return nil
}

// bridgeCallFields is [BridgeCall] without its methods, for the carry.
type bridgeCallFields BridgeCall

// MarshalJSON encodes the call with every member this build does not know
// ([BridgeCall.Extra]).
func (c BridgeCall) MarshalJSON() ([]byte, error) {
	return encodeCarrying(bridgeCallFields(c), c.Extra)
}

// UnmarshalJSON decodes a call, keeping every member this build does not know
// in [BridgeCall.Extra].
func (c *BridgeCall) UnmarshalJSON(raw []byte) error {
	var fields bridgeCallFields
	extra, err := decodeCarrying(raw, &fields)
	if err != nil {
		return err
	}
	*c = BridgeCall(fields)
	c.Extra = extra
	return nil
}
