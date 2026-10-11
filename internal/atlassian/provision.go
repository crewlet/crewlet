package atlassian

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
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

// Plan is what one Atlassian pass intends to do: the seats it provisions, the
// caveats about them, and the seats it deliberately leaves to the company.
type Plan struct {
	provision.Plan

	// ByHand is the handles of the seats whose Atlassian credential the
	// company manages itself: an agent whose mcp_env token slot holds a
	// value rather than a whole ${VAR}, so there is nowhere to write a token
	// this engine mints and no account for it to create.
	//
	// APART FROM [provision.Plan.Notes], because the two are opposite facts
	// with opposite owners. A note is a seat this engine provisions and
	// cannot finish — its address has nowhere to go — which only an edit of
	// the company document fixes. This is a DECISION the document already
	// states, which no edit is owed for and no pass will ever change. Filed
	// as a note it was a finding that never cleared: a short grant once, a
	// config fault since, either way telling somebody to fix a seat that was
	// set up exactly as they meant it. See [Result.Findings] for what it is
	// reported as instead.
	ByHand []string
}

// PlanFor is the seats this company wants an Atlassian identity for.
//
// A seat opts IN by naming a ${VAR} in one of its mcp_env Atlassian blocks,
// exactly as every other provisioned app works: the variable is where the
// minted token is written, and a seat with nowhere to put one is a seat this
// pass leaves alone — [Plan.ByHand] — rather than an error.
func PlanFor(o *org.Organization) (*Plan, error) {
	plan := &Plan{}
	if o == nil {
		return plan, nil
	}
	for seat := range o.AllRoles() {
		if !seat.IsAgent() {
			// A human seat is a person with their own Atlassian account;
			// minting one an identity would create a second for somebody
			// who already has one.
			continue
		}
		handle := seat.Handle()
		key, value := seatCredential(seat.MCPEnv)
		if key == "" {
			continue
		}
		name, ok := provision.SoleVar(value)
		if !ok {
			// THE HANDLE AND NOTHING ELSE, never the slot's value: it is
			// reported where an operator pastes it into a ticket, and the
			// value is either a credential or a string containing one.
			plan.ByHand = append(plan.ByHand, handle)
			continue
		}
		entry := provision.Seat{Handle: handle, Role: seat.Name, TokenVar: name}
		// WHERE THE ADDRESS GOES. Atlassian names the account itself, and its
		// product APIs take Basic base64(address:token), so a seat holding
		// only the token authenticates as nobody. The address is not
		// something an operator can write down in advance.
		if where, value := seatEmail(seat.MCPEnv); where != "" {
			if emailVar, ok := provision.SoleVar(value); ok {
				entry.EmailVar = emailVar
			} else {
				plan.Note("%s: mcp_env %s is not a whole ${VAR} reference, so there "+
					"is nowhere to write the address Atlassian assigns this "+
					"account — point it at a variable", handle, where)
			}
		} else {
			plan.Note("%s: no mcp_env address slot, so this seat cannot use the "+
				"account: Atlassian authenticates its products as "+
				"base64(address:token), and the token alone is refused. Add "+
				"JIRA_USERNAME with a ${VAR} beside the token", handle)
		}
		plan.Add(entry)
	}
	// ORDERED BY HANDLE, as [provision.Plan.Add] keeps the seats and for its
	// reason: a report read beside the previous pass's must list the same
	// seats in the same places.
	slices.Sort(plan.ByHand)
	return plan, nil
}
