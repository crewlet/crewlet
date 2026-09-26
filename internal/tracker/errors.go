package tracker

import "fmt"

// PartialError is a GESTURE that stopped part-way with its first commit
// landed: a subtree removal whose root is in the trash, a restore whose root is
// out of it, a cross-project move whose root has moved, a promotion whose
// subtask exists, a merge whose duplicate is marked as merging.
//
// # Why it is a type
//
// Every other error a write returns means the change was not made, and a
// caller told that about one of these makes the change again, or reports that
// nothing happened, while the first commit stands. The message says which of
// the two it is; a caller that has to act on the difference asks this type
// rather than parsing the message.
//
// # What finishes it
//
// [PartialError.Rerun] says whether calling the same gesture again finishes
// it. A removal's and a restore's commits are each nothing to do when their
// task is already where the gesture puts it, so a second call steps over what
// the first one wrote and writes the rest. A promotion's second call finds the
// subtask the first one filed — its id is derived from the checklist item —
// and goes on to mark the item. A cross-project move's second call is refused,
// because its root has already moved; a merge's marker hands what is left to
// the tracker duty. The message of each says what finishes it.
type PartialError struct {
	// Rerun reports whether calling the same gesture again finishes it.
	Rerun bool

	err error
}

// partial is a [PartialError] whose message is format with args, wrapping what
// fmt.Errorf would.
func partial(rerun bool, format string, args ...any) error {
	return &PartialError{Rerun: rerun, err: fmt.Errorf(format, args...)}
}

func (e *PartialError) Error() string { return e.err.Error() }

// Unwrap keeps the cause the gesture stopped on reachable, so a caller asking
// what went wrong underneath — a conflict, a node that is behind — still can.
func (e *PartialError) Unwrap() error { return e.err }
