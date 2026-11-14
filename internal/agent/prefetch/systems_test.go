package prefetch_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/provenance"
)

// systemsOf answers a fixed atlas, and counts how often it was read.
type systemsOf struct {
	view     atlas.View
	readable bool
	reads    atomic.Int32
}

func (s *systemsOf) Systems(context.Context, prefetch.Request) (atlas.View, bool, error) {
	s.reads.Add(1)
	return s.view, s.readable, nil
}

var atlasNow = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// fold is the atlas of claims, failing the test on a claim the atlas refuses.
func fold(t *testing.T, claims ...func() (atlas.Claim, error)) atlas.View {
	t.Helper()
	out := make([]atlas.Claim, 0, len(claims))
	for _, c := range claims {
		claim, err := c()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, claim)
	}
	return atlas.Fold(out, atlasNow)
}

func exists(r atlas.Ref, src atlas.Source) func() (atlas.Claim, error) {
	return func() (atlas.Claim, error) { return atlas.Exists(r, src, atlasNow) }
}

func says(r atlas.Ref, f atlas.Fact, raw string, src atlas.Source) func() (atlas.Claim, error) {
	return func() (atlas.Claim, error) { return atlas.Says(r, f, raw, src, atlasNow) }
}

// systemsRequest is the fixture turn asked text, as a seat that may read the
// atlas, under a fence token of its own.
func systemsRequest(t *testing.T, text string) prefetch.Request {
	t.Helper()
	r := request(t)
	r.Ask = prefetch.PlainAsk(text)
	r.Boundary = provenance.NewBoundary()
	r.ReadsAtlas = true
	return r
}

// THE SYSTEMS BLOCK IS FENCED AT THE LEAST TRUST ITS WRITERS EARN: the entries
// the work names are somebody else's words, so the block is handed over in a
// fence under the turn's own token, labelled with the atlas as its source and
// the writers of the listed entries alone. The settings and a person read as
// a colleague's; one listed entry a vendor's listing vouches for makes the
// whole block an outside party's, because nobody the seat can name wrote it.
//
// Mutation: hand the lines over unfenced, label them by every entry's writers
// rather than the listed ones', or drop the vendor rule, and this fails.
func TestTheSystemsBlockIsFencedAtTheLeastTrustItsWritersEarn(t *testing.T) {
	t.Parallel()
	settings, ada := atlas.SettingsSource("r1"), atlas.PersonSource("ada")
	api := atlas.MustRef(atlas.KindService, "checkout-api")
	host := atlas.MustRef(atlas.KindHostname, "api.acme.example")
	billing := atlas.MustRef(atlas.KindService, "billing-api")
	claims := []func() (atlas.Claim, error){
		exists(api, settings), says(api, atlas.FactName, "Checkout", ada),
		exists(host, settings),
		exists(billing, atlas.AgentSource("sre", "turn-1", atlas.EvidenceCall)),
	}
	r := systemsRequest(t, "Checkout answers 502 behind api.acme.example since this morning")

	src := &systemsOf{view: fold(t, claims...), readable: true}
	blocks := fetch(t, prefetch.Sources{Systems: src}, r)
	got := blocks.Systems
	if !strings.HasPrefix(got, prefetch.SystemsLead) {
		t.Fatalf("the block does not open with its lead:\n%s", got)
	}
	opener := "[untrusted:" + string(r.Boundary) + " source=" + string(provenance.SourceAtlas) + " trust=" +
		string(provenance.TrustColleague)
	if !strings.Contains(got, opener) {
		t.Errorf("the settings' and a person's entries are not fenced as a colleague's (%q):\n%s", opener, got)
	}
	for _, want := range []string{"- api.acme.example (hostname): confirmed", "- Checkout (service, checkout-api)", "ada"} {
		if !strings.Contains(got, want) {
			t.Errorf("the block lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "billing-api") || strings.Contains(got, "sre") {
		t.Errorf("the block names an entry, or a writer of one, the work does not name:\n%s", got)
	}
	if blocks.SystemsNamed != 2 {
		t.Errorf("the block reports %d entries listed, want 2", blocks.SystemsNamed)
	}

	listed := &systemsOf{view: fold(t, append(claims, exists(host, atlas.IntegrationSource("cloudflare")))...),
		readable: true}
	external := fetch(t, prefetch.Sources{Systems: listed}, r).Systems
	if !strings.Contains(external, "trust="+string(provenance.TrustExternal)) {
		t.Errorf("an entry a vendor lists is not read as an outside party's:\n%s", external)
	}
}

// THE SYSTEMS BLOCK LISTS ONLY WHAT THE WORK NAMES, WITHIN ITS BOUNDS: at most
// [prefetch.MaxSystems] entries and [prefetch.MaxSystemsBytes] of lines, the
// rest counted and pointed at the search; nothing at all for a turn that may
// not read the atlas, for an atlas the seat may read none of, or for work
// that names no entry, and a turn that may not read it never reads it.
//
// Mutation: drop either bound, or the ReadsAtlas check, and this fails.
func TestTheSystemsBlockListsOnlyWhatTheWorkNamesWithinItsBounds(t *testing.T) {
	t.Parallel()
	settings := atlas.SettingsSource("r1")
	var claims []func() (atlas.Claim, error)
	var names []string
	for i := range prefetch.MaxSystems + 4 {
		name := fmt.Sprintf("svc-%02d-api", i)
		claims, names = append(claims, exists(atlas.MustRef(atlas.KindService, name), settings)), append(names, name)
	}
	many := &systemsOf{view: fold(t, claims...), readable: true}
	r := systemsRequest(t, "every one of "+strings.Join(names, ", ")+" is down")
	blocks := fetch(t, prefetch.Sources{Systems: many}, r)
	if n := strings.Count(blocks.Systems, "\n- "); n != prefetch.MaxSystems || blocks.SystemsNamed != prefetch.MaxSystems {
		t.Errorf("the block lists %d entries and reports %d, want the cap of %d:\n%s", n, blocks.SystemsNamed,
			prefetch.MaxSystems, blocks.Systems)
	}
	if !strings.Contains(blocks.Systems, "(4 more the work names; find them with `atlas_search`.)") {
		t.Errorf("the block does not count what it left out:\n%s", blocks.Systems)
	}

	claims, names = nil, nil
	label := strings.Repeat("a", 60)
	for i := range prefetch.MaxSystems {
		name := fmt.Sprintf("h%d-%s.%s.%s.acme.example", i, label, label, label)
		claims, names = append(claims, exists(atlas.MustRef(atlas.KindHostname, name), settings)), append(names, name)
	}
	long := &systemsOf{view: fold(t, claims...), readable: true}
	got := fetch(t, prefetch.Sources{Systems: long}, systemsRequest(t, strings.Join(names, " and "))).Systems
	lines := 0
	size := 0
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(line, "- ") {
			lines, size = lines+1, size+len(line)+1
		}
	}
	if lines == 0 || lines == prefetch.MaxSystems || size > prefetch.MaxSystemsBytes {
		t.Errorf("long entries take %d lines and %d bytes, want fewer than %d within %d bytes:\n%s", lines, size,
			prefetch.MaxSystems, prefetch.MaxSystemsBytes, got)
	}

	stranger := systemsRequest(t, r.Ask.Text())
	stranger.ReadsAtlas = false
	unread := &systemsOf{view: many.view, readable: true}
	if got := fetch(t, prefetch.Sources{Systems: unread}, stranger); got.Systems != "" || unread.reads.Load() != 0 {
		t.Errorf("a turn that may not read the atlas read it %d times and was handed %q", unread.reads.Load(),
			got.Systems)
	}
	if got := fetch(t, prefetch.Sources{Systems: &systemsOf{view: many.view}}, r).Systems; got != "" {
		t.Errorf("an atlas the seat may read none of was handed over: %q", got)
	}
	if got := fetch(t, prefetch.Sources{Systems: many}, systemsRequest(t, theAsk)).Systems; got != "" {
		t.Errorf("work that names no entry was handed %q", got)
	}
}

// THE ENTRIES A WORK ITEM NAMES COME FIRST, in the item's own order, before
// what the ask names, each once and through whichever entry its id merged
// into; an id the seat's atlas does not hold, or a retired entry, lists
// nothing. A turn whose ask names nothing still lists what its item names,
// and the bounds hold whatever the item names (ADR-0140).
//
// Mutation: list what the ask names first and the first assertion fails; drop
// the Linked half of the empty-ask check and the second does.
func TestTheSystemsBlockListsWhatTheWorkItemNamesFirst(t *testing.T) {
	t.Parallel()
	settings, ada := atlas.SettingsSource("r1"), atlas.PersonSource("ada")
	api := atlas.MustRef(atlas.KindService, "checkout-api")
	db := atlas.MustRef(atlas.KindService, "orders-db")
	queue := atlas.MustRef(atlas.KindService, "orders-queue")
	old := atlas.MustRef(atlas.KindService, "legacy-api")
	gone := atlas.MustRef(atlas.KindService, "never-recorded")
	// A PERSON RECORDED IT AND THEN RETIRED IT.
	retired := func() (atlas.Claim, error) {
		recorded, err := atlas.Exists(old, ada, atlasNow.Add(-time.Hour))
		if err != nil {
			return atlas.Claim{}, err
		}
		return atlas.Denial(recorded, ada, atlasNow)
	}
	view := fold(t, exists(api, settings), exists(db, settings), exists(queue, settings), retired)
	if e, found := view.Find(old.ID()); !found || e.State != atlas.StateRetired {
		t.Fatalf("the fixture's retired entry reads %+v", e)
	}
	src := &systemsOf{view: view, readable: true}

	r := systemsRequest(t, "checkout-api answers 502 since this morning")
	r.Linked = []uuid.UUID{queue.ID(), gone.ID(), db.ID(), old.ID(), queue.ID(), api.ID()}
	blocks := fetch(t, prefetch.Sources{Systems: src}, r)
	at := func(name string) int { return strings.Index(blocks.Systems, "\n- "+name+" (") }
	if at("orders-queue") < 0 || at("orders-queue") > at("orders-db") || at("orders-db") > at("checkout-api") {
		t.Errorf("the block does not list the item's entries in its order, before the ask's:\n%s", blocks.Systems)
	}
	if blocks.SystemsNamed != 3 || strings.Contains(blocks.Systems, "legacy-api") {
		t.Errorf("the block lists %d entries, want the three live ones, each once:\n%s", blocks.SystemsNamed,
			blocks.Systems)
	}

	quiet := systemsRequest(t, "")
	quiet.Linked = []uuid.UUID{db.ID()}
	if got := fetch(t, prefetch.Sources{Systems: src}, quiet); got.SystemsNamed != 1 ||
		!strings.Contains(got.Systems, "- orders-db (service)") {
		t.Errorf("a turn asked nothing does not list what its work item names:\n%s", got.Systems)
	}

	var claims []func() (atlas.Claim, error)
	var linked []uuid.UUID
	for i := range prefetch.MaxSystems + 3 {
		ref := atlas.MustRef(atlas.KindService, fmt.Sprintf("svc-%02d-api", i))
		claims, linked = append(claims, exists(ref, settings)), append(linked, ref.ID())
	}
	many := systemsRequest(t, "")
	many.Linked = linked
	capped := fetch(t, prefetch.Sources{Systems: &systemsOf{view: fold(t, claims...), readable: true}}, many)
	if capped.SystemsNamed != prefetch.MaxSystems || len(capped.Systems) > prefetch.MaxSystemsBytes+1024 ||
		!strings.Contains(capped.Systems, "(3 more the work names;") {
		t.Errorf("an item naming %d entries lists %d:\n%s", len(linked), capped.SystemsNamed, capped.Systems)
	}

	unread := systemsRequest(t, "")
	unread.Linked, unread.ReadsAtlas = []uuid.UUID{db.ID()}, false
	if got := fetch(t, prefetch.Sources{Systems: src}, unread); got.Systems != "" {
		t.Errorf("a turn that may not read the atlas was told what its work item names:\n%s", got.Systems)
	}
}
