package turn

import "errors"

// ErrStoppedByPerson reports that a person ended this turn: they paused its
// seat and asked for the turn it was on to stop rather than finish.
//
// IT ARRIVES THROUGH THE FENCE, the per-round check the tool loop already makes
// before any tokens are spent and before each tool call, so a stop lands at a
// round boundary and never between a tool call and its result. The engine
// composes it with the seat's ownership fence (internal/engine/seatpause.go);
// every frame between the loop and the dispatcher wraps with %w, so it is
// comparable with [errors.Is] all the way out.
//
// NOT A FAILURE AND NOT A RETRY, which is why it is a sentinel of its own
// rather than an error the dispatcher reads like any other. A failed phase is
// NAKed so a redelivery can run it cleanly; that redelivery would run a turn a
// person deliberately ended, the moment they resumed the seat. So the
// dispatcher spends the trigger (it records it in the completion ledger and
// acks it) and publishes who stopped it, and the turn's completion reads
// `stopped` rather than `failed`. It is also not [Abandon]'s question: a turn
// stopped after it wrote outside the engine is still a turn a person ended,
// not one that broke.
var ErrStoppedByPerson = errors.New("turn: stopped by a person")

// Stopped reports whether err is a turn a person ended.
func Stopped(err error) bool { return errors.Is(err, ErrStoppedByPerson) }
