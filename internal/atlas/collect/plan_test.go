package collect_test

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/integration"
)

var (
	t0         = time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
	cloudflare = atlas.IntegrationSource("cloudflare")
	zone       = atlas.MustRef(atlas.KindDomain, "acme.example")
	api        = atlas.MustRef(atlas.KindHostname, "api.acme.example")
	www        = atlas.MustRef(atlas.KindHostname, "www.acme.example")
	originA    = atlas.MustRef(atlas.KindHost, "addr/9.9.9.9")
	originB    = atlas.MustRef(atlas.KindHost, "addr/149.112.112.112")
)

func must(t *testing.T) func(atlas.Claim, error) atlas.Claim {
	return func(c atlas.Claim, err error) atlas.Claim {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
}

// world is the read of one zone with two names, as a builder would make it.
func world(t *testing.T, at time.Time, apiOrigin atlas.Ref) []atlas.Claim {
	t.Helper()
	m := must(t)
	return []atlas.Claim{
		m(atlas.Exists(zone, cloudflare, at)),
		m(atlas.Exists(api, cloudflare, at)),
		m(atlas.Relates(api, atlas.RelationInZone, zone, cloudflare, at)),
		m(atlas.Relates(api, atlas.RelationResolvesTo, apiOrigin, cloudflare, at)),
		m(atlas.Exists(www, cloudflare, at)),
		m(atlas.Relates(www, atlas.RelationInZone, zone, cloudflare, at)),
		m(atlas.Exists(apiOrigin, cloudflare, at)),
		m(atlas.Says(apiOrigin, atlas.FactAddress, strings.TrimPrefix(apiOrigin.Key, "addr/"), cloudflare, at)),
	}
}

func standing() collect.Status { return collect.Status{Kind: integration.KindCloudflare} }

// entries is the entries a plan's records are on, sorted.
func entries(p collect.Plan) []string {
	var out []string
	for _, r := range p.Records {
		out = append(out, r.Entry.String())
	}
	slices.Sort(out)
	return out
}

// A FIRST READ RECORDS EVERYTHING, ONE RECORD AN ENTRY, AND AN UNCHANGED READ
// RECORDS NOTHING: the whole point of diffing against the log.
//
// Mutation: compare claims by their slot alone, or by their instant, and the
// second read writes every entry again.
func TestAnUnchangedReadRecordsNothing(t *testing.T) {
	t.Parallel()
	first := collect.Decide(nil, collect.Read{Claims: world(t, t0, originA), Calls: 3}, standing(), t0)
	if got := entries(first); !slices.Equal(got, []string{zone.String(), originA.String(), api.String(), www.String()}) {
		t.Fatalf("the first read records %v", got)
	}
	if first.Status.Entries != 4 || first.Status.Calls != 3 || first.Status.Recorded != 4 || first.Status.Digest == "" {
		t.Errorf("the standing after the first read: %+v", first.Status)
	}
	again := collect.Decide(world(t, t0, originA), collect.Read{Claims: world(t, t0.Add(time.Hour), originA)},
		first.Status, t0.Add(time.Hour))
	if len(again.Records) != 0 {
		t.Fatalf("an unchanged read an hour later records %v", entries(again))
	}
	if again.Status.Digest != first.Status.Digest || len(again.Status.Missing) != 0 {
		t.Errorf("an unchanged read moved the standing: %+v", again.Status)
	}
}

// A CHANGE IS RECORDED WHOLE AND AT ONCE: a name whose origin moved writes the
// new relation and withdraws the old on that one entry, and the old origin's
// server, which the read no longer lists, is only missed.
func TestAChangedEntryIsRecordedWholeAndAtOnce(t *testing.T) {
	t.Parallel()
	live := world(t, t0, originA)
	p := collect.Decide(live, collect.Read{Claims: world(t, t0.Add(time.Hour), originB)}, standing(), t0.Add(time.Hour))
	if got := entries(p); !slices.Equal(got, []string{originB.String(), api.String()}) {
		t.Fatalf("a moved origin records %v, want the name and the new server", got)
	}
	moved := p.Records[slices.IndexFunc(p.Records, func(r collect.EntryRead) bool { return r.Entry == api })]
	if len(moved.Claims) != 1 || moved.Claims[0].Facet.Target.Entry != originB {
		t.Errorf("the name's record carries %+v, want its new origin alone", moved.Claims)
	}
	old := must(t)(atlas.Relates(api, atlas.RelationResolvesTo, originA, cloudflare, t0))
	if !slices.Equal(moved.Withdrawn, []string{old.Slot()}) {
		t.Errorf("the name's record withdraws %q, want the old origin's slot", moved.Withdrawn)
	}
	if len(p.Status.Missing) != 1 || !slices.Equal(p.Status.Missing[0].Entries, []string{originA.ID().String()}) {
		t.Errorf("the old server is missed as %+v", p.Status.Missing)
	}
}

// RETIREMENT TAKES TWO COMPLETE MISSES A DAY APART: one miss records nothing,
// a second within the day records nothing, a second a day after the first
// retires the entry with every other slot withdrawn, and a read that lists it
// again in between forgets the miss.
//
// Mutation: retire on the first miss, or on the second whatever the gap, and
// the first two plans record a retirement.
func TestRetirementTakesTwoCompleteMissesADayApart(t *testing.T) {
	t.Parallel()
	live := world(t, t0, originA)
	without := func(at time.Time) collect.Read {
		var kept []atlas.Claim
		for _, c := range world(t, at, originA) {
			if c.Subject != www {
				kept = append(kept, c)
			}
		}
		return collect.Read{Claims: kept}
	}
	first := collect.Decide(live, without(t0.Add(time.Hour)), standing(), t0.Add(time.Hour))
	if len(first.Records) != 0 || len(first.Status.Missing) != 1 {
		t.Fatalf("the first miss records %v and misses %+v", entries(first), first.Status.Missing)
	}
	soon := collect.Decide(live, without(t0.Add(13*time.Hour)), first.Status, t0.Add(13*time.Hour))
	if len(soon.Records) != 0 || !soon.Status.Missing[0].Since.Equal(t0.Add(time.Hour)) {
		t.Fatalf("a second miss within the day records %v and misses since %s", entries(soon),
			soon.Status.Missing[0].Since)
	}
	later := collect.Decide(live, without(t0.Add(25*time.Hour+time.Minute)), soon.Status, t0.Add(25*time.Hour+time.Minute))
	if len(later.Records) != 1 || !later.Records[0].Retires || later.Records[0].Entry != www {
		t.Fatalf("a second miss a day after the first records %+v, want www retired", later.Records)
	}
	rec := later.Records[0]
	if len(rec.Claims) != 1 || !rec.Claims[0].Denied || rec.Claims[0].Facet.Kind != atlas.FacetExists {
		t.Errorf("the retirement carries %+v, want one denied exists claim", rec.Claims)
	}
	inZone := must(t)(atlas.Relates(www, atlas.RelationInZone, zone, cloudflare, t0))
	if !slices.Equal(rec.Withdrawn, []string{inZone.Slot()}) {
		t.Errorf("the retirement withdraws %q, want the name's zone", rec.Withdrawn)
	}
	if len(later.Status.Missing) != 0 || later.Status.Retired != 1 {
		t.Errorf("the standing after the retirement: %+v", later.Status)
	}
	back := collect.Decide(live, collect.Read{Claims: world(t, t0.Add(13*time.Hour), originA)}, first.Status,
		t0.Add(13*time.Hour))
	if len(back.Records) != 0 || len(back.Status.Missing) != 0 {
		t.Errorf("a read that lists the name again records %v and still misses %+v", entries(back), back.Status.Missing)
	}
}

// A RETIRED ENTRY STAYS RETIRED AND IS NEVER MISSED AGAIN, and a read that
// lists it again restores it with one record.
func TestARetiredEntryIsNeitherMissedNorRetiredAgain(t *testing.T) {
	t.Parallel()
	m := must(t)
	gone := m(atlas.Exists(www, cloudflare, t0))
	gone.Denied = true
	var live, without []atlas.Claim
	for _, c := range world(t, t0, originA) {
		if c.Subject != www {
			live, without = append(live, c), append(without, c)
		}
	}
	live = append(live, gone)
	p := collect.Decide(live, collect.Read{Claims: without}, standing(), t0.Add(48*time.Hour))
	if len(p.Records) != 0 || len(p.Status.Missing) != 0 {
		t.Fatalf("a retired name is missed again: %v, %+v", entries(p), p.Status.Missing)
	}
	back := collect.Decide(live, collect.Read{Claims: world(t, t0, originA)}, standing(), t0.Add(48*time.Hour))
	if len(back.Records) != 1 || back.Records[0].Entry != www || len(back.Records[0].Claims) != 2 {
		t.Fatalf("a retired name listed again records %+v, want it restored with its zone", back.Records)
	}
}

// A DELETION PAST THE CAP RETIRES THE OLDEST MISSES FIRST and holds the rest
// for a later read, and the standing says how many.
func TestMissesPastTheCapAreHeldForALaterRead(t *testing.T) {
	t.Parallel()
	m := must(t)
	var live []atlas.Claim
	for i := range collect.MaxMissing + 10 {
		host := atlas.MustRef(atlas.KindHostname, "h"+strings.Repeat("0", 2)+itoa(i)+".acme.example")
		live = append(live, m(atlas.Exists(host, cloudflare, t0)))
	}
	p := collect.Decide(live, collect.Read{}, standing(), t0.Add(time.Hour))
	tracked := 0
	for _, g := range p.Status.Missing {
		tracked += len(g.Entries)
	}
	if tracked != collect.MaxMissing || p.Status.MissingOverflow != 10 {
		t.Fatalf("%d misses tracked, %d held back; want %d and 10", tracked, p.Status.MissingOverflow, collect.MaxMissing)
	}
	later := collect.Decide(live, collect.Read{}, p.Status, t0.Add(26*time.Hour))
	if len(later.Records) != collect.MaxMissing || later.Status.MissingOverflow != 0 || len(later.Status.Missing) != 1 {
		t.Errorf("a day later %d retirements, %d held back, %d groups still missed; want the cap retired and the "+
			"rest newly missed", len(later.Records), later.Status.MissingOverflow, len(later.Status.Missing))
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}

// A SOURCE IS DUE ONCE AN INTERVAL AFTER A COMPLETE READ, SOONER AFTER A
// FAILED ONE, AND AT ONCE WHEN NEVER READ, judged against the interval in force.
func TestASourceIsDueByTheIntervalInForce(t *testing.T) {
	t.Parallel()
	never := standing()
	if !never.Due(t0, time.Hour) {
		t.Error("a source never read is not due")
	}
	done := collect.Status{Kind: integration.KindCloudflare, LastAttemptAt: t0, Completed: true}
	if done.Due(t0.Add(59*time.Minute), time.Hour) || !done.Due(t0.Add(time.Hour), time.Hour) {
		t.Error("a source read whole is due before, or not at, an interval")
	}
	if !done.Due(t0.Add(6*time.Minute), 5*time.Minute) {
		t.Error("a shortened interval does not apply at once")
	}
	failed := collect.Status{Kind: integration.KindCloudflare, LastAttemptAt: t0}
	if failed.Due(t0.Add(4*time.Minute), time.Hour) || !failed.Due(t0.Add(collect.RetryAfter), time.Hour) {
		t.Error("a source whose read failed is not retried after the retry wait")
	}
}

// A PART THE READ WAS REFUSED CHANGES NOTHING: what an earlier read of it
// recorded is neither withdrawn nor missed, a miss it already had is kept at
// its first instant and never advanced to a retirement, the rest of the read
// is diffed as ever, and the standing names the part.
//
// Mutation: skip carry in Decide, and the refused part's entry is missed,
// then retired a day later.
func TestARefusedPartIsCarriedAndRetiresNothing(t *testing.T) {
	t.Parallel()
	live := world(t, t0, originA)
	// The read lists everything but www, and was refused the part www is in.
	read := collect.Read{
		Claims: slices.DeleteFunc(world(t, t0.Add(time.Hour), originB), func(c atlas.Claim) bool { return c.Subject == www }),
		Unread: []collect.Unread{{Part: "names", Detail: "Grant the permission to read names.",
			Holds: func(c atlas.Claim) bool { return c.Subject == www }}},
	}
	missed := standing()
	missed.Missing = []collect.Missing{{Since: t0.Add(-48 * time.Hour), Entries: []string{www.ID().String()}}}
	for name, status := range map[string]collect.Status{"never missed": standing(), "missed two days ago": missed} {
		plan := collect.Decide(live, read, status, t0.Add(time.Hour))
		// The part that was read is diffed as ever: api moved to a new origin.
		if got := entries(plan); !slices.Equal(got, []string{originB.String(), api.String()}) {
			t.Errorf("%s: recorded %v, want the moved name and its new origin alone", name, got)
		}
		// The old origin is newly missed; www keeps exactly the miss it had.
		since := map[string]time.Time{}
		for _, g := range plan.Status.Missing {
			for _, id := range g.Entries {
				since[id] = g.Since
			}
		}
		want := map[string]time.Time{originA.ID().String(): t0.Add(time.Hour)}
		if len(status.Missing) > 0 {
			want[www.ID().String()] = t0.Add(-48 * time.Hour)
		}
		if !maps.EqualFunc(since, want, time.Time.Equal) {
			t.Errorf("%s: missing %v, want %v", name, since, want)
		}
		if plan.Status.Retired != 0 {
			t.Errorf("%s: a read refused a part retired %d", name, plan.Status.Retired)
		}
		if len(plan.Status.Unread) != 1 || plan.Status.Unread[0].Part != "names" ||
			plan.Status.Unread[0].Detail != "Grant the permission to read names." {
			t.Errorf("%s: the standing's findings: %+v", name, plan.Status.Unread)
		}
	}
	// A later read that is whole again clears the finding.
	if plan := collect.Decide(live, collect.Read{Claims: world(t, t0, originA)}, missed, t0.Add(time.Hour)); len(plan.Status.Unread) != 0 {
		t.Errorf("a whole read kept the finding: %+v", plan.Status.Unread)
	}
}

// A VOICE DENIES ONLY WHAT IT SAID EXISTS: an entry it only related or
// described is released after the same two misses a day apart, its slots
// withdrawn and nothing denied, so it never retires an entry another source
// holds.
//
// Mutation: build a denial for an entry the voice never claimed, and a
// monitor somebody deletes retires the service it watched.
func TestAVoiceDeniesOnlyWhatItSaidExists(t *testing.T) {
	t.Parallel()
	m := must(t)
	// The voice says www is in the zone, and never that www exists.
	live := []atlas.Claim{
		m(atlas.Exists(zone, cloudflare, t0)),
		m(atlas.Relates(www, atlas.RelationInZone, zone, cloudflare, t0)),
	}
	read := collect.Read{Claims: []atlas.Claim{m(atlas.Exists(zone, cloudflare, t0))}}
	first := collect.Decide(live, read, standing(), t0.Add(time.Hour))
	if len(first.Records) != 0 || len(first.Status.Missing) != 1 {
		t.Fatalf("the first miss recorded %+v and missed %+v", first.Records, first.Status.Missing)
	}
	second := collect.Decide(live, read, first.Status, t0.Add(26*time.Hour))
	if len(second.Records) != 1 {
		t.Fatalf("the second miss recorded %+v", second.Records)
	}
	rec := second.Records[0]
	if rec.Entry != www || rec.Retires || len(rec.Claims) != 0 || len(rec.Withdrawn) != 1 {
		t.Errorf("an entry the voice never claimed was not simply released: %+v", rec)
	}
	if second.Status.Retired != 0 || second.Status.Recorded != 1 || len(second.Status.Missing) != 0 {
		t.Errorf("the standing after a release: %+v", second.Status)
	}
}
