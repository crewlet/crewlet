package iamdomain

import "slices"

// THE TABLE INVENTORIES, and what each list is for.
//
// Three readers derive from these and none of them can see the others: the
// domain's own Tables() map, which the framework turns into a scrub list, an
// identity claim and a local sweep; the completeness audit, which asserts a
// replay from zero reproduces every one of them; and the applier, which is the
// only writer of any of them.

// ReproducibleTables is every table a record's payload must be able to rebuild.
//
// THE COMPLETENESS AUDIT AS A LIST, so a table added later without a payload
// field to fill it goes red rather than shipping. It is what a replay from
// zero into an empty database has to end up with.
//
// THREE TABLES ARE DELIBERATELY ABSENT, and they are the log's own machinery
// rather than state a record reproduces: the operation ledger, the deferred
// records and their scope index. The framework's checkpoint is absent for the
// same reason and is not this domain's to name.
//
// WHAT A REPLAY REPRODUCES IS CIPHERTEXT, not cleartext. A person's name and
// address are sealed under the fleet keyring BEFORE publication, so the bytes
// a replay writes are the bytes the writer published — which is what keeps
// the identity claim a byte comparison rather than a claim about which nodes
// hold which keys. A replay on a node whose ring no longer holds the key a
// value was sealed under still rebuilds the row; it just cannot read it.
var ReproducibleTables = []string{
	// The person, with their login, address blind and seat on their row.
	//
	// COLUMNS AND NOT A TABLE OF THEIR OWN, which is the one place this
	// domain looks like it is missing a normalisation and is not. The
	// unique values live on the row; the directory subject is what keeps
	// them unique — every write that sets one decides from the whole
	// directory — so a table would add no constraint, only a join.
	"iam_people",

	// What a person proves themselves with: a password verifier, a second
	// factor, recovery codes, a machine token's verifier. One row per
	// credential rather than one per person, because a machine holds
	// several and a person holds a password and a second factor at once.
	"iam_credentials",

	// An address spoken for by somebody who has no person yet, and the
	// record of its redemption.
	"iam_invites",

	// One row per live session lineage. Rotations are NOT rows — a
	// rotation id is derived — so this table grows with sign-ins rather
	// than with requests.
	"iam_sessions",

	// A person's REVOCATION EPOCH, in a table of its own rather than a
	// column on iam_people, and the reason is the READ: every request
	// carrying a session bearer compares against it, so it is the hottest
	// row in the domain. Keeping it beside a person's sealed document
	// would make the validation path read and decode a blob to learn one
	// integer, on every request, for ever.
	"iam_revocation_epochs",

	// And the FLEET-WIDE generation a bearer also carries: one row about
	// nobody, which `invalidate` moves to end every session in the
	// company at once. It is separate from the epochs above because a
	// restore rolls those back to an artefact's own instant — a
	// revocation taken after the copy was made comes back with it — and
	// this is the only number that can be moved forward without knowing
	// who was affected.
	"iam_session_generation",

	// The authentication trail: who did what to whom, and why. It is the
	// only table here with TWO retention horizons, because a change and a
	// session are different questions — "who suspended this person" is an
	// audit somebody asks a year later, "who signed in on Tuesday" is not.
	"iam_history",

	// THE THREE GATE TABLES, which are not the objects a record writes
	// but the state an apply reads BEFORE it writes anything — and each
	// is reproducible for the same reason its gate is trustworthy: the
	// record that installs it is on this log, in this order, and every
	// node reaches the same verdict from it with no clock and no
	// coordination read.
	//
	// They also OUTLIVE the records that wrote them. A removal below the
	// trim floor has no record left on the log to prove it happened, and
	// `iam_removed` is what still says so — which is what a replay from a
	// snapshot reproduces, and what a write fence reads before publishing
	// at an expectation of zero.
	"iam_evictions", "iam_log_generations", "iam_removed",
}

// MachineryTables are the log's own, excluded from the audit and from the
// identity claim.
//
// THEY DO NOT ALL TRAVEL. The deferred record and its scope are this node's
// own verdict about bytes this build could not decode, and are scrubbed out of
// every donated snapshot. The operation ledger (iam_ops) TRAVELS, and
// [Domain.Tables] classes it Divergent to say so: its rows are what every
// node's applier writes from the same records — only `applied_at`, this node's
// own clock, differs — so the donor's are exactly the ones this node would have
// written, and an adopter without them cannot tell a first attempt from a retry
// of anything the donor applied. It used to be scrubbed as "the sharpest" of
// these, on the reading that a donor's ledger would resolve this node's
// ambiguous publish against somebody else's history; but an operation id names
// one operation fleet-wide, so the donor's row for it IS its history, and
// without the row the adopter answered a turn's first attempt `unknown` or
// decided a landed one again. See [statelog.Domain.OpsTable].
var MachineryTables = []string{
	"iam_ops", "iam_log_deferred", "iam_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool {
	return slices.Contains(ReproducibleTables, table)
}
