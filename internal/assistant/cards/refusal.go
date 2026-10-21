package cards

import (
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/redact"
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
//
// A PERSON'S OWN WORDS ARE READ AS THEY ARE, where the route wrote them for a
// person: a route answering a tool's refusal to a person answers it in the
// words the refusal carries for a person, and in no `detail` where it carries
// none (internal/api/operator's Fail, ADR-0060), as an artifact in the trash
// says to restore it first. The route marks that `detail` as a person's by
// telling the tool's own sentence beside it in process, never on the answer
// ([wire.Refusal.Said]). No sentence chosen by a code can say who changes a
// state, so those words are the person's, redacted and held to the rule every
// sentence here keeps ([ForAPerson]); an unmarked detail is the model's.
//
// A RACE IS ONLY A RACE. A 409 is a write that lost to somebody else's, a name
// something else holds, or the state of what the write is about (in the
// trash, past the company's storage quota, a full inbox), and answered by its
// status every one of them read "Another change landed at the same moment.",
// which sent a person to try again at what trying again never changes. So a
// 409 or a 412 is said by its code ([codeSentences]), and one whose code this
// build has no sentence for says only that it was not accepted.

// The sentences a person reads, by kind of refusal. Each is whole on its own,
// says nothing about what changed (a failed proposal's card adds "Nothing
// changed." itself), and names no tool, field, route or value.
const (
	refusedAccess    = "You do not have the access this needs."
	refusedSignedOut = "Your session has ended. Sign in again, then ask once more."
	refusedStepUp    = "This needs you to confirm it is you first."
	refusedMissing   = "The record this needs could not be found."
	refusedExists    = "That already exists."
	refusedRace      = "Another change landed at the same moment."
	refusedConflict  = "It has changed, or is in a state that does not take this change."
	refusedTooLarge  = "It is larger than the engine accepts."
	refusedBusy      = "The engine is busy. Try again in a moment."
	refusedValue     = "One of its values was not accepted."
	refusedUnsent    = "The assistant asked for it in a form the engine does not accept, " +
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
// handed the route's whole refusal, so the assistant says what can be done.
// Not "a change that can apply": a state refusal is cleared by whoever changes
// the state (an archived project's lead, a trashed task's restore), which no
// change the assistant could prepare does.
const RefusalResumes = "The assistant has the details and will say what can be done."

// codeSentences are the sentences for the codes that say more than their
// status: above all a 409's or a 412's, which is a race, a collision or the
// state of what the write is about, each a different thing for a person to do.
var codeSentences = map[string]string{
	// THE RACES: somebody else's change landed first, and asking again
	// against what is there now is the whole of it.
	"stale":             refusedRace,
	"stale_version":     refusedRace,
	"version_moved":     refusedRace,
	"revision_advanced": refusedRace,
	"proposal_stale":    refusedRace,
	// THE COLLISIONS: what the write would make is there already.
	"exists":            refusedExists,
	"not_creatable":     refusedExists,
	"entity_exists":     refusedExists,
	"label_taken":       "Another version already has that label.",
	"source_taken":      "Another of its owner's artifacts already has that address.",
	"folder_name_taken": "Something in that folder already has that name.",
	// THE STATES: what the write is about refuses it until somebody changes
	// it. `conflict` is a state or a race the domain said no more about.
	"conflict":            refusedConflict,
	"already_answered":    "It has already been answered.",
	"reassignment_budget": "It has been handed on as many times as it may be.",
	"inbox_full":          "The inbox list is full.",
	"not_running":         "It is not running, or not waiting for this.",
	"steer_unsupported":   "That turn cannot take a note while it runs.",
	"budget_exhausted":    "The token budget has no room left for now, so nothing was spent.",
	"quota_exceeded":      "The company has used all the storage its quota allows.",
	"artifact_trashed":    "It is in the trash. Restore it first.",
	"artifact_purging":    "It is being deleted for good.",
	"labels_full":         "Every label it can keep is in use.",
	"folder_limit":        "There is no room for another folder there.",
	"folder_widens":       "This would show the folder to more people, which needs confirming first.",
	"folder_cycle":        "A folder cannot go inside itself.",
	"folder_depth":        "Folders cannot nest that deep.",
	"folder_ambiguous":    "That names more than one folder.",
	"carried_object_gone": "A file it carries over is gone.",
	"upload_missing":      "A file it names has not arrived.",
	"upload_consumed":     "That upload has already been used.",
	"upload_expired":      "That upload is too old to use. Upload it again.",
	"not_undoable":        "It can no longer be undone.",
	// WHATEVER THE STATUS.
	"step_up_required":    refusedStepUp,
	"body_too_large":      refusedTooLarge,
	CodePreviewUnreadable: refusedUnread,
	CodeDeepUnavailable:   refusedDeep,
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
// rule's, where the authority table refused; a person's words, where the
// route wrote them for a person ([wire.Refusal.Said]) and they keep the rule
// every sentence here keeps; else its code's ([codeSentences]); else its
// status's class.
func personSentence(r wire.Refusal, status int) string {
	if r.Reason != "" {
		if said := httpjson.RefusalMessage(httpjson.Code(r.Code), r.Reason, r.Grants); said != "" {
			return said
		}
	}
	if said := redact.Secrets(r.Said); ForAPerson(said) {
		return said
	}
	code := r.Code
	if said, ok := codeSentences[code]; ok {
		return said
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

// internalWords are what a sentence written for a model carries and a person
// never reads: a code span, a route, a snake_case name (a tool, a field, a
// code) and the em dash the product's copy does not use.
var internalWords = regexp.MustCompile("`|\\b(GET|PUT|POST|PATCH|DELETE) /|\\b[a-z]+_[a-z_]+\\b|\u2014")

// ForAPerson reports whether s is a sentence a person may read as it was
// written: whole, and naming none of the engine's internals. Every sentence
// [PersonRefusal] chooses keeps it, and the words a route wrote for a person
// are read only where they do.
func ForAPerson(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && strings.ContainsRune(".?!", rune(s[len(s)-1])) &&
		!internalWords.MatchString(s)
}
