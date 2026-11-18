package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// principlesApp is an administrator's App over the parity company, whose
// active revision states a mission and a policy, and the turn's read grant.
func principlesApp(t *testing.T) (*assistApp, exec.Grant) {
	t.Helper()
	sources := parityWorld(t)
	fx := newAssistAppWith(t, []iam.Grant{iam.GrantAdmin}, sources.mount)
	return fx, fx.derive(t, readTurn())
}

// readOver is a proposal.Read over one grant of fx's dispatcher.
func readOver(fx *assistApp, g exec.Grant) proposal.Read {
	return func(ctx context.Context, r exec.Request) (exec.Response, error) {
		return fx.disp.Do(ctx, g, r)
	}
}

// preparePrinciples previews a principles change through the read grant and
// answers the request an approval of it sends: [proposal.RequestOf] of the
// proposal its preview draws, so the If-Match is the one the approval derives.
func preparePrinciples(t *testing.T, fx *assistApp, read exec.Grant,
	args map[string]any) (proposal.Preview, exec.Request) {

	t.Helper()
	tool, _ := catalog.Lookup("update_company_principles")
	p, err := tool.Preview(t.Context(), readOver(fx, read), time.Now(), args)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	return p, proposal.RequestOf(assistantdomain.Proposal{Method: p.Request.Method,
		Path: p.Request.Path, Query: p.Request.Query, Body: p.Request.Body,
		SourceRevision: p.SourceRevision})
}

// mission reads the active mission as the person.
func mission(t *testing.T, fx *assistApp) string {
	t.Helper()
	status, body := fx.asPerson(t, exec.Request{Method: http.MethodGet, Path: "/config/charter"})
	if status != http.StatusOK {
		t.Fatalf("GET /config/charter = %d %s", status, body)
	}
	var charter struct {
		Mission string `json:"mission"`
	}
	if err := json.Unmarshal(body, &charter); err != nil {
		t.Fatal(err)
	}
	return charter.Mission
}

// AN APPROVED PRINCIPLES CHANGE IS THE PAGE'S OWN WRITE, AND NOTHING MORE.
//
// The preview diffs the mission against the STORED one; the approval is `PUT
// /config/charter` under the proposal's If-Match. On an exec grant it lands;
// the read grant cannot send it; principles that moved meanwhile are refused
// (412), so nothing unseen is overwritten; a stale sign-in is asked to confirm
// (step-up) as at the keyboard; the whole company document patches the same
// way. Mutation: send the write without the derived If-Match (428), or put
// config.principles.write or config.org.write on the human-only list.
func TestAnApprovedPrinciplesChangeIsThePagesOwnWrite(t *testing.T) {
	t.Parallel()
	fx, read := principlesApp(t)
	before := mission(t, fx)
	p, req := preparePrinciples(t, fx, read, map[string]any{"mission": "Make every bill add up."})

	rows := slices.IndexFunc(p.Changes, func(c wire.Change) bool { return c.Field == "mission" })
	if rows < 0 || p.Changes[rows].Kind != wire.ChangeText || p.Changes[rows].From != before ||
		p.Changes[rows].To != "Make every bill add up." {
		t.Fatalf("the preview's rows are %+v, want the mission's word diff from %q", p.Changes,
			before)
	}
	if req.Method != http.MethodPut || req.Path != "/config/charter" || !exec.ValidEntityTag(req.IfMatch) {
		t.Fatalf("the approval would send %s %s under If-Match %q", req.Method, req.Path, req.IfMatch)
	}
	// THE THREE FIELDS AND NOTHING ELSE: the route's whole body is the
	// mission, the vision and the policies, so no proposal can carry a
	// change to any other part of the document.
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil || len(body) != 3 ||
		body["mission"] == nil || body["vision"] == nil || body["policies"] == nil ||
		len(body["policies"].([]any)) != 1 {
		t.Fatalf("the approval's body %s is not the three fields, with the policy it did "+
			"not change kept", req.Body)
	}

	if resp, err := fx.disp.Do(t.Context(), read, req); err != nil ||
		resp.Status != http.StatusForbidden {
		t.Errorf("the turn's read grant sent the write: %d %s, %v", resp.Status, resp.Body, err)
	}
	if got := mission(t, fx); got != before {
		t.Fatalf("a refused write changed the mission to %q", got)
	}

	exec1 := fx.derive(t, approvedFor(req, authz.ActionPrinciplesWrite))
	resp, err := fx.disp.Do(t.Context(), exec1, req)
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("the approved change answered %d %s, %v", resp.Status, resp.Body, err)
	}
	if got := mission(t, fx); got != "Make every bill add up." {
		t.Errorf("after the approved change the mission reads %q", got)
	}

	// STALE: prepared against the principles before the change above.
	stale := req
	stale.Body = []byte(`{"mission":"Something else.","vision":"",` +
		`"policies":["Money is an integer number of cents."]}`)
	resp, err = fx.disp.Do(t.Context(), fx.derive(t, approvedFor(stale,
		authz.ActionPrinciplesWrite)), stale)
	if err != nil || resp.Status != http.StatusPreconditionFailed ||
		exec.RefusalOf(resp).Code != string(httpjson.CodeVersionMoved) {
		t.Errorf("a change prepared against moved principles answered %d %s, %v, want 412 %s",
			resp.Status, resp.Body, err, httpjson.CodeVersionMoved)
	}
	if got := mission(t, fx); got != "Make every bill add up." {
		t.Errorf("a stale change overwrote the mission with %q", got)
	}

	// THE WHOLE DOCUMENT IS THE ADMINISTRATOR'S OWN THROUGH THEIR ASSISTANT
	// TOO (ADR-0052's parity): an approved patch of it, on the verb its route
	// is decided by, lands as their own save would, and the read grant
	// still cannot send it.
	whole := exec.Request{Method: http.MethodPatch, Path: "/config",
		Body: []byte(`{"mission":"Through the document.","_summary":"a mission"}`)}
	if resp, err := fx.disp.Do(t.Context(), read, whole); err != nil ||
		resp.Status != http.StatusForbidden {
		t.Errorf("the turn's read grant patched the document: %d %s, %v", resp.Status,
			resp.Body, err)
	}
	resp, err = fx.disp.Do(t.Context(), fx.derive(t, approvedFor(whole, authz.ActionOrgWrite)),
		whole)
	if err != nil || resp.Status != http.StatusCreated {
		t.Errorf("an approved patch of the document answered %d %s, %v", resp.Status,
			resp.Body, err)
	}
	if got := mission(t, fx); got != "Through the document." {
		t.Errorf("the mission reads %q after an approved patch of the document", got)
	}
}

// A PRINCIPLES CHANGE ASKS A STEP-UP OF A PERSON WHOSE SIGN-IN IS NOT RECENT,
// through the assistant exactly as at their keyboard: the row asks the
// step-up window, so the approved write is refused `step_up_required` and the
// principles stay as they are, and the preview's card says beforehand that
// the approval will ask. Mutation: give config.principles.write no recency and
// the write lands on an old sign-in.
func TestAPrinciplesChangeAsksAStepUp(t *testing.T) {
	t.Parallel()
	fx, read := principlesApp(t)
	before := mission(t, fx)
	p, req := preparePrinciples(t, fx, read, map[string]any{"mission": "Make every bill add up."})
	if !p.StepUp {
		t.Error("the principles change's card does not say its approval asks a step-up")
	}
	identity := fx.rows[fx.person]
	identity.Session.ProvedAt = time.Now().Add(-24 * time.Hour)
	fx.rows[fx.person] = identity
	g, err := deriveOrNil(fx, approvedFor(req, authz.ActionPrinciplesWrite))
	if err == nil {
		resp, sendErr := fx.disp.Do(t.Context(), g, req)
		if sendErr != nil || resp.Status != http.StatusForbidden ||
			exec.RefusalOf(resp).Code != string(httpjson.CodeStepUpRequired) {
			t.Errorf("an approved change on an old sign-in answered %d %s, %v, want 403 %s",
				resp.Status, resp.Body, sendErr, httpjson.CodeStepUpRequired)
		}
	}
	if got := mission(t, fx); got != before {
		t.Errorf("the mission changed to %q on an old sign-in", got)
	}
	if !strings.Contains(string(p.Request.Body), "Make every bill add up.") {
		t.Errorf("the preview's request %s is not the change", p.Request.Body)
	}
}

// deriveOrNil derives a grant through the App's guard, answering the refusal
// rather than failing the test on it.
func deriveOrNil(fx *assistApp, req auth.AssistRequest) (exec.Grant, error) {
	var g exec.Grant
	var err error
	h := fx.app.Guard().Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		g, err = fx.d.Derive(r.Context(), req)
	}))
	r := httptest.NewRequest(http.MethodPost, "/assistant/conversations", nil)
	r.AddCookie(fx.cookie)
	h.ServeHTTP(httptest.NewRecorder(), r)
	return g, err
}
