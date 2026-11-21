package atlasdomain_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PAGE OF THE LOG ENDS AT ITS BYTES AND RESUMES AFTER ITS LAST LINE: under a
// bound of one or two of its heaviest lines, every page holds its lines within
// the bound (or one line alone), and paging on reads every line once, newest
// first, merged across both streams as one unbounded page orders them.
//
// Mutations, each red: drop the Take check over the merged page (a page past
// the bound); mint the cut page's cursor from the refused line (a line skipped
// per page); leave a cut page without a cursor (paging stops after one).
func TestAPageOfTheLogEndsAtItsBytes(t *testing.T) {
	t.Parallel()
	e := logged(t)
	whole := e.log(atlasdomain.LogQuery{Limit: atlasdomain.MaxPageSize})
	heaviest := 0
	var want []string
	for _, l := range whole.Lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		heaviest, want = max(heaviest, len(raw)), append(want, l.Cursor())
	}
	stale := statelog.Freshness{Level: statelog.ReadStale}
	for _, bound := range []int{heaviest, 2 * heaviest} {
		ctx := statelog.WithAnswerBytes(t.Context(), bound)
		var got []string
		pages := 0
		for q := (atlasdomain.LogQuery{Limit: atlasdomain.MaxPageSize, Fresh: stale}); pages <= len(want); pages++ {
			page, err := e.reader().Log(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			held := 0
			for _, l := range page.Lines {
				raw, _ := json.Marshal(l)
				held += len(raw)
				got = append(got, l.Cursor())
			}
			if len(page.Lines) > 1 && held > bound {
				t.Errorf("a page under %d bytes holds %d lines of %d", bound, len(page.Lines), held)
			}
			if q.Cursor = page.Next; q.Cursor == "" {
				break
			}
		}
		if !slices.Equal(got, want) || pages < 1 {
			t.Errorf("under %d bytes the pages hold %v over %d pages, want every line once in order %v",
				bound, got, pages+1, want)
		}
	}
}

// THE LIVE CLAIMS AND THE PURGES PAGE AT THEIR BYTES, AND THE COUNTED LOG SAYS
// IT WAS CUT: under a bound one row fits, every live claim and every purge
// comes back a row a page and once, the claims joined as one read answers them,
// and the activity read in one snapshot answers what fits and says the rest
// is missing.
//
// Mutations, each red: drop the Take check in LivePage or PurgesPage (one
// page holds every row); page the claims by position alone (a record's other
// claims are skipped); answer a cut page no next cursor (paging stops after
// one); leave Truncated unset on a cut activity.
func TestTheLiveClaimsAndPurgesPageAndTheActivitySaysItWasCut(t *testing.T) {
	t.Parallel()
	e := logged(t)
	e.apply(purgeEntryRecord(t, "op-6", checkout), t3.Add(time.Minute))
	e.apply(purgeValueRecord(t, "op-7", "Takes refunds"), t3.Add(2*time.Minute))
	stale := statelog.Freshness{Level: statelog.ReadStale}
	whole := e.live()
	one := statelog.WithAnswerBytes(t.Context(), 1)
	var claims []string
	pages := 0
	for after := ""; pages < 20; {
		page, next, err := e.reader().LivePage(one, after, stale)
		if err != nil {
			t.Fatal(err)
		}
		if pages++; len(page.Claims) != 1 {
			t.Errorf("a page of live claims under one row's bytes held %d", len(page.Claims))
		}
		for _, c := range page.Claims {
			claims = append(claims, c.Slot())
		}
		if after = next; after == "" {
			break
		}
	}
	if len(claims) != len(whole) || pages != len(whole) {
		t.Errorf("paged %d live claims over %d pages, want the %d one read answers", len(claims), pages, len(whole))
	}
	var purges int
	pages = 0
	for before := int64(0); pages < 10; {
		rows, next, err := e.reader().PurgesPage(one, before, stale)
		if err != nil {
			t.Fatal(err)
		}
		purges, pages = purges+len(rows), pages+1
		if before = next; before == 0 {
			break
		}
	}
	if purges != 2 || pages != 2 {
		t.Errorf("paged %d purges over %d pages, want two of each", purges, pages)
	}
	cut, err := e.reader().Activity(one, t1, stale)
	if err != nil || !cut.Truncated || len(cut.Days)+len(cut.Last) != 1 {
		t.Errorf("the activity under one row's bytes is %d days and %d lines, truncated %v (%v); "+
			"want one row, truncated", len(cut.Days), len(cut.Last), cut.Truncated, err)
	}
	if all, err := e.reader().Activity(t.Context(), t1, stale); err != nil || all.Truncated {
		t.Errorf("the activity within the bound says truncated %v (%v)", all.Truncated, err)
	}
}

// A CLAIM REPLACED BETWEEN TWO PAGES COMES BACK ONCE, as its newest: a voice
// holds one live claim on a slot of an entry, which one read answers.
//
// Mutation: keep the first claim seen on a slot rather than the newest.
func TestLiveClaimsKeepAVoicesNewestClaimOnASlot(t *testing.T) {
	t.Parallel()
	m := must(t)
	ada := atlas.PersonSource("ada")
	old := m(atlas.Says(checkout, atlas.FactPurpose, "Takes payments", ada, t0))
	old.Position = 1
	newer := m(atlas.Says(checkout, atlas.FactPurpose, "Takes refunds", ada, t1))
	newer.Position = 2
	other := m(atlas.Relates(checkout, atlas.RelationDependsOn, orders, ada, t1))
	other.Position = 3
	got := atlasdomain.LiveClaims([]atlas.Claim{old, other, newer})
	if len(got) != 2 || got[0].Position != 2 || got[1].Position != 3 {
		t.Errorf("the joined claims are %+v, want the newer purpose and the relation", got)
	}
}
