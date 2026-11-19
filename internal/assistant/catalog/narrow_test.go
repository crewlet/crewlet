package catalog_test

import (
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

// EVERY NARROWING PARAMETER NARROWS. What a read too large to show offers as
// narrowing is an explicit list, none of which has a widening shape, and a read
// offers the route's documented parameters that list holds, so the task list's
// `show_closed` and the projects' `archived` are never offered. Mutation: add
// show_closed to the list and this fails.
func TestEveryNarrowingArgumentNarrows(t *testing.T) {
	t.Parallel()
	for name := range catalog.NarrowingQuery {
		if widening.MatchString(name) {
			t.Errorf("narrowingQuery holds %s, which widens, pages or orders a read", name)
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
			if widening.MatchString(strings.ReplaceAll(words, " ", "_")) || words == "show closed" {
				t.Errorf("a generic read of %s offers %s, which widens", route, words)
			}
		}
	}
}
