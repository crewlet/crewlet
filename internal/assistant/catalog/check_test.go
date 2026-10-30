package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/org"
)

// checkedRoutes are a unit's read and its write as the configuration surface
// mounts them: the write answers `dry_run=true` and records a summary.
var checkedRoutes = []catalog.Route{
	{Pattern: "GET /config/units/{id}", Action: authz.ActionOrgRead},
	{Pattern: "PUT /config/units/{id}", Action: authz.ActionOrgWrite,
		Absent: authz.AddedElsewhere(unitsElsewhere), DryRun: true, Summary: true},
}

// unknownField is the route's refusal of a seat field it does not take.
const unknownField = "units[0].roles[1].tool_sources: unknown field: \"tool_sources\" " +
	"isn't a setting"

// unitRoute is the unit's read and its check, as the route answers them: the
// unit under a strong tag, a check refusing a seat with `tool_sources`, and a
// warning on any other. Every request is kept; a write without the flag fails
// the test, because a preview never writes.
type unitRoute struct {
	t    *testing.T
	mu   sync.Mutex
	sent []exec.Request
}

func (u *unitRoute) read(_ context.Context, r exec.Request) (exec.Response, error) {
	u.mu.Lock()
	u.sent = append(u.sent, r)
	u.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.Path == "/config/units/core":
		body, _ := json.Marshal(map[string]any{"id": "core", "name": "Product",
			"roles": []any{map[string]any{"handle": "pm", "name": "Product Manager"}}})
		h := http.Header{}
		h.Set("ETag", `"rev-3"`)
		return exec.Response{Status: http.StatusOK, Header: h, Body: body}, nil
	case r.Method != http.MethodPut:
		return exec.Response{Status: http.StatusNotFound,
			Body: []byte(`{"error":"not_found"}`)}, nil
	case r.Query.Get(authz.DryRunQuery) != "true":
		u.t.Errorf("the preview sent %s %s?%s, a write", r.Method, r.Path, r.Query.Encode())
		return exec.Response{Status: http.StatusInternalServerError}, nil
	case strings.Contains(string(r.Body), "tool_sources"):
		return exec.Response{Status: http.StatusBadRequest, Body: []byte(`{"error":` +
			`"validation_error","message":"The document is not valid.","detail":` +
			mustJSON(unknownField) + `}`)}, nil
	}
	return exec.Response{Status: http.StatusOK, Body: []byte(`{"valid":true,` +
		`"warnings":[{"kind":"advisory","introduced":true,"message":"pm's daily ceiling ` +
		`sits above the company's"},{"kind":"advisory","introduced":false,"seat":"miles",` +
		`"message":"miles is a human seat with no chat contact"}]}`)}, nil
}

func (u *unitRoute) checks() []exec.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []exec.Request
	for _, r := range u.sent {
		if r.Method == http.MethodPut {
			out = append(out, r)
		}
	}
	return out
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// unitChange is the generic change over checkedRoutes, for an administrator.
func unitChange(t *testing.T) catalog.Tool {
	t.Helper()
	table, err := catalog.NewRouteTable(checkedRoutes)
	if err != nil {
		t.Fatal(err)
	}
	return catalog.GenericTools(table, admin, nil)[2]
}

// seatBody is the unit with one seat added, carrying extra beside it.
func seatBody(extra map[string]any) map[string]any {
	qa := map[string]any{"handle": "qa", "name": "QA Engineer"}
	for k, v := range extra {
		qa[k] = v
	}
	return map[string]any{"id": "core", "name": "Product", "roles": []any{
		map[string]any{"handle": "pm", "name": "Product Manager"}, qa}}
}

// A CHANGE THE ROUTE WOULD REFUSE IS NEVER A CARD (ADR-0055).
//
// A route that answers `dry_run=true` is asked it before any card is drawn,
// with the exact request and the precondition the approval would send: a body
// it refuses (a field the unit does not take) is the step's refusal in the
// route's own words, and nothing is proposed; a body it admits is a card whose
// rows carry the warnings the check says this change introduces, never one the
// company already carried. A preview never writes. Mutation: skip the
// check, or send it without the flag or the If-Match, and a case here fails.
func TestAChangeTheRouteWouldRefuseIsNeverACard(t *testing.T) {
	t.Parallel()
	change := unitChange(t)
	route := &unitRoute{t: t}
	_, err := change.Preview(t.Context(), route.read, wednesday, map[string]any{
		"route": "PUT /config/units/{id}", "path": map[string]any{"id": "core"},
		"body": seatBody(map[string]any{"tool_sources": []any{"builtin"}})})
	var refused *catalog.PreviewRefusal
	if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest ||
		refused.Refusal.Code != "validation_error" ||
		!strings.Contains(refused.Refusal.ForModel(), "tool_sources: unknown field") {
		t.Fatalf("a body the route refuses previewed %v, want its refusal", err)
	}

	route = &unitRoute{t: t}
	p, err := change.Preview(t.Context(), route.read, wednesday, map[string]any{
		"route": "PUT /config/units/{id}", "path": map[string]any{"id": "core"},
		"body": seatBody(nil), "summary": "Add a QA Engineer seat to Product"})
	if err != nil {
		t.Fatal(err)
	}
	checks := route.checks()
	if len(checks) != 1 || checks[0].IfMatch != `"rev-3"` ||
		checks[0].Query.Get(authz.DryRunQuery) != "true" ||
		string(checks[0].Body) != string(p.Request.Body) {
		t.Fatalf("the route was asked %+v, want one check of the proposed request under "+
			"the tag it was shown against", checks)
	}
	if p.Request.Query.Has(authz.DryRunQuery) {
		t.Errorf("the proposal would send %s, a check, when the person approves a write",
			p.Request.Query.Encode())
	}
	got := rows(p)
	if w := got["check.warning.1"]; w.Label != "Warning" ||
		!strings.Contains(w.To, "above the company's") {
		t.Errorf("the check's warning is %+v on the card", w)
	}
	// ONLY WHAT THIS CHANGE INTRODUCES: a warning the company already
	// carried is not news about this change.
	for field, row := range got {
		if strings.Contains(row.To, "miles") {
			t.Errorf("row %s carries a warning the change did not introduce: %+v", field, row)
		}
	}
	if got["roles.qa.name"].To != "QA Engineer" {
		t.Errorf("the added seat's rows are %+v", got)
	}
}

// A WRITE THAT KEEPS A HISTORY CARRIES THE LINE THE PERSON APPROVED.
//
// The summary is in the body, which the request digest binds, and on the card
// as its own row, never as a changed field: the call's `summary`, else one
// the body states, else one naming the change from its route. A call naming
// `dry_run` is refused, since what the person approves is the write.
// Mutation: carry it in a header, diff it as a field, or default nothing, and
// a case here fails.
func TestAWriteThatKeepsAHistoryCarriesTheLineThePersonApproved(t *testing.T) {
	t.Parallel()
	change := unitChange(t)
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"the call's": {map[string]any{"body": seatBody(nil),
			"summary": "Add a QA Engineer seat"}, "Add a QA Engineer seat"},
		"the body's": {map[string]any{"body": func() map[string]any {
			b := seatBody(nil)
			b[catalog.SummaryKey] = "Stated in the body"
			return b
		}()}, "Stated in the body"},
		"the route's": {map[string]any{"body": seatBody(nil)},
			"Change unit · core through Ask Crewlet"},
	} {
		args := c.args
		args["route"] = "PUT /config/units/{id}"
		args["path"] = map[string]any{"id": "core"}
		route := &unitRoute{t: t}
		p, err := change.Preview(t.Context(), route.read, wednesday, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var body map[string]any
		if err := json.Unmarshal(p.Request.Body, &body); err != nil ||
			body[catalog.SummaryKey] != c.want {
			t.Errorf("%s: the request carries the summary %v (%v), want %q", name,
				body[catalog.SummaryKey], err, c.want)
		}
		got := rows(p)
		if got["request.summary"].To != c.want || got["request.summary"].Label != "History note" {
			t.Errorf("%s: the card's history row is %+v", name, got["request.summary"])
		}
		if _, diffed := got[catalog.SummaryKey]; diffed {
			t.Errorf("%s: the summary is drawn as a changed field", name)
		}
	}

	one, err := change.Request(map[string]any{"route": "PUT /config/units/{id}",
		"path": map[string]any{"id": "core"}, "body": seatBody(nil), "summary": "One"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := change.Request(map[string]any{"route": "PUT /config/units/{id}",
		"path": map[string]any{"id": "core"}, "body": seatBody(nil), "summary": "Other"})
	if err != nil {
		t.Fatal(err)
	}
	a, errA := proposal.RequestDigest(one)
	b, errB := proposal.RequestDigest(other)
	if errA != nil || errB != nil || a == b {
		t.Errorf("two summaries digest alike (%v, %v): the history line is not bound", errA, errB)
	}

	if _, err := change.Preview(t.Context(), (&unitRoute{t: t}).read, wednesday,
		map[string]any{"route": "PUT /config/units/{id}", "path": map[string]any{"id": "core"},
			"query": map[string]any{"dry_run": "true"}, "body": seatBody(nil)}); !errors.Is(err,
		catalog.ErrInvalidArguments) {
		t.Errorf("a change naming dry_run previewed: %v", err)
	}
}

// A CREATE'S CARD NAMES WHERE IT LANDS, OR IS NO CARD.
//
// A task or a page whose call names no project or container is shown, and
// sent, with the person's own home as GET /viewer answers it, through the one
// derivation the write applies; a person whose team owns none is refused at
// the preview in the write's own words, never shown a placeholder the write
// then refuses. Mutation: draw "Your team's space" again, or leave the
// resolved value out of the request, and this fails.
func TestACreatesCardNamesWhereItLandsOrIsNoCard(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		tool, arg, field, refusal string
		args                      map[string]any
	}{
		{"create_page", "container", "space", org.NoHomeSpace("write_page"),
			map[string]any{"title": "Brand guide", "body": "Colours."}},
		{"create_task", "project", "project", org.NoHomeProject("create_work_item"),
			map[string]any{"title": "File the report"}},
	} {
		tool, ok := catalog.Lookup(c.tool)
		if !ok {
			t.Fatalf("%s is not in the catalogue", c.tool)
		}
		world := assistanttest.World()
		world["/viewer"] = map[string]any{"handle": "pm", c.field: ""}
		_, err := tool.Preview(t.Context(), reader(world), wednesday, c.args)
		if !errors.Is(err, catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), c.refusal) {
			t.Errorf("%s with no home previewed %v, want the write's refusal %q", c.tool, err,
				c.refusal)
		}

		world["/viewer"] = map[string]any{"handle": "pm", c.field: "BRAND"}
		p, err := tool.Preview(t.Context(), reader(world), wednesday, c.args)
		if err != nil {
			t.Fatalf("%s: %v", c.tool, err)
		}
		if got := rows(p)[c.arg].To; got != "BRAND" {
			t.Errorf("%s's card names %q, want the resolved BRAND", c.tool, got)
		}
		var body struct {
			Args map[string]any `json:"args"`
		}
		if err := json.Unmarshal(p.Request.Body, &body); err != nil || body.Args[c.arg] != "BRAND" {
			t.Errorf("%s sends %s %v (%v), want the place the card names", c.tool, c.arg,
				body.Args[c.arg], err)
		}
	}
}
