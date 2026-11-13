package atlas

import (
	"slices"
	"testing"
	"time"
)

// WHAT WAITS FOR A PERSON IS READ OFF THE VIEW ON EVERY READ: a fact a newer
// report contradicts, an entry only an agent backed, and one nothing backs,
// in that order, each once, and gone the moment its cause is. A retired entry
// waits for nobody, and a page ends where the next begins.
//
// Mutation: list an entry by its state instead of its confidence word and the
// agent's backed entry is never offered for confirmation.
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
	ada, cf := PersonSource("ada"), IntegrationSource("cloudflare")
	backed, guessed := AgentSource("sre", "turn-1", EvidenceCall), AgentSource("sre", "turn-2", EvidenceNone)
	checkout, orders := MustRef(KindService, "checkout-api"), MustRef(KindDatastore, "acme/orders-db")
	cache, old := MustRef(KindDatastore, "acme/cache"), MustRef(KindService, "legacy")
	gone := ok(Exists(old, backed, now))
	claims := []Claim{
		ok(Exists(checkout, ada, now.Add(-3*time.Hour))),
		ok(Says(checkout, FactName, "Checkout", ada, now.Add(-2*time.Hour))),
		ok(Says(checkout, FactName, "Checkout API", cf, now.Add(-time.Hour))),
		ok(Exists(orders, backed, now.Add(-time.Hour))),
		ok(Exists(cache, guessed, now.Add(-time.Hour))),
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

	// The person says the name again, after the report: nothing disagrees.
	settled := Fold(append(claims, ok(Says(checkout, FactName, "Checkout", ada, now))), now)
	after, err := settled.Review(ReviewDisagreement, "", PageMax)
	if err != nil || len(after.Items) != 0 {
		t.Errorf("after the person settled it the review still lists %+v, %v", after.Items, err)
	}
}
