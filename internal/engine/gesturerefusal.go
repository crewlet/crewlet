package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// THE RETENTION GESTURES' REFUSALS, TOLD.
//
// A reanchor, a capacity change, its abandonment and an exclusion are each
// refused for a reason the operator acts on, written where the refusal is
// decided. They used to be returned as `engine: ...` errors, some ending in the
// store's or the broker's own words, and the routes answered that whole text as
// the `detail` an operator read. Each is a [statelog.Told] now: the sentence is
// what the route answers, and the error's text, which keeps what it stands on,
// is what the route logs. An error a gesture returns that is not a Told is a
// fault, and the route tells it in words of its own.

// refused is a gesture refused for the reason format says, told in those
// words.
func refused(format string, args ...any) error {
	return &statelog.Telling{Say: fmt.Sprintf(format, args...)}
}

// stopped is a gesture that stopped on fault, told as format says (what is
// known and what to do), with the fault kept for the log.
func stopped(fault error, format string, args ...any) error {
	return &statelog.Telling{Say: fmt.Sprintf(format, args...), Err: fault}
}

// reanchorUnread is a reanchor refused because something it is decided on
// couldn't be read: told as format says, standing on
// [statelog.ErrReanchorRefused] and on the fault.
func reanchorUnread(fault error, format string, args ...any) error {
	return &statelog.Telling{Say: fmt.Sprintf(format, args...),
		Err: fmt.Errorf("%w: %w", statelog.ErrReanchorRefused, fault)}
}

// toldOrError is the sentence err tells its operator, or its whole text where
// it tells none: the reanchor status's `nothing_to_reanchor` and
// `discarding`, which the transition's own refusals fill.
func toldOrError(err error) string {
	if said, ok := statelog.Tell(err); ok {
		return said
	}
	return err.Error()
}

// UnknownStreamError is [ErrUnknownStream] naming the stream asked about and
// the streams this node does keep, which is what a person who mistyped one
// needs to read.
type UnknownStreamError struct {
	Stream string

	// Streams are the logs this node keeps; none on a node that keeps no
	// state log at all (a node without the `data` role).
	Streams []string
}

func (e *UnknownStreamError) Error() string { return "engine: " + e.Tell() }

// Tell is the refusal as the operator who named the stream reads it
// ([statelog.Told]).
func (e *UnknownStreamError) Tell() string {
	if len(e.Streams) == 0 {
		return fmt.Sprintf("this node keeps no logs (only a node with the `data` "+
			"role does), so %q isn't one of them", e.Stream)
	}
	return fmt.Sprintf("%q isn't a log this node keeps. Use %s", e.Stream,
		words.Or(e.Streams))
}

// Unwrap is [ErrUnknownStream], which callers branch on.
func (e *UnknownStreamError) Unwrap() error { return ErrUnknownStream }
