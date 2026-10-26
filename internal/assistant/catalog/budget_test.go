package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// budgetRoutes answers the configuration surface as Budgets reads and checks
// it: the QA seat and the company under strong tags, and a check that warns
// when a seat's daily ceiling rises past the company's.
type budgetRoutes struct {
	t    *testing.T
	mu   sync.Mutex
	sent []exec.Request
}

func (b *budgetRoutes) read(_ context.Context, r exec.Request) (exec.Response, error) {
	b.mu.Lock()
	b.sent = append(b.sent, r)
	b.mu.Unlock()
	answer := func(tag string, v any) exec.Response {
		raw, _ := json.Marshal(v)
		return exec.Response{Status: http.StatusOK, Header: http.Header{"Etag": {tag}}, Body: raw}
	}
	switch {
	case r.Method == http.MethodGet && r.Path == "/config/roles/qa":
		return answer(`"rev-9"`, map[string]any{"handle": "qa", "name": "QA Engineer",
			"backstory": "Reads tests before code.", "token_budget": map[string]any{"day": 1000000}}), nil
	case r.Method == http.MethodGet && r.Path == "/config":
		return answer(`"rev-9"`, map[string]any{"name": "FairSplit",
			"token_budget": map[string]any{"day": 10000000, "month": 120000000}}), nil
	case r.Method == http.MethodGet:
		return exec.Response{Status: http.StatusNotFound, Body: []byte(`{"error":"not_found",` +
			`"message":"There is no roles called nobody in the active revision."}`)}, nil
	case r.Query.Get(authz.DryRunQuery) != "true":
		b.t.Errorf("the preview sent %s %s, a write", r.Method, r.Path)
		return exec.Response{Status: http.StatusInternalServerError}, nil
	}
	warnings := []any{}
	if strings.Contains(string(r.Body), `"day":25000000`) {
		warnings = append(warnings, map[string]any{"message": "qa's daily ceiling sits " +
			"above the company's"})
	}
	raw, _ := json.Marshal(map[string]any{"valid": true, "warnings": warnings})
	return exec.Response{Status: http.StatusOK, Body: raw}, nil
}

func (b *budgetRoutes) checks() []exec.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []exec.Request
	for _, r := range b.sent {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// A SEAT'S CEILING IS CHANGED AS BUDGETS CHANGES IT.
//
// The QA run: "Set the QA Engineer's daily token budget to 1.5M" became a whole
// seat replaced through the generic change, its every field on the card. The
// change is the seat as read with only that window moved, sent where Budgets
// sends it (PUT /config/roles/qa, conditional on the tag it was read at), its
// card one row from the ceiling now to the new one, its history line the
// card's, its step-up stated. Mutation: send a seat built from the call
// alone, or bind no revision, and a case here fails.
func TestASeatsCeilingIsChangedAsBudgetsChangesIt(t *testing.T) {
	t.Parallel()
	tool := mustTool(t, "set_token_budget")
	routes := &budgetRoutes{t: t}
	p, err := tool.Preview(t.Context(), routes.read, wednesday, map[string]any{
		"seat": "qa", "window": "day", "tokens": float64(1500000)})
	if err != nil {
		t.Fatal(err)
	}
	if p.Request.Method != http.MethodPut || p.Request.Path != "/config/roles/qa" {
		t.Fatalf("the change is sent as %s %s, want the seat's own write", p.Request.Method,
			p.Request.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(p.Request.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["backstory"] != "Reads tests before code." ||
		body["token_budget"].(map[string]any)["day"] != float64(1500000) {
		t.Errorf("the seat is sent back as %v, want it as read with the one ceiling moved", body)
	}
	if body[catalog.SummaryKey] != "Raise QA Engineer's daily token budget from 1,000,000 "+
		"tokens to 1,500,000 tokens" {
		t.Errorf("the history line is %q", body[catalog.SummaryKey])
	}
	got := rows(p)
	row := got["token_budget.day"]
	if row.Label != "QA Engineer's daily token budget" || row.From != "1,000,000 tokens" ||
		row.To != "1,500,000 tokens" {
		t.Errorf("the card's row is %+v", row)
	}
	fields := 0
	for field := range got {
		if !strings.HasPrefix(field, "check.") && !strings.HasPrefix(field, "request.") {
			fields++
		}
	}
	if fields != 1 {
		t.Errorf("the card shows %d field rows, want the one window: %v", fields, got)
	}
	r, err := proposal.ParseRevision(p.SourceRevision)
	if err != nil || r.Kind != proposal.RevisionEntity || r.ID != "/config/roles/qa" ||
		r.Version != "rev-9" {
		t.Errorf("the change is bound to %q, want the seat at its tag", p.SourceRevision)
	}
	if !p.StepUp || p.ApproveLabel != "Approve and save" || p.Target != "QA Engineer" {
		t.Errorf("the card is %+v", p)
	}
}

// A CEILING THE ROUTE WOULD WARN ABOUT SAYS SO ON THE CARD.
//
// The route's own check is asked of the exact change, under the tag it was
// read at, before any card is drawn, and its warning is a row. Mutation: skip
// the check and the card is silent about a ceiling above the company's.
func TestABudgetChangeCarriesTheRoutesWarning(t *testing.T) {
	t.Parallel()
	routes := &budgetRoutes{t: t}
	p, err := mustTool(t, "set_token_budget").Preview(t.Context(), routes.read, wednesday,
		map[string]any{"seat": "qa", "window": "day", "tokens": float64(25000000)})
	if err != nil {
		t.Fatal(err)
	}
	checks := routes.checks()
	if len(checks) != 1 || checks[0].IfMatch != `"rev-9"` ||
		string(checks[0].Body) != string(p.Request.Body) {
		t.Fatalf("the route was asked %+v, want the change checked under its tag", checks)
	}
	if w := rows(p)["check.warning.1"]; !strings.Contains(w.To, "above the company's") {
		t.Errorf("the route's warning is %+v on the card", w)
	}
}

// THE COMPANY'S CEILING IS A MERGE PATCH OF THE ONE WINDOW, and removing it
// patches the window to null, as Budgets writes it.
func TestTheCompanysCeilingIsAMergePatchOfTheOneWindow(t *testing.T) {
	t.Parallel()
	routes := &budgetRoutes{t: t}
	p, err := mustTool(t, "set_token_budget").Preview(t.Context(), routes.read, wednesday,
		map[string]any{"window": "month", "clear": true})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(p.Request.Body, &body); err != nil {
		t.Fatal(err)
	}
	budget, _ := body["token_budget"].(map[string]any)
	if p.Request.Method != http.MethodPatch || p.Request.Path != "/config" || len(body) != 2 ||
		len(budget) != 1 || budget["month"] != nil {
		t.Errorf("the company's ceiling is sent as %s %s %s", p.Request.Method, p.Request.Path,
			p.Request.Body)
	}
	if _, present := budget["month"]; !present {
		t.Error("the patch leaves the window out, which removes nothing")
	}
	row := rows(p)["token_budget.month"]
	if row.Label != "The company's monthly token budget" || row.From != "120,000,000 tokens" ||
		p.Title != "Remove a token budget" {
		t.Errorf("the card is %q with the row %+v", p.Title, row)
	}
}

// A CALL THAT CANNOT BE A CHANGE IS NEVER A CARD: both a value and a removal,
// neither, the ceiling it already is, or a seat the route does not hold.
func TestABudgetCallThatChangesNothingIsNeverACard(t *testing.T) {
	t.Parallel()
	tool := mustTool(t, "set_token_budget")
	for name, args := range map[string]map[string]any{
		"tokens and clear":  {"seat": "qa", "window": "day", "tokens": float64(5), "clear": true},
		"neither":           {"seat": "qa", "window": "day"},
		"the same ceiling":  {"seat": "qa", "window": "day", "tokens": float64(1000000)},
		"a window it lacks": {"seat": "qa", "window": "year", "tokens": float64(5)},
	} {
		if _, err := tool.Request(args); err == nil {
			if _, err = tool.Preview(t.Context(), (&budgetRoutes{t: t}).read, wednesday,
				args); !errors.Is(err, catalog.ErrInvalidArguments) {
				t.Errorf("%s previewed %v", name, err)
			}
		}
	}
	var refused *catalog.PreviewRefusal
	_, err := tool.Preview(t.Context(), (&budgetRoutes{t: t}).read, wednesday,
		map[string]any{"seat": "nobody", "window": "day", "tokens": float64(5)})
	if !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
		t.Errorf("a seat the route does not hold previewed %v, want its refusal", err)
	}
}

// ONLY AN ADMINISTRATOR IS OFFERED IT, and it is decided by the route.
func TestABudgetChangeIsOfferedToAnAdministrator(t *testing.T) {
	t.Parallel()
	offered := func(p iam.Principal) bool {
		for _, tool := range catalog.Offered(p, nil) {
			if tool.Name == "set_token_budget" {
				return true
			}
		}
		return false
	}
	if !offered(admin) {
		t.Error("an administrator is not offered set_token_budget")
	}
	member := admin
	member.Grants = []iam.Grant{iam.GrantWorklet}
	if offered(member) {
		t.Error("a member with no administration is offered set_token_budget")
	}
}
