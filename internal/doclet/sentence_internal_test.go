package doclet

import (
	"errors"
	"fmt"
	"testing"
)

// TestARefusalSaysItsFieldAsAValue pins the two readings of [invalid]: the Go
// error keeps the form every log and model has read, and [Sentence] is what a
// page tool tells a model — the argument as a value and what to do, without
// "doclet: invalid:" in front of it.
func TestARefusalSaysItsFieldAsAValue(t *testing.T) {
	t.Parallel()
	err := invalid("title", "%d bytes, past the %d-byte cap", 600, 256)
	if got, want := err.Error(), "doclet: invalid: title: 600 bytes, past the 256-byte cap"; got != want {
		t.Errorf("the error is %q, want %q", got, want)
	}
	if got, want := Sentence(err), "`title`: 600 bytes, past the 256-byte cap"; got != want {
		t.Errorf("the sentence is %q, want %q", got, want)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Error("the refusal lost its mark")
	}
	// CONTEXT IS NEVER DROPPED TO TIDY A PREFIX: an error this package wrote
	// loses the "doclet:" it opens with and keeps every word after it, the
	// context a caller in this package gave it included; one somebody else
	// wrapped opens with their context and is its whole message.
	wrapped := fmt.Errorf("doclet: save ENG/Runbook: %w", err)
	if got, want := Sentence(wrapped), "save ENG/Runbook: doclet: invalid: title: 600 bytes, "+
		"past the 256-byte cap"; got != want {
		t.Errorf("a wrapped refusal's sentence is %q, want %q", got, want)
	}
	if got, want := Sentence(ErrNotFound), "no such record"; got != want {
		t.Errorf("a sentinel's sentence is %q, want %q", got, want)
	}
	outside := fmt.Errorf("rename the page: %w", ErrNotFound)
	if got := Sentence(outside); got != outside.Error() {
		t.Errorf("a refusal another package wrapped is %q, want its whole message", got)
	}
}
