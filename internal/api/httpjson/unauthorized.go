package httpjson

import (
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// What `unauthorized` says, which depends on the rule that refused.
//
// `unauthorized` is EVERY refusal the authority table makes, and most of the
// rules behind one are not about a permission at all: a verb only an agent's
// seat makes, one made as a seat by a credential acting as none, an account
// that is not active, a proof of identity older than the verb allows. One
// sentence naming every permission told every one of those callers to go
// and ask an admin for access — which, holding admin already, they had — while
// the refusal's own `reason` beside it said something else. So the sentence
// FOLLOWS THE REASON, and names a permission only where one is the remedy:
// the grants a rule names beside a relation it refused (a lead's row, an
// author's, somebody's own record) admit the caller through the admin path, so
// those name it; the rest do not, and say what does clear them.

// The two keys of a refusal on authority the sentence is chosen by: the
// deciding rule's reason and the grants that would have admitted the caller —
// internal/authz's DetailReason and DetailGrants, which every surface refusing
// on authority writes through authz.RefusalFields. Spelled here because this
// package imports no engine machinery, and held to authz's by this package's
// tests, which build every refusal they read through authz itself.
const (
	keyReason = "reason"
	keyGrants = "grants"
)

// situated are the codes whose sentence depends on what the refusal says
// about itself, each with the function that chooses it from the detail.
//
// A TABLE BESIDE [codes] rather than a branch in [Envelope], so a code whose
// remedies differ by a fact it carries adds a row here and every writer of it
// is told the right sentence without a change of its own. A function that
// answers "" leaves the code's own sentence in place.
var situated = map[Code]func(Detail) string{
	CodeUnauthorized: unauthorizedMessage,
}

// messageFor is the sentence a refusal under code is shown, given its detail:
// the one its [situated] row chooses, or the code's own.
func messageFor(code Code, detail Detail) string {
	if choose, ok := situated[code]; ok {
		if message := choose(detail); message != "" {
			return message
		}
	}
	return code.Message()
}

// authority is what `unauthorized` says for one reason the table refuses by.
type authority struct {
	// said is what decided, as a sentence a person reads — or "" for the
	// reason whose sentence IS the permission it lacks ([lacksSentence]).
	said string
	// grantCures reports whether a permission the refusal names is a way
	// past it. True for the RELATIONS, whose rows the admin path admits
	// whatever the chart says: the grants beside one are a remedy, and an
	// empty list says none would be. False for every reason no permission
	// changes, whatever the refusal names beside it.
	grantCures bool
}

// refusals are the sentences, keyed by the authority table's own reason words.
//
// EVERY REASON THE TABLE ANSWERS `unauthorized`, AND NO OTHER: an admission
// ("grant", "self", …) never reaches a refusal; a stale proof, a machine
// token where a person must be present and no relation to an artifact are
// answered under codes of their own (`step_up_required`, `token_refused`,
// `not_found`), whose own sentences say it, so a row for one would be words no
// refusal is told; and a reason this table does not carry is told the code's
// own sentence. This package's tests hold the keys to the reasons the
// authority table's one mapping answers `unauthorized` in both directions, so
// a reason the table grows reaches a person with its own words or fails the
// build.
var refusals = map[string]authority{
	"no_grant":   {grantCures: true},
	"not_self":   {said: "This belongs to someone else.", grantCures: true},
	"not_lead":   {said: "Only its lead in the org chart can do this.", grantCures: true},
	"not_author": {said: "Only whoever wrote this can do that.", grantCures: true},
	"unnamed": {said: "The request doesn't say which record it's about. Send " +
		"it again naming one.", grantCures: true},
	"seat_refused": {said: "Only a person can do this, not an agent."},
	"seat_only":    {said: "Only an agent can do this."},
	"unseated": {said: "This is done as a seat, and this account has none. Ask " +
		"an admin to bind it to a human seat (crewlet iam bind)."},
	"stage": {said: "This account isn't active. Ask an admin to activate it " +
		"(crewlet iam activate)."},
	"unknown_action": {said: "This engine has no rule for that request. It may " +
		"be running a different version. Reload the page."},
	"role": {said: "Your role on this doesn't allow that. Whoever owns it can " +
		"share it with you at a higher one.", grantCures: true},
	"ceiling": {said: "The person this agent is working for can't do this, so " +
		"the agent can't either."},
}

// unauthorizedMessage is `unauthorized`'s sentence for the refusal detail
// carries: the deciding rule's own, followed — where a permission is a way
// past it — by the permissions the refusal names, or by the fact that none
// would. "" for a refusal that names no reason this table carries, which is
// told the code's own sentence.
func unauthorizedMessage(detail Detail) string {
	reason, _ := detail[keyReason].(string)
	rule, known := refusals[reason]
	if !known {
		return ""
	}
	grants, _ := detail[keyGrants].([]string)
	switch {
	case rule.said == "":
		// NO GRANT, and the grants ARE the sentence. A refusal that names
		// none names every permission, which is the code's own sentence.
		if len(grants) == 0 {
			return ""
		}
		return lacksSentence(grantWords(grants))
	case !rule.grantCures:
		return rule.said
	case len(grants) == 0:
		return rule.said + " No access would change that."
	default:
		return rule.said + " With " + grantWords(grants) + " access you could do " +
			"it anyway. Ask an admin for it."
	}
}

// lacksSentence is the sentence for a credential short of a permission: the
// permissions named, any one of which would admit it, and who gives one.
func lacksSentence(named string) string {
	return "You don't have the access this needs (" + named + "). Ask an admin " +
		"for it."
}

// grantWords names permissions as a person reads them: each word capitalised,
// in the list's own order, joined by commas with "or" before the last —
// "Admin, Tracker or Knowledge", the words People & access gives the
// permissions.
//
// SAID, not quoted: a person reads an app by its name, as the screen that
// grants it prints it, and never as a value they type.
func grantWords(grants []string) string {
	said := make([]words.Said, 0, len(grants))
	for _, word := range grants {
		if word != "" {
			word = strings.ToUpper(word[:1]) + word[1:]
		}
		said = append(said, words.Said(word))
	}
	return words.Or(said)
}

// grantNames is grants as the words a refusal's detail carries them in.
func grantNames(grants []iam.Grant) []string {
	names := make([]string, 0, len(grants))
	for _, g := range grants {
		names = append(names, string(g))
	}
	return names
}
