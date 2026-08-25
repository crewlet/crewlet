package llm

import "context"

// seatKey is a private context key type, so nothing outside this package can
// write the value by another route.
type seatKey struct{}

// WithSeat binds the seat a call belongs to.
//
// It rides the context rather than the request because [Request] is the
// contract every backend shares, and today only ONE backend has any use for
// it: an HTTP provider has no per-seat state to isolate, while the cli-agent
// backend keeps a whole home directory per seat. Adding a field to Request
// that three of four backends must ignore is the worse trade.
//
// It is one of the few values this engine leaves ambient, and it meets the
// bar internal/agent/turnctx sets for that: immutable, read by a genuine
// leaf, and safe when absent — see [SeatOf].
func WithSeat(ctx context.Context, handle string) context.Context {
	return context.WithValue(ctx, seatKey{}, handle)
}

// SeatOf is the seat bound to this context, or "shared" when none is.
//
// A NAMED fallback rather than an empty string, because the value becomes a
// directory: an unbound call still needs somewhere isolated to run, and every
// unbound call sharing one home is the honest reading of "nobody said whose
// this is". Auxiliary work — summarisation, the relevance filter — legitimately
// arrives this way.
func SeatOf(ctx context.Context) string {
	if ctx == nil {
		return "shared"
	}
	handle, _ := ctx.Value(seatKey{}).(string)
	if handle == "" {
		return "shared"
	}
	return handle
}
