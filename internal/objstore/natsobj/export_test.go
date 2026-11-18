package natsobj

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// WalkAhead is [walkAhead], for the case that stages a listing past what one
// pull holds.
const WalkAhead = walkAhead

// ReadAhead, PullHeartbeat, QuietWait and FirstPullRemakes are [readAhead],
// [pullHeartbeat], [quietWait] and [jsapi.FirstPullRemakes], for the cases
// that hold a read's waits to the floor and to the pulls it can lose.
const (
	ReadAhead        = readAhead
	PullHeartbeat    = pullHeartbeat
	QuietWait        = quietWait
	FirstPullRemakes = jsapi.FirstPullRemakes
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

// MemberWindow, MessageCrossing, LinkHeartbeat and PurgeWait are
// [memberWindow], [messageCrossing], [linkHeartbeat] and [purgeWait], for the
// cases that count what a connection's puts hold out, hold a read across a
// link to its heartbeat, and time what a clean-up may add.
const (
	MemberWindow    = memberWindow
	MessageCrossing = messageCrossing
	LinkHeartbeat   = linkHeartbeat
	PurgeWait       = purgeWait
)

// PieceTake is what a put's message of dataBytes takes of its window
// ([pieceTake]), and PieceBound what a download's pull of a piece of want
// bytes does ([pieceBound]).
func PieceTake(dataBytes int) int { return pieceTake(dataBytes) }
func PieceBound(want int) int     { return pieceBound(want) }

// SetWindow gives b's transfers w in place of the window Open chose, so a
// case can make them wait their turn.
func SetWindow(b *Backend, w *jsapi.Window) { b.window = w }

// HeldCap is how long b's transfers across a metered link may hear nothing
// while it moves ([Backend.heldCap]).
func HeldCap(b *Backend) time.Duration { return b.heldCap() }

// CaughtUp is a piece read's question to the stream's leader of what follows
// after on filter ([reading.caughtUp]), as b's gets and walks ask it.
func CaughtUp(ctx context.Context, b *Backend, filter string, after uint64) (bool, error) {
	r := &reading{b: b, filter: filter, leader: b.leaderFor(filter), through: everything}
	return r.caughtUp(ctx, after)
}

// SetStall shortens how long b's puts wait for an acknowledgement before they
// end as stalled, so a case sees a stall without waiting [objstore.Stall].
func SetStall(b *Backend, d time.Duration) { b.stall = d }

// SetBeginPace gives b's listings d a mebibyte of bucket to begin, so a case
// can give a bucket of a few bytes the beginning a large one is given.
func SetBeginPace(b *Backend, d time.Duration) { b.beginPace = d }

// ListBegin is how long a listing of a bucket of bytes is given to begin
// ([Backend.listBegin]).
func ListBegin(b *Backend, bytes uint64) time.Duration { return b.listBegin(bytes) }

// LeaderTerm is how long one of b's leader message reads of what filter names
// is given before it is asked again ([Backend.leaderFor]).
func LeaderTerm(b *Backend, filter string) time.Duration { return b.leaderFor(filter).Term() }

// MetaSubject and PieceSubject are [metaSubject] and [pieceSubject].
func MetaSubject(name string) string  { return metaSubject(name) }
func PieceSubject(nuid string) string { return pieceSubject(nuid) }
