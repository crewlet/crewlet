package atlasdomain_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// readRecord is cloudflare's read of r under opID: the claims it holds, and
// the slots of its earlier claims it stopped holding.
func readRecord(t *testing.T, opID string, r atlas.Ref, withdrawn []string, claims ...atlas.Claim) []byte {
	t.Helper()
	rd := atlasdomain.Read{V: 1, Source: atlasdomain.Source{Kind: cloudflare.Kind, Name: cloudflare.Name},
		ObservedAt: claimsAt(claims), Withdrawn: withdrawn}
	for _, c := range claims {
		wire := wireClaim(opID, c)
		wire.ID = atlasdomain.ReadClaimID(opID, c.Slot())
		rd.Claims = append(rd.Claims, wire)
	}
	body, err := rawRecord(atlasdomain.EntrySubject(r), "node-a", opID, atlasdomain.OpRead, sentinel(), rd)
	if err != nil {
		t.Fatalf("encode a read: %v", err)
	}
	return body
}

// claimsAt is when the first of claims was seen, t0 where there is none.
func claimsAt(claims []atlas.Claim) time.Time {
	if len(claims) == 0 {
		return t0
	}
	return claims[0].ObservedAt
}

// logged is an estate with a small log: ada's purpose for checkout and her
// correction of it, checkout's dependency on orders, cloudflare's read of the
// API's name with its zone and origin, and a later read that stopped holding
// the origin. Days apart, so each record is a day of its own.
func logged(t *testing.T) *estate {
	t.Helper()
	e := newEstate(t)
	m := must(t)
	ada := atlas.PersonSource("ada")
	e.apply(claimRecord(t, "op-1", m(atlas.Says(checkout, atlas.FactPurpose, "Takes payments", ada, t0))), t0)
	e.apply(claimRecord(t, "op-2", m(atlas.Says(checkout, atlas.FactPurpose, "Takes refunds", ada, t1))), t1)
	e.apply(claimRecord(t, "op-3", m(atlas.Relates(checkout, atlas.RelationDependsOn, orders, ada, t1))), t1.Add(time.Minute))
	toA := m(atlas.Relates(apiHost, atlas.RelationResolvesTo, originA, cloudflare, t2))
	e.apply(readRecord(t, "op-4", apiHost, nil, m(atlas.Exists(apiHost, cloudflare, t2)),
		m(atlas.Relates(apiHost, atlas.RelationInZone, acmeZone, cloudflare, t2)), toA), t2)
	e.apply(readRecord(t, "op-5", apiHost, []string{toA.Slot()}), t3)
	return e
}

// THE LOG READS NEWEST FIRST, A WITHDRAWAL AS A LINE OF ITS OWN: a claim a
// read stopped holding is a line at the read that withdrew it, a claim its
// voice replaced is not, and pages of any size hold every line once.
//
// Mutation: page by the instant alone (drop the claim id from the cursor) and
// the three lines of one read lose one across a page boundary.
func TestTheLogReadsNewestFirstAndPagesHoldEveryLineOnce(t *testing.T) {
	t.Parallel()
	e := logged(t)
	whole := e.log(atlasdomain.LogQuery{Limit: atlasdomain.MaxPageSize})
	if whole.Next != "" || len(whole.Lines) != 7 {
		t.Fatalf("the log holds %d lines and continues at %q, want 6 claims and one withdrawal",
			len(whole.Lines), whole.Next)
	}
	newest := whole.Lines[0]
	if !newest.Withdrawn || !newest.At.Equal(t3) || newest.Claim.Facet.Target.Entry != originA ||
		newest.Entry != apiHost.ID().String() {
		t.Errorf("the newest line is %+v, want the origin's withdrawal at the read that made it", newest)
	}
	withdrawn := 0
	for i, l := range whole.Lines {
		if l.Withdrawn {
			withdrawn++
		}
		if i > 0 && l.At.After(whole.Lines[i-1].At) {
			t.Errorf("line %d at %s follows one at %s: not newest first", i, l.At, whole.Lines[i-1].At)
		}
	}
	if withdrawn != 1 {
		t.Errorf("%d withdrawals, want one: a purpose its voice replaced is no withdrawal", withdrawn)
	}
	for _, size := range []int{1, 2, 3} {
		var got []string
		for cursor, pages := "", 0; ; pages++ {
			page := e.log(atlasdomain.LogQuery{Cursor: cursor, Limit: size})
			for _, l := range page.Lines {
				got = append(got, l.Cursor())
			}
			if cursor = page.Next; cursor == "" || pages > len(whole.Lines) {
				break
			}
		}
		want := make([]string, len(whole.Lines))
		for i, l := range whole.Lines {
			want[i] = l.Cursor()
		}
		if !slices.Equal(got, want) {
			t.Errorf("pages of %d hold %d lines %v, want every line once in order %v", size, len(got), got, want)
		}
	}
	own := e.log(atlasdomain.LogQuery{Entries: []string{checkout.ID().String()}})
	if len(own.Lines) != 3 || own.Lines[2].Claim.Value.Text != "Takes payments" {
		t.Errorf("checkout's own log is %+v, want its three lines, the first purpose last", own.Lines)
	}
	both := e.log(atlasdomain.LogQuery{Entries: []string{checkout.ID().String(), apiHost.ID().String()}})
	if len(both.Lines) != 7 || !both.Lines[0].Withdrawn {
		t.Errorf("two entries' log holds %d lines, want all seven merged newest first", len(both.Lines))
	}
	stale := statelog.Freshness{Level: statelog.ReadStale}
	for name, q := range map[string]atlasdomain.LogQuery{
		"a cursor no page answered": {Cursor: "yesterday", Fresh: stale},
		"an id that is no entry's":  {Entries: []string{"checkout"}, Fresh: stale},
		"a page past the bound":     {Limit: atlasdomain.MaxPageSize + 1, Fresh: stale},
	} {
		if _, err := e.reader().Log(t.Context(), q); err == nil {
			t.Errorf("%s was answered", name)
		}
	}
}

// THE LOG IS COUNTED A DAY AND AN ENTRY AT A TIME, and each entry's newest
// record is read however old: lines before the window are not counted, a
// line naming another entry is counted apart, and a withdrawal counts on the
// day of the read that made it.
func TestTheLogIsCountedByDayAndEachEntrysNewestRecordIsRead(t *testing.T) {
	t.Parallel()
	e := logged(t)
	activity, err := e.reader().Activity(t.Context(), t1, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	day := func(at time.Time) time.Time { return at.Truncate(24 * time.Hour) }
	got := map[atlasdomain.ActivityDay]bool{}
	for _, d := range activity.Days {
		got[d] = true
	}
	api, shop := apiHost.ID().String(), checkout.ID().String()
	want := []atlasdomain.ActivityDay{
		{Entry: shop, Day: day(t1), Lines: 1},
		{Entry: shop, Target: orders.ID().String(), Day: day(t1), Lines: 1},
		{Entry: api, Day: day(t2), Lines: 1},
		{Entry: api, Target: acmeZone.ID().String(), Day: day(t2), Lines: 1},
		{Entry: api, Target: originA.ID().String(), Day: day(t2), Lines: 1},
		{Entry: api, Target: originA.ID().String(), Day: day(t3), Lines: 1},
	}
	if len(got) != len(want) {
		t.Errorf("%d counts %+v, want %d: the first purpose is before the window", len(got), activity.Days, len(want))
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("no count %+v in %+v", w, activity.Days)
		}
	}
	last := map[string][]atlasdomain.LogLine{}
	for _, l := range activity.Last {
		last[l.Entry] = append(last[l.Entry], l)
	}
	if lines := last[api]; len(lines) != 1 || !lines[0].Withdrawn || !lines[0].At.Equal(t3) {
		t.Errorf("the API name's newest record is %+v, want the origin's withdrawal", lines)
	}
	if lines := last[shop]; len(lines) != 1 || lines[0].Claim.Facet.Kind != atlas.FacetRelation {
		t.Errorf("checkout's newest record is %+v, want its dependency on orders", lines)
	}
}
