package atlassian

import (
	"fmt"
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

// PlanFor is the seats this company wants an Atlassian identity for.
//
// A seat opts IN by naming a ${VAR} in one of its mcp_env Atlassian blocks,
// exactly as every other provisioned app works: the variable is where the
// minted token is written, and a seat with nowhere to put one is a seat this
// pass leaves alone rather than an error.
func PlanFor(o *org.Organization) (*provision.Plan, error) {
	plan := &provision.Plan{}
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
			// THE NOTE NAMES THE SHAPE, NEVER THE VALUE: it is printed in a
			// report an operator pastes into a ticket, and the value is
			// either a credential or a string containing one.
			plan.Note("%s: mcp_env %s is not a whole ${VAR} reference, so there is "+
				"nowhere to write a minted token — point it at a variable, or "+
				"manage this seat's account by hand", handle, key)
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
	return plan, nil
}
