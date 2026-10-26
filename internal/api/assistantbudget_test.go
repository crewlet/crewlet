package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
)

// seatRecord reads one seat's configuration as the person.
func seatRecord(t *testing.T, fx *assistApp, handle string) map[string]any {
	t.Helper()
	status, body := fx.asPerson(t, exec.Request{Method: http.MethodGet,
		Path: "/config/roles/" + handle})
	if status != http.StatusOK {
		t.Fatalf("GET /config/roles/%s = %d %s", handle, status, body)
	}
	var seat map[string]any
	if err := json.Unmarshal(body, &seat); err != nil {
		t.Fatal(err)
	}
	return seat
}

// AN APPROVED CEILING CHANGE IS BUDGETS' OWN WRITE, AND NOTHING MORE.
//
// Over the real configuration routes: set_token_budget's preview reads the
// seat through the turn's read grant, and the approval's request is the seat
// as read with only the one window's ceiling set, sent to PUT
// /config/roles/{handle} under the If-Match the proposal's revision names,
// exactly as Company › Budgets saves it. Carried on an exec grant bound to
// it, it lands and every other field of the seat reads as before; the read
// grant cannot send it; and a change prepared against a seat that moved since
// is refused by the route itself (412 version_moved), while one prepared
// before an unrelated seat changed still lands. The company's ceiling is a merge patch of
// the document's one window, which lands the same way. Mutation: build the
// body from the call instead of the seat as read and the seat loses its
// model; send it without the derived If-Match and the route refuses it.
func TestAnApprovedCeilingChangeIsBudgetsOwnWrite(t *testing.T) {
	t.Parallel()
	fx, read := principlesApp(t)
	before := seatRecord(t, fx, "cto")
	tool, _ := catalog.Lookup("set_token_budget")
	prepare := func(args map[string]any) exec.Request {
		t.Helper()
		p, err := tool.Preview(t.Context(), readOver(fx, read), time.Now(), args)
		if err != nil {
			t.Fatalf("preview %v: %v", args, err)
		}
		return proposal.RequestOf(assistantdomain.Proposal{Method: p.Request.Method,
			Path: p.Request.Path, Query: p.Request.Query, Body: p.Request.Body,
			SourceRevision: p.SourceRevision})
	}
	req := prepare(map[string]any{"seat": "cto", "window": "day", "tokens": float64(1500000)})
	if req.Method != http.MethodPut || req.Path != "/config/roles/cto" ||
		!exec.ValidEntityTag(req.IfMatch) {
		t.Fatalf("the approval would send %s %s under If-Match %q", req.Method, req.Path,
			req.IfMatch)
	}
	if resp, err := fx.disp.Do(t.Context(), read, req); err != nil ||
		resp.Status != http.StatusForbidden {
		t.Errorf("the turn's read grant sent the write: %d %s, %v", resp.Status, resp.Body, err)
	}

	resp, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(req, authz.ActionOrgWrite)), req)
	if err != nil || resp.Status/100 != 2 {
		t.Fatalf("the approved change answered %d %s, %v", resp.Status, resp.Body, err)
	}
	after := seatRecord(t, fx, "cto")
	budget, _ := after["token_budget"].(map[string]any)
	if budget["day"] != float64(1500000) {
		t.Errorf("the seat's ceilings read %v after the approved change", after["token_budget"])
	}
	for field, value := range before {
		if field == "token_budget" {
			continue
		}
		got, _ := json.Marshal(after[field])
		want, _ := json.Marshal(value)
		if string(got) != string(want) {
			t.Errorf("the seat's %s moved from %s to %s, and the change was a ceiling", field,
				want, got)
		}
	}

	// STALE: prepared before the change above, against the seat as it was.
	resp, err = fx.disp.Do(t.Context(), fx.derive(t, approvedFor(req, authz.ActionOrgWrite)), req)
	if err != nil || resp.Status != http.StatusPreconditionFailed ||
		exec.RefusalOf(resp).Code != string(httpjson.CodeVersionMoved) {
		t.Errorf("a change prepared against a moved seat answered %d %s, %v, want 412 %s",
			resp.Status, resp.Body, err, httpjson.CodeVersionMoved)
	}

	// AN UNRELATED CHANGE IS NO CONFLICT: a ceiling prepared on the CEO's
	// seat lands although the CTO's changed after it was read, because the
	// precondition is the seat it edits.
	ceo := prepare(map[string]any{"seat": "ceo", "window": "week", "tokens": float64(7000000)})
	other := prepare(map[string]any{"seat": "cto", "window": "week", "tokens": float64(9000000)})
	if resp, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(other,
		authz.ActionOrgWrite)), other); err != nil || resp.Status/100 != 2 {
		t.Fatalf("the CTO's ceiling answered %d %s, %v", resp.Status, resp.Body, err)
	}
	if resp, err := fx.disp.Do(t.Context(), fx.derive(t, approvedFor(ceo,
		authz.ActionOrgWrite)), ceo); err != nil || resp.Status/100 != 2 {
		t.Errorf("a ceiling prepared before an unrelated seat changed answered %d %s, %v, "+
			"want it written", resp.Status, resp.Body, err)
	}

	// THE COMPANY'S CEILING, as a merge patch of the one window.
	company := prepare(map[string]any{"window": "month", "tokens": float64(90000000)})
	if company.Method != http.MethodPatch || company.Path != "/config" {
		t.Fatalf("the company's ceiling would be sent as %s %s", company.Method, company.Path)
	}
	resp, err = fx.disp.Do(t.Context(), fx.derive(t, approvedFor(company, authz.ActionOrgWrite)),
		company)
	if err != nil || resp.Status/100 != 2 {
		t.Fatalf("the approved company ceiling answered %d %s, %v", resp.Status, resp.Body, err)
	}
	status, body := fx.asPerson(t, exec.Request{Method: http.MethodGet, Path: "/config"})
	var doc struct {
		TokenBudget map[string]any `json:"token_budget"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &doc) != nil ||
		doc.TokenBudget["month"] != float64(90000000) {
		t.Errorf("the company's ceilings read %s after the approved change", body)
	}
}
