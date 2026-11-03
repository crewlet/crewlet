package natsobj

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// WalkAhead is [walkAhead], for the case that stages a listing past what one
// pull holds.
const WalkAhead = walkAhead

// ReadAhead, PullHeartbeat, QuietWait and FirstPullRemakes are [readAhead],
// [pullHeartbeat], [quietWait] and [firstPullRemakes], for the cases that hold
// a read's waits to the floor and to the pulls it can lose.
const (
	ReadAhead        = readAhead
	PullHeartbeat    = pullHeartbeat
	QuietWait        = quietWait
	FirstPullRemakes = firstPullRemakes
)

// SwapStream puts what wrap makes of b's stream between b and the broker, so
// a case can stand where the broker's consumers are made.
func SwapStream(b *Backend, wrap func(jetstream.Stream) jetstream.Stream) {
	b.stream = wrap(b.stream)
}

// OpenAt is [Open] at timing, so a case that exhausts a lookup's ceiling on
// purpose spends a fraction of a second rather than the production thirty.
func OpenAt(ctx context.Context, js jetstream.JetStream, cfg Config, timing jsprovision.Timing) (*Backend, error) {
	return open(ctx, js, cfg, timing)
}

// MemberWindow, MessageCrossing and PurgeWait are [memberWindow],
// [messageCrossing] and [purgeWait], for the cases that count what a
// connection's puts hold out, hold the link's window to the lease, and time
// what a clean-up may add.
const (
	MemberWindow    = memberWindow
	MessageCrossing = messageCrossing
	PurgeWait       = purgeWait
)

// LinkWindow is [linkWindow]: the window a connection across a link is given
// for a node renewing its leases every heartbeat.
func LinkWindow(heartbeat time.Duration) int { return linkWindow(heartbeat) }

// SetWindow gives b's puts a window of n messages in place of the one Open
// chose, so a case can make its puts wait their turn for a place.
func SetWindow(b *Backend, n int) { b.window = newWindow(n) }

// SetStall shortens how long b's puts wait for an acknowledgement before they
// end as stalled, so a case sees a stall without waiting [objstore.Stall].
func SetStall(b *Backend, d time.Duration) { b.stall = d }

// LeaderTerm is how long one of b's leader message reads of what filter names
// is given before it is asked again ([Backend.leaderFor]).
func LeaderTerm(b *Backend, filter string) time.Duration { return b.leaderFor(filter).Term() }

// MetaSubject and PieceSubject are [metaSubject] and [pieceSubject].
func MetaSubject(name string) string  { return metaSubject(name) }
func PieceSubject(nuid string) string { return pieceSubject(nuid) }
