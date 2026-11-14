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
// record's top follows: [RunSteer], [RunNote], [NoteRead], [BridgeCall],
// [ElidedCall], [SessionCapture], [HeldCall], [ConversationBase] with its
// [BaseSession] and each [BaseSibling], and [LaunchRecord] — the job's own
// record, which grows a fact about the job with nearly every piece that touches
// launching, and which every write but a relaunch's hands back. A section that
// does not is extended the other way, with a key of its own at the top of the
// record (as [PendingRun.Stop] is, beside [RecordedEnding]). And the log kept
// beside the record, [BridgeLog], is a value of its own with an Extra of its
// own at its top, under the same rule. So is the handed part kept beside it
// ([handedSet]), and the set it seals ([HandedSecrets]): the attach of each job
// writes its own whole, but every token the completion poll renews in the
// job's box is a read of the part, an open of the set, a change and a write
// back of both ([CoordStore.AddHanded], [Coordinator.addHanded]), on whichever
// node polls the box, every few minutes of a long run. And the leads kept
// beside the record ([leadsPart]), with each [KeptLead] it keeps: every take of
// a job's leads rewrites the part whole, so each carries an Extra of its own,
// under the same rule.

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

// MarshalJSON writes the owed call with the keys a later build added to it.
func (o OwedCall) MarshalJSON() ([]byte, error) {
	type plain OwedCall
	return marshalCarrying(plain(o), wireKeys(reflect.TypeFor[OwedCall]()), o.Extra)
}

// UnmarshalJSON reads the owed call and keeps the keys this build does not
// know.
func (o *OwedCall) UnmarshalJSON(raw []byte) error {
	type plain OwedCall
	p := plain(*o)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[OwedCall]()))
	if err != nil {
		return err
	}
	*o = OwedCall(p)
	o.Extra = extra
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

// MarshalJSON writes the identity with the keys a later build added to it.
func (e ElidedCall) MarshalJSON() ([]byte, error) {
	type plain ElidedCall
	return marshalCarrying(plain(e), wireKeys(reflect.TypeFor[ElidedCall]()), e.Extra)
}

// UnmarshalJSON reads the identity and keeps the keys this build does not
// know.
func (e *ElidedCall) UnmarshalJSON(raw []byte) error {
	type plain ElidedCall
	p := plain(*e)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[ElidedCall]()))
	if err != nil {
		return err
	}
	*e = ElidedCall(p)
	e.Extra = extra
	return nil
}

// MarshalJSON writes the log with the keys a later build added to it.
func (l BridgeLog) MarshalJSON() ([]byte, error) {
	type plain BridgeLog
	return marshalCarrying(plain(l), wireKeys(reflect.TypeFor[BridgeLog]()), l.Extra)
}

// UnmarshalJSON reads the log and keeps the keys this build does not know.
func (l *BridgeLog) UnmarshalJSON(raw []byte) error {
	type plain BridgeLog
	p := plain(*l)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[BridgeLog]()))
	if err != nil {
		return err
	}
	*l = BridgeLog(p)
	l.Extra = extra
	return nil
}

// MarshalJSON writes the part with the keys a later build added to it.
func (h handedSet) MarshalJSON() ([]byte, error) {
	type plain handedSet
	return marshalCarrying(plain(h), wireKeys(reflect.TypeFor[handedSet]()), h.Extra)
}

// UnmarshalJSON reads the part and keeps the keys this build does not know.
func (h *handedSet) UnmarshalJSON(raw []byte) error {
	type plain handedSet
	p := plain(*h)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[handedSet]()))
	if err != nil {
		return err
	}
	*h = handedSet(p)
	h.Extra = extra
	return nil
}

// MarshalJSON writes the set with the keys a later build added to it.
func (h HandedSecrets) MarshalJSON() ([]byte, error) {
	type plain HandedSecrets
	return marshalCarrying(plain(h), wireKeys(reflect.TypeFor[HandedSecrets]()), h.Extra)
}

// UnmarshalJSON reads the set and keeps the keys this build does not know.
func (h *HandedSecrets) UnmarshalJSON(raw []byte) error {
	type plain HandedSecrets
	p := plain(*h)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[HandedSecrets]()))
	if err != nil {
		return err
	}
	*h = HandedSecrets(p)
	h.Extra = extra
	return nil
}

// MarshalJSON writes the job's record with the keys a later build added to it.
func (l LaunchRecord) MarshalJSON() ([]byte, error) {
	type plain LaunchRecord
	return marshalCarrying(plain(l), wireKeys(reflect.TypeFor[LaunchRecord]()), l.Extra)
}

// UnmarshalJSON reads the job's record and keeps the keys this build does not
// know.
func (l *LaunchRecord) UnmarshalJSON(raw []byte) error {
	type plain LaunchRecord
	p := plain(*l)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[LaunchRecord]()))
	if err != nil {
		return err
	}
	*l = LaunchRecord(p)
	l.Extra = extra
	return nil
}
