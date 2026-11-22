package queue

import "context"

// MaxAside is the most deliveries one attachment holds aside at once
// ([OutcomeAside]). An aside that would take it past is handed back at the head
// as a deferral is, one delivery spent, and the attachment goes on, so a
// handler that never readmits cannot pin a mailbox's every unacknowledged slot.
//
// SIXTY-FOUR: each aside delivery holds one of its consumer's ack-pending slots
// (JetStream's MaxAckPending, the server's default 1000, per seat inbox), so 64
// leaves the rest for a drain and its hand-backs; and it is the most a busy
// seat's router reads at once (internal/engine's routeReadLimit). Past it a seat
// is so far behind its running work that its inbox is held instead, the mail
// waiting on the broker in order (ADR-0169).
const MaxAside = 64

// Aside sets the delivery aside under key, for reason, which is logged
// ([OutcomeAside]): it waits, unacknowledged and spending nothing, until
// [EventQueue.Readmit] of key hands it back.
func Aside(key, reason string) Result {
	return Result{Outcome: OutcomeAside, Key: key, Reason: reason}
}

type asideRoomKey struct{}

// WithAsideRoom states, for one handler invocation, how many more deliveries
// the attachment handing it over takes aside before [MaxAside]. Only a backend
// calls it, on the delivery it hands over.
func WithAsideRoom(ctx context.Context, room int) context.Context {
	return context.WithValue(ctx, asideRoomKey{}, max(room, 0))
}

// AsideRoom reports how many more deliveries the attachment handing over ctx's
// delivery takes aside, and whether the backend said. A partition is set aside
// whole, so one larger than the room is handed back rather than set aside: a
// handler that must not spend a delivery holds its mailbox instead.
func AsideRoom(ctx context.Context) (int, bool) {
	room, ok := ctx.Value(asideRoomKey{}).(int)
	return room, ok
}
