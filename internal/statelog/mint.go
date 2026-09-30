package statelog

import "time"

// Where a derived operation id's instant comes from once the work it belongs to
// began longer ago than the operation ledger remembers.
//
// A DERIVED id ([DeriveOpID]) carries the instant its unit of work began, and a
// retry reproduces it: a turn re-run after a crash, or the resumed half of a
// turn a coding run parked, derives the ids its first attempt derived, and the
// ledger collapses each repeated write onto the first copy. That holds only
// while the ledger can VOUCH for the instant ([Publisher.vouches]). Every node's
// sweep deletes the rows it applied more than [OpsRetention] ago and records
// how far back it deleted, so an operation minted before that point whose row
// is gone is answered `unknown` and never published — on every node, for good.
// Every NEW write of an attempt that runs that long after its work began is
// such an operation: a trigger dispatched a month after it arrived (a seat
// nobody placed, a fleet that was down), a turn resumed a month after it
// parked. And the collapse the old instant was kept for is gone by then as
// well, because the rows it needed are the ones the sweep took.
//
// So an attempt that finds the instant it inherits more than [MintHorizon]
// behind its own clock REBASES: it mints at its own instant instead, and
// records that instant before it writes anything, so a later attempt at the
// same work — a crash re-run, a failed resume retried, the next half of the
// turn — inherits it rather than the start. [MintAt] is the rule, and it
// judges EVERY attempt against that attempt's own clock: an instant decided by
// one attempt and reused by a retry days later is the same loss again, because
// the retry's writes are decided at the retry.

// MintHorizon is how far behind an attempt's own clock the instant it mints
// its derived operation ids at may lie.
//
// A DAY SHORT OF [OpsRetention], because an attempt is judged once, when it
// starts, and then goes on deciding writes for as long as it runs: its rounds
// and its review. A write is vouched for while the instant its id carries is no
// older than the retention at the moment THAT WRITE is decided — the sweep's
// cutoff trails the clock by the whole retention — so the margin covers the
// span of ONE attempt, from the instant it was judged to its last write. A
// turn's attempt is bounded by its rounds (turn_engine.max_iterations) and the
// timeouts of each phase, and a turn that has to wait longer than that parks and
// is judged again when it resumes: a day is two orders of magnitude past it.
// Measured against the start alone, at the bare retention, an attempt judged
// at twenty-nine days and twenty-three hours kept the start and had every
// write it decided after the next sweep answered `unknown`.
//
// What the day costs is the collapse, for an attempt that starts in the
// retention's last day: a write an earlier attempt made under the old instant
// and this one repeats is a second write. That is the cheaper failure by far —
// the other is every write the attempt makes, lost — and it is the same one a
// retry that happens to cross the line pays whatever the margin, so a smaller
// margin would narrow a window nobody can close without widening the one that
// loses writes.
const MintHorizon = OpsRetention - 24*time.Hour

// MintAt is the instant an attempt running at `at` mints its derived operation
// ids at, given `carried` — the instant the earlier attempts at the same work
// minted theirs at, which is the work's start until an attempt rebased — and
// whether the attempt REBASES, onto a new instant the caller must record
// before the attempt writes anything.
//
// CARRIED WHILE IT IS WITHIN [MintHorizon] OF THE ATTEMPT, so a retry derives
// the ids the attempt before it derived and each repeated write collapses onto
// the first copy.
//
// THE ATTEMPT'S OWN INSTANT ONCE IT IS NOT, truncated to the millisecond an id
// keeps (a UUIDv7's time bits, see [OpMintedAt]), so the instant recorded is
// exactly the one every id carries and a reader comparing the two finds them
// equal. The zero instant — a seed whose start is not known, such as a run id
// an older build minted with no instant in it — is behind every horizon, and
// is rebased.
func MintAt(carried, at time.Time) (time.Time, bool) {
	if at.Sub(carried) > MintHorizon {
		return at.UTC().Truncate(time.Millisecond), true
	}
	return carried, false
}
