package iamdomain

import "slices"

// THE TABLE INVENTORIES. Three readers derive from them and none sees the
// others: the domain's Tables() map (the framework's scrub list, identity claim
// and local sweep), the completeness audit (a replay from zero reproduces every
// one) and the applier, their only writer.

// ReproducibleTables is every table a record's payload must be able to rebuild:
// what a replay from zero into an empty database ends up with. As a list, a
// table added without a payload field to fill it goes red rather than shipping.
//
// The log's own machinery ([MachineryTables]) is absent, and so is the
// framework's checkpoint, which is not this domain's to name.
//
// A replay reproduces CIPHERTEXT: a person's name and address are sealed under
// the fleet keyring before publication, so the identity claim stays a byte
// comparison, not a claim about which nodes hold which keys. A node whose ring
// lost a value's key still rebuilds the row; it just cannot read it.
var ReproducibleTables = []string{
	// The person, with login, address blind and seat as columns: the
	// directory subject keeps them unique (every write that sets one decides
	// from the whole directory), so a table of their own adds a join, no
	// constraint.
	"iam_people",

	// What a person proves themselves with. One row per credential, not per
	// person: a machine holds several, a person a password and a second
	// factor at once.
	"iam_credentials",

	// An address and a human seat spoken for by somebody with no person
	// yet, and its redemption. `seat_id` is written by the apply from the
	// record, and recomputed by the applier's Rederive when a checkpoint's rule set
	// differs (replicated migration 0042).
	"iam_invites",

	// One row per live session lineage. A re-issue is no row (the deadline
	// it moves is inside the signed bearer), so this grows with sign-ins,
	// not requests.
	"iam_sessions",

	// A person's revocation epoch, in its own table rather than a column on
	// iam_people: every request with a session bearer reads it, and beside
	// the sealed document that read would decode a blob for one integer.
	"iam_revocation_epochs",

	// The fleet-wide generation a bearer also carries: one row, which
	// `invalidate` moves to end every session at once. Separate from the
	// epochs because a restore rolls those back to the artefact's instant;
	// this is the one number that moves forward without knowing who was
	// affected.
	"iam_session_generation",

	// The authentication trail, the only table with TWO retention horizons:
	// "who suspended this person" is asked a year later, "who signed in on
	// Tuesday" is not.
	"iam_history",

	// The three GATE tables: state an apply reads before it writes,
	// reproducible because the record installing each is on this log, in
	// order, and every node reaches the same verdict with no clock and no
	// coordination read. They outlive their records: below the trim floor
	// only `iam_removed` says a removal happened, which a snapshot replay
	// reproduces and a write fence reads before publishing at an
	// expectation of zero.
	"iam_evictions", "iam_log_generations", "iam_removed",
}

// MachineryTables are the log's own, excluded from the audit and the identity
// claim.
//
// Not all travel. The deferred record and its scope are this node's verdict on
// bytes its build could not decode, scrubbed from every donated snapshot. The
// operation ledger (iam_ops) travels, classed Divergent by [Domain.Tables]: an
// operation id names one operation fleet-wide, so the donor's rows are the
// ones this node would have written (only `applied_at` differs), and an
// adopter without them cannot tell a first attempt from a retry. See
// [statelog.Domain.OpsTable].
var MachineryTables = []string{
	"iam_ops", "iam_log_deferred", "iam_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool {
	return slices.Contains(ReproducibleTables, table)
}
