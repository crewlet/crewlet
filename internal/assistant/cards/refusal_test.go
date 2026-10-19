package cards_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
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

// A REFUSAL ON AUTHORITY SAYS ITS RULE: a refusal of each kind the authority
// table makes, answered as the engine answers it and read as the assistant
// reads it, reads to the person as the sentence the envelope says for that
// rule, with the rule and the permissions beside it, never as the one sentence
// every `unauthorized` was worded by, which told a person whose assistant never
// takes the verb, or may only propose the change, that they lacked access.
// Mutation: word a refusal by its code and status alone, or leave its reason
// behind, and every row fails.
func TestARefusalOnAuthoritySaysItsRule(t *testing.T) {
	t.Parallel()
	flat := cards.PersonRefusal(wire.Refusal{Code: string(httpjson.CodeUnauthorized)},
		http.StatusForbidden).Message
	for _, c := range []struct {
		reason authz.Reason
		grants []iam.Grant
	}{
		{authz.ReasonNoGrant, []iam.Grant{iam.GrantWorklet}},
		{authz.ReasonNotLead, []iam.Grant{iam.GrantAdmin}},
		{authz.ReasonStepUp, []iam.Grant{iam.GrantAdmin}},
		{authz.ReasonStage, nil},
		{authz.ReasonSeatOnly, nil},
		{authz.ReasonRole, []iam.Grant{iam.GrantAdmin}},
		{authz.ReasonCeiling, nil},
		{authz.ReasonAssistRefused, nil},
		{authz.ReasonAssistScope, nil},
	} {
		rec := httptest.NewRecorder()
		authz.EnvelopeRefusal(rec, nil, authz.Policy{},
			authz.Decision{Reason: c.reason, Grants: c.grants})
		var envelope struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s was answered %q, which is not the envelope: %v", c.reason,
				rec.Body.Bytes(), err)
		}
		got := cards.PersonRefusal(exec.RefusalOf(exec.Response{Status: rec.Code,
			Header: rec.Header(), Body: rec.Body.Bytes()}), rec.Code)
		names := make([]string, 0, len(c.grants))
		for _, g := range c.grants {
			names = append(names, string(g))
		}
		switch {
		case got.Message != envelope.Message || got.Message == flat:
			t.Errorf("%s reads %q to the person, want the envelope's %q", c.reason,
				got.Message, envelope.Message)
		case got.Reason != string(c.reason) || !slices.Equal(got.Grants, names):
			t.Errorf("%s reads with reason %q and grants %v, want %q and %v", c.reason,
				got.Reason, got.Grants, c.reason, names)
		case internalWords.MatchString(got.Message):
			t.Errorf("%s reads %q, which names the engine's internals", c.reason, got.Message)
		}
	}
}
