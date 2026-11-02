package atlassian

import (
	"fmt"
	"strings"
)

// AccountName is the display name one seat's account carries.
//
// IT CARRIES THE HANDLE, and that is what makes the pass idempotent.
// Atlassian assigns the account id and derives the address from the name, so
// there is nothing to store on the seat and no stable field of this engine's
// own to match on — except this one.
//
// The description would have been the natural place, and it is where this
// started. Atlassian ACCEPTS a description on the create call, answers 200,
// and stores nothing: every account came back with a null description, so
// every pass failed to recognise the accounts the last one made and created
// another. Four runs, four accounts for one agent, before a listing showed
// it. The name is the field that survives.
//
// The marker is the handle because a handle is immutable (ADR-0013): a
// document that changes one has removed the seat, and per-seat access reaches
// its account through the ledger. The label is the role as the document names it
// today.
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
