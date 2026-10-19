package configapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// admissionDoc is a company with a lead in it: Platform and the Tooling team
// inside it are the Platform lead's, and Design beside them is the designer's.
const admissionDoc = `
name: Acme
providers:
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: github
    command: github-mcp
roles:
  - name: CEO
    handle: ceo
    llm: zulu
units:
  - name: Platform
    id: platform
    lead: platform-lead
    roles:
      - name: Platform Lead
        handle: platform-lead
        kind: human
      - name: Staff Engineer
        handle: staff-eng
        llm: zulu
        mcp_env:
          github:
            GITHUB_TOKEN: ghp-literal-token
    children:
      - name: Tooling
        id: tooling
        purpose: build tools
        roles:
          - name: Toolsmith
            handle: toolsmith
            llm: zulu
  - name: Design
    id: design
    lead: designer
    purpose: make it lovely
    roles:
      - name: Designer
        handle: designer
        llm: zulu
`

// admissionCompany is admissionDoc with each edit applied to its text.
func admissionCompany(t *testing.T, edits ...[2]string) *config.Company {
	t.Helper()
	doc := admissionDoc
	for _, e := range edits {
		if !strings.Contains(doc, e[0]) {
			t.Fatalf("the document holds no %q to edit", e[0])
		}
		doc = strings.Replace(doc, e[0], e[1], 1)
	}
	company, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	return company
}

// The edits the cases make: one inside the lead's subtree, one outside it, and
// a credential, which is never a lead's.
var (
	insideEdit     = [2]string{"purpose: build tools", "purpose: build better tools"}
	outsideEdit    = [2]string{"purpose: make it lovely", "purpose: make it louder"}
	designerEdit   = [2]string{"handle: designer\n        llm: zulu", "handle: designer\n        goal: draw\n        llm: zulu"}
	credentialEdit = [2]string{"ghp-literal-token", "ghp-another-token"}
)

// A REFUSED CONFIGURATION WRITE SAYS THE RULE THAT REFUSED IT.
//
// The answer used to be the FIRST part's reason beside a permission written
// at the call site — `admin` — whatever had decided: an agent's seat was told
// it needed admin, a write refused for a relation and for a credential named
// the relation alone, and a lead whose proof had lapsed was handed every part
// of their write as needing admin rather than the `403 step_up_required` a
// client re-proves and replays on. The answer is now the write's own decision,
// folded from its parts ([authz.Combine]) and rendered by the table's own
// mapping ([authz.RefusalFields]), with every part a permission lifts listed
// under its own reason and grants.
//
// Mutations: name admin beside the first part's reason again and the mixed,
// the lapsed and the seat cases fail; list the parts refused only for their
// proof and the lapsed-beside-outside case lists two; skip the chart's verb
// on the whole document and an admin's seat is admitted.
func TestAnAdmissionRefusalSaysTheRuleThatDecidedIt(t *testing.T) {
	t.Parallel()
	fresh := time.Now().Add(time.Hour)
	lead := iam.Principal{ID: uuid.New(), Login: "pat.lead", Kind: iam.KindPerson,
		Stage: iam.StageActive, Seat: "platform-lead", ReauthAt: fresh}
	lapsed := lead
	lapsed.ReauthAt = time.Now().Add(-time.Minute)
	agent := iam.Principal{ID: uuid.New(), Kind: iam.KindSeat, Seat: "staff-eng",
		Stage: iam.StageActive, Grants: slices.Clone(iam.Apps), ReauthAt: fresh}
	adminSeat := agent
	adminSeat.Grants = []iam.Grant{iam.GrantAdmin}
	admin := []string{string(iam.GrantAdmin)}
	for _, tc := range []struct {
		name     string
		p        iam.Principal
		edits    [][2]string
		code     httpjson.Code
		reason   authz.Reason
		grants   []string
		refused  []string // kind/id/side/place/reason of every listed part, sorted
		window   bool
		admitted bool
	}{
		{name: "a lead inside their subtree", p: lead,
			edits: [][2]string{insideEdit}, admitted: true},
		{name: "a lead outside their subtree", p: lead,
			edits: [][2]string{outsideEdit}, code: httpjson.CodeUnauthorized,
			reason: authz.ReasonNotLead, grants: admin,
			refused: []string{"unit/design/after/design/not_lead",
				"unit/design/before/design/not_lead"}},
		{name: "a relation listed first and a credential after", p: lead,
			edits: [][2]string{designerEdit, credentialEdit},
			code:  httpjson.CodeUnauthorized, reason: authz.ReasonNoGrant, grants: admin,
			refused: []string{"seat/designer/after/design/not_lead",
				"seat/designer/before/design/not_lead", "seat/staff-eng///no_grant"}},
		{name: "a lapsed proof inside their subtree", p: lapsed,
			edits: [][2]string{insideEdit}, code: httpjson.CodeStepUpRequired,
			reason: authz.ReasonStepUp, grants: admin, window: true},
		{name: "a lapsed proof beside a part outside", p: lapsed,
			edits: [][2]string{insideEdit, outsideEdit}, code: httpjson.CodeUnauthorized,
			reason: authz.ReasonNotLead, grants: admin,
			refused: []string{"unit/design/after/design/not_lead",
				"unit/design/before/design/not_lead"}},
		{name: "an agent's seat", p: agent,
			edits: [][2]string{insideEdit, credentialEdit},
			code:  httpjson.CodeUnauthorized, reason: authz.ReasonSeatRefused,
			grants: []string{}},
		{name: "an agent's seat holding admin", p: adminSeat,
			edits: [][2]string{insideEdit}, code: httpjson.CodeUnauthorized,
			reason: authz.ReasonSeatRefused, grants: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := admit(context.Background(), tc.p, admissionCompany(t),
				admissionCompany(t, tc.edits...), false, false)
			if tc.admitted {
				if err != nil {
					t.Fatalf("admit = %v, want the write admitted", err)
				}
				return
			}
			var refused *AdmissionError
			if !errors.As(err, &refused) {
				t.Fatalf("admit = %v, want an admission refusal", err)
			}
			rec := httptest.NewRecorder()
			refuseAdmission(rec, refused)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			grants := []string{}
			for _, g := range asList(body[authz.DetailGrants]) {
				grants = append(grants, g.(string))
			}
			if rec.Code != http.StatusForbidden || body["error"] != string(tc.code) ||
				body[authz.DetailReason] != string(tc.reason) ||
				!slices.Equal(grants, tc.grants) {
				t.Errorf("answered %d %v, want 403 %s, reason %s, grants %v",
					rec.Code, body, tc.code, tc.reason, tc.grants)
			}
			if _, has := body[authz.DetailWindow]; has != tc.window {
				t.Errorf("the answer names a window: %v, want %v", has, tc.window)
			}
			var parts []string
			for _, item := range asList(body["refused"]) {
				r, _ := item.(map[string]any)
				parts = append(parts, strings.Join([]string{str(r["kind"]),
					str(r["id"]), str(r["side"]), str(r["place"]), str(r["reason"])}, "/"))
				// EACH PART NAMES ITS OWN REMEDY, beside the write's.
				if partGrants := asList(r["grants"]); len(partGrants) != 1 ||
					partGrants[0] != string(iam.GrantAdmin) {
					t.Errorf("the part %v names grants %v, want admin", r, r["grants"])
				}
			}
			slices.Sort(parts)
			if !slices.Equal(parts, tc.refused) {
				t.Errorf("listed %v, want %v", parts, tc.refused)
			}
			if _, hint := body["hint"]; hint != (len(tc.refused) > 0) {
				t.Errorf("the answer carries a lead's hint: %v, with %d parts listed",
					hint, len(tc.refused))
			}
		})
	}
}

func asList(v any) []any { list, _ := v.([]any); return list }

func str(v any) string { s, _ := v.(string); return s }
