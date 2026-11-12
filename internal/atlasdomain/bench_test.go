package atlasdomain_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE REFERENCE COMPANY'S ATLAS, measured for ADR-0126: about two thousand
// entries and twenty thousand live claims, every person's word on it, and a
// tenth as much replaced history.

// referenceClaims is the reference company's claims, each as a person made it.
func referenceClaims(b *testing.B) []atlas.Claim {
	b.Helper()
	var out []atlas.Claim
	add := func(c atlas.Claim, err error) {
		if err != nil {
			b.Fatalf("build a reference claim: %v", err)
		}
		out = append(out, c)
	}
	src, at := atlas.PersonSource("ada"), t0
	host := func(i int) atlas.Ref { return atlas.MustRef(atlas.KindHost, fmt.Sprintf("ssh/host-%d", i%800)) }
	data := func(i int) atlas.Ref {
		return atlas.MustRef(atlas.KindDatastore, fmt.Sprintf("on/ssh/host-%d/postgres/%d", i%300, 5432+i%300))
	}
	service := func(i int) atlas.Ref { return atlas.MustRef(atlas.KindService, fmt.Sprintf("svc-%d", i%400)) }
	zone := func(i int) atlas.Ref { return atlas.MustRef(atlas.KindDomain, fmt.Sprintf("zone-%d.example", i%20)) }
	page := func(i int) string { return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint(i))).String() }
	for i := range 20 {
		add(atlas.Exists(zone(i), src, at))
		add(atlas.Says(zone(i), atlas.FactName, fmt.Sprintf("Zone %d", i), src, at))
	}
	for i := range 800 {
		h := host(i)
		add(atlas.Exists(h, src, at))
		add(atlas.Says(h, atlas.FactName, fmt.Sprintf("Host %d", i), src, at))
		for j := range 3 {
			add(atlas.Says(h, atlas.FactAddress, fmt.Sprintf("10.%d.%d.%d", j, i/250, i%250), src, at))
		}
		add(atlas.Says(h, atlas.FactPort, "22", src, at))
		add(atlas.Says(h, atlas.FactRegion, "eu-west-1", src, at))
		add(atlas.Names(h, atlas.RelationOwnedBy, atlas.ExternalUnit, "platform", src, at))
		add(atlas.Says(h, atlas.FactEnvironment, "production", src, at))
		add(atlas.Says(h, atlas.FactImportance, "standard", src, at))
	}
	for i := range 300 {
		d := data(i)
		add(atlas.Exists(d, src, at))
		add(atlas.Says(d, atlas.FactName, fmt.Sprintf("Database %d", i), src, at))
		add(atlas.Says(d, atlas.FactEngine, "postgres", src, at))
		add(atlas.Says(d, atlas.FactEngineVersion, "16.4", src, at))
		add(atlas.Says(d, atlas.FactPort, "5432", src, at))
		add(atlas.Relates(d, atlas.RelationRunsOn, host(i), src, at))
		add(atlas.Names(d, atlas.RelationOwnedBy, atlas.ExternalUnit, "data", src, at))
		add(atlas.Says(d, atlas.FactRegion, "eu-west-1", src, at))
		add(atlas.Says(d, atlas.FactPurpose, "Keeps orders", src, at))
		add(atlas.Says(d, atlas.FactImportance, "critical", src, at))
	}
	for i := range 400 {
		s := service(i)
		add(atlas.Exists(s, src, at))
		add(atlas.Says(s, atlas.FactName, fmt.Sprintf("Service %d", i), src, at))
		add(atlas.Says(s, atlas.FactPurpose, "Serves customers", src, at))
		add(atlas.Says(s, atlas.FactImportance, "critical", src, at))
		add(atlas.Says(s, atlas.FactEnvironment, "production", src, at))
		add(atlas.Names(s, atlas.RelationOwnedBy, atlas.ExternalSeat, "sre", src, at))
		add(atlas.Relates(s, atlas.RelationDependsOn, data(i), src, at))
		add(atlas.Relates(s, atlas.RelationDependsOn, service(i+1), src, at))
		add(atlas.Relates(s, atlas.RelationRunsOn, host(i), src, at))
		add(atlas.Names(s, atlas.RelationDocumentedIn, atlas.ExternalPage, page(i), src, at))
	}
	for i := range 500 {
		h := atlas.MustRef(atlas.KindHostname, fmt.Sprintf("api-%d.zone-%d.example", i, i%20))
		add(atlas.Exists(h, src, at))
		add(atlas.Says(h, atlas.FactName, fmt.Sprintf("Name %d", i), src, at))
		add(atlas.Relates(h, atlas.RelationResolvesTo, host(i), src, at))
		add(atlas.Relates(h, atlas.RelationInZone, zone(i), src, at))
		add(atlas.Names(h, atlas.RelationOwnedBy, atlas.ExternalSeat, "sre", src, at))
		add(atlas.Says(h, atlas.FactEnvironment, "production", src, at))
		add(atlas.Says(h, atlas.FactImportance, "standard", src, at))
		add(atlas.Says(h, atlas.FactPurpose, "Public address", src, at))
	}
	return out
}

// seedReference applies the reference company into a fresh estate, a tenth of
// its purposes corrected once so the history holds replaced claims, and answers
// the estate and how many records it took.
func seedReference(b *testing.B) (store.ReplicatedHandle, uint64, []atlas.Claim) {
	b.Helper()
	node, db := storetest.OpenEstate(b, filepath.Join(b.TempDir(), "node.db"), store.Options{}, 1)
	b.Cleanup(func() { _ = node.Close() })
	claims := referenceClaims(b)
	var bodies [][]byte
	for i, c := range claims {
		bodies = append(bodies, benchRecord(b, fmt.Sprintf("seed-%d", i), c))
	}
	for i, c := range claims {
		if c.Facet.Fact == atlas.FactPurpose && i%10 == 0 {
			c.Value = atlas.Value{Type: atlas.TypeLabel, Text: "Corrected purpose"}
			bodies = append(bodies, benchRecord(b, fmt.Sprintf("seed-fix-%d", i), c))
		}
	}
	applier := atlasdomain.NewApplier()
	var seq uint64
	for start := 0; start < len(bodies); start += 1000 {
		end := min(start+1000, len(bodies))
		if err := db.Tx(b.Context(), func(tx *sql.Tx) error {
			for _, body := range bodies[start:end] {
				env, err := atlasdomain.Domain{}.Envelope(body)
				if err != nil {
					return err
				}
				seq++
				rec := statelog.Record{Envelope: env, Position: position(seq), Payload: body, StoredAt: t0}
				if _, err := applier.Apply(b.Context(), tx, rec, statelog.ApplyOptions{StoredAt: t0.Add(
					time.Duration(seq) * time.Millisecond), MaxVariables: 999}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatalf("seed the reference company: %v", err)
		}
	}
	return db, seq, claims
}

// benchRecord is c as a person's claim under opID.
func benchRecord(b *testing.B, opID string, c atlas.Claim) []byte {
	b.Helper()
	body, err := rawRecord(atlasdomain.EntrySubject(c.Subject), "node-a", opID, atlasdomain.OpClaim,
		sentinel(), wireClaim(opID, c))
	if err != nil {
		b.Fatalf("encode: %v", err)
	}
	return body
}

// BenchmarkAtlasLiveRead reads every live claim of the reference company: the
// recorded half of an atlas read.
func BenchmarkAtlasLiveRead(b *testing.B) {
	db, seq, _ := seedReference(b)
	authority, err := statelogtest.LocalReader(atlasdomain.Domain{}, db, position(seq))
	if err != nil {
		b.Fatal(err)
	}
	r, err := atlasdomain.NewReader(atlasdomain.ReaderOptions{DB: db.Reader(), Log: authority})
	if err != nil {
		b.Fatal(err)
	}
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	b.ResetTimer()
	for b.Loop() {
		live, err := r.Live(b.Context(), fresh)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(live.Claims)), "claims")
	}
}

// BenchmarkAtlasFold folds the reference company's live claims into its view:
// the half of an atlas read no store is involved in.
func BenchmarkAtlasFold(b *testing.B) {
	claims := referenceClaims(b)
	now := t0.Add(time.Hour)
	b.ResetTimer()
	for b.Loop() {
		v := atlas.Fold(claims, now)
		b.ReportMetric(float64(len(v.Entries)), "entries")
	}
}

// BenchmarkAtlasClaimApply applies one person's claim into the reference
// company's estate: what each record on the log costs a data node.
func BenchmarkAtlasClaimApply(b *testing.B) {
	db, seq, _ := seedReference(b)
	applier := atlasdomain.NewApplier()
	svc := atlas.MustRef(atlas.KindService, "svc-7")
	src := atlas.PersonSource("grace")
	b.ResetTimer()
	i := 0
	for b.Loop() {
		i++
		seq++
		c, err := atlas.Says(svc, atlas.FactPurpose, fmt.Sprintf("Purpose %d", i), src, t1)
		if err != nil {
			b.Fatal(err)
		}
		body := benchRecord(b, fmt.Sprintf("bench-%d", i), c)
		env, err := atlasdomain.Domain{}.Envelope(body)
		if err != nil {
			b.Fatal(err)
		}
		rec := statelog.Record{Envelope: env, Position: position(seq), Payload: body, StoredAt: t1}
		if err := db.Tx(b.Context(), func(tx *sql.Tx) error {
			if _, gated, err := applier.Gated(b.Context(), tx, rec); err != nil || gated {
				return fmt.Errorf("gated %v: %w", gated, err)
			}
			_, err := applier.Apply(b.Context(), tx, rec, statelog.ApplyOptions{StoredAt: t1, MaxVariables: 999})
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// referenceReader is a reader over the reference company's estate.
func referenceReader(b *testing.B) *atlasdomain.Reader {
	b.Helper()
	db, seq, _ := seedReference(b)
	authority, err := statelogtest.LocalReader(atlasdomain.Domain{}, db, position(seq))
	if err != nil {
		b.Fatal(err)
	}
	r, err := atlasdomain.NewReader(atlasdomain.ReaderOptions{DB: db.Reader(), Log: authority})
	if err != nil {
		b.Fatal(err)
	}
	return r
}

// BenchmarkAtlasLogPage reads the newest page of the reference company's log,
// at the largest page a question scans: what one atlas_log costs a data node.
func BenchmarkAtlasLogPage(b *testing.B) {
	r := referenceReader(b)
	q := atlasdomain.LogQuery{Limit: atlasdomain.MaxPageSize, Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	b.ResetTimer()
	for b.Loop() {
		page, err := r.Log(b.Context(), q)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(page.Lines)), "lines")
	}
}

// BenchmarkAtlasActivity counts the reference company's log with every line
// inside the window, the worst case, and reads each entry's newest record:
// what one atlas_activity costs a data node.
func BenchmarkAtlasActivity(b *testing.B) {
	r := referenceReader(b)
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	b.ResetTimer()
	for b.Loop() {
		activity, err := r.Activity(b.Context(), t0.Add(-time.Hour), fresh)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(activity.Days)), "counts")
		b.ReportMetric(float64(len(activity.Last)), "last")
	}
}
