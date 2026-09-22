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
// address are sealed under that person's own key BEFORE publication, so the
// bytes a replay writes are the bytes the writer published — which is what
// keeps the identity claim a byte comparison rather than a claim about which
// nodes hold which keys. A replay on a node whose key store has lost the
// person's key still rebuilds the row; it just cannot read it, which is
// exactly what a removal is supposed to leave behind.
var ReproducibleTables = []string{
	// The person, and the three claims denormalised onto their row.
	//
	// THE CLAIMS ARE COLUMNS AND NOT A CLAIMS TABLE, which is the one
	// place this domain looks like it is missing a normalisation and is
	// not. A claim's uniqueness is enforced by its SUBJECT at the broker,
	// so a table would add no constraint; what the columns buy is the
	// DUPLICATE-CLAIM SCAN — three partial indexes over them are how a
	// duty reports two people holding one address, which is a state only a
	// restore or a reanchor can produce and which nothing else in this
	// estate could ever notice.
	"iam_people",

	// What a person proves themselves with: a password verifier, an
	// identity-provider subject, a machine token's verifier. One row per
	// credential rather than one per person, because a machine holds
	// several and a person holds a password and an IdP binding at once.
	"iam_credentials",

	// An address spoken for by somebody who has no person yet, and the
	// record of its redemption.
	"iam_invites",

	// The company's own way in before it has anybody: the codes that turn
	// an engine nobody can sign in to into one with an administrator.
	"iam_bootstrap_codes",

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
// identity claim, and scrubbed out of every donated snapshot.
//
// A DONOR'S OPERATION LEDGER IS THE SHARPEST OF THEM: an adopted peer's ops
// table would let this node resolve its own ambiguous publish against somebody
// else's history, which is a write reported as landed that never happened.
var MachineryTables = []string{
	"iam_ops", "iam_log_deferred", "iam_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool {
	return slices.Contains(ReproducibleTables, table)
}
