package seataccess

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/words"
)

// GrantDecline is the option of a request's decision that grants nothing.
const GrantDecline = "decline"

// RequestAsk is a request as the engine posts it (ADR-0118): the comment's body
// and its decision. The decision's capped fields hold only bounded words; the
// body states the grant whole, with what applies to it as a whole.
func RequestAsk(seat string, req GrantRequest, entry GrantEntry) (string, decision.Decision) {
	vendor := LaneName(req.Lane)
	reach := requestReach(entry, req.Scope, GrantTermsOf(req.Lane))
	hours := words.Count(req.Hours, "hour", "hours")
	options := []decision.Option{{ID: GrantOptionTemporary(req.ID),
		Label: fmt.Sprintf("Grant for this change (expires in %s)", hours),
		Detail: fmt.Sprintf("The agent holds it for %s from when you grant it. You review the exact "+
			"change first.", hours)}}
	if !entry.OneChange() {
		options = append(options, decision.Option{ID: GrantOptionPermanent(req.ID),
			Label:  "Add to this agent permanently",
			Detail: "The agent keeps it until somebody removes it on the Agents tab. You review the exact change first."})
	}
	options = append(options, decision.Option{ID: GrantDecline, Label: "Not now",
		Detail: "Nothing changes, and the agent is told."})
	return requestBody(seat, req, entry, vendor, reach), decision.Decision{
		Question: requestQuestion(seat, entry.Name, reach, vendor, req.Target),
		Options:  options, Recommended: GrantOptionTemporary(req.ID), Rationale: req.Reason,
		Role: decision.RoleApprover,
	}
}

// requestQuestion is the most specific phrasing that fits; the body names
// whatever it leaves out.
func requestQuestion(seat, permission, reach, vendor, target string) string {
	once, to := "", ""
	if target != "" {
		once = ", for one change"
		to = once + " to " + target
	}
	return decision.FittingQuestion(
		fmt.Sprintf("Grant %s %s on %s at %s%s?", seat, permission, reach, vendor, to),
		fmt.Sprintf("Grant %s %s at %s%s?", seat, permission, vendor, to),
		fmt.Sprintf("Grant %s %s at %s%s?", seat, permission, vendor, once),
		fmt.Sprintf("Grant this agent %s at %s%s?", permission, vendor, once),
		fmt.Sprintf("Grant this agent the permission below at %s%s?", vendor, once))
}

// requestBody is the agent's reason, then the grant whole: what the permission
// allows beyond its name, the sign-in a write asks, what the provisioner lacks.
func requestBody(seat string, req GrantRequest, entry GrantEntry, vendor, reach string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks for %s on %s at %s", seat, entry.Name, reach, vendor)
	if req.Target != "" {
		fmt.Fprintf(&b, ", for one change to %s", req.Target)
	}
	b.WriteString(".")
	if entry.Note != "" {
		b.WriteString(" " + entry.Note)
	}
	if entry.Write {
		b.WriteString(" It can change things there, so granting it asks you to sign in again.")
	}
	if entry.Unavailable != "" {
		fmt.Fprintf(&b, " %s must first give the company's own credential %s.", vendor, words.And(entry.Missing))
	}
	return req.Reason + "\n\n" + b.String()
}

// requestReach is where a requested grant would apply: its scope entries, or
// what the lane's grant reaches naming none ([GrantTerms]).
func requestReach(entry GrantEntry, scope []string, w GrantTerms) string {
	switch {
	case len(scope) > 0:
		return strings.Join(scope, ", ")
	case entry.ScopeKind == GrantScopeAccount:
		return cmp.Or(w.Account, "the account")
	default:
		return cmp.Or(w.Whole, "every "+string(entry.ScopeKind)+" the company's agents reach")
	}
}
