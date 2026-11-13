package catalog

import (
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// THE ATLAS'S READS: three curated reads over the atlas questions the
// dashboard's own screens read (internal/api/queries' atlas_entries,
// atlas_entry and atlas_trace), answered as the person, so a person without
// the atlas permission is refused by the route exactly as the screens refuse
// them, and never handed a row. Nothing here is a second reader of the atlas:
// every answer is the route's own JSON, carded from its own keys.
//
// AN ENTRY IS A CHIP. The entry card's kind is the frame's atlas object, so
// the answer's first mention of an entry's name is its chip and its peek, and
// each card's link opens the map with that entry's peek open ("Show on the
// map"), lit along the trace where the read was one. The links are the link
// table's, by the keys the atlas screens read on arrival, never a fact's
// value: an address in the atlas is a fact like any other, fenced and read,
// and nothing the model writes is a link.

// The atlas reads' names.
const (
	searchAtlasTool     = "search_atlas"
	getAtlasEntryTool   = "get_atlas_entry"
	traceAtlasEntryTool = "trace_atlas_entry"
)

// atlasApp is the one app every atlas read needs: the question refuses
// anybody else, and so does the tool's offer.
var atlasApp = []iam.Grant{iam.GrantAtlas}

// atlasReads are the atlas reads, in the order a model meets them.
func atlasReads() []Tool {
	entry := text("The entry's id, as search_atlas answers it, or its exact name or key.", nameBytes)
	return []Tool{
		{
			Name: searchAtlasTool, Phrase: "search the atlas",
			Narrow: []string{"q", "kind", "lane", "vendor", "confidence", "health"},
			Description: "Find entries of the company's atlas, the map of everything it runs on: " +
				"domains and host names, services, servers, databases, repositories, monitors, " +
				"vendor accounts, tool servers and models, and how they depend on each other. " +
				"Narrow by words of a name or key, by kind, by the map's lane (observability " +
				"holds what monitors, code what delivers), by vendor, by confidence or by " +
				"health. What a vendor is used for is on its account's entry (its capabilities " +
				"fact), so \"what do we use for monitoring\" is the vendor_account entries, read " +
				"with get_atlas_entry.",
			schema: object(nil, map[string]*schema{
				"q":          text("Words of the entry's name or key.", queryBytes),
				"kind":       oneOf("The entry's kind.", wordsOf(atlas.Kinds)...),
				"lane":       oneOf("The map's lane.", wordsOf(atlas.Lanes)...),
				"vendor":     oneOf("The vendor an entry belongs to.", wordsOf(atlas.Vendors)...),
				"confidence": oneOf("How far the atlas trusts the entry.", wordsOf(atlas.Words)...),
				"health":     oneOf("What the entry's open signals add up to.", wordsOf(atlas.Healths)...),
				"show": oneOf("Current entries (the default), the retired ones, or all.",
					string(atlas.ShowCurrent), string(atlas.ShowRetired), string(atlas.ShowAll)),
				"unconfirmed": flag("Include entries only an agent reported with nothing the " +
					"engine could check, which are left out unless asked."),
				"limit": positive("How many entries, at most.", atlas.PageMax),
			}),
			Routes: []string{"GET /atlas"}, Apps: atlasApp, Fence: true,
			// NAMES FROM ANYWHERE: an integration names a monitor as its
			// own users did, and a row does not say which source named it,
			// so the least any row's writer earns is the whole table's.
			Trust: provenance.TrustExternal,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /atlas", nil,
					query(a).str("q", "q").str("kind", "kind").str("lane", "lane").str("vendor", "vendor").
						str("confidence", "confidence").str("health", "health").str("show", "show").
						truth("unconfirmed", "unconfirmed").num("limit", "limit", atlas.PageDefault).values())
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Atlas entries", Rows: "rows",
				More:  "next_cursor",
				Brief: []string{"name", "kind", "vendor", "health"},
				Columns: []cards.Column{prose("name", "Name", "name"),
					col("kind", "Kind", "kind", tLabel), col("key", "Key", "key", tText),
					col("vendor", "Vendor", "vendor", tLabel), col("state", "State", "state", tLabel),
					col("confidence", "Confidence", "word", tLabel),
					col("health", "Health", "health", tLabel),
					col("last_seen", "Last seen", "last_seen", tDatetime),
					col("id", "Entry", "id", tText)},
				Open: &cards.Open{Screen: "atlas"}}}),
			Label: func(map[string]any) string { return "Searching the atlas" },
			Scope: atlasScope,
		},
		{
			Name: getAtlasEntryTool, Phrase: "read this atlas entry",
			Description: "Read one entry of the atlas in full: its kind, vendor, state, confidence " +
				"and health, and every fact the atlas holds on it (a vendor account's capabilities, " +
				"a host's address, a database's engine), each with its value. What it depends on " +
				"and what depends on it is trace_atlas_entry. Name it by id, or by its exact name " +
				"or key.",
			schema: object([]string{"entry"}, map[string]*schema{"entry": entry}),
			Routes: []string{atlasEntryRoute}, Apps: atlasApp, Fence: true,
			Names: map[string]Directory{"entry": DirAtlasEntry},
			// AN ENTRY SAYS WHO NAMED IT: external where an integration is
			// among its sources, the company's own words otherwise.
			TrustOf: func(_ Args, answer any) provenance.Trust { return atlasEntryTrust(answer) },
			Build: func(a map[string]any) (exec.Request, error) {
				return get(atlasEntryRoute, map[string]string{"id": Args(a).String("entry")}, nil)
			},
			Cards: one(cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceAtlasEntry,
				ID: "entry.id", Title: "entry.name", Subtitle: "entry.ref.key", Prose: true,
				Facts: []cards.Fact{fact("Kind", "entry.ref.kind", tLabel),
					fact("Vendor", "vendor", tLabel), fact("State", "entry.state", tLabel),
					fact("Confidence", "entry.word", tLabel), fact("Health", "health", tLabel),
					fact("Last seen", "entry.last_seen", tDatetime)},
				// EACH FACT BY ITS NAME, its value beside it; a fact with
				// several values (capabilities) is one item per value.
				Items: &cards.Items{Path: "entry.facts", ID: "value.text", Label: "fact",
					Fact: "value.text", Prose: true, Noun: wire.Noun{One: "fact", Many: "facts"}},
				Open: &cards.Open{Screen: "atlas-entry-on-map",
					Params: map[string]string{"id": "entry.id"}}}}),
			Label: func(map[string]any) string { return "Reading an atlas entry" },
			Scope: func(a Args) string { return a.String("entry") },
		},
		{
			Name: traceAtlasEntryTool, Phrase: "trace the atlas",
			Description: "Walk the atlas from one entry along the relations a request travels " +
				"through, as the map draws them: up (the default) for what depends on it, what " +
				"breaks if it breaks, and down for what it depends on, hop by hop. Name the entry " +
				"by id, or by its exact name or key. Who monitors or owns an entry is on the " +
				"entry itself (get_atlas_entry).",
			// THE TRACE THE MAP LIGHTS, and nothing the map does not: no
			// depth and no choice of relations, so what the card lists and
			// what "Show on the map" shows are one walk, and two traces of
			// one entry are one read to the drawer.
			schema: object([]string{"entry"}, map[string]*schema{
				"entry": entry,
				"direction": oneOf("up for what depends on the entry, down for what it depends on.",
					wordsOf(atlas.Directions)...),
			}),
			Routes: []string{"GET /atlas/{id}/trace"}, Apps: atlasApp, Fence: true,
			Names: map[string]Directory{"entry": DirAtlasEntry},
			Trust: provenance.TrustExternal,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /atlas/{id}/trace", map[string]string{"id": Args(a).String("entry")},
					query(a).str("direction", "direction").values())
			},
			Variant: by("direction", string(atlas.DirectionUp)),
			Cards: map[string]cards.Spec{
				string(atlas.DirectionUp):   atlasTraceCard(wire.Noun{One: "dependent", Many: "dependents"}),
				string(atlas.DirectionDown): atlasTraceCard(wire.Noun{One: "dependency", Many: "dependencies"}),
			},
			Label: func(a map[string]any) string {
				if Args(a).String("direction") == string(atlas.DirectionDown) {
					return "Tracing what an atlas entry depends on"
				}
				return "Tracing what depends on an atlas entry"
			},
			// THE ENTRY, IN THE TITLE: the traces of two entries are two reads.
			Scope: func(a Args) string { return a.String("entry") },
		},
	}
}

// atlasTraceCard is the trace's card: the entry traced from as the record,
// and every entry reached as an item, indented by how many hops away it is.
func atlasTraceCard(noun wire.Noun) cards.Spec {
	return cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceAtlasEntry,
		ID: "from.id", Title: "from.name", Subtitle: "from.key", Prose: true,
		Facts: []cards.Fact{fact("Kind", "from.kind", tLabel), fact("Health", "from.health", tLabel),
			fact("Direction", "direction", tLabel)},
		Items: &cards.Items{Path: "hops", Expand: "entries", Kind: wire.ResourceAtlasEntry,
			ID: "id", Label: "name", Fact: "kind", Depth: "parent.distance", Prose: true, Noun: noun},
		Open: &cards.Open{Screen: "atlas-trace-on-map",
			Params: map[string]string{"id": "from.id", "direction": "direction"}}}}
}

// atlasScope is search_atlas's narrowing, in the order a person says it.
func atlasScope(a Args) string {
	unconfirmed := ""
	if a.Bool("unconfirmed") {
		unconfirmed = "including unconfirmed"
	}
	show := ""
	switch atlas.Show(a.String("show")) {
	case atlas.ShowRetired:
		show = "retired"
	case atlas.ShowAll:
		show = "including retired"
	}
	return scopeOf(strings.ReplaceAll(a.String("kind"), "_", " "), labelled(a, "lane", "lane"),
		a.String("vendor"), a.String("confidence"), strings.ReplaceAll(a.String("health"), "_", " "),
		quoted(a), unconfirmed, show)
}

// atlasEntryTrust is how far the model may rely on an entry's answer: external
// where an integration or a repository's files are among the sources that
// named it, since a vendor's users name what it reports and anybody with a
// push writes a file; the company's own words otherwise, by the one rule for
// a surface the engine hosts.
func atlasEntryTrust(answer any) provenance.Trust {
	sources, _ := cards.Lookup(answer, "entry.sources").([]any)
	for _, src := range sources {
		if kind := value(src, "kind"); kind == string(atlas.SourceIntegration) || kind == string(atlas.SourceCode) {
			return provenance.TrustExternal
		}
	}
	return provenance.TrustForHosted("", nil, false)
}
