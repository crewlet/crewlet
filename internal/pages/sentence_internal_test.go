package pages

import (
	"errors"
	"fmt"
	"testing"
)

// TestARefusalSaysItsFieldToAPerson pins the two readings of [invalid]: the Go
// error keeps the form every log and model has read, and [Sentence] is what a
// person's write surface prints — the argument as a value and what to do,
// without "pages: invalid:" in front of it.
func TestARefusalSaysItsFieldToAPerson(t *testing.T) {
	t.Parallel()
	err := invalid("title", "%d bytes, past the %d-byte cap", 600, 256)
	if got, want := err.Error(), "pages: invalid: title: 600 bytes, past the 256-byte cap"; got != want {
		t.Errorf("the error is %q, want %q", got, want)
	}
	if got, want := Sentence(err), "`title`: 600 bytes, past the 256-byte cap"; got != want {
		t.Errorf("the sentence is %q, want %q", got, want)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Error("the refusal lost its mark")
	}
	// CONTEXT IS NEVER DROPPED TO TIDY A PREFIX.
	wrapped := fmt.Errorf("pages: save ENG/Runbook: %w", err)
	if got := Sentence(wrapped); got != wrapped.Error() {
		t.Errorf("a wrapped refusal's sentence is %q, want its whole message", got)
	}
	if got := Sentence(ErrNotFound); got != ErrNotFound.Error() {
		t.Errorf("an unmarked error's sentence is %q, want its whole message", got)
	}
}
