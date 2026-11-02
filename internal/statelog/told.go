package statelog

import "errors"

// A GESTURE'S REFUSAL AS THE OPERATOR WHO ASKED READS IT.
//
// The retention gestures (an acknowledgement, an eviction or a readmission, a
// reanchor, a capacity change and its abandonment or exclusion) are each
// refused for a reason the operator acts on: a confirmation that names the
// wrong instant, a node that still holds a lease, a window opened at another
// target, a node behind the floor. That reason is a sentence written where the
// refusal is decided. What used to reach the operator was the error's whole
// text instead, the package that wrote it at the front ("statelog: reanchor
// refused: ...") and, where a read had failed underneath, the store's or the
// broker's own words at the end, so the retention routes answered a `detail`
// no screen and no command line should print.
//
// So a refusal an operator reads is a [Told]: [Told.Tell] is the sentence
// alone, which the route answers, and its Error is that sentence with what it
// stands on, which the route logs. An error that is not a Told is a fault, and
// the route answers a sentence of its own for it and logs the error.

// Told is an error that carries the sentence its operator reads: what was
// refused and what to do, without the package that said it or the fault
// underneath.
type Told interface {
	error
	Tell() string
}

// Telling is a [Told] made where a refusal is decided. Say is the sentence,
// and Err, where there is one, what it stands on: a sentinel a caller branches
// on ([ErrReanchorRefused]), a fault only the log reads, or both.
type Telling struct {
	Say string
	Err error
}

// Error is the sentence followed by what it stands on, for the log.
func (t *Telling) Error() string {
	if t.Err == nil {
		return t.Say
	}
	return t.Say + ": " + t.Err.Error()
}

// Tell is the sentence alone.
func (t *Telling) Tell() string { return t.Say }

// Unwrap is what the sentence stands on, so errors.Is still finds a sentinel.
func (t *Telling) Unwrap() error { return t.Err }

// Tell is the sentence err carries for its operator, and false when it carries
// none: a fault, which its surface tells in words of its own.
func Tell(err error) (string, bool) {
	var told Told
	if errors.As(err, &told) {
		return told.Tell(), true
	}
	return "", false
}
