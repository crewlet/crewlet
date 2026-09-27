package types

import "github.com/crewlet/crewlet/internal/jsoncarry"

// An object nested inside a payload carries the members this build does not
// know, through [github.com/crewlet/crewlet/internal/jsoncarry] — whose
// package doc is the contract.
//
// # Why the objects inside a body, when the envelope carries already
//
// A payload is flat: a member a newer build added beside a payload's own is
// kept in the envelope's Extra, and re-published with the event. But the
// carry is PER OBJECT. A member a newer build added INSIDE one of a payload's
// objects — a prompt message, a model's part of the spend, a coalesced
// message, the trigger that woke a turn — is decoded into this build's struct
// for that object, and a node that relays the event, or stores it and serves
// it again, would publish it without the member. So every object a payload
// holds carries, and one added to a payload does the same:
// TestEveryObjectInsideAPayloadCarries walks every registered payload's
// members and fails on one that does not.
//
// [Trigger] carries through methods of its own, since it is written as a map
// (types.go).

// MarshalJSON writes [PromptMessage.Extra] back beside the members this build
// knows.
func (m PromptMessage) MarshalJSON() ([]byte, error) {
	type fields PromptMessage
	return jsoncarry.Marshal(fields(m), m.Extra)
}

// UnmarshalJSON keeps every member of the message this build does not know in
// [PromptMessage.Extra].
func (m *PromptMessage) UnmarshalJSON(b []byte) error {
	type fields PromptMessage
	return jsoncarry.Unmarshal(b, (*fields)(m), &m.Extra)
}

// MarshalJSON writes [SubagentNode.Extra] back beside the members this build
// knows.
func (n SubagentNode) MarshalJSON() ([]byte, error) {
	type fields SubagentNode
	return jsoncarry.Marshal(fields(n), n.Extra)
}

// UnmarshalJSON keeps every member of the node this build does not know in
// [SubagentNode.Extra].
func (n *SubagentNode) UnmarshalJSON(b []byte) error {
	type fields SubagentNode
	return jsoncarry.Unmarshal(b, (*fields)(n), &n.Extra)
}

// MarshalJSON writes [ModelSpend.Extra] back beside the members this build
// knows.
func (s ModelSpend) MarshalJSON() ([]byte, error) {
	type fields ModelSpend
	return jsoncarry.Marshal(fields(s), s.Extra)
}

// UnmarshalJSON keeps every member of the spend this build does not know in
// [ModelSpend.Extra].
func (s *ModelSpend) UnmarshalJSON(b []byte) error {
	type fields ModelSpend
	return jsoncarry.Unmarshal(b, (*fields)(s), &s.Extra)
}

// MarshalJSON writes [BudgetMeter.Extra] back beside the members this build
// knows.
func (m BudgetMeter) MarshalJSON() ([]byte, error) {
	type fields BudgetMeter
	return jsoncarry.Marshal(fields(m), m.Extra)
}

// UnmarshalJSON keeps every member of the meter this build does not know in
// [BudgetMeter.Extra].
func (m *BudgetMeter) UnmarshalJSON(b []byte) error {
	type fields BudgetMeter
	return jsoncarry.Unmarshal(b, (*fields)(m), &m.Extra)
}

// MarshalJSON writes [CanonicalIdentity.Extra] back beside the members this
// build knows.
func (c CanonicalIdentity) MarshalJSON() ([]byte, error) {
	type fields CanonicalIdentity
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the identity this build does not know in
// [CanonicalIdentity.Extra].
func (c *CanonicalIdentity) UnmarshalJSON(b []byte) error {
	type fields CanonicalIdentity
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [InboundInteraction.Extra] back beside the members this
// build knows.
func (i InboundInteraction) MarshalJSON() ([]byte, error) {
	type fields InboundInteraction
	return jsoncarry.Marshal(fields(i), i.Extra)
}

// UnmarshalJSON keeps every member of the interaction this build does not know
// in [InboundInteraction.Extra].
func (i *InboundInteraction) UnmarshalJSON(b []byte) error {
	type fields InboundInteraction
	return jsoncarry.Unmarshal(b, (*fields)(i), &i.Extra)
}

// MarshalJSON writes [CoalescedMessage.Extra] back beside the members this
// build knows.
func (m CoalescedMessage) MarshalJSON() ([]byte, error) {
	type fields CoalescedMessage
	return jsoncarry.Marshal(fields(m), m.Extra)
}

// UnmarshalJSON keeps every member of the message this build does not know in
// [CoalescedMessage.Extra].
func (m *CoalescedMessage) UnmarshalJSON(b []byte) error {
	type fields CoalescedMessage
	return jsoncarry.Unmarshal(b, (*fields)(m), &m.Extra)
}
