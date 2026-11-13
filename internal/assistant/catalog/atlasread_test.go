package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// atlasRow is one entry as the atlas's list and a trace answer it.
func atlasRow(id, kind, key, name, state, health string) map[string]any {
	return map[string]any{"id": id, "kind": kind, "lane": "services", "name": name, "key": key,
		"state": state, "word": "confirmed", "health": health, "signals": 0, "review": false,
		"not_used_recently": false, "last_seen": "2026-11-11T09:00:00Z"}
}

// atlasWorld is [assistanttest.World] with the atlas's list, as a search for
// anything answers it, and the trace up from the server: the service runs on
// it.
func atlasWorld() map[string]any {
	world := assistanttest.World()
	world["/atlas"] = map[string]any{"revision": "r1", "coverage": []any{}, "withheld": 0, "total": 3,
		"rows": []any{
			atlasRow(assistanttest.AtlasServiceID, "service", "checkout-api", "Checkout API", "confirmed", "quiet"),
			atlasRow(assistanttest.AtlasTwinID, "service", "checkout", "checkout", "retired", "quiet"),
			atlasRow(assistanttest.AtlasHostID, "host", "addr/web-2.acme.example", "web-2", "confirmed", "alerting")}}
	world["/atlas/"+assistanttest.AtlasHostID+"/trace"] = map[string]any{"revision": "r1",
		"coverage": []any{}, "withheld": 0,
		"from":      atlasRow(assistanttest.AtlasHostID, "host", "addr/web-2.acme.example", "web-2", "confirmed", "alerting"),
		"direction": "up",
		"hops": []any{map[string]any{"distance": 1, "entries": []any{
			atlasRow(assistanttest.AtlasServiceID, "service", "checkout-api", "Checkout API", "confirmed", "quiet")}}},
		"edges":     []any{map[string]any{"from": assistanttest.AtlasServiceID, "to": assistanttest.AtlasHostID, "relation": "runs_on"}},
		"truncated": false}
	return world
}

// shaped is tool's cards and model text for args answered by world's record at
// the request's path.
func shaped(t *testing.T, tool string, args map[string]any, world map[string]any) catalog.Result {
	t.Helper()
	read := mustTool(t, tool)
	req, err := read.Request(args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	res, err := catalog.Shape(read, args, assistanttest.Answer(world, req),
		provenance.Boundary("K7Q2M4XWB3ZP6TNA"), evals.Epoch, cards.Options{})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

// THE ATLAS READS ARE OFFERED TO WHOEVER HOLDS THE ATLAS, AND TO NOBODY ELSE:
// a person holding Worklet alone is never handed a tool over the company's
// map, and an atlas holder is handed all three. Mutation: offer the reads on
// no app and the Worklet person is handed search_atlas.
func TestTheAtlasReadsAreOfferedOnlyToAtlasHolders(t *testing.T) {
	t.Parallel()
	reads := []string{"search_atlas", "get_atlas_entry", "trace_atlas_entry"}
	names := func(p iam.Principal) []string {
		var out []string
		for _, tool := range catalog.Offered(p, nil) {
			out = append(out, tool.Name)
		}
		return out
	}
	worker := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantWorklet}}
	holder := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAtlas}}
	for _, name := range reads {
		if slices.Contains(names(worker), name) {
			t.Errorf("a person without the atlas is offered %s", name)
		}
		if !slices.Contains(names(holder), name) {
			t.Errorf("an atlas holder is not offered %s", name)
		}
	}
}

// A SEARCH OF THE ATLAS IS A TABLE OF ENTRIES, titled by what narrowed it, each
// row carrying the id the other reads take, with the map to open, and a page
// where the answer says more lie past it. Mutation: drop the id column, or
// the `next_cursor` more, and this fails.
func TestAnAtlasSearchIsATableOfEntriesWithTheMapToOpen(t *testing.T) {
	t.Parallel()
	world := atlasWorld()
	world["/atlas"].(map[string]any)["next_cursor"] = "c2"
	res := shaped(t, "search_atlas", map[string]any{"kind": "host", "health": "needs_look",
		"q": "web"}, world)
	if len(res.Cards) != 1 || res.Cards[0].Type != wire.CardTable {
		t.Fatalf("a search drew %+v, want one table", res.Cards)
	}
	table := res.Cards[0].Table
	if table.Title != "Atlas entries, host, needs look, “web”" {
		t.Errorf("the table is titled %q", table.Title)
	}
	if table.Open == nil || table.Open.Href != "#/company/atlas" || table.Open.Screen != "atlas" {
		t.Errorf("the table opens %+v, want the atlas", table.Open)
	}
	if !table.More || table.RowCount != 3 {
		t.Errorf("the table says more=%v over %d rows, want a page of three", table.More, table.RowCount)
	}
	if !slices.ContainsFunc(table.Columns, func(c wire.Column) bool { return c.Key == "id" && c.Detail }) {
		t.Errorf("the table's columns %+v carry no entry id for the model to read by", table.Columns)
	}
	if res.Trust != provenance.TrustExternal {
		t.Errorf("a search is fenced at %q, want external: an integration may have named a row", res.Trust)
	}
}

// AN ENTRY IS A CHIP THAT OPENS THE MAP ON IT: the entry card is the frame's
// atlas object by its id, headed by its name, its facts listed by name with
// their values, and its link the map with the entry's peek open. It is fenced
// at the trust its sources earn: external where an integration named it, the
// company's own words where only people did. Mutation: card the entry under
// no kind, or link its page by its id, and this fails.
func TestAnAtlasEntryIsAChipThatOpensTheMapOnIt(t *testing.T) {
	t.Parallel()
	world := atlasWorld()
	res := shaped(t, "get_atlas_entry", map[string]any{"entry": assistanttest.AtlasServiceID}, world)
	if len(res.Cards) != 1 || res.Cards[0].Type != wire.CardEntity {
		t.Fatalf("an entry drew %+v, want one record", res.Cards)
	}
	e := res.Cards[0].Entity
	if e.Kind != wire.ResourceAtlasEntry || e.ID != assistanttest.AtlasServiceID ||
		e.Title != "Checkout API" || e.Subtitle != "checkout-api" {
		t.Errorf("the entry is carded as %+v", e)
	}
	if e.Open == nil || e.Open.Label != "Show on the map" ||
		e.Open.Href != "#/company/atlas?peek=atlas%3A"+assistanttest.AtlasServiceID {
		t.Errorf("the entry opens %+v, want the map with its peek open", e.Open)
	}
	if !slices.ContainsFunc(e.Items, func(it wire.EntityItem) bool {
		return it.Label == "environment" && it.Fact == "production"
	}) {
		t.Errorf("the entry's facts %+v do not list its environment", e.Items)
	}
	if res.Trust != provenance.TrustExternal {
		t.Errorf("an entry GitHub named is fenced at %q, want external", res.Trust)
	}
	// NAMED BY PEOPLE ALONE, it is the company's own words.
	own := shaped(t, "get_atlas_entry", map[string]any{"entry": assistanttest.AtlasHostID}, world)
	if own.Trust != provenance.TrustColleague {
		t.Errorf("an entry only a person named is fenced at %q, want colleague", own.Trust)
	}
	// NAMED BY A REPOSITORY'S FILES, it is text anybody with a push wrote.
	record, _ := world["/atlas/"+assistanttest.AtlasHostID].(map[string]any)
	entry, _ := record["entry"].(map[string]any)
	if entry == nil {
		t.Fatal("the world holds no record of the host entry")
	}
	entry["sources"] = []any{map[string]any{"kind": "code", "name": "shop"}}
	declared := shaped(t, "get_atlas_entry", map[string]any{"entry": assistanttest.AtlasHostID}, world)
	if declared.Trust != provenance.TrustExternal {
		t.Errorf("an entry a repository's files named is fenced at %q, want external", declared.Trust)
	}
}

// A TRACE LISTS WHAT IT REACHED, HOP BY HOP, UNDER THE ENTRY IT STARTED FROM,
// and shows the same on the map: the record is the entry traced from, each
// reached entry an item indented by its distance, and the link the map lit
// along the trace. Up lists dependents, down dependencies. Mutation: read the
// items from the hops unexpanded, or drop the direction from the link, and
// this fails.
func TestATraceListsWhatItReachedAndShowsItOnTheMap(t *testing.T) {
	t.Parallel()
	world := atlasWorld()
	res := shaped(t, "trace_atlas_entry", map[string]any{"entry": assistanttest.AtlasHostID}, world)
	if len(res.Cards) != 1 || res.Cards[0].Type != wire.CardEntity {
		t.Fatalf("a trace drew %+v, want one record", res.Cards)
	}
	e := res.Cards[0].Entity
	if e.Kind != wire.ResourceAtlasEntry || e.ID != assistanttest.AtlasHostID || e.Title != "web-2" {
		t.Errorf("the trace is headed by %+v, want the server it started from", e)
	}
	want := []wire.EntityItem{{Kind: wire.ResourceAtlasEntry, ID: assistanttest.AtlasServiceID,
		Label: "Checkout API", Fact: "service", Depth: 1}}
	if !slices.Equal(e.Items, want) {
		t.Errorf("the trace lists %+v, want %+v", e.Items, want)
	}
	if e.ItemsNoun == nil || e.ItemsNoun.Many != "dependents" {
		t.Errorf("a trace up counts its items as %+v, want dependents", e.ItemsNoun)
	}
	if e.Open == nil || e.Open.Label != "Show on the map" ||
		e.Open.Href != "#/company/atlas?peek=atlas%3A"+assistanttest.AtlasHostID+"&trace=up" {
		t.Errorf("the trace opens %+v, want the map lit along it", e.Open)
	}
	trace := world["/atlas/"+assistanttest.AtlasHostID+"/trace"].(map[string]any)
	trace["direction"] = "down"
	down := shaped(t, "trace_atlas_entry", map[string]any{"entry": assistanttest.AtlasHostID,
		"direction": "down"}, world)
	if noun := down.Cards[0].Entity.ItemsNoun; noun == nil || noun.Many != "dependencies" {
		t.Errorf("a trace down counts its items as %+v, want dependencies", noun)
	}
	if href := down.Cards[0].Entity.Open.Href; !strings.HasSuffix(href, "&trace=down") {
		t.Errorf("a trace down opens %q", href)
	}
}

// AN ENTRY NAMED BY ITS NAME OR KEY IS READ BY ITS ID, through the atlas's own
// search for it as the person: "Checkout API", "checkout-api" and "the web-2
// server" each resolve to one entry; an id is sent as written and reads
// nothing; a name two entries share is refused naming both with their kinds;
// one nothing answers to is refused listing what the search found where it
// can only be a name, and sent as written for the route to decide where it is
// spelled like a key. Mutation: send a name as written, or resolve the first
// of several, and this fails.
func TestAnAtlasEntryNamedByItsNameIsReadByItsID(t *testing.T) {
	t.Parallel()
	world := atlasWorld()
	list := world["/atlas"].(map[string]any)
	list["rows"] = append(list["rows"].([]any),
		atlasRow("3e914d85-6f70-4182-9d2e-3f4a5b6c7d8e", "service", "web-2", "web-2", "confirmed", "quiet"))
	get := mustTool(t, "get_atlas_entry")
	var reads atomic.Int32
	counted := func(_ context.Context, r exec.Request) (exec.Response, error) {
		reads.Add(1)
		if r.Path == "/atlas" && r.Query.Get("q") == "" {
			t.Errorf("the atlas was read whole: %s?%s", r.Path, r.Query.Encode())
		}
		return assistanttest.Answer(world, r), nil
	}
	for name, tc := range map[string]struct {
		entry, want, refused string
	}{
		"by its name": {"Checkout API", assistanttest.AtlasServiceID, ""},
		"by its key":  {"checkout-api", assistanttest.AtlasServiceID, ""},
		"by its id":   {assistanttest.AtlasServiceID, assistanttest.AtlasServiceID, ""},
		"by a name of twins": {"the web-2 server", "", "names 2 atlas entries; name one by its key: web-2, host (" +
			assistanttest.AtlasHostID + "), web-2, service (3e914d85-6f70-4182-9d2e-3f4a5b6c7d8e)"},
		"by a name nothing answers to": {"the orders database", "",
			`"the orders database" is not one of the atlas entries you can see`},
		"by a key nothing answers to": {"orders-db", "orders-db", ""},
	} {
		reads.Store(0)
		r := catalog.NewResolver(counted)
		got, err := r.Resolve(t.Context(), get, map[string]any{"entry": tc.entry})
		switch {
		case tc.refused != "":
			if !errors.Is(err, catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("%s: resolved %v (%v), want refused saying %q", name, got, err, tc.refused)
			}
		case err != nil || got["entry"] != tc.want:
			t.Errorf("%s: entry = %v (%v), want %q", name, got["entry"], err, tc.want)
		}
		if n := reads.Load(); (tc.entry == assistanttest.AtlasServiceID) != (n == 0) {
			t.Errorf("%s: the atlas was read %d times", name, n)
		}
	}
	// WHAT THE SEARCH FOUND is what a refusal lists.
	r := catalog.NewResolver(counted)
	_, err := r.Resolve(t.Context(), get, map[string]any{"entry": "the orders database"})
	if err == nil || !strings.Contains(err.Error(), "Checkout API, service ("+assistanttest.AtlasServiceID+")") {
		t.Errorf("a name nothing answers to was refused %v, want the entries found listed", err)
	}
}

// A PERSON WITHOUT THE ATLAS GETS THE LOCKED ANSWER, NEVER THE DATA: the
// route's refusal is the card, in the engine's words for the read ("search
// the atlas"), and no table is drawn; the model is told the refusal, not a
// row. Mutation: card a refused answer's body and this fails.
func TestARefusedAtlasReadIsTheLockedAnswer(t *testing.T) {
	t.Parallel()
	search := mustTool(t, "search_atlas")
	args := map[string]any{"kind": "datastore"}
	body, _ := json.Marshal(map[string]any{"error": "forbidden", "reason": "app_required",
		"grants": []string{"atlas"}, "message": "Reading the atlas takes the atlas permission.",
		"rows": []any{atlasRow(assistanttest.AtlasHostID, "host", "addr/web-2.acme.example", "web-2",
			"confirmed", "alerting")}})
	res, err := catalog.Shape(search, args, exec.Response{Status: http.StatusForbidden, Body: body},
		provenance.Boundary("K7Q2M4XWB3ZP6TNA"), evals.Epoch, cards.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cards) != 1 || res.Cards[0].Type != wire.CardError || res.Cards[0].Error.Phrase != "search the atlas" {
		t.Fatalf("a refused search drew %+v, want the locked answer alone", res.Cards)
	}
	if res.Refusal == nil || res.Refusal.Code != "forbidden" || strings.Contains(res.Model, "web-2") {
		t.Errorf("the model was handed %q (%+v), want the refusal and no row", res.Model, res.Refusal)
	}
}
