package queries_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/named"
)

// referenceSources is a registry whose record questions are stand-ins that say
// what they were asked and by whom.
func referenceSources(t *testing.T) (*queries.Registry, *[]string) {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{})
	var asked []string
	answer := func(what string, body func(p queries.Params) (any, error)) queries.Answer {
		return func(ctx context.Context, p queries.Params) (any, error) {
			who, _ := iam.From(ctx)
			asked = append(asked, fmt.Sprintf("%s %v as %s", what, p.Values(), who.Seat))
			return body(p)
		}
	}
	held := func(v any) func(queries.Params) (any, error) {
		return func(queries.Params) (any, error) { return v, nil }
	}
	r.Register("work_item", queries.Everybody(), answer("work_item", func(p queries.Params) (any, error) {
		if p.String("id") != "0198-task" {
			return nil, queries.ErrNotFound
		}
		return map[string]any{"task": map[string]any{"key": "ENG-287", "title": "Rotate keys"}}, nil
	}))
	r.Register("code_change", queries.Everybody(), answer("code_change", func(p queries.Params) (any, error) {
		if p.String("name") != "web" || fmt.Sprint(p.Values()["change"]) != "42" {
			return nil, queries.ErrNotFound
		}
		return map[string]any{"change": map[string]any{"ref": "web!42", "title": "Fix login"}}, nil
	}))
	r.Register("page", queries.Everybody(), answer("page", func(p queries.Params) (any, error) {
		if p.String("id") == "secret-page" {
			return nil, errors.Join(queries.ErrUnauthorized)
		}
		return map[string]any{"page": map[string]any{"title": "Runbook"}}, nil
	}))
	r.Register("artifact", queries.Everybody(), answer("artifact", func(p queries.Params) (any, error) {
		if p.String("id") == "binned" {
			return nil, queries.ErrNotFound
		}
		return map[string]any{"title": "Launch plan"}, nil
	}))
	r.Register("turn", queries.Everybody(), answer("turn", func(queries.Params) (any, error) {
		return nil, errors.New("the store could not be read")
	}))
	r.Register("schedules", queries.Everybody(), answer("schedules", held(map[string]any{
		"schedules": []any{map[string]any{"scope_type": "role", "scope_id": "dev", "name": "standup"}}})))
	return r, &asked
}

// WHAT A PAGE'S LINES NAME IS RESOLVED IN ONE ASK, each reference through the
// question its object's own page asks, AS THE CALLER: a work item, a change, a
// page and an artifact the caller may open come back with their titles; one
// refused and one gone are both absent, never told apart; one the node could
// not settle is named as unresolved, so a screen asks again rather than
// drawing it closed; a list read for two of its entries is read once.
//
// Mutations: answer a refusal as open and the secret page is linked; fold a
// node's failure into "closed" and the turn is never asked again.
func TestAPagesReferencesAreResolvedAsTheCallerInOneAsk(t *testing.T) {
	t.Parallel()
	r, asked := referenceSources(t)
	got, err := r.Answer(asAna(t), "references", map[string]any{"refs": []any{
		"work_item:0198-task", "code_change:web!42", "page:runbook", "artifact:plan",
		"page:secret-page", "artifact:binned", "work_item:0198-gone", "turn:turn-1",
		"schedule:role/dev/standup", "schedule:role/dev/retro", "work_item:0198-task",
	}})
	body := chatJSON(t, got, err)
	want := []any{
		map[string]any{"kind": "work_item", "id": "0198-task", "title": "Rotate keys"},
		map[string]any{"kind": "code_change", "id": "web!42", "title": "Fix login"},
		map[string]any{"kind": "page", "id": "runbook", "title": "Runbook"},
		map[string]any{"kind": "artifact", "id": "plan", "title": "Launch plan"},
		map[string]any{"kind": "schedule", "id": "role/dev/standup"},
	}
	if !reflect.DeepEqual(body["references"], want) {
		t.Errorf("open references = %v\nwant %v", body["references"], want)
	}
	if !reflect.DeepEqual(body["unresolved"], []any{"turn:turn-1"}) {
		t.Errorf("unresolved = %v, want the turn whose read failed", body["unresolved"])
	}
	schedules := 0
	for _, a := range *asked {
		if a == "schedules map[] as ana" {
			schedules++
		}
		if a[len(a)-len(" as ana"):] != " as ana" {
			t.Errorf("%q was not asked as the caller", a)
		}
	}
	if schedules != 1 {
		t.Errorf("the schedules were read %d times for two of their entries, want once: %v", schedules, *asked)
	}
}

// ONE ASK IS BOUNDED, and names only what this build resolves.
func TestAReferencesAskIsBoundedAndNamesKnownKinds(t *testing.T) {
	t.Parallel()
	r, _ := referenceSources(t)
	many := make([]any, queries.MaxReferences+1)
	for i := range many {
		many[i] = fmt.Sprintf("work_item:T-%d", i)
	}
	for name, refs := range map[string][]any{
		"too many":     many,
		"none":         {},
		"unknown kind": {"galaxy:42"},
		"no id":        {"work_item:"},
		"not kind:id":  {"ENG-287"},
		"not a string": {42},
	} {
		if _, err := r.Answer(asAna(t), "references", map[string]any{"refs": refs}); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s: answered %v, want bad params", name, err)
		}
	}
}

// THE DASHBOARD NAMES WHAT THE ENGINE NAMES: its kinds (`contract/named.ts`'s
// NAMED_REF_KINDS) are named.Kinds, in the engine's order, and its batch is cut at
// this question's bound, so a kind the engine names is never drawn as words
// and an ask is never refused for its size.
func TestTheDashboardNamesWhatTheEngineNames(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	body, err := clientsource.Literal(tree, "NAMED_REF_KINDS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	engine := make([]string, 0, len(named.Kinds))
	for _, k := range named.Kinds {
		engine = append(engine, string(k))
	}
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard's NAMED_REF_KINDS is %q and the engine's is %q: change contract/named.ts", client, engine)
	}
	raw, err := clientsource.Scalar(tree, "MAX_REFERENCES")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := strconv.Atoi(raw); err != nil || n != queries.MaxReferences {
		t.Errorf("the dashboard cuts an ask at %q references and the engine answers at most %d",
			raw, queries.MaxReferences)
	}
}
