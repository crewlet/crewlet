package api_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// actingAssistApp is the assistant's App with the act transport mounted,
// auditing into one record.
func actingAssistApp(t *testing.T) (*assistApp, *recordedAudit) {
	t.Helper()
	audit := &recordedAudit{}
	operators, err := operator.New(operator.Options{
		Halves: func() (operator.Halves, bool) {
			return operator.Halves{Work: builtin.WorkDeps{
				Reader: stubWorkReader{},
				Writer: func(builtin.Actor) builtin.WorkWriter { return auditWork{} },
			}}, true
		},
		Authorize: builtin.Decide(authz.NoChart{}),
		Audit:     audit,
	})
	if err != nil {
		t.Fatalf("operator.New: %v", err)
	}
	fx := newAssistAppWith(t, []iam.Grant{iam.GrantWorklet}, func(opts *api.Options) {
		opts.Operator, opts.Audit = operators, audit
	})
	return fx, audit
}

// createCall is the act request a create_task proposal sends.
func createCall() exec.Request {
	return exec.Request{Method: http.MethodPost,
		Path:           operator.ActPathPrefix + worklet.CreateWorkItemTool,
		Body:           []byte(`{"args":{"title":"Rotate the signing key","project":"ENG"}}`),
		IdempotencyKey: uuid.Must(uuid.NewV7()).String()}
}

// approvedFor is an exec grant request bound to call, approved for act.
func approvedFor(call exec.Request, act authz.Action) auth.AssistRequest {
	req := approved(call)
	req.Action = act
	return req
}

// AN ASSISTED WRITE IS AUDITED AS THE ASSISTANT: the person is the author, as
// every record the write left names them; the credential is the grant; the
// transport is the assistant's; and the reference names the conversation,
// turn, grant, session and proposal that carried it, with the conversation
// promoted to a listing tag. Mutation: drop the reference from Acted and the
// row cannot be found by its conversation.
func TestAnAssistedWriteIsAuditedAsTheAssistant(t *testing.T) {
	t.Parallel()
	fx, audit := actingAssistApp(t)
	call := createCall()
	req := approvedFor(call, authz.ActionWorkCreate)
	g := fx.derive(t, req)
	resp, err := fx.disp.Do(t.Context(), g, call)
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("the approved write answered %d %s, %v", resp.Status, resp.Body, err)
	}
	_, recorded := audit.take()
	if len(recorded) != 1 {
		t.Fatalf("an approved write left %d audit records", len(recorded))
	}
	body := decodeFlat(t, recorded[0])
	ref, _ := body["assistant"].(map[string]any)
	switch {
	case body["transport"] != types.TransportAssistant:
		t.Errorf("transport %v, want %s", body["transport"], types.TransportAssistant)
	case body["operator_id"] != iam.AssistName(g.ID()):
		t.Errorf("credential %v, want the grant %s", body["operator_id"], iam.AssistName(g.ID()))
	case body["actor"] != assistSeat || body["actor_kind"] != string(iam.ActorHuman):
		t.Errorf("author %v (%v), want the person's seat %s", body["actor"], body["actor_kind"], assistSeat)
	case ref == nil:
		t.Fatal("the record names no assistant reference")
	case ref["conversation_id"] != req.Conversation.String() || ref["turn_id"] != req.Turn.String() ||
		ref["grant_id"] != g.ID().String() || ref["proposal_id"] != req.Proposal.String() ||
		!strings.HasPrefix(ref["session"].(string), iam.SessionPrefix):
		t.Errorf("the reference is %v", ref)
	case body["conversation_id"] != req.Conversation.String():
		t.Errorf("the conversation is not a listing field: %v", body["conversation_id"])
	}
}

// THE ACT ROUTE CARRIES ONLY THE VERB A GRANT WAS APPROVED FOR: a grant whose
// approval was for one verb is refused another (so a human-only verb is never
// carried by an approval of anything else), and the refusal is audited under
// the assistant's transport. Mutation: drop the action check in the act
// route and the update is written.
func TestAnAssistedWriteTakesOnlyTheVerbItWasApprovedFor(t *testing.T) {
	t.Parallel()
	fx, audit := actingAssistApp(t)
	call := exec.Request{Method: http.MethodPost,
		Path:           operator.ActPathPrefix + worklet.UpdateWorkItemTool,
		Body:           []byte(`{"args":{"id":"ENG-1","title":"Something else"}}`),
		IdempotencyKey: uuid.Must(uuid.NewV7()).String()}
	g := fx.derive(t, approvedFor(call, authz.ActionWorkCreate))
	resp, err := fx.disp.Do(t.Context(), g, call)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusForbidden || !strings.Contains(string(resp.Body),
		string(authz.ReasonAssistRefused)) {
		t.Fatalf("a grant approved for %s carried %s: %d %s", authz.ActionWorkCreate,
			worklet.UpdateWorkItemTool, resp.Status, resp.Body)
	}
	_, recorded := audit.take()
	if len(recorded) != 1 {
		t.Fatalf("the refusal left %d audit records, want one", len(recorded))
	}
	body := decodeFlat(t, recorded[0])
	if body["transport"] != types.TransportAssistant || body["outcome"] != string(types.AuditRefused) ||
		body["assistant"] == nil {
		t.Errorf("the refusal was audited as %v %v with reference %v", body["transport"],
			body["outcome"], body["assistant"])
	}
}

// A READING TURN CANNOT WRITE: a turn's read grant is refused the act route
// before any tool runs, and nothing is recorded as written. Mutation: let the
// guard's read scope take a POST and the write lands.
func TestAReadingTurnCannotWrite(t *testing.T) {
	t.Parallel()
	fx, audit := actingAssistApp(t)
	g := fx.derive(t, readTurn())
	resp, err := fx.disp.Do(t.Context(), g, createCall())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusForbidden || !strings.Contains(string(resp.Body),
		string(authz.ReasonAssistScope)) {
		t.Errorf("a read grant's write answered %d %s, want 403 %s", resp.Status, resp.Body,
			authz.ReasonAssistScope)
	}
	if _, recorded := audit.take(); len(recorded) != 0 {
		t.Errorf("a refused read grant left %d audit records of a write", len(recorded))
	}
}

// THE ASSISTANT CANNOT APPROVE ITS OWN PROPOSAL, by two locks: no exec grant is
// ever derived for a decision route, and the turn's own read grant presented to
// one is refused at the guard's edge, before routing. Mutation: drop
// `/assistant/` from the refused edges and the read grant reaches the route.
func TestAnAssistGrantNeverReachesADecisionRoute(t *testing.T) {
	t.Parallel()
	fx, _ := actingAssistApp(t)
	proposal := uuid.Must(uuid.NewV7()).String()
	digest := `{"protocol_version":1,"digest":"sha256:` + strings.Repeat("a", 64) + `"}`
	read := fx.derive(t, readTurn())
	for _, call := range []exec.Request{
		{Method: http.MethodPost, Path: "/assistant/proposals/" + proposal + "/approve",
			Body: []byte(digest)},
		{Method: http.MethodPost, Path: "/assistant/proposals/" + proposal + "/decline",
			Body: []byte(digest)},
		{Method: http.MethodPut, Path: "/assistant/actions/" + proposal,
			Body: []byte(`{"protocol_version":1,"state":"applying","revision":"r1"}`)},
	} {
		call.IdempotencyKey = uuid.Must(uuid.NewV7()).String()
		if err := fx.tryDerive(approvedFor(call, authz.ActionAssistantApprove)); !errors.Is(err, auth.ErrAssistRefused) ||
			!strings.Contains(err.Error(), "never reaches") {
			t.Errorf("an exec grant for %s %s was not refused for its path: %v", call.Method,
				call.Path, err)
		}
		resp, err := fx.disp.Do(t.Context(), read, call)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != http.StatusForbidden ||
			!strings.Contains(string(resp.Body), string(authz.ReasonAssistRefused)) {
			t.Errorf("an assistant grant's %s %s answered %d %s", call.Method, call.Path,
				resp.Status, resp.Body)
		}
	}
}

// tryDerive derives a grant through the App's own guard from the person's
// cookie, answering the refusal rather than failing the test.
func (a *assistApp) tryDerive(req auth.AssistRequest) error {
	var err error
	h := a.app.Guard().Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, err = a.d.Derive(r.Context(), req)
	}))
	r := httptest.NewRequest(http.MethodPost, "/assistant/conversations", nil)
	r.AddCookie(a.cookie)
	h.ServeHTTP(httptest.NewRecorder(), r)
	return err
}

// A REFUSED ASSISTED WRITE TELLS THE MODEL THE TOOL'S SENTENCE, AND NEVER THE
// ANSWER (ADR-0060). The act route answers a person: a refusal's detail is a
// person's phrasing, or none. The assistant's model is the one reader that can
// act on the tool's own sentence, so the route tells it in process
// (internal/modelnote) and the dispatcher hands it back beside the answer as
// [exec.Response.ModelDetail]; the same request from a browser carries the
// person's words alone. Mutation: drop the route's Tell and the model is told
// only the floor; write the sentence into the answer and the browser reads it.
func TestARefusedAssistedWriteTellsTheModelAndNeverTheAnswer(t *testing.T) {
	t.Parallel()
	fx, _ := actingAssistApp(t)
	call := createCall()
	call.Body = []byte(`{"args":{"title":" ","project":"ENG"}}`)
	g := fx.derive(t, approvedFor(call, authz.ActionWorkCreate))
	resp, err := fx.disp.Do(t.Context(), g, call)
	if err != nil || resp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("a create with no title answered %d %s, %v", resp.Status, resp.Body, err)
	}
	if !strings.Contains(resp.ModelDetail, "needs a `title`") {
		t.Errorf("the model was told %q, want the tool's own sentence", resp.ModelDetail)
	}
	if strings.Contains(string(resp.Body), "`title`") ||
		!strings.Contains(string(resp.Body), caveat.TitleMissing().Person()) {
		t.Errorf("the answer is %s, want the person's words and never the model's", resp.Body)
	}

	// THE SAME REFUSAL OVER THE WIRE, as the dashboard sends it: nothing of
	// the model's sentence, and nothing else to hear it.
	browser := httptest.NewRequest(http.MethodPost, call.Path, strings.NewReader(string(call.Body)))
	browser.AddCookie(fx.cookie)
	browser.Header.Set("Content-Type", "application/json")
	browser.Header.Set("Origin", fx.b.API.ExternalURL)
	browser.Header.Set(opkey.Header, uuid.Must(uuid.NewV7()).String())
	rec := httptest.NewRecorder()
	fx.app.ServeHTTP(rec, browser)
	if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "`title`") ||
		strings.Contains(rec.Body.String(), worklet.CreateWorkItemTool+" needs") {
		t.Errorf("the browser's refusal answered %d %s", rec.Code, rec.Body)
	}
}
