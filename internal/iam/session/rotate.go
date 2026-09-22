package session

import (
	"time"

	"github.com/google/uuid"
)

// THE ROTATION IS DERIVED, WHICH IS WHY THERE IS NO ROTATION RECORD.
//
// The index a bearer carries is the session's AGE in rotation windows, and the
// age is the instant inside the lineage's own uuid7. Any ingress node
// recomputes it with zero I/O and nothing is written anywhere — which is the
// whole point: the design this replaced wrote one record per session per hour,
// measured at ten per person per working day and nine tenths of the
// authentication trail, to store a CLOCK in a log.
//
// # The age is wall-clock, and there is no choice about that
//
// The session's start is the instant inside its own uuid7 lineage, written by
// whichever node handled the sign-in. A monotonic reading's epoch is per-boot
// and means nothing anywhere else, so an age computed across two processes has
// to be wall-clock arithmetic on two wall clocks.
//
// WHAT THAT COSTS is stated rather than hidden: a node whose clock is ahead
// computes a larger index than the node that minted the cookie, and one whose
// clock is behind computes a smaller one. [Overlap] is what absorbs the
// ordinary case — two minutes is far beyond any fleet running NTP — and the
// arms below are shaped so that the EXPENSIVE verdict is the one a fast clock
// cannot reach by accident.
//
// # What an index can prove, and what it cannot
//
// This is the decision in this file, and the design it implements stated the
// opposite one, so the reasoning is here in full.
//
// With no rotation record, a node holds two numbers: the index the cookie
// carries and the index the age implies. A cookie whose index is BEHIND the
// age is one that was issued in an earlier window. Two completely different
// things produce exactly that cookie:
//
//   - a person who stopped working at four and came back at seven, whose
//     browser still holds the cookie it was last issued; and
//   - somebody replaying a cookie they captured, while its owner has gone on
//     being re-issued past it.
//
// THE TWO ARE THE SAME BYTES. Nothing derived can separate them, because the
// only thing that could is a record of which index was last issued — which is
// exactly what deriving the index removed. So treating a lagging index as
// reuse does not detect theft with a false-positive rate; it detects IDLENESS,
// and calls the majority of those thefts.
//
// The original rule — an index more than one window plus the overlap behind
// the age is reuse, and reuse bumps the person's revocation epoch — therefore
// signs an honest person out of every session they hold after a lunch break,
// on a one-hour window, and fires a WARN alarm naming them. It also makes
// [Idle] unreachable: a twelve-hour idle deadline cannot be hit by a session
// that is refused after one hour and two minutes, so the whole idle mechanism
// would be dead code. Two mechanisms in one design cannot both be right when
// one makes the other unreachable, and the idle deadline is the one that is
// load-bearing.
//
// SO A LAGGING INDEX IS SERVED AND RE-ISSUED, and what bounds a captured
// cookie is the idle deadline (twelve hours from the last use), the absolute
// deadline (configured), and the revocation epoch (one write, immediate). What
// is still POSITIVE evidence, and still bumps the epoch, is an index the
// engine could not have issued:
//
//   - an index AHEAD of the age by more than the overlap. The engine issues an
//     index for the window it is in, so a bearer claiming a window that has
//     not begun was not issued in the ordinary way — and a clock difference
//     large enough to reach this is itself a fault worth ending sessions over,
//     because it is also large enough to have minted cookies whose deadlines
//     are wrong;
//   - an index somebody EDITED, which the bearer's own signature refuses
//     before any of this runs — the index is inside the signed payload, so a
//     cookie assembled from two others' halves is malformed rather than
//     reused.
//
// Both are facts about the bearer alone, so theft detection survives as a
// positive fact and no honest session is ever ended by it.

// rotationAt is the window index a session of this lineage is in at now.
//
// ZERO BEFORE THE FIRST BOUNDARY, and zero for a clock that reads BEFORE the
// session began — a negative age is a node whose clock is behind the minting
// node's, and the honest floor is the first window rather than an index that
// underflowed into the far future.
func (s *Signer) rotationAt(lineage uuid.UUID, now time.Time) uint64 {
	age := now.Sub(startOf(lineage))
	if age <= 0 {
		return 0
	}
	return uint64(age / s.rotateAfter)
}

// startOf is the instant a lineage was minted, read out of its own uuid7.
//
// THE LINEAGE IS THE CLOCK, which is what lets the bearer carry no start
// instant of its own: a uuid7's first six bytes are the millisecond it was
// created at, every node reads the same value out of them, and there is no
// second field for a writer to disagree with.
func startOf(lineage uuid.UUID) time.Time {
	seconds, nanos := lineage.Time().UnixTime()
	return time.Unix(seconds, nanos).UTC()
}

// THE INDEX TRAVELS AS AN INTEGER, NOT AS A DERIVED OPAQUE TOKEN.
//
// The obvious move is to carry an HMAC over (lineage, N) instead, so a cookie
// in a proxy log does not read as "this session is four hours old". It buys
// NOTHING HERE, and the reason is one field to its left: the lineage is a
// uuid7, so the cookie already discloses the exact millisecond the session
// began, and an observer holding it can divide by the rotation window
// themselves. Hiding the quotient beside the dividend is not privacy.
//
// It would also cost. An opaque id cannot be READ, only guessed at: a node
// would derive a candidate per plausible index and compare, which is a bounded
// search — every window inside the idle deadline — run on every request, to
// conceal a number the same cookie already implies. And the bearer's own
// signature covers the index either way, so a second MAC over it would be a
// second thing to keep in step with the first.
//
// What the index is NOT is a secret or a capability: it grants nothing, and
// editing it invalidates the bearer.

// rotationVerdict is what one bearer's index earns.
type rotationVerdict int

const (
	// rotationCurrent is an index in the window the clock says it should
	// be in.
	rotationCurrent rotationVerdict = iota

	// rotationBehind is an index from an earlier window: an idle session,
	// or a captured cookie, and nothing derived can tell those apart. It
	// is SERVED and re-issued at the current index.
	rotationBehind

	// rotationReuse is an index the engine could not have issued — ahead
	// of the clock beyond the overlap. It bumps the person's revocation
	// epoch, which ends every session they hold.
	rotationReuse
)

// rotationOf judges one bearer's index at now.
func (s *Signer) rotationOf(b Bearer, now time.Time) rotationVerdict {
	current := s.rotationAt(b.Lineage, now)
	switch {
	case b.Rotation == current:
		return rotationCurrent
	case b.Rotation < current:
		return rotationBehind
	}
	// AHEAD OF THE CLOCK. The overlap is measured in WINDOWS here rather
	// than in seconds, because that is the quantity the index is in: a
	// request in flight across a boundary is at most one window ahead of a
	// node whose clock trails by less than the overlap, and anything
	// further ahead is a bearer this fleet did not issue in the ordinary
	// way.
	ahead := b.Rotation - current
	if ahead == 1 && now.Add(Overlap).After(s.boundary(b.Lineage, b.Rotation)) {
		return rotationCurrent
	}
	return rotationReuse
}

// boundary is when the window with this index begins.
func (s *Signer) boundary(lineage uuid.UUID, index uint64) time.Time {
	return startOf(lineage).Add(time.Duration(index) * s.rotateAfter)
}
