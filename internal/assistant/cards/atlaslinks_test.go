package cards_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

const atlasEntryID = "0b6e1a52-3c4d-4e5f-8a9b-0c1d2e3f4a5b"

// A MAP LINK CARRIES ITS QUERY, AND THE QUERY IS THE TABLE'S: the map opened on
// an entry carries the frame's peek key with the entry's id, the map lit along
// a trace carries the trace's direction as well, each value held to its
// grammar, and the link reads "Show on the map" however opaque the id. Read
// back, each address names the screen that wrote it and no other: a map
// address with a peek alone is not the trace screen's, and one with a key the
// table never writes is nobody's. Mutation: drop the query from Link, or match
// a template by its path alone in ScreenOf, and this fails.
func TestAMapLinkCarriesTheQueryTheTableWrites(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		screen string
		params map[string]string
		href   string
	}{
		{"atlas", map[string]string{}, "#/company/atlas"},
		{"atlas-entry", map[string]string{"id": atlasEntryID}, "#/company/atlas/" + atlasEntryID},
		{"atlas-entry-on-map", map[string]string{"id": atlasEntryID},
			"#/company/atlas?peek=atlas%3A" + atlasEntryID},
		{"atlas-trace-on-map", map[string]string{"id": atlasEntryID, "direction": "up"},
			"#/company/atlas?peek=atlas%3A" + atlasEntryID + "&trace=up"},
	} {
		link, err := cards.Link(tc.screen, tc.params, "")
		if err != nil {
			t.Fatalf("%s: %v", tc.screen, err)
		}
		if link.Href != tc.href || link.Screen != tc.screen {
			t.Errorf("%s links %q as %s, want %q", tc.screen, link.Href, link.Screen, tc.href)
		}
		if strings.Contains(tc.screen, "on-map") && link.Label != "Show on the map" {
			t.Errorf("%s reads %q, want the map's own words", tc.screen, link.Label)
		}
		if len(tc.params) == 0 {
			continue
		}
		screen, params, ok := cards.ScreenOf(link.Href)
		if !ok || screen != tc.screen || !mapsEqual(params, tc.params) {
			t.Errorf("%s read back as %s %v (%v), want itself with %v", link.Href, screen, params,
				ok, tc.params)
		}
	}
	for _, bad := range []struct {
		screen string
		params map[string]string
	}{
		{"atlas-trace-on-map", map[string]string{"id": atlasEntryID, "direction": "sideways"}},
		{"atlas-entry-on-map", map[string]string{"id": "web-1"}},
		{"atlas", map[string]string{"id": atlasEntryID}},
	} {
		if _, err := cards.Link(bad.screen, bad.params, ""); !errors.Is(err, cards.ErrNoLink) {
			t.Errorf("%s with %v linked: %v", bad.screen, bad.params, err)
		}
	}
	for _, stray := range []string{
		"#/company/atlas?peek=seat%3Apm",
		"#/company/atlas?peek=atlas%3A" + atlasEntryID + "&trace=sideways",
		"#/company/atlas?peek=atlas%3A" + atlasEntryID + "&filter=x",
		"#/company/atlas/" + atlasEntryID + "?peek=atlas%3A" + atlasEntryID,
	} {
		if screen, _, ok := cards.ScreenOf(stray); ok {
			t.Errorf("%s read back as %s; the table writes no such query", stray, screen)
		}
	}
	if got := cards.ScreenParams("atlas-trace-on-map"); !slices.Equal(got, []string{"id", "direction"}) {
		t.Errorf("the trace screen takes %v, want its path's then its query's parameters", got)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// AN ENTITY'S ITEMS EXPAND AS A TABLE'S ROWS DO: a list whose entries each
// hold a list (a trace's hops, each holding the entries it reached) is listed
// entry by entry, each reading its parent's fields under `parent`, so a hop's
// distance indents the entries it reached. Mutation: drop Expand from
// buildEntity and the card lists two hops in place of three entries.
func TestAnEntitysItemsExpandLikeRows(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceAtlasEntry, ID: "from.id",
		Title: "from.name",
		Items: &cards.Items{Path: "hops", Expand: "entries", Kind: wire.ResourceAtlasEntry,
			ID: "id", Label: "name", Fact: "kind", Depth: "parent.distance",
			Noun: wire.Noun{One: "dependent", Many: "dependents"}}}}
	answer, _ := json.Marshal(map[string]any{
		"from": map[string]any{"id": atlasEntryID, "name": "orders-db"},
		"hops": []any{
			map[string]any{"distance": 1, "entries": []any{
				map[string]any{"id": "a", "name": "checkout-api", "kind": "service"},
				map[string]any{"id": "b", "name": "billing", "kind": "service"}}},
			map[string]any{"distance": 2, "entries": []any{
				map[string]any{"id": "c", "name": "web-1", "kind": "host"}}},
		}})
	built, err := cards.Build(spec, answer, evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	items := built.Card.Entity.Items
	if len(items) != 3 {
		t.Fatalf("the card lists %d items, want the three entries the hops reached: %+v", len(items), items)
	}
	for i, want := range []wire.EntityItem{
		{Kind: wire.ResourceAtlasEntry, ID: "a", Label: "checkout-api", Fact: "service", Depth: 1},
		{Kind: wire.ResourceAtlasEntry, ID: "b", Label: "billing", Fact: "service", Depth: 1},
		{Kind: wire.ResourceAtlasEntry, ID: "c", Label: "web-1", Fact: "host", Depth: 2},
	} {
		if items[i] != want {
			t.Errorf("item %d is %+v, want %+v", i, items[i], want)
		}
	}
	if !slices.ContainsFunc(spec.Paths(), func(p cards.Path) bool {
		return p.From == "item" && p.Path == "entries"
	}) {
		t.Error("the spec's paths do not name the expanded list, so no walk holds it to an answer")
	}
}
