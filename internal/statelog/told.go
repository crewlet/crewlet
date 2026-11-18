package statelog

import "errors"

// A GESTURE'S REFUSAL AS THE OPERATOR WHO ASKED READS IT.
//
// Each retention gesture (an acknowledgement, an eviction or readmission, a
// reanchor, a capacity change and its abandonment or exclusion) is refused for
// a reason the operator acts on, written as a sentence where the refusal is
// decided. A [Told] carries it: [Told.Tell] is the sentence alone, which the
// retention route answers, and Error adds what it stands on (the package
// prefix, a store's or broker's fault), which only the route's log reads. An
// error that is not a Told is a fault: the route answers a sentence of its own
// and logs the error.

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
