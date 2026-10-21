package cards_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			case !cards.ForAPerson(got.Message):
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
		case !cards.ForAPerson(got.Message):
			t.Errorf("%s reads %q, which names the engine's internals", c.reason, got.Message)
		}
	}
}

// A RACE IS ONLY A RACE: a 409 or a 412 is said by its code. A write that lost
// to somebody else's reads that another change landed first; a collision and
// the state of what a write is about (in the trash, past the storage quota, a
// full inbox, handed on as often as it may be) each read a sentence of their
// own, never the race's, which sent a person to try again at what trying
// again never changes; and a code this build does not know reads as not
// accepted. Mutation: answer every 409 and 412 by its status, the race
// sentence, and every state and collision row fails.
func TestARaceIsOnlyARace(t *testing.T) {
	t.Parallel()
	race := "Another change landed at the same moment."
	for _, c := range []struct {
		code   string
		status int
	}{
		{"stale", http.StatusConflict},
		{"stale_version", http.StatusConflict},
		{"revision_advanced", http.StatusConflict},
		{"proposal_stale", http.StatusConflict},
		{"version_moved", http.StatusPreconditionFailed},
		{"revision_advanced", http.StatusPreconditionFailed},
	} {
		if got := cards.PersonRefusal(wire.Refusal{Code: c.code}, c.status).Message; got != race {
			t.Errorf("%s at %d, a lost race, reads %q, want %q", c.code, c.status, got, race)
		}
	}
	said := map[string]string{}
	for _, code := range []string{"conflict", "exists", "label_taken", "source_taken",
		"folder_name_taken", "already_answered", "reassignment_budget", "inbox_full",
		"not_running", "steer_unsupported", "budget_exhausted", "quota_exceeded",
		"artifact_trashed", "artifact_purging", "labels_full", "folder_limit", "folder_widens",
		"folder_cycle", "folder_depth", "folder_ambiguous", "carried_object_gone",
		"upload_missing", "upload_consumed", "upload_expired", "not_undoable",
		"a_code_from_a_newer_engine"} {
		got := cards.PersonRefusal(wire.Refusal{Code: code}, http.StatusConflict).Message
		switch {
		case got == race:
			t.Errorf("%s at 409 is not a race, and reads %q", code, got)
		case !cards.ForAPerson(got):
			t.Errorf("%s at 409 reads %q, which is not a sentence a person reads", code, got)
		}
		said[code] = got
	}
	// EACH STATE SAYS WHAT IT IS: one sentence for all of them would be the
	// status class again under another name.
	for _, pair := range [][2]string{{"artifact_trashed", "quota_exceeded"},
		{"inbox_full", "reassignment_budget"}, {"labels_full", "folder_limit"},
		{"conflict", "artifact_trashed"}} {
		if said[pair[0]] == said[pair[1]] {
			t.Errorf("%s and %s read alike: %q", pair[0], pair[1], said[pair[0]])
		}
	}
}

// A PERSON'S WORDS ARE READ WHERE THE ROUTE WROTE THEM FOR A PERSON: a state
// the route told in the words its refusal carries for a person (ADR-0060),
// marked by the tool's own sentence told beside it in process, reads in those
// words, which say what changes it as no sentence a code chooses can. Redacted,
// and held to the rule every sentence keeps: words naming the engine's
// internals give way to the code's sentence, and a detail nothing marked as a
// person's is never read. Mutation: drop the person's words and the first row
// fails; read them unchecked and the second does, unredacted the third, and
// any detail at all the last.
func TestAPersonsWordsAreReadWhereTheRouteMarkedThem(t *testing.T) {
	t.Parallel()
	trashed := "This artifact is in the trash. Restore it first, then try again."
	conflict := cards.PersonRefusal(wire.Refusal{Code: "conflict"}, http.StatusConflict).Message
	key := "sk-ant-" + strings.Repeat("A", 24)
	// answered is a conflict the route answered with detail, and with told
	// beside it in process as the dispatcher hands it back, "" for none.
	answered := func(detail, told string) wire.Refusal {
		rec := httptest.NewRecorder()
		httpjson.FailWithFields(rec, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": detail})
		return exec.RefusalOf(exec.Response{Status: rec.Code, Header: rec.Header(),
			Body: rec.Body.Bytes(), ModelDetail: told})
	}
	tool := "update_artifact refused that: the artifact is in the trash"
	for _, c := range []struct {
		name    string
		refusal wire.Refusal
		want    func(string) bool
	}{
		{"a state in a person's words", answered(trashed, tool),
			func(got string) bool { return got == trashed }},
		{"words naming the engine's internals",
			answered("Send `tags_add` to write_project.", tool),
			func(got string) bool { return got == conflict }},
		{"words carrying a credential", answered("The key "+key+" was refused.", tool),
			func(got string) bool { return !strings.Contains(got, key) && got != conflict }},
		{"a detail nothing marked as a person's", answered(trashed, ""),
			func(got string) bool { return got == conflict }},
	} {
		if got := cards.PersonRefusal(c.refusal, http.StatusConflict).Message; !c.want(got) {
			t.Errorf("%s reads %q", c.name, got)
		}
	}
}
