package catalog_test

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/atlas"
)

// AN ATLAS CHANGE THE TOOL WOULD REFUSE IS NEVER A CARD (ADR-0055): a fact the
// settings declare, an entry somebody else's read cannot find, an entry the
// atlas holds already, and a purge of what the settings declare are each
// refused at the preview, where the model can read why; a purge that can land
// asks a fresh sign-in and says it cannot be undone.
//
// Mutation: drop the declared check from correctAtlasPreview and the first
// case draws a card.
func TestAnAtlasChangeTheToolWouldRefuseIsNeverACard(t *testing.T) {
	t.Parallel()
	world := assistanttest.World()
	declared := maps.Clone(world)
	entry := maps.Clone(world["/atlas/"+assistanttest.AtlasServiceID].(map[string]any))
	shown := maps.Clone(entry["entry"].(map[string]any))
	shown["facts"] = []any{map[string]any{"fact": "environment",
		"value":  map[string]any{"type": "word", "text": "production"},
		"source": map[string]any{"kind": "settings"}, "word": "confirmed", "standing": "settled"}}
	shown["sources"] = []any{map[string]any{"kind": "settings"}}
	entry["entry"] = shown
	declared["/atlas/"+assistanttest.AtlasServiceID] = entry
	// AN ENTRY THE ATLAS HOLDS ALREADY, at the id its kind and key make.
	held := maps.Clone(world)
	held["/atlas/"+atlas.MustRef(atlas.KindService, "checkout-api").ID().String()] =
		world["/atlas/"+assistanttest.AtlasServiceID]
	for _, c := range []struct {
		tool  string
		world map[string]any
		args  map[string]any
	}{
		{"correct_atlas_fact", declared, map[string]any{"atlas_entry": assistanttest.AtlasServiceID,
			"fact": "environment", "value": "staging"}},
		{"retire_atlas_entry", declared, map[string]any{"atlas_entry": assistanttest.AtlasServiceID}},
		{"purge_atlas_entry", declared, map[string]any{"atlas_entry": assistanttest.AtlasServiceID}},
		{"retire_atlas_entry", world, map[string]any{"atlas_entry": "7e8f9a0b-1c2d-4e3f-8a4b-5c6d7e8f9a0b"}},
		{"add_atlas_entry", held, map[string]any{"kind": "service", "key": "checkout-api"}},
		{"add_atlas_entry", world, map[string]any{"kind": "tool_server", "key": "mcp/tracker"}},
		{"restore_atlas_entry", world, map[string]any{"atlas_entry": assistanttest.AtlasServiceID}},
		{"confirm_atlas_entry", world, map[string]any{"atlas_entry": assistanttest.AtlasServiceID,
			"fact": "environment"}},
		{"confirm_atlas_entry", world, map[string]any{"atlas_entry": assistanttest.AtlasServiceID,
			"fact": "name", "relation": "runs_on"}},
		{"correct_atlas_fact", world, map[string]any{"atlas_entry": assistanttest.AtlasServiceID,
			"fact": "purpose", "value": "gh" + "p_" + strings.Repeat("a", 36)}},
		{"set_atlas_owner", world, map[string]any{"atlas_entry": assistanttest.AtlasServiceID,
			"seat": "qa", "clear": true}},
		{"set_atlas_owner", world, map[string]any{"atlas_entry": assistanttest.AtlasServiceID,
			"seat": "nobody"}},
	} {
		tool, ok := catalog.Lookup(c.tool)
		if !ok {
			t.Fatalf("the catalogue holds no %s", c.tool)
		}
		_, err := tool.Preview(t.Context(), reader(c.world), wednesday, c.args)
		if !errors.Is(err, catalog.ErrInvalidArguments) && !errors.Is(err, catalog.ErrPreviewRead) {
			t.Errorf("%s %v drew a card (%v), want it refused before one", c.tool, c.args, err)
		}
		// A REFUSAL NAMES AN ENTRY BY ITS KIND AND KEY, never by its name,
		// which a source or another person wrote, nor a value it refused.
		if err != nil && (strings.Contains(err.Error(), "Checkout API") || strings.Contains(err.Error(), "ghp_")) {
			t.Errorf("%s's refusal repeats a name or a value: %v", c.tool, err)
		}
	}
	purge, _ := catalog.Lookup("purge_atlas_entry")
	p, err := purge.Preview(t.Context(), reader(world), wednesday,
		map[string]any{"atlas_entry": assistanttest.AtlasTwinID})
	if err != nil {
		t.Fatal(err)
	}
	if !p.StepUp || p.Risk != wire.RiskDestructive || !strings.Contains(p.Note.Text(), "undoes") {
		t.Errorf("the purge previews step-up %v, risk %q, note %q; want a fresh sign-in and no undo",
			p.StepUp, p.Risk, p.Note.Text())
	}
}

// A VALUE'S PURGE IS NEVER PROPOSED: a proposal keeps the request its approval
// sends, which would keep the value the purge erases.
func TestAnAtlasPurgeProposesAnEntryNeverAValue(t *testing.T) {
	t.Parallel()
	purge, ok := catalog.Lookup("purge_atlas_entry")
	if !ok {
		t.Fatal("the catalogue holds no purge_atlas_entry")
	}
	props, _ := purge.Parameters["properties"].(map[string]any)
	if _, takes := props["value"]; takes || props["atlas_entry"] == nil {
		t.Errorf("purge_atlas_entry takes %v, want an entry and never a value", props)
	}
}

// soleRef is the one record a phrase names, failing the test on none or several.
func soleRef(t *testing.T, p wire.Phrase) wire.Ref {
	t.Helper()
	refs := p.Refs()
	if len(refs) != 1 {
		t.Fatalf("%q names %d records, want one", p.Text(), len(refs))
	}
	return refs[0]
}

// AN ATLAS CARD NAMES ITS ENTRIES AND OWNERS AS RECORDS: each entry by its id
// and name, each owner a seat by its name, never its handle.
// Mutation: name the entry fact with plain words and the first case fails.
func TestAnAtlasCardNamesItsEntriesAndOwnersAsRecords(t *testing.T) {
	t.Parallel()
	world := assistanttest.World()
	preview := func(tool string) map[string]wire.Change {
		t.Helper()
		got, err := mustTool(t, tool).Preview(t.Context(), reader(world), wednesday,
			assistanttest.ProposalArgs[tool])
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return rows(got)
	}
	entry := func(tool, field string, p wire.Phrase, id, name, words string) {
		t.Helper()
		r := soleRef(t, p)
		if r.Kind != wire.ResourceAtlasEntry || r.ID != id || r.Name != name || r.Open == nil ||
			!strings.Contains(r.Open.Href, id) {
			t.Errorf("%s's %s names %+v, want the entry %s by its name, opened on the map", tool,
				field, r, id)
		}
		if p.Text() != words {
			t.Errorf("%s's %s reads %q, want %q", tool, field, p.Text(), words)
		}
	}
	for _, tool := range []string{"correct_atlas_fact", "confirm_atlas_entry", "relate_atlas_entries",
		"unrelate_atlas_entries", "set_atlas_owner", "retire_atlas_entry", "merge_atlas_entries"} {
		entry(tool, "entry", preview(tool)["entry"].To, assistanttest.AtlasServiceID, "Checkout API",
			"Checkout API (service)")
	}
	entry("relate_atlas_entries", "relation", preview("relate_atlas_entries")["relation"].To,
		assistanttest.AtlasTwinID, "checkout", "checkout (service)")
	entry("unrelate_atlas_entries", "relation", preview("unrelate_atlas_entries")["relation"].From,
		assistanttest.AtlasHostID, "web-2", "web-2 (host)")
	entry("merge_atlas_entries", "other", preview("merge_atlas_entries")["other"].To,
		assistanttest.AtlasTwinID, "checkout", "checkout (service)")

	owner := preview("set_atlas_owner")["owner"]
	for side, c := range map[string]struct {
		p        wire.Phrase
		id, name string
	}{"from": {owner.From, "pm", "Product manager"}, "to": {owner.To, "qa", "QA Engineer"}} {
		r := soleRef(t, c.p)
		if r.Kind != wire.ResourceSeat || r.ID != c.id || r.Name != c.name || r.Open == nil ||
			r.Open.Screen != "seat" || c.p.Text() != c.name {
			t.Errorf("the owner %s names %+v as %q, want the seat %s by its name", side, r,
				c.p.Text(), c.id)
		}
	}

	purge, err := mustTool(t, "purge_atlas_entry").Preview(t.Context(), reader(world), wednesday,
		assistanttest.ProposalArgs["purge_atlas_entry"])
	if err != nil {
		t.Fatal(err)
	}
	if r := soleRef(t, purge.Summary); r.Kind != wire.ResourceAtlasEntry || r.ID != assistanttest.AtlasTwinID ||
		!strings.HasPrefix(purge.Summary.Text(), "Erases checkout (service) and everything") {
		t.Errorf("the purge says %q naming %+v, want the entry it erases as a record",
			purge.Summary.Text(), r)
	}
}
