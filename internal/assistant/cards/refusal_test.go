package cards_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// internalWords are what a refusal written for the model carries and a person
// never reads: a code span, a route, a snake_case name (a tool, a field, a
// code) and the em dash the product's copy does not use.
var internalWords = regexp.MustCompile("`|\\b(GET|PUT|POST|PATCH|DELETE) /|\\b[a-z]+_[a-z_]+\\b|\u2014")

// EVERY REFUSAL A PERSON READS IS THE ENGINE'S SENTENCE: for every code the
// API answers, at every status a refusal comes with, and for the assistant's
// own codes, the person's refusal keeps the code and the grants and says a
// sentence naming no tool, field, route or value, whatever the route's own
// words were. Mutation: carry the route's message, detail or hint through, and
// this fails.
func TestEveryRefusalAPersonReadsIsTheEnginesSentence(t *testing.T) {
	t.Parallel()
	statuses := []int{0, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusConflict, http.StatusGone, http.StatusPreconditionFailed,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity,
		http.StatusPreconditionRequired, http.StatusTooManyRequests, http.StatusTeapot,
		http.StatusServiceUnavailable}
	codes := []string{cards.CodeInvalidCall, cards.CodePreviewUnreadable, cards.CodeDeepUnavailable}
	for _, c := range httpjson.Codes() {
		codes = append(codes, string(c))
	}
	for _, code := range codes {
		for _, status := range statuses {
			route := wire.Refusal{Code: code, Message: "write_page refused that: `container`",
				Hint: "PUT /config/units/{id}", Detail: "knowledge.root_space", Grants: []string{"doclet"}}
			got := cards.PersonRefusal(route, status)
			switch {
			case got.Code != code || len(got.Grants) != 1 || got.Grants[0] != "doclet":
				t.Errorf("%s at %d lost its code or grants: %+v", code, status, got)
			case got.Message == "" || got.Hint != "" || got.Detail != "":
				t.Errorf("%s at %d reads %+v, want the engine's sentence alone", code, status, got)
			case internalWords.MatchString(got.Message):
				t.Errorf("%s at %d reads %q, which names the engine's internals", code, status,
					got.Message)
			case !strings.HasSuffix(got.Message, "."):
				t.Errorf("%s at %d reads %q, which is not a sentence", code, status, got.Message)
			}
		}
	}
}

// A REFUSAL THAT IS NO COLLISION IS NEVER WORDED AS ONE. A 409 says the request
// met a state of the target, and most of those states are not a race: a turn
// holding as many notes as it may, or one that has taken all it takes, a turn
// that ended, an item handed on as often as it may be, a full inbox, a spent
// budget, a question already answered. Each reads as what it is, and one no
// change to the request can mend is not promised a change that can apply. A
// conflict with no code of its own is still a collision.
//
// Mutation: drop a code from personSentence, or give a settled refusal the
// hint that promises a change, and this goes red.
func TestARefusalThatIsNoCollisionIsNeverWordedAsOne(t *testing.T) {
	t.Parallel()
	collision := cards.PersonRefusal(wire.Refusal{Code: string(httpjson.CodeConflict)}, http.StatusConflict).Message
	for code, want := range map[httpjson.Code]struct {
		says    string
		settled bool
	}{
		httpjson.CodeNotesFull:          {"Send it again once it has read them", false},
		httpjson.CodeNotesSpent:         {"Put the rest on its work item", true},
		httpjson.CodeNotRunning:         {"not running any more", true},
		httpjson.CodeAlreadyAnswered:    {"already answered", true},
		httpjson.CodeReassignmentBudget: {"A person has to pick it up", false},
		httpjson.CodeInboxFull:          {"Mark everything up to here read", false},
		httpjson.CodeBudgetExhausted:    {"budget for this window is spent", false},
	} {
		got := cards.PersonRefusal(wire.Refusal{Code: string(code)}, http.StatusConflict).Message
		if got == collision || !strings.Contains(got, want.says) {
			t.Errorf("%s reads %q, want its own sentence saying %q", code, got, want.says)
		}
		hint := cards.RefusalHint(string(code))
		if settled := hint == cards.RefusalSettled; settled != want.settled ||
			(!want.settled && hint != cards.RefusalResumes) {
			t.Errorf("%s is hinted %q, want settled=%v", code, hint, want.settled)
		}
	}
	if !strings.Contains(collision, "same moment") || cards.RefusalHint(string(httpjson.CodeConflict)) != cards.RefusalResumes {
		t.Errorf("a collision reads %q and is hinted %q", collision, cards.RefusalHint(string(httpjson.CodeConflict)))
	}
}
