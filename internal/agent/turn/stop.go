package turn

import "errors"

// ErrStoppedByPerson reports that a person paused the seat and ended its turn.
//
// IT ARRIVES THROUGH THE FENCE, the tool loop's per-round check before any
// tokens or tool call, so a stop lands at a round boundary. The engine composes
// it with the seat's ownership fence (internal/engine/seatpause.go); every
// frame wraps with %w for [errors.Is].
//
// NOT A FAILURE AND NOT A RETRY: a NAKed phase is redelivered, rerunning a turn
// a person ended the moment they resumed the seat. So the dispatcher spends the
// trigger, publishes who stopped it, and the completion reads `stopped`. Nor
// is it [Abandon]'s question: a stopped turn that wrote outside the engine is
// still one a person ended.
var ErrStoppedByPerson = errors.New("turn: stopped by a person")

// Stopped reports whether err is a turn a person ended.
func Stopped(err error) bool { return errors.Is(err, ErrStoppedByPerson) }
