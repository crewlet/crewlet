package catalog_test

import (
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
)

// widening are the shapes of a query parameter that asks for more, or for the
// same differently, rather than for less: finished, archived, removed or
// snoozed work included, a transcript, a previous window, a page, an order, a
// grouping or a view.
var widening = regexp.MustCompile(`^(show_.*|include_.*|with_.*|archived|removed|snoozed|` +
	`closed.*|history|previous|limit|cursor|after|before.*|around|sort|group.*|subgroup|` +
	`view|preset|fields|totals|counts|read_level|mode|roots|subtasks)$`)

// narrowedValue is a value of a parameter that asks for less than its default:
// an enum's last member, a list of one, the shortest window, a flag set, or
// words more specific than any a test passes by default.
func narrowedValue(prop map[string]any) any {
	switch kind, _ := prop["type"].(string); {
	case prop["enum"] != nil:
		enum := prop["enum"].([]string)
		return enum[len(enum)-1]
	case kind == "integer" || kind == "number":
		lo, _ := prop["minimum"].(int)
		return float64(lo)
	case kind == "boolean":
		return true
	case kind == "array":
		items, _ := prop["items"].(map[string]any)
		return []any{narrowedValue(items)}
	default:
		return "brand identity kit"
	}
}

// changed is every parameter whose value differs between two queries.
func changed(a, b url.Values) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(b)) {
		if !slices.Equal(a[name], b[name]) {
			out = append(out, name)
		}
	}
	for name := range a {
		if _, kept := b[name]; !kept {
			out = append(out, name)
		}
	}
	return out
}

// EVERY NARROWING ARGUMENT NARROWS. What a read too large to show offers as
// narrowing is an explicit list, never every argument but a few: each
// [catalog.Tool.Narrow] entry is one of the tool's arguments, and setting it
// changes the request the read sends in at least one of its variants, and in
// every variant only by query parameters the one list of narrowing parameters
// holds, none of which has a widening shape. A generic read offers the route's
// documented parameters that list holds, so the task list's `show_closed`
// and `closed_since` and the projects' `archived` are never offered.
// Mutation: derive the list from the schema again, or add show_closed to
// list_tasks' Narrow, and this fails.
func TestEveryNarrowingArgumentNarrows(t *testing.T) {
	t.Parallel()
	for name := range catalog.NarrowingQuery {
		if widening.MatchString(name) {
			t.Errorf("narrowingQuery holds %s, which widens, pages or orders a read", name)
		}
	}
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindRead || tool.Infer {
			continue
		}
		props, _ := tool.Parameters["properties"].(map[string]any)
		variants := slices.Sorted(maps.Keys(tool.Cards))
		for _, arg := range tool.Narrow {
			if _, ok := props[arg]; !ok {
				t.Errorf("%s narrows by %s, which it does not take", tool.Name, arg)
				continue
			}
			narrows := false
			for _, v := range variants {
				base := variantArgs(t, tool, v)
				before, err := tool.Request(base)
				if err != nil {
					t.Fatalf("%s/%s: %v", tool.Name, v, err)
				}
				args := maps.Clone(base)
				prop, _ := props[arg].(map[string]any)
				args[arg] = narrowedValue(prop)
				after, err := tool.Request(args)
				if err != nil {
					t.Logf("%s/%s with %s=%v: %v", tool.Name, v, arg, args[arg], err)
					continue
				}
				diff := changed(before.Query, after.Query)
				for _, param := range diff {
					if !catalog.NarrowingQuery[param] {
						t.Errorf("%s/%s narrows by %s, which sends %s, no narrowing parameter",
							tool.Name, v, arg, param)
					}
				}
				narrows = narrows || len(diff) > 0
			}
			if !narrows {
				t.Errorf("%s narrows by %s, which changes no request it sends", tool.Name, arg)
			}
		}
	}
	for tool, never := range map[string]string{"list_tasks": "show closed"} {
		if got := catalog.Narrowing(mustTool(t, tool), ""); slices.Contains(got, never) {
			t.Errorf("%s offers %v as narrowing", tool, got)
		}
	}
	table, err := catalog.NewRouteTable([]catalog.Route{
		{Pattern: "GET /worklet", Action: authz.ActionWorkRead},
		{Pattern: "POST /worklet/items", Action: authz.ActionWorkCreate},
	})
	if err != nil {
		t.Fatal(err)
	}
	generic := catalog.GenericTools(table, admin, nil)[1]
	for route, want := range map[string][]string{
		"GET /worklet":          {"assignee", "status", "the search words"},
		"GET /worklet/projects": {"the search words", "unit"},
		"GET /events":           {"work key", "trace id", "since"},
	} {
		got := catalog.Narrowing(generic, route)
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("a generic read of %s offers %v, without %s", route, got, w)
			}
		}
		for _, words := range got {
			if widening.MatchString(strings.ReplaceAll(words, " ", "_")) {
				t.Errorf("a generic read of %s offers %s, which widens", route, words)
			}
		}
	}
}
