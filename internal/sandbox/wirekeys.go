package sandbox

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
)

// A run's record keeps what this build cannot read, AT EVERY LEVEL THAT GROWS.
//
// The record is read, modified and written whole by every node that touches it
// ([PendingRun.Extra]), so a key this build does not know survives a write of
// this build's only where the decode kept it. A key at the top of the record is
// kept in [PendingRun.Extra]. A key INSIDE a section was not: the decode put the
// section into its struct, which dropped every key the struct does not declare,
// and the next status flip, lease, bridged call or note wrote the section back
// without it — so a field a later build added to a note, to where a note was
// read, to the notes section itself or to a bridged call was gone from the
// record within seconds of that build writing it, on a fleet running both
// builds, and nothing said so.
//
// So each section that a later build may add fields to carries an Extra of its
// own, filled by its decode and written back by its encode under the rule the
// record's top follows: [RunSteer], [RunNote], [NoteRead] and [BridgeCall]. A
// section that does not is extended the other way, with a key of its own at
// the top of the record (as [PendingRun.Stop] is, beside [RecordedEnding]).

// wireKeys is every wire key a struct type declares, read off its own json tags
// so a field added to the struct is known the moment it exists — a hand-kept
// list would be one more place a new field could be forgotten, and a forgotten
// one would be carried in Extra AND written from the struct.
func wireKeys(typ reflect.Type) map[string]bool {
	if known, ok := wireKeysCache.Load(typ); ok {
		return known.(map[string]bool)
	}
	keys := map[string]bool{}
	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch {
		case name == "-" || !field.IsExported():
			continue
		case name == "":
			name = field.Name
		}
		keys[name] = true
	}
	known, _ := wireKeysCache.LoadOrStore(typ, keys)
	return known.(map[string]bool)
}

// wireKeysCache holds [wireKeys]' answer per type: a record decodes a section
// per note and per bridged call, every time it is read.
var wireKeysCache sync.Map

// marshalCarrying encodes v — a section's plain twin, the same fields with none
// of its methods — and adds every key of extra the section does not declare.
//
// THE DECLARED FIELDS WIN. Extra holds only what a decode could not place in
// the struct, so a key in both can only mean a caller put it there by hand, and
// the value this build decided is the one that lands. "Declared" is what the
// struct's tags say, never what the marshal happened to emit: a field left at
// its zero value under omitempty emits nothing, and testing presence would let
// a carried key of the same name bring back the value this build just cleared.
func marshalCarrying(v any, declared map[string]bool, extra map[string]json.RawMessage) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return raw, err
	}
	fields := map[string]json.RawMessage{}
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, value := range extra {
		if !declared[key] {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

// unmarshalCarrying decodes raw into v — a section's plain twin — and answers
// every key of raw the section does not declare, nil where there is none. A
// null leaves v as it was and carries nothing, as encoding/json leaves a value
// that has no decoder of its own.
func unmarshalCarrying(raw []byte, v any, declared map[string]bool) (map[string]json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	for key, value := range fields {
		if declared[key] {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[key] = value
	}
	return extra, nil
}

// MarshalJSON writes the section with the keys a later build added to it.
func (s RunSteer) MarshalJSON() ([]byte, error) {
	type plain RunSteer
	return marshalCarrying(plain(s), wireKeys(reflect.TypeFor[RunSteer]()), s.Extra)
}

// UnmarshalJSON reads the section and keeps the keys this build does not know.
func (s *RunSteer) UnmarshalJSON(raw []byte) error {
	type plain RunSteer
	p := plain(*s)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[RunSteer]()))
	if err != nil {
		return err
	}
	*s = RunSteer(p)
	s.Extra = extra
	return nil
}

// MarshalJSON writes the note with the keys a later build added to it.
func (n RunNote) MarshalJSON() ([]byte, error) {
	type plain RunNote
	return marshalCarrying(plain(n), wireKeys(reflect.TypeFor[RunNote]()), n.Extra)
}

// UnmarshalJSON reads the note and keeps the keys this build does not know.
func (n *RunNote) UnmarshalJSON(raw []byte) error {
	type plain RunNote
	p := plain(*n)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[RunNote]()))
	if err != nil {
		return err
	}
	*n = RunNote(p)
	n.Extra = extra
	return nil
}

// MarshalJSON writes the read with the keys a later build added to it.
func (r NoteRead) MarshalJSON() ([]byte, error) {
	type plain NoteRead
	return marshalCarrying(plain(r), wireKeys(reflect.TypeFor[NoteRead]()), r.Extra)
}

// UnmarshalJSON reads the read and keeps the keys this build does not know.
func (r *NoteRead) UnmarshalJSON(raw []byte) error {
	type plain NoteRead
	p := plain(*r)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[NoteRead]()))
	if err != nil {
		return err
	}
	*r = NoteRead(p)
	r.Extra = extra
	return nil
}

// MarshalJSON writes the call with the keys a later build added to it.
func (c BridgeCall) MarshalJSON() ([]byte, error) {
	type plain BridgeCall
	return marshalCarrying(plain(c), wireKeys(reflect.TypeFor[BridgeCall]()), c.Extra)
}

// UnmarshalJSON reads the call and keeps the keys this build does not know.
func (c *BridgeCall) UnmarshalJSON(raw []byte) error {
	type plain BridgeCall
	p := plain(*c)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[BridgeCall]()))
	if err != nil {
		return err
	}
	*c = BridgeCall(p)
	c.Extra = extra
	return nil
}
