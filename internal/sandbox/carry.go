package sandbox

import "github.com/crewlet/crewlet/internal/jsoncarry"

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
// So EVERY OBJECT ON THE ROW CARRIES WHAT IT DOES NOT KNOW, through
// [github.com/crewlet/crewlet/internal/jsoncarry], whose package doc is the
// contract: its type keeps those members in a field named Extra, tagged
// `json:"-"`, and its MarshalJSON and UnmarshalJSON below go through the
// package. AN OBJECT ADDED TO THE ROW DOES THE SAME, and
// TestEveryObjectOnARunsRowCarriesWhatItDoesNotKnow walks [PendingRun]'s type
// and fails on an object on the row that does not.

// MarshalJSON encodes the run with every member this build does not know
// ([PendingRun.Extra]).
func (r PendingRun) MarshalJSON() ([]byte, error) {
	type fields PendingRun
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON decodes the run, keeping every member this build does not know
// in [PendingRun.Extra].
func (r *PendingRun) UnmarshalJSON(raw []byte) error {
	type fields PendingRun
	return jsoncarry.Unmarshal(raw, (*fields)(r), &r.Extra)
}

// MarshalJSON encodes the held answer with every member this build does not
// know ([HeldAnswer.Extra]).
func (h HeldAnswer) MarshalJSON() ([]byte, error) {
	type fields HeldAnswer
	return jsoncarry.Marshal(fields(h), h.Extra)
}

// UnmarshalJSON decodes the held answer, keeping every member this build does
// not know in [HeldAnswer.Extra].
func (h *HeldAnswer) UnmarshalJSON(raw []byte) error {
	type fields HeldAnswer
	return jsoncarry.Unmarshal(raw, (*fields)(h), &h.Extra)
}

// MarshalJSON encodes the held spend with every member this build does not know
// ([HeldSpend.Extra]).
func (h HeldSpend) MarshalJSON() ([]byte, error) {
	type fields HeldSpend
	return jsoncarry.Marshal(fields(h), h.Extra)
}

// UnmarshalJSON decodes the held spend, keeping every member this build does
// not know in [HeldSpend.Extra].
func (h *HeldSpend) UnmarshalJSON(raw []byte) error {
	type fields HeldSpend
	return jsoncarry.Unmarshal(raw, (*fields)(h), &h.Extra)
}

// MarshalJSON encodes the part with every member this build does not know
// ([HeldModel.Extra]).
func (m HeldModel) MarshalJSON() ([]byte, error) {
	type fields HeldModel
	return jsoncarry.Marshal(fields(m), m.Extra)
}

// UnmarshalJSON decodes the part, keeping every member this build does not know
// in [HeldModel.Extra].
func (m *HeldModel) UnmarshalJSON(raw []byte) error {
	type fields HeldModel
	return jsoncarry.Unmarshal(raw, (*fields)(m), &m.Extra)
}

// MarshalJSON encodes the reference with every member this build does not know
// ([HeldParts.Extra]).
func (p HeldParts) MarshalJSON() ([]byte, error) {
	type fields HeldParts
	return jsoncarry.Marshal(fields(p), p.Extra)
}

// UnmarshalJSON decodes the reference, keeping every member this build does not
// know in [HeldParts.Extra].
func (p *HeldParts) UnmarshalJSON(raw []byte) error {
	type fields HeldParts
	return jsoncarry.Unmarshal(raw, (*fields)(p), &p.Extra)
}

// MarshalJSON encodes the call with every member this build does not know
// ([BridgeCall.Extra]).
func (c BridgeCall) MarshalJSON() ([]byte, error) {
	type fields BridgeCall
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON decodes the call, keeping every member this build does not know
// in [BridgeCall.Extra].
func (c *BridgeCall) UnmarshalJSON(raw []byte) error {
	type fields BridgeCall
	return jsoncarry.Unmarshal(raw, (*fields)(c), &c.Extra)
}
