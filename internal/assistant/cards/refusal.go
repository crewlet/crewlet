package cards

import (
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A REFUSAL A PERSON READS IS THE ENGINE'S OWN SENTENCE, chosen by what kind of
// refusal it is, never the route's account of the request.
//
// The route's `detail` and `hint`, and every error the assistant's own checks
// raise about a call, are written for whoever corrects the request: they name
// tools ("write_page refused that"), fields and config keys in backticks
// (`knowledge.root_space`), routes ("PUT /config/units/{id}") and the values
// to send instead. That is exactly what the model needs, and it reads all of
// it ([wire.Refusal.ForModel]). A person reading the same words in the drawer
// learns nothing they can act on, and is shown the engine's internals. So the
// sentence a person reads is decided here, once, from the refusal's code and
// the status it came with, and every place a refusal leaves the engine for a
// person (a step's line, a read's error card, a failed proposal's strip) goes
// through [PersonRefusal]. What to do next is the assistant's to say in plain
// words, from the detail it was handed, which the prompt asks of it.
//
// A REFUSAL ON AUTHORITY SAYS ITS RULE. The authority table answers most of
// its refusals `403 unauthorized`, and few of them are a missing permission: a
// verb the assistant never takes, a change it can only propose, a role on an
// artifact below the verb's, a verb only an agent makes. Worded by their code
// and status, every one of them read "You do not have the access this
// needs.", an administrator's among them. So a refusal carrying the table's
// reason says the sentence the envelope says for that rule
// ([httpjson.RefusalMessage]), the words every reader of the API is told, and
// only the rest are worded here by their code and status.
//
// THE RULE AND THE GRANTS STAY: closed-set words a client decides by, the
// reason that refused and the permissions it names. The grants alone cannot
// say whether a permission is the way past (a stale proof's are the ones that
// admitted the person), and the reason can.

// The sentences a person reads, by kind of refusal. Each is whole on its own,
// says nothing about what changed (a failed proposal's card adds "Nothing
// changed." itself), and names no tool, field, route or value.
const (
	refusedAccess    = "You do not have the access this needs."
	refusedSignedOut = "Your session has ended. Sign in again, then ask once more."
	refusedStepUp    = "This needs you to confirm it is you first."
	refusedMissing   = "The record this needs could not be found."
	refusedExists    = "That already exists."
	refusedConflict  = "Another change landed at the same moment."
	refusedTooLarge  = "It is larger than the engine accepts."

	// A 409 IS NOT ALWAYS A COLLISION: these are states the request met
	// that no rereading changes, each said as what it is.
	refusedAnswered   = "Somebody has already answered it."
	refusedHandedOn   = "It has been handed between seats as often as it may be. A person has to pick it up now."
	refusedInboxFull  = "The inbox holds as many marks as it may. Mark everything up to here read first."
	refusedNotesFull  = "The turn already holds as many notes as it can before it reads them. Send it again once it has read them, at its next step."
	refusedNotesSpent = "The turn has taken as many notes as a turn takes. Put the rest on its work item."
	refusedNotRunning = "It is not running any more, or not waiting for this."
	refusedBudget     = "The token budget for this window is spent. Raise it, or wait for the window to turn over."

	refusedBusy   = "The engine is busy. Try again in a moment."
	refusedValue  = "One of its values was not accepted."
	refusedUnsent = "The assistant asked for it in a form the engine does not accept, " +
		"so it was not sent."
	refusedUnread  = "What this would change could not be read."
	refusedDeep    = "Deep work is not available right now."
	refusedGeneric = "The engine did not accept it."
)

// Codes the assistant raises itself, before or instead of a route's answer.
// They are the turn's own (internal/assistant), named here because this is
// where their sentence is chosen.
const (
	// CodeInvalidCall is a call refused as the model wrote it, before
	// anything was sent.
	CodeInvalidCall = "invalid_input"
	// CodePreviewUnreadable is a change whose preview could not read the
	// object it changes.
	CodePreviewUnreadable = "preview_unreadable"
	// CodeDeepUnavailable is a deep run that could not start.
	CodeDeepUnavailable = "deep_work_unavailable"
)

// RefusalResumes is the hint a failed proposal's card carries: the turn is
// handed the route's whole refusal, so the assistant says what to change.
const RefusalResumes = "The assistant has the details and will suggest a change that can apply."

// RefusalSettled is the hint in its place for a refusal no change to the same
// request can mend — the turn has ended, has taken all the notes it takes, or
// the question was answered: the assistant still has the details, and says
// what can be done instead.
const RefusalSettled = "The assistant has the details and will say what can still be done."

// RefusalHint is the hint a failed proposal's card carries for a refusal of
// code: [RefusalSettled] where no change to the request can apply, else
// [RefusalResumes].
func RefusalHint(code string) string {
	switch code {
	case string(httpjson.CodeNotRunning), string(httpjson.CodeNotesSpent), string(httpjson.CodeAlreadyAnswered):
		return RefusalSettled
	}
	return RefusalResumes
}

// PersonRefusal is r as a person reads it: its code, its rule's reason and its
// grants, and the engine's own sentence for its kind ([personSentence]) in
// place of the route's or the model's words. status is the route's answer,
// zero for a call refused before anything was sent. The detail is left
// behind: it is the model's.
func PersonRefusal(r wire.Refusal, status int) wire.Refusal {
	return wire.Refusal{Code: r.Code, Message: personSentence(r, status), Reason: r.Reason,
		Grants: slices.Clone(r.Grants)}
}

// personSentence is the sentence for refusal r, answered at status: its
// rule's, where the authority table refused; else its code's, where that says
// more than its status (a 409 that is an existing record, a full or spent
// turn, a turn that ended, rather than a race); else its status's class.
func personSentence(r wire.Refusal, status int) string {
	if r.Reason != "" {
		if said := httpjson.RefusalMessage(httpjson.Code(r.Code), r.Reason, r.Grants); said != "" {
			return said
		}
	}
	code := r.Code
	switch code {
	case "step_up_required":
		return refusedStepUp
	case "exists", "not_creatable":
		return refusedExists
	case "body_too_large":
		return refusedTooLarge
	case string(httpjson.CodeAlreadyAnswered):
		return refusedAnswered
	case string(httpjson.CodeReassignmentBudget):
		return refusedHandedOn
	case string(httpjson.CodeInboxFull):
		return refusedInboxFull
	case string(httpjson.CodeNotesFull):
		return refusedNotesFull
	case string(httpjson.CodeNotesSpent):
		return refusedNotesSpent
	case string(httpjson.CodeNotRunning):
		return refusedNotRunning
	case string(httpjson.CodeBudgetExhausted):
		return refusedBudget
	case CodePreviewUnreadable:
		return refusedUnread
	case CodeDeepUnavailable:
		return refusedDeep
	}
	switch {
	case status == 0 && code == CodeInvalidCall:
		return refusedUnsent
	case status == http.StatusUnauthorized:
		return refusedSignedOut
	case status == http.StatusForbidden:
		return refusedAccess
	case status == http.StatusNotFound, status == http.StatusGone:
		return refusedMissing
	case status == http.StatusConflict, status == http.StatusPreconditionFailed:
		return refusedConflict
	case status == http.StatusRequestEntityTooLarge:
		return refusedTooLarge
	case status == http.StatusTooManyRequests, status == http.StatusServiceUnavailable:
		return refusedBusy
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity,
		status == http.StatusPreconditionRequired, status == http.StatusUnsupportedMediaType:
		return refusedValue
	}
	return refusedGeneric
}
