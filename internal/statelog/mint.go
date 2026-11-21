package statelog

import "time"

// A DERIVED id ([DeriveOpID]) carries the instant its unit of work began, so a
// retry reproduces it and the ledger collapses each repeated write. That holds
// only while the ledger can VOUCH for the instant ([Publisher.vouches]): once
// the sweep has passed it, every node answers a new write under it `unknown`,
// for good (a trigger dispatched a month late, a turn resumed a month after it
// parked), and the collapse it was kept for went with the swept rows. So an
// attempt whose inherited instant lies more than [MintHorizon] behind its own
// clock REBASES ([MintAt]), and records the new instant before writing so
// later attempts inherit it. Every attempt is judged at its own clock: an
// instant judged once and reused by a retry days later is the same loss.

// A subject kind swept sooner than the retention (internal/maintenance's
// OpsHorizon.Kinds, such as the identity estate's hour-long sessions) NEVER
// takes a derived id, since any carried instant is soon past its watermark:
// its operations mint at the gesture ([StepOpID] over a sign-in's lineage, or
// [NewOpID]) and are re-asked only within the publisher's resolve budget.

// MintHorizon is how far behind an attempt's own clock the instant it mints
// its derived operation ids at may lie: A DAY SHORT OF [OpsRetention], because
// an attempt is judged once, when it starts, and goes on deciding writes, each
// vouched for only while its instant is within the retention when THAT write
// is decided. The day covers one attempt (bounded by turn_engine.max_iterations
// and its phases' timeouts; a longer wait parks and is judged again), by two
// orders of magnitude. Its cost, a write repeated as a second write by an
// attempt starting in the retention's last day, is far cheaper than losing
// every write, and no margin removes it. coord.RebaseRetention ([OpsRetention])
// must outlive whatever can inherit a rebase; coord cannot import this package,
// so coordtest's TestTheRetentionsOutlastWhatTheyCover holds it, this horizon
// and the retention together.
const MintHorizon = OpsRetention - 24*time.Hour

// MintAt is the instant an attempt running at `at` mints its derived operation
// ids at, given `carried` (what earlier attempts at the same work minted at,
// the work's start until one rebased), and whether it REBASES onto a new
// instant the caller must record (coord.Rebases) before the attempt writes.
// Carried while within [MintHorizon] of the attempt, so a retry's writes
// collapse onto the first copy; otherwise the attempt's own instant truncated
// to the millisecond an id keeps ([OpMintedAt]), so the recorded instant
// equals the one every id carries. The zero instant (a seed whose start cannot
// be read) is behind every horizon, and is rebased.
func MintAt(carried, at time.Time) (time.Time, bool) {
	if at.Sub(carried) > MintHorizon {
		return at.UTC().Truncate(time.Millisecond), true
	}
	return carried, false
}
