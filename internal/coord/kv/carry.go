// Why every row this package writes carries what this build does not know.
//
// A row in a coordination bucket is read and written by every build in the
// fleet, and a rolling upgrade has two of them doing it at once. A row this
// package decodes, changes and writes back — a counter it increments, a budget
// it charges, a channel it counts a message on, a lease it renews, a
// resource's epoch it moves — would, decoded into a struct that has no field
// for a member a newer build added, be written back without it: one CAS by an
// older node and the newer build's fact is gone from the fleet, with nothing
// failing. So every row type declared here holds what it does not know in its
// Extra and writes it back, through [jsoncarry].
//
// EVERY ROW, including the ones only ever written anew — a claim, a ledger
// entry, a follow — because which rows a write reads first is a property of
// the write rather than of the row, and a write that starts reading one must
// not be the one that drops a member. A row carrying nothing encodes byte for
// byte as its struct does (TestEveryRowEncodesAsItAlwaysHas), so a peer
// without the carry reads and writes exactly the bytes it always has.
//
// THE CARRY LIVES ON THE VALUE, so it survives a write that changes a decoded
// row and not one that replaces it: a write built from a caller's arguments
// carries nothing, which is right for a new tenure of a lease or a new
// activation, whose members describe what their own writer decided. And a row
// this package writes from a CONTRACT value — a positions-register row, which
// is the coord type itself, a mailbox record from a [coord.MailboxRecord], a
// secret from a [coord.SecretRecord] — carries only what that value hands it,
// so its carry is the coord type's to hold.

package kv

import "github.com/crewlet/crewlet/internal/jsoncarry"

// MarshalJSON writes [rateRecord.Extra] back beside the members this build
// knows.
func (r rateRecord) MarshalJSON() ([]byte, error) {
	type fields rateRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [rateRecord.Extra].
func (r *rateRecord) UnmarshalJSON(b []byte) error {
	type fields rateRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [ledgerRecord.Extra] back beside the members this build
// knows.
func (r ledgerRecord) MarshalJSON() ([]byte, error) {
	type fields ledgerRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [ledgerRecord.Extra].
func (r *ledgerRecord) UnmarshalJSON(b []byte) error {
	type fields ledgerRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [budgetRecord.Extra] back beside the members this build
// knows.
func (r budgetRecord) MarshalJSON() ([]byte, error) {
	type fields budgetRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [budgetRecord.Extra].
func (r *budgetRecord) UnmarshalJSON(b []byte) error {
	type fields budgetRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [activationRecord.Extra] back beside the members this
// build knows.
func (r activationRecord) MarshalJSON() ([]byte, error) {
	type fields activationRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [activationRecord.Extra].
func (r *activationRecord) UnmarshalJSON(b []byte) error {
	type fields activationRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [payloadRecord.Extra] back beside the members this build
// knows.
func (r payloadRecord) MarshalJSON() ([]byte, error) {
	type fields payloadRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [payloadRecord.Extra].
func (r *payloadRecord) UnmarshalJSON(b []byte) error {
	type fields payloadRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [applyRecord.Extra] back beside the members this build
// knows.
func (r applyRecord) MarshalJSON() ([]byte, error) {
	type fields applyRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [applyRecord.Extra].
func (r *applyRecord) UnmarshalJSON(b []byte) error {
	type fields applyRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [secretRecord.Extra] back beside the members this build
// knows.
func (r secretRecord) MarshalJSON() ([]byte, error) {
	type fields secretRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [secretRecord.Extra].
func (r *secretRecord) UnmarshalJSON(b []byte) error {
	type fields secretRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [channelRecord.Extra] back beside the members this build
// knows.
func (r channelRecord) MarshalJSON() ([]byte, error) {
	type fields channelRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [channelRecord.Extra].
func (r *channelRecord) UnmarshalJSON(b []byte) error {
	type fields channelRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [fireRecord.Extra] back beside the members this build
// knows.
func (r fireRecord) MarshalJSON() ([]byte, error) {
	type fields fireRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [fireRecord.Extra].
func (r *fireRecord) UnmarshalJSON(b []byte) error {
	type fields fireRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [followRecord.Extra] back beside the members this build
// knows.
func (r followRecord) MarshalJSON() ([]byte, error) {
	type fields followRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [followRecord.Extra].
func (r *followRecord) UnmarshalJSON(b []byte) error {
	type fields followRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [mailboxRecord.Extra] back beside the members this build
// knows.
func (r mailboxRecord) MarshalJSON() ([]byte, error) {
	type fields mailboxRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [mailboxRecord.Extra].
func (r *mailboxRecord) UnmarshalJSON(b []byte) error {
	type fields mailboxRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [leaseValue.Extra] back beside the members this build
// knows.
func (v leaseValue) MarshalJSON() ([]byte, error) {
	type fields leaseValue
	return jsoncarry.Marshal(fields(v), v.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [leaseValue.Extra].
func (v *leaseValue) UnmarshalJSON(b []byte) error {
	type fields leaseValue
	return jsoncarry.Unmarshal(b, (*fields)(v), &v.Extra)
}

// MarshalJSON writes [resourceValue.Extra] back beside the members this build
// knows.
func (v resourceValue) MarshalJSON() ([]byte, error) {
	type fields resourceValue
	return jsoncarry.Marshal(fields(v), v.Extra)
}

// UnmarshalJSON keeps every member this build does not know in
// [resourceValue.Extra].
func (v *resourceValue) UnmarshalJSON(b []byte) error {
	type fields resourceValue
	return jsoncarry.Unmarshal(b, (*fields)(v), &v.Extra)
}
