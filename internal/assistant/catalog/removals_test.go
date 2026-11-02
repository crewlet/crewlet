package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
)

// replacingRoutes are a unit's read and its replacing write, as the
// configuration surface mounts them.
var replacingRoutes = []catalog.Route{
	{Pattern: "GET /config/units/{id}", Action: authz.ActionOrgRead},
	{Pattern: "PUT /config/units/{id}", Action: authz.ActionOrgWrite,
		Absent: authz.AddedElsewhere(unitsElsewhere), DryRun: true, Summary: true,
		Replaces: true},
	{Pattern: "PATCH /config", Action: authz.ActionOrgWrite, DryRun: true, Summary: true},
}

// placedUnit is the unit as its read answers it: placed in a project and a
// space, with two seats.
func placedUnit() map[string]any {
	return map[string]any{"id": "core", "name": "Product", "project": "FAIR", "space": "FAIR",
		"goals": []any{"Ship the MVP"},
		"roles": []any{
			map[string]any{"handle": "pm", "name": "Product Manager"},
			map[string]any{"handle": "qa", "name": "QA Engineer", "goal": "Review tests"},
		}}
}

// placedRoute answers the unit's read, admits every check, and fails the test
// on a write: a preview never writes.
func placedRoute(t *testing.T) func(context.Context, exec.Request) (exec.Response, error) {
	return func(_ context.Context, r exec.Request) (exec.Response, error) {
		switch {
		case r.Method == http.MethodGet && r.Path == "/config/units/core":
			body, _ := json.Marshal(placedUnit())
			h := http.Header{}
			h.Set("ETag", `"rev-7"`)
			return exec.Response{Status: http.StatusOK, Header: h, Body: body}, nil
		case r.Method == http.MethodGet:
			return exec.Response{Status: http.StatusNotFound,
				Body: []byte(`{"error":"not_found"}`)}, nil
		case r.Query.Get(authz.DryRunQuery) != "true":
			t.Errorf("the preview sent %s %s, a write", r.Method, r.Path)
			return exec.Response{Status: http.StatusInternalServerError}, nil
		}
		return exec.Response{Status: http.StatusOK, Body: []byte(`{"valid":true}`)}, nil
	}
}

func replacingChange(t *testing.T) catalog.Tool {
	t.Helper()
	table, err := catalog.NewRouteTable(replacingRoutes)
	if err != nil {
		t.Fatal(err)
	}
	return catalog.GenericTools(table, admin, nil)[2]
}

// withAnalyst is the unit as the model rebuilt it to add a seat, with edit
// applied to it first.
func withAnalyst(edit func(map[string]any)) map[string]any {
	u := placedUnit()
	u["roles"] = append(u["roles"].([]any),
		map[string]any{"handle": "analyst", "name": "Data Analyst"})
	if edit != nil {
		edit(u)
	}
	return u
}

func unitArgs(body map[string]any, removes ...string) map[string]any {
	args := map[string]any{"route": "PUT /config/units/{id}",
		"path": map[string]any{"id": "core"}, "body": body,
		"summary": "Add a Data Analyst seat"}
	if len(removes) > 0 {
		list := make([]any, 0, len(removes))
		for _, r := range removes {
			list = append(list, r)
		}
		args["removes"] = list
	}
	return args
}

// A REPLACE NEVER REMOVES BY OMISSION (ADR-0055).
//
// The browser case: a unit rebuilt to add a seat, its project and space left
// out, is refused before any card in words naming both, so the model keeps
// them. Named in `removes`, the same change is a card that leads with what it
// removes. Adding a seat and nothing else removes nothing; dropping a seat
// on purpose is declared by the seat's path; a path named that the change
// does not remove is refused; and a route that does not replace takes no
// `removes`. The unit's own `id`, which its path names, is the route's to
// fill, and every removal at fault, of either kind, is one refusal. Mutation:
// skip [removalCheck], compare against the body rather than the read, or stop
// at the first fault, and a case here fails.
func TestAReplaceNeverRemovesByOmission(t *testing.T) {
	t.Parallel()
	change := replacingChange(t)
	read := placedRoute(t)
	refusal := func(err error) string {
		t.Helper()
		var refused *catalog.PreviewRefusal
		if !errors.As(err, &refused) {
			t.Fatalf("previewed %v, want a refusal", err)
		}
		return refused.Refusal.Code + ": " + refused.Refusal.Message + " " + refused.Refusal.Hint
	}

	dropped := withAnalyst(func(u map[string]any) { delete(u, "project"); delete(u, "space") })
	_, err := change.Preview(t.Context(), read, wednesday, unitArgs(dropped))
	if got := refusal(err); !strings.Contains(got, "undeclared_removal") ||
		!strings.Contains(got, "would remove project and space from unit · core") ||
		!strings.Contains(got, "Include them in the body to keep them") {
		t.Errorf("a unit dropping its project and space was refused with %q", got)
	}

	p, err := change.Preview(t.Context(), read, wednesday, unitArgs(dropped, "project", "space"))
	if err != nil {
		t.Fatalf("a declared removal: %v", err)
	}
	if !slices.Equal(p.Removes, []string{"Project", "Space"}) {
		t.Errorf("the card warns it removes %v, want Project and Space", p.Removes)
	}
	if got := rows(p)["request.removes"]; got.Label != "Removes" || got.To != "Project, Space" {
		t.Errorf("the removal row is %+v", got)
	}

	p, err = change.Preview(t.Context(), read, wednesday, unitArgs(withAnalyst(nil)))
	if err != nil {
		t.Fatalf("an added seat: %v", err)
	}
	if len(p.Removes) != 0 {
		t.Errorf("adding a seat warns it removes %v", p.Removes)
	}

	noID := withAnalyst(func(u map[string]any) { delete(u, "id") })
	if _, err = change.Preview(t.Context(), read, wednesday, unitArgs(noID)); err != nil {
		t.Errorf("leaving out the id the path names was refused: %v", err)
	}

	withoutQA := withAnalyst(func(u map[string]any) {
		u["roles"] = slices.DeleteFunc(u["roles"].([]any), func(r any) bool {
			return r.(map[string]any)["handle"] == "qa"
		})
	})
	_, err = change.Preview(t.Context(), read, wednesday, unitArgs(withoutQA))
	if got := refusal(err); !strings.Contains(got, "would remove roles.qa from") {
		t.Errorf("a dropped seat was refused with %q", got)
	}
	p, err = change.Preview(t.Context(), read, wednesday, unitArgs(withoutQA, "roles.qa"))
	if err != nil || !slices.Equal(p.Removes, []string{"QA Engineer"}) ||
		p.Risk != wire.RiskDestructive || p.Title != "Change unit Product" ||
		!strings.HasPrefix(p.Summary, "This removes seat QA Engineer from unit ") {
		t.Errorf("a seat removed on purpose previewed %v: %q, %q at risk %q, warning %v", err,
			p.Title, p.Summary, p.Risk, p.Removes)
	}

	emptied := withAnalyst(func(u map[string]any) { u["goals"] = []any{} })
	_, err = change.Preview(t.Context(), read, wednesday, unitArgs(emptied))
	if got := refusal(err); !strings.Contains(got, "would remove goals from") {
		t.Errorf("an emptied list was refused with %q", got)
	}

	_, err = change.Preview(t.Context(), read, wednesday, unitArgs(withAnalyst(nil), "lead"))
	if got := refusal(err); !strings.Contains(got, "names lead, which this change does not remove") {
		t.Errorf("a removal named but not made was refused with %q", got)
	}
	// BOTH FAULTS, EVERY PATH OF EACH, ARE ONE REFUSAL (ADR-0059): a removal
	// the call does not declare beside two it declares and does not make.
	noProject := withAnalyst(func(u map[string]any) { delete(u, "project") })
	_, err = change.Preview(t.Context(), read, wednesday, unitArgs(noProject, "lead", "owner"))
	if got := refusal(err); !strings.Contains(got, "would remove project from") ||
		!strings.Contains(got, "names lead and owner, neither of which this change removes") ||
		!strings.Contains(got, "Include them in the body") ||
		!strings.Contains(got, "List in `removes` only") {
		t.Errorf("an undeclared removal beside two unmade ones was refused with %q", got)
	}

	_, err = change.Preview(t.Context(), read, wednesday, map[string]any{
		"route": "PATCH /config", "body": map[string]any{"vision": "Fair"},
		"removes": []any{"vision"}})
	if got := refusal(err); !strings.Contains(got, "does not replace a whole object") {
		t.Errorf("`removes` on a merge was refused with %q", got)
	}
}
