package atlassian

import (
	"fmt"
	"strings"
)

// AccountName is the display name one seat's account carries.
//
// IT CARRIES THE HANDLE, which is what makes the pass idempotent: Atlassian
// assigns the id and derives the address from the name, so the name is the
// only field of ours to match on. The description is not an option: the create
// call accepts one, answers 200 and stores nothing, so every pass would
// create another account.
//
// The marker is the handle because a handle is immutable (ADR-0013): changing
// one removes the seat, and per-seat access reaches its account through the
// ledger. The label is the role as the document names it today.
func AccountName(role, handle string) string {
	label := strings.TrimSpace(role)
	if label == "" {
		label = handle
	}
	return fmt.Sprintf("%s (crewlet:%s)", label, handle)
}

// HandleFrom reads the seat back out of an account's display name, or empty
// for an account this engine did not create.
//
// It is what keeps a disconnect from deleting somebody else's service
// account, so it matches the whole marker rather than a prefix.
func HandleFrom(displayName string) string {
	_, rest, found := strings.Cut(strings.TrimSpace(displayName), "(crewlet:")
	if !found {
		return ""
	}
	handle, closed := strings.CutSuffix(rest, ")")
	if !closed {
		return ""
	}
	return strings.TrimSpace(handle)
}
