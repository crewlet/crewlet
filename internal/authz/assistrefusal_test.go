package authz_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE ASSISTANT IS TOLD WHY A HUMAN-ONLY GESTURE STAYS WITH ITS PERSON.
//
// A refusal of a verb on the human-only list is the engine's envelope with
// the `assist_refused` reason, and its `detail`, which the assistant's model
// reads to tell its person what they can do, is the sentence the list gives
// the verb's family: why it stays with the person and where they do it,
// naming no internal verb. Every verb on the list has one. Mutation: render
// the refusal without the sentence and this fails.
func TestAHumanOnlyRefusalSaysWhyInPlainWords(t *testing.T) {
	t.Parallel()
	p := iam.Principal{ID: uuid.New(), Login: "ana.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		Via: iam.AssistName(uuid.Must(uuid.NewV7())), AssistScope: iam.AssistScopeExec,
		ReauthAt: time.Now().Add(time.Hour)}
	for _, a := range authz.Actions() {
		if class, _ := authz.AssistOf(a); class != authz.AssistNever {
			continue
		}
		tell := authz.AssistNeverSentence(a)
		if tell == "" || strings.Contains(tell, string(a)) {
			t.Errorf("%s tells the person %q", a, tell)
		}
	}
	guard := func(*http.Request, authz.Policy) authz.Decision {
		return authz.Decide(t.Context(), p, authz.ActionSignInWrite, authz.Object{},
			authz.NoChart{}, time.Now())
	}
	rec := httptest.NewRecorder()
	if authz.Admit(rec, httptest.NewRequest(http.MethodPost, "/iam/invitations", nil), guard,
		authz.Policy{Action: authz.ActionSignInWrite}) {
		t.Fatal("an invitation was admitted through the assistant")
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body[authz.DetailReason] != string(authz.ReasonAssistRefused) ||
		body["detail"] != authz.AssistNeverSentence(authz.ActionSignInWrite) {
		t.Errorf("the refusal is %d %v", rec.Code, body)
	}
}
