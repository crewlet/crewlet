package atlas

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// WHAT WAITS FOR A PERSON IS READ OFF THE VIEW ON EVERY READ: a fact a newer
// report contradicts, an entry only an agent backed, and one nothing backs,
// in that order, each once, and gone the moment its cause is. A retired entry
// waits for nobody, and a page ends where the next begins. An entry that is
// likely only because a repository's files declare it waits for nobody
// either, until an agent reports it too.
//
// Mutation: list an entry by its state instead of its confidence word and the
// agent's backed entry is never offered for confirmation; list every likely
// entry and what a repository's files declare fills the queue.
func TestTheReviewIsWhatWaitsForAPersonAndNothingElse(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	ok := func(c Claim, err error) Claim {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ada, cf, files := PersonSource("ada"), IntegrationSource("cloudflare"), CodeSource("shop")
	backed, guessed := AgentSource("sre", "turn-1", EvidenceCall), AgentSource("sre", "turn-2", EvidenceNone)
	checkout, orders := MustRef(KindService, "checkout-api"), MustRef(KindDatastore, "acme/orders-db")
	cache, old := MustRef(KindDatastore, "acme/cache"), MustRef(KindService, "legacy")
	declared := MustRef(KindQueue, "shop/jobs")
	gone := ok(Exists(old, backed, now))
	claims := []Claim{
		ok(Exists(checkout, ada, now.Add(-3*time.Hour))),
		ok(Says(checkout, FactName, "Checkout", ada, now.Add(-2*time.Hour))),
		ok(Says(checkout, FactName, "Checkout API", cf, now.Add(-time.Hour))),
		ok(Exists(orders, backed, now.Add(-time.Hour))),
		ok(Exists(cache, guessed, now.Add(-time.Hour))),
		ok(Exists(declared, files, now.Add(-time.Hour))),
		gone, ok(Denial(gone, ada, now)),
	}
	view := Fold(claims, now)
	review, err := view.Review("", "", PageMax)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range review.Items {
		got = append(got, string(item.Kind)+" "+item.Entry.Key)
	}
	want := []string{"disagreement checkout-api", "unconfirmed acme/cache", "likely acme/orders-db"}
	if !slices.Equal(got, want) {
		t.Fatalf("the review lists %v, want %v", got, want)
	}
	if review.Total != 3 || review.Counts[ReviewDisagreement] != 1 || review.Counts[ReviewLikely] != 1 {
		t.Errorf("the review counts %d in all and %v", review.Total, review.Counts)
	}
	first := review.Items[0]
	if first.Fact == nil || first.Fact.Value.Text != "Checkout" || len(first.Fact.Others) != 1 ||
		!first.Since.Equal(now.Add(-time.Hour)) {
		t.Errorf("the disagreement reads %+v, want the person's value, the report beside it, since the report", first)
	}

	// One item a page, each page starting after the last.
	var paged []string
	for cursor, pages := "", 0; ; pages++ {
		page, failed := view.Review("", cursor, 1)
		if failed != nil || pages > 3 {
			t.Fatalf("page %d: %v", pages, failed)
		}
		for _, item := range page.Items {
			paged = append(paged, string(item.Kind)+" "+item.Entry.Key)
		}
		if cursor = page.Next; cursor == "" {
			break
		}
	}
	if !slices.Equal(paged, want) {
		t.Errorf("read a page at a time the review lists %v, want %v", paged, want)
	}
	only, err := view.Review(ReviewLikely, "", PageMax)
	if err != nil || len(only.Items) != 1 || only.Total != 1 || only.Counts[ReviewDisagreement] != 1 {
		t.Errorf("asked for the likely ones the review answers %+v, %v", only, err)
	}
	for _, bad := range []struct{ kind, cursor string }{{"drift", ""}, {"", "yesterday"}} {
		if _, refused := view.Review(ReviewKind(bad.kind), bad.cursor, PageMax); refused == nil {
			t.Errorf("a review asked with %+v was answered", bad)
		}
	}

	// What the files declared is likely, and waits only once an agent reports it too.
	if e, found := view.Find(declared.ID()); !found || e.Word != WordLikely {
		t.Fatalf("what the files declared reads %+v, %v; want a likely entry", e, found)
	}
	reported := Fold(append(claims, ok(Exists(declared, backed, now))), now)
	both, err := reported.Review(ReviewLikely, "", PageMax)
	if err != nil || len(both.Items) != 2 {
		t.Errorf("after an agent reported what the files declared the review lists %+v, %v; want both likely entries", both.Items, err)
	}

	// The person says the name again, after the report: nothing disagrees.
	settled := Fold(append(claims, ok(Says(checkout, FactName, "Checkout", ada, now))), now)
	after, err := settled.Review(ReviewDisagreement, "", PageMax)
	if err != nil || len(after.Items) != 0 {
		t.Errorf("after the person settled it the review still lists %+v, %v", after.Items, err)
	}
}

// AN ENTRY THAT WAITS CARRIES WHAT EACH AGENT SAID BEHIND IT: its seat, the
// turn, the call it cited and what that call is evidence of, the newest
// report first and each agent named by its latest turn, however the claims
// arrive. A disagreement carries none: its fact names its own sources.
//
// Mutation: name a voice by whichever claim a map walk met last, and the
// report names turn-1 on some runs.
func TestAWaitingEntryCarriesEachAgentsNewestReport(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	ok := func(c Claim, err error) Claim {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := AgentSource("sre", "turn-1", EvidenceCall).Citing("ssh_read")
	later := AgentSource("sre", "turn-2", EvidenceCall).Citing("tickets__search")
	other := AgentSource("dev", "turn-9", EvidenceFile).Citing("code_read")
	orders := MustRef(KindDatastore, "acme/orders-db")
	claims := []Claim{
		ok(Exists(orders, first, now.Add(-3*time.Hour))),
		ok(Says(orders, FactEngine, "postgres", first, now.Add(-3*time.Hour))),
		ok(Says(orders, FactPort, "5432", later, now.Add(-time.Hour))),
		ok(Says(orders, FactEngineVersion, "16.4", other, now.Add(-2*time.Hour))),
	}
	for range 20 {
		review, err := Fold(claims, now).Review(ReviewLikely, "", PageMax)
		if err != nil || len(review.Items) != 1 {
			t.Fatalf("the review answers %+v, %v", review, err)
		}
		got := review.Items[0].Reports
		want := []Report{{Source: later, ObservedAt: now.Add(-time.Hour)},
			{Source: other, ObservedAt: now.Add(-2 * time.Hour)}}
		if !slices.Equal(got, want) {
			t.Fatalf("the reports are %+v, want %+v", got, want)
		}
	}
}

// THE REVIEW NARROWED TO ONE PERSON'S WORD lists only what a newer report
// contradicts of what THEY set: never a colleague's contested value, never
// two sources no person settled, never an entry that only waits to be
// confirmed. It is what reaches that person's Inbox.
//
// Mutation: narrow by the standing alone, and grace is told of a value ada
// set.
func TestTheReviewNarrowsToWhatContradictsOnePersonsWord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	ok := func(c Claim, err error) Claim {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ada, grace := PersonSource("ada"), PersonSource("grace")
	cf, dd := IntegrationSource("cloudflare"), IntegrationSource("datadog")
	checkout, api, waits := MustRef(KindService, "checkout-api"), MustRef(KindService, "api"),
		MustRef(KindDatastore, "acme/cache")
	claims := []Claim{
		// Ada's name, which Cloudflare later contradicts.
		ok(Exists(checkout, ada, now.Add(-3*time.Hour))),
		ok(Says(checkout, FactName, "Checkout", ada, now.Add(-2*time.Hour))),
		ok(Says(checkout, FactName, "Checkout API", cf, now.Add(-time.Hour))),
		// Grace's name, set AFTER the report that differs: nothing drifted.
		ok(Exists(api, grace, now.Add(-3*time.Hour))),
		ok(Says(api, FactName, "API", dd, now.Add(-2*time.Hour))),
		ok(Says(api, FactName, "Public API", grace, now.Add(-time.Hour))),
		// An entry only an agent reported.
		ok(Exists(waits, AgentSource("sre", "turn-1", EvidenceNone), now.Add(-time.Hour))),
	}
	view := Fold(claims, now)
	mine := func(handle string) Review {
		t.Helper()
		r, err := view.ReviewOf(ReviewQuery{Limit: PageMax, ContradictedFor: handle})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	adas := mine("ada")
	if len(adas.Items) != 1 || adas.Total != 1 || adas.Items[0].Entry.Key != "checkout-api" ||
		adas.Items[0].Fact == nil || adas.Items[0].Fact.Standing != StandingContested {
		t.Errorf("ada is told of %+v, want her contradicted name alone", adas.Items)
	}
	if graces := mine("grace"); len(graces.Items) != 0 || graces.Total != 0 || len(graces.Counts) != 0 {
		t.Errorf("grace is told of %+v, and nothing contradicts her word", graces.Items)
	}
	whole, err := view.Review("", "", PageMax)
	if err != nil || whole.Total != 2 {
		t.Errorf("the whole review lists %d, want the contradiction and the waiting entry: %v", whole.Total, err)
	}
}

// THE TOOL AN AGENT'S CLAIM CITES IS KEPT BY NAME, AND NEVER AS A CREDENTIAL:
// a tool server names its tools itself, so a name outside the grammar or
// shaped like a credential is kept by no record, and only an agent's source
// names one.
func TestOnlyAnAgentsSourceNamesAToolAndNeverACredential(t *testing.T) {
	t.Parallel()
	agent := AgentSource("sre", "turn-1", EvidenceCall)
	for _, name := range []string{"ssh_read", "tickets__search", "mcp/tickets:search", "code.read-2"} {
		if got := agent.Citing(name); got.Tool != name || got.validate() != nil {
			t.Errorf("%q is not kept as the cited tool: %+v, %v", name, got, got.validate())
		}
	}
	for _, name := range []string{"", "has space", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", strings.Repeat("a", 129)} {
		if got := agent.Citing(name); got.Tool != "" {
			t.Errorf("%q was kept as a cited tool", name)
		}
	}
	forged := agent
	forged.Tool = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	if forged.validate() == nil {
		t.Error("a source citing a credential-shaped tool is valid")
	}
	if got := PersonSource("ada").Citing("ssh_read"); got.Tool != "" {
		t.Error("a person's source names a tool")
	}
	person := PersonSource("ada")
	person.Tool = "ssh_read"
	if person.validate() == nil {
		t.Error("a person's source naming a tool is valid")
	}
}
