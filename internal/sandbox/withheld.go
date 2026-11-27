package sandbox

import "context"

// A Withholding is a piece of a run's box a read of it refused, or did not
// show, and why: counted by the node's [CollectMeter], so what a collection
// gives up is seen beside what it reads.
type Withholding struct {
	Piece  WithheldPiece
	Reason WithheldReason
}

// WithheldPiece is the kind of piece a read withheld.
type WithheldPiece string

const (
	// WithheldErrorStream is the end of a run's error stream, which its
	// failure and transcript show.
	WithheldErrorStream WithheldPiece = "error_stream"
	// WithheldEventStream is a run's event stream, decoded for its result.
	WithheldEventStream WithheldPiece = "event_stream"
	// WithheldWholePiece is a file a collection reads whole: its result,
	// report, session record, exit status and the rest.
	WithheldWholePiece WithheldPiece = "whole_piece"
	// WithheldSessionRecord is a harness's session record, walked for its
	// notes and newest summary.
	WithheldSessionRecord WithheldPiece = "session_record"
)

// Valid reports whether p is a piece a read names.
func (p WithheldPiece) Valid() bool {
	switch p {
	case WithheldErrorStream, WithheldEventStream, WithheldWholePiece, WithheldSessionRecord:
		return true
	}
	return false
}

// WithheldReason is why a read withheld a piece.
type WithheldReason string

const (
	// WithheldPastBound is a read its time ran out on before it ended.
	WithheldPastBound WithheldReason = "bound"
	// WithheldRefused is a text the redaction stream refused.
	WithheldRefused WithheldReason = "refused"
	// WithheldChanged is a file cut or written over while it was read, or
	// read short of the size its open found ([FileChanged]).
	WithheldChanged WithheldReason = "changed"
	// WithheldGrown is a file grown past the size its open found
	// ([FileAppended]), where the read stands for the whole file.
	WithheldGrown WithheldReason = "grown"
	// WithheldTooLarge is a file past the box's whole-read cap.
	WithheldTooLarge WithheldReason = "too_large"
	// WithheldNotRegular is a path that is no regular file.
	WithheldNotRegular WithheldReason = "not_regular"
	// WithheldUnsettled is a stream still being written whose redaction had
	// settled too little of what was read to show its end.
	WithheldUnsettled WithheldReason = "unsettled"
)

// Valid reports whether r is a reason a read gives.
func (r WithheldReason) Valid() bool {
	switch r {
	case WithheldPastBound, WithheldRefused, WithheldChanged, WithheldGrown, WithheldTooLarge,
		WithheldNotRegular, WithheldUnsettled:
		return true
	}
	return false
}

// CollectMeter counts what this node's reads of its runs' boxes withheld
// ([CoordinatorOptions.Collects]).
type CollectMeter interface {
	Withheld(Withholding)
}

// withheldKey is the context key a read's [CollectMeter] rides under.
type withheldKey struct{}

// CountingWithheld is ctx carrying meter, which a read of a box under it
// hands what it withholds ([MeterOf]): a context value, since a run's handle
// is compared and a meter is not. A nil meter counts nothing.
func CountingWithheld(ctx context.Context, meter CollectMeter) context.Context {
	if meter == nil {
		return ctx
	}
	return context.WithValue(ctx, withheldKey{}, meter)
}

// MeterOf is the meter ctx carries ([CountingWithheld]), nil where none.
func MeterOf(ctx context.Context) CollectMeter {
	meter, _ := ctx.Value(withheldKey{}).(CollectMeter)
	return meter
}

// To hands w to meter, where there is one.
func (w Withholding) To(meter CollectMeter) {
	if meter != nil {
		meter.Withheld(w)
	}
}
