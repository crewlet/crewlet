package atlasdomain_test

import (
	"bytes"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The instants the apply cases store records at.
var (
	t0 = time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)
	t1 = t0.Add(10 * 24 * time.Hour)
	t2 = t0.Add(20 * 24 * time.Hour)
	t3 = t0.Add(30 * 24 * time.Hour)
)

// The entries the cases talk about.
var (
	checkout = atlas.MustRef(atlas.KindService, "checkout-api")
	orders   = atlas.MustRef(atlas.KindDatastore, "on/ssh/db-1/postgres/5432")
	web      = atlas.MustRef(atlas.KindHost, "ssh/web-1")
	cloudWeb = atlas.MustRef(atlas.KindHost, "aws/123456789012/eu-west-1/i-0abc")
	internal = atlas.MustRef(atlas.KindHostname, "secret-merger.internal")
)

// A PERSON'S CLAIM APPLIES ONCE ON EVERY NODE: two data nodes on one log hold
// byte-identical rows, and a redelivery of every record changes none.
func TestAPersonsClaimAppliesOnceOnEveryNode(t *testing.T) {
	t.Parallel()
	log := openAtlasLog(t)
	a, b := newWriteNode(t, log, "data-1"), newWriteNode(t, log, "data-2")
	a.record("ada", must(t)(atlas.Exists(checkout, asPerson, writerClock)))
	a.record("ada", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes payments", asPerson, writerClock)))
	b.record("grace", must(t)(atlas.Relates(checkout, atlas.RelationDependsOn, orders, asPerson, writerClock)))
	a.drain()
	b.drain()
	if got, want := a.dump(), b.dump(); !maps.EqualFunc(got, want, func(x, y []string) bool {
		return strings.Join(x, "\n") == strings.Join(y, "\n")
	}) {
		t.Fatalf("two nodes on one log hold different rows:\n%v\n%v", got, want)
	}
	before := a.dump()
	a.redeliver()
	if after := a.dump(); strings.Join(after["atlas_claims"], "\n") != strings.Join(before["atlas_claims"], "\n") {
		t.Errorf("a redelivery changed the claims:\n%v\n%v", before["atlas_claims"], after["atlas_claims"])
	}
	if n := len(a.live()); n != 3 {
		t.Errorf("%d live claims, want 3", n)
	}
}

// THE SOURCE IS THE CALLER, never what the claim says: a person cannot record
// a claim in somebody else's name, or as the settings.
func TestTheSourceIsTheCallerNotTheClaim(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	forged := must(t)(atlas.Exists(checkout, atlas.PersonSource("somebody-else"), writerClock))
	forged.Source = atlas.SettingsSource("r1")
	n.record("ada", forged)
	n.drain()
	live := n.live()
	if len(live) != 1 || live[0].Source != atlas.PersonSource("ada") {
		t.Fatalf("the claim landed as %+v, want ada's", live)
	}
}

// ONLY A PERSON RECORDS A PERSON'S CLAIM: an agent's is a record of its own,
// with its turn's evidence, and the engine records nothing as a person.
func TestOnlyAPersonRecordsAPersonsClaim(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	claim := must(t)(atlas.Exists(checkout, asPerson, writerClock))
	for _, kind := range []iam.ActorKind{iam.ActorAgent, iam.ActorSystem} {
		c := personCall("sre")
		c.By.Kind = kind
		if _, err := n.writer.Record(t.Context(), c, claim, atlasdomain.NoIfMatch); !errors.Is(err, atlasdomain.ErrInvalid) {
			t.Errorf("a %s recorded a person's claim: %v", kind, err)
		}
	}
	operator := personCall("ci:deploy")
	operator.By.Kind = iam.ActorOperator
	if _, err := n.writer.Record(t.Context(), operator, claim, atlasdomain.NoIfMatch); err != nil {
		t.Errorf("an operator credential was refused: %v", err)
	}
}

// A VALUE SHAPED LIKE A CREDENTIAL IS REFUSED BY THE WRITER, and nothing is
// published: the applier judges no text, so the writer is the only place a
// value can be stopped.
func TestAValueShapedLikeACredentialIsRefusedAtTheWriter(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	claim := atlas.Claim{Subject: checkout, Facet: atlas.Facet{Kind: atlas.FacetFact, Fact: atlas.FactPurpose},
		Value: atlas.Value{Type: atlas.TypeLabel, Text: "ghp_" + strings.Repeat("a", 36)}}
	_, err := n.writer.Record(t.Context(), personCall("ada"), claim, atlasdomain.NoIfMatch)
	if !errors.Is(err, atlasdomain.ErrInvalid) {
		t.Fatalf("a token as a purpose: %v, want ErrInvalid", err)
	}
	if end, _ := n.log.End(t.Context()); end != 0 {
		t.Errorf("the log holds %d records after a refusal", end)
	}
}

// A KEY SHAPED LIKE A CREDENTIAL IS REFUSED AT THE WRITER, the entry's own and
// another entry's a claim names, though the key's grammar admits it, and the
// refusal repeats neither: a key the vocabulary reads is not yet a key a writer
// may record.
//
// Mutation: drop the claim's Recordable check in Writer.Record and the
// relation to the token-keyed host lands.
func TestAKeyShapedLikeACredentialIsRefusedAtTheWriter(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	token := "ghp_" + strings.Repeat("a", 36)
	host := atlas.Ref{Kind: atlas.KindHost, Key: "addr/" + token}
	if _, err := atlas.NewRef(host.Kind, host.Key); err != nil {
		t.Fatalf("the case needs a key the grammar admits: %v", err)
	}
	for _, claim := range []atlas.Claim{
		{Subject: host, Facet: atlas.Facet{Kind: atlas.FacetExists}},
		{Subject: checkout, Facet: atlas.Facet{Kind: atlas.FacetRelation, Relation: atlas.RelationRunsOn,
			Target: atlas.Target{Entry: host}}},
	} {
		_, err := n.writer.Record(t.Context(), personCall("ada"), claim, atlasdomain.NoIfMatch)
		if !errors.Is(err, atlasdomain.ErrInvalid) || !errors.Is(err, atlas.ErrCredential) {
			t.Errorf("a claim naming a token-keyed host: %v, want ErrInvalid and ErrCredential", err)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Errorf("the refusal repeats the key: %v", err)
		}
	}
	if end, _ := n.log.End(t.Context()); end != 0 {
		t.Errorf("the log holds %d records after a refusal", end)
	}
}

// A REFUSAL READS AS ITS SENTENCE, without the package or class words a log
// reads: what a person's write surface and a tool answer are told.
func TestAnAtlasRefusalReadsAsItsSentence(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	claim := atlas.Claim{Subject: checkout, Facet: atlas.Facet{Kind: atlas.FacetFact, Fact: atlas.FactImportance},
		Value: atlas.Value{Type: atlas.TypeWord, Text: "vital"}}
	_, err := n.writer.Record(t.Context(), personCall("ada"), claim, atlasdomain.NoIfMatch)
	said := atlasdomain.Sentence(err)
	if err == nil || strings.Contains(said, "atlasdomain") || strings.Contains(said, "atlas:") ||
		!strings.Contains(said, "importance") {
		t.Errorf("the refusal %v reads %q, want its sentence naming the fact alone", err, said)
	}
}

// AN EDIT OVER AN EDIT IT DID NOT SEE IS REFUSED: a person's write names the
// entry's version they decided from.
func TestAnEditOverAnEditItDidNotSeeIsRefused(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	n.record("ada", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes payments", asPerson, writerClock)))
	n.drain()
	read, found, err := n.reader().Entry(t.Context(), checkout.ID().String(), statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || !found {
		t.Fatalf("read the entry: %v, found %v", err, found)
	}
	n.record("grace", must(t)(atlas.Says(checkout, atlas.FactImportance, "critical", asPerson, writerClock)))
	stale := must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes refunds", asPerson, writerClock))
	if _, err := n.writer.Record(t.Context(), personCall("ada"), stale, read.Version); !errors.Is(err, atlasdomain.ErrStaleVersion) {
		t.Fatalf("an edit from version %d after grace's: %v, want ErrStaleVersion", read.Version, err)
	}
	n.drain()
	fresh, _, err := n.reader().Entry(t.Context(), checkout.ID().String(), statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.Record(t.Context(), personCall("ada"), stale, fresh.Version); err != nil {
		t.Errorf("an edit from the current version: %v", err)
	}
}

// A VOICE'S NEWER CLAIM REPLACES ITS OLDER ONE, and the older stays as the
// entry's history; another person's word on the same fact is a voice of its own.
func TestAVoicesNewerClaimReplacesItsOlderOne(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	ada, grace := atlas.PersonSource("ada"), atlas.PersonSource("grace")
	e.apply(claimRecord(t, "op-1", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes payments", ada, t0))), t0)
	e.apply(claimRecord(t, "op-2", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes refunds", ada, t1))), t1)
	e.apply(claimRecord(t, "op-3", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Bills customers", grace, t2))), t2)
	live := e.live()
	if len(live) != 2 {
		t.Fatalf("%d live claims, want ada's newest and grace's", len(live))
	}
	for _, c := range live {
		if c.Source == ada && c.Value.Text != "Takes refunds" {
			t.Errorf("ada's live purpose is %q, want her newest", c.Value.Text)
		}
	}
	history := e.history(checkout)
	if len(history) != 3 {
		t.Fatalf("%d claims in the history, want 3", len(history))
	}
	if oldest := history[2]; !oldest.Replaced || oldest.ReplacedBy != position(2) || !oldest.ReplacedAt.Equal(t1) {
		t.Errorf("ada's first purpose is %+v, want replaced by her second at %s", oldest, t1)
	}
	if v := atlas.Fold(live, t3); len(v.Entries) != 1 || v.Entries[0].Facts[0].Value.Text != "Bills customers" {
		t.Errorf("the fold reads %+v, want the newest person's purpose", v.Entries)
	}
}

// A RETIRED ENTRY IS RESTORED BY A PERSON'S NEWER WORD, and the person's
// newest word on the entry itself is what the retention reads.
func TestARetiredEntryIsRestoredByAPersonsNewerWord(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	ada, grace := atlas.PersonSource("ada"), atlas.PersonSource("grace")
	exists := must(t)(atlas.Exists(checkout, ada, t0))
	e.apply(claimRecord(t, "op-1", exists), t0)
	e.apply(claimRecord(t, "op-2", must(t)(atlas.Denial(exists, grace, t1))), t1)
	if got := e.count(`SELECT COUNT(*) FROM atlas_entries WHERE retired = 1`); got != 1 {
		t.Fatalf("grace's retirement left %d entries retired, want 1", got)
	}
	if v := atlas.Fold(e.live(), t3); v.Entries[0].State != atlas.StateRetired {
		t.Fatalf("the fold reads the retired entry as %s", v.Entries[0].State)
	}
	e.apply(claimRecord(t, "op-3", must(t)(atlas.Exists(checkout, ada, t2))), t2)
	if got := e.count(`SELECT COUNT(*) FROM atlas_entries WHERE retired = 1`); got != 0 {
		t.Errorf("ada's restore left %d entries retired, want 0", got)
	}
	if v := atlas.Fold(e.live(), t3); v.Entries[0].State == atlas.StateRetired {
		t.Error("the restored entry still reads as retired")
	}
}

// A PERSON'S "NOT THE SAME" WINS ON THE LOG as it does in the fold: two
// people's words about one pair, recorded on the two entries' own subjects.
func TestAPersonsNotTheSameWinsOnTheLog(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	n.record("ada", must(t)(atlas.Exists(web, asPerson, writerClock)))
	n.record("ada", must(t)(atlas.Exists(cloudWeb, asPerson, writerClock)))
	n.record("ada", must(t)(atlas.Same(cloudWeb, web, asPerson, writerClock)))
	n.drain()
	if got := len(n.view().Entries); got != 1 {
		t.Fatalf("%d entries after ada joined them, want 1", got)
	}
	n.record("grace", must(t)(atlas.Distinct(web, cloudWeb, asPerson, writerClock)))
	n.drain()
	if got := len(n.view().Entries); got != 2 {
		t.Errorf("%d entries after grace kept them apart, want 2: \"not the same\" always wins", got)
	}
}

// A PURGED ENTRY IS ERASED EVERYWHERE, ITS HISTORY INCLUDED, AND NOTHING
// BRINGS IT BACK: every claim on it and naming it goes, the writer refuses the
// next one, and a record decided before the purge that lands after it is
// dropped on every node.
func TestAPurgedEntryIsErasedEverywhereAndNeverComesBack(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	ada := atlas.PersonSource("ada")
	e.apply(claimRecord(t, "op-1", must(t)(atlas.Exists(orders, ada, t0))), t0)
	e.apply(claimRecord(t, "op-2", must(t)(atlas.Says(orders, atlas.FactEngine, "postgres", ada, t0))), t0)
	e.apply(claimRecord(t, "op-3", must(t)(atlas.Says(orders, atlas.FactEngine, "mysql", ada, t1))), t1)
	e.apply(claimRecord(t, "op-4", must(t)(atlas.Exists(checkout, ada, t1))), t1)
	e.apply(claimRecord(t, "op-5", must(t)(atlas.Relates(checkout, atlas.RelationDependsOn, orders, ada, t1))), t1)
	late := claimRecord(t, "op-late", must(t)(atlas.Says(orders, atlas.FactPort, "5432", ada, t1)))
	lateRelation := claimRecord(t, "op-late-relation",
		must(t)(atlas.Relates(atlas.MustRef(atlas.KindService, "billing"), atlas.RelationDependsOn, orders, ada, t1)))

	if reason := e.apply(purgeEntryRecord(t, "purge-1", orders), t2); reason != "" {
		t.Fatalf("the purge was dropped as %s", reason)
	}
	if got := e.count(`SELECT COUNT(*) FROM atlas_claims WHERE entry_id = ? OR target_id = ?`,
		orders.ID().String(), orders.ID().String()); got != 0 {
		t.Errorf("%d claims on or naming the purged entry remain, its history included", got)
	}
	if got := len(e.history(orders)); got != 0 {
		t.Errorf("the purged entry's history holds %d claims", got)
	}
	if got := len(e.live()); got != 1 {
		t.Errorf("%d live claims after the purge, want checkout's own", got)
	}
	for name, body := range map[string][]byte{"a late fact": late, "a late relation naming it": lateRelation} {
		if reason := e.apply(body, t3); reason != statelog.ReasonDeleted {
			t.Errorf("%s about the purged entry was %q, want dropped as deleted", name, reason)
		}
	}
	if got := e.count(`SELECT rejects FROM atlas_purges WHERE kind = 'entry' AND id = ?`, orders.ID().String()); got != 2 {
		t.Errorf("the marker counts %d returns it blocked, want 2", got)
	}
	if reason := e.apply(purgeEntryRecord(t, "purge-1", orders), t3); reason != "" {
		t.Errorf("a redelivery of the purge's own record was %q, want applied", reason)
	}
	if reason := e.apply(purgeEntryRecord(t, "purge-2", orders), t3); reason != statelog.ReasonDeleted {
		t.Errorf("a second purge of a purged entry was %q, want dropped", reason)
	}
}

// A PURGED VALUE IS ERASED EVERYWHERE IT APPEARS: every claim carrying it, the
// entry it is the key of with everything about it, and every claim naming that
// entry, history included; and it never comes back on any entry.
func TestAPurgedValueIsErasedEverywhere(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	ada := atlas.PersonSource("ada")
	const secret = "secret-merger.internal"
	e.apply(claimRecord(t, "op-1", must(t)(atlas.Exists(internal, ada, t0))), t0)
	e.apply(claimRecord(t, "op-2", must(t)(atlas.Says(web, atlas.FactAddress, secret, ada, t0))), t0)
	e.apply(claimRecord(t, "op-3", must(t)(atlas.Says(web, atlas.FactAddress, "10.0.0.7", ada, t0))), t0)
	e.apply(claimRecord(t, "op-4", must(t)(atlas.Relates(internal, atlas.RelationResolvesTo, web, ada, t0))), t0)
	e.apply(claimRecord(t, "op-5", must(t)(atlas.Exists(checkout, ada, t0))), t0)
	e.apply(claimRecord(t, "op-6", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Fronts "+secret, ada, t0))), t0)

	e.apply(purgeValueRecord(t, "purge-1", secret), t1)
	for _, c := range e.live() {
		if c.Subject == internal || c.Value.Text == secret || c.Facet.Target.Entry == internal {
			t.Errorf("a live claim still holds the purged value: %+v", c)
		}
	}
	if got := len(e.live()); got != 3 {
		t.Errorf("%d live claims after the purge, want web's other address, checkout and its purpose", got)
	}
	if got := e.count(`SELECT COUNT(*) FROM atlas_entries WHERE key = ?`, secret); got != 0 {
		t.Errorf("the entry whose key was purged remains")
	}
	for name, c := range map[string]atlas.Claim{
		"on another entry":                 must(t)(atlas.Says(cloudWeb, atlas.FactAddress, secret, ada, t2)),
		"on the entry it was the key of":   must(t)(atlas.Exists(internal, ada, t2)),
		"from the entry it was the key of": must(t)(atlas.Relates(internal, atlas.RelationInZone, atlas.MustRef(atlas.KindDomain, "acme.example"), ada, t2)),
	} {
		if reason := e.apply(claimRecord(t, "late-"+name, c), t2); reason != statelog.ReasonDeleted {
			t.Errorf("a claim %s was %q, want dropped as deleted", name, reason)
		}
	}
}

// A PURGE NEVER WRITES THE VALUE DOWN: its record and its marker name the
// value by its digest alone.
func TestAPurgeNeverWritesTheValueDown(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	const secret = "Project Nightingale"
	n.record("ada", must(t)(atlas.Says(checkout, atlas.FactPurpose, secret, asPerson, writerClock)))
	if _, err := n.writer.PurgeValue(t.Context(), adminCall(), secret); err != nil {
		t.Fatalf("purge: %v", err)
	}
	n.drain()
	end, err := n.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, payload, _, _, err := n.log.At(t.Context(), end)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(secret)) {
		t.Error("the purge's record carries the value it erased")
	}
	for table, rows := range n.dump() {
		for _, row := range rows {
			if strings.Contains(row, secret) {
				t.Errorf("%s still holds the purged value: %s", table, row)
			}
		}
	}
	purges, err := n.reader().Purges(t.Context(), statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(purges) != 1 || purges[0].Entry || purges[0].ID != atlasdomain.Digest(secret) || purges[0].By.Name != "admin" {
		t.Errorf("the purges read %+v (%v), want one value purge by admin", purges, err)
	}
	if _, err := n.writer.Record(t.Context(), personCall("ada"),
		must(t)(atlas.Says(web, atlas.FactName, secret, asPerson, writerClock)), atlasdomain.NoIfMatch); !errors.Is(err, atlasdomain.ErrPurged) {
		t.Errorf("recording the purged value again: %v, want ErrPurged", err)
	}
	if _, err := n.writer.PurgeValue(t.Context(), adminCall(), secret); !errors.Is(err, atlasdomain.ErrPurged) {
		t.Errorf("purging it twice: %v, want ErrPurged", err)
	}
}

// AN AGENT NEVER REMOVES ANYTHING from the atlas, a purge least of all.
func TestAnAgentNeverPurges(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	c := personCall("sre")
	c.By.Kind = iam.ActorAgent
	if _, err := n.writer.PurgeEntry(t.Context(), c, checkout); !errors.Is(err, atlasdomain.ErrInvalid) {
		t.Errorf("an agent's purge of an entry: %v", err)
	}
	if _, err := n.writer.PurgeValue(t.Context(), c, "checkout-api"); !errors.Is(err, atlasdomain.ErrInvalid) {
		t.Errorf("an agent's purge of a value: %v", err)
	}
}

// THE PUBLISHER'S GATE READER AGREES WITH THE APPLIER about a record that
// carries a purged value on another entry: with the record's body it reads the
// value, and without it the subject alone, which the decide then meets.
func TestTheGateReaderReadsAPurgedValueInTheBody(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	ada := atlas.PersonSource("ada")
	const secret = "10.9.9.9"
	e.apply(purgeValueRecord(t, "purge-1", secret), t0)
	body := claimRecord(t, "op-1", must(t)(atlas.Says(web, atlas.FactAddress, secret, ada, t1)))
	gates := atlasdomain.NewGates(e.db.Reader())
	subject := statelog.Subject{Kind: string(atlasdomain.KindEntry), ID: web.ID().String()}
	reason, gated, err := gates.GatedAt(t.Context(), subject, "node-a", "op-1", position(e.seq+1), body)
	if err != nil || !gated || reason != statelog.ReasonDeleted {
		t.Errorf("with the body: (%q, %v, %v), want deleted", reason, gated, err)
	}
	if reason := e.apply(body, t1); reason != statelog.ReasonDeleted {
		t.Errorf("the applier: %q, want deleted", reason)
	}
}

// THE RETENTION TAKES ONLY HISTORY: a claim its voice replaced, and an entry a
// person retired that nothing was recorded about since, each once it is past
// the instant the expiry names. A live claim never expires.
func TestTheRetentionTakesOnlyHistory(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	ada := atlas.PersonSource("ada")
	gone := atlas.MustRef(atlas.KindService, "old-batch")
	still := atlas.MustRef(atlas.KindService, "legacy-sync")
	e.apply(claimRecord(t, "op-1", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes payments", ada, t0))), t0)
	e.apply(claimRecord(t, "op-2", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes refunds", ada, t1))), t1)
	goneExists := must(t)(atlas.Exists(gone, ada, t0))
	e.apply(claimRecord(t, "op-3", goneExists), t0)
	e.apply(claimRecord(t, "op-4", must(t)(atlas.Denial(goneExists, ada, t1))), t1)
	stillExists := must(t)(atlas.Exists(still, ada, t0))
	e.apply(claimRecord(t, "op-5", stillExists), t0)
	e.apply(claimRecord(t, "op-6", must(t)(atlas.Denial(stillExists, ada, t1))), t1)
	// SOMEBODY STILL REPORTS IT: a retired entry is kept while claims arrive.
	e.apply(claimRecord(t, "op-7", must(t)(atlas.Says(still, atlas.FactPurpose, "Syncs ledgers", atlas.PersonSource("grace"), t3))), t3)

	hours, err := e.reader().ExpirableHours(t.Context(), t2, 10)
	if err != nil || len(hours) != 1 || !hours[0].Equal(t1.Truncate(time.Hour)) {
		t.Fatalf("the hours past %s are %v (%v), want the hour of %s", t2, hours, err, t1)
	}
	if got := e.count(`SELECT COUNT(*) FROM atlas_claims`); got != 7 {
		t.Fatalf("%d claims before the expiry, want 7", got)
	}
	e.apply(expireRecord(t, "expire-early", t1), t2)
	if got := e.count(`SELECT COUNT(*) FROM atlas_claims`); got != 7 {
		t.Errorf("an expiry through the instant the history was made took %d claims", 7-got)
	}
	e.apply(expireRecord(t, "expire-1", t1.Add(time.Hour)), t2)
	if got := e.count(`SELECT COUNT(*) FROM atlas_claims WHERE entry_id = ?`, checkout.ID().String()); got != 1 {
		t.Errorf("checkout keeps %d claims, want its live purpose alone", got)
	}
	if got := e.count(`SELECT COUNT(*) FROM atlas_entries WHERE entry_id = ?`, gone.ID().String()); got != 0 {
		t.Error("the retired entry nothing reports outlived the retention")
	}
	if got := e.count(`SELECT COUNT(*) FROM atlas_claims WHERE entry_id = ?`, still.ID().String()); got != 2 {
		t.Errorf("the retired entry grace still reports keeps %d claims, want its retirement and her purpose", got)
	}
}

// THE WRITER'S EXPIRY IS DECIDED IN ITS OWN SNAPSHOT: an hour with nothing past
// the retention is not published.
func TestAnExpiryWithNothingDueIsNotPublished(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	n.record("ada", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes payments", asPerson, writerClock)))
	n.drain()
	hour := time.Now().UTC().Truncate(time.Hour)
	if _, err := n.writer.Expire(t.Context(), engineCall(), hour, hour.Add(2*time.Hour)); !errors.Is(err, atlasdomain.ErrNotDue) {
		t.Errorf("an expiry of an hour holding only live claims: %v, want ErrNotDue", err)
	}
}

// THE APPLIER JUDGES NO TEXT: a word this build's vocabulary does not know
// lands as the same row on every build, and the fold names the claim it skips
// rather than failing.
func TestTheApplierJudgesNoText(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	wire := wireClaim("op-1", must(t)(atlas.Says(checkout, atlas.FactPurpose, "Takes payments", atlas.PersonSource("ada"), t0)))
	wire.Fact = "carbon_footprint"
	body, err := rawRecord(atlasdomain.EntrySubject(checkout), "node-a", "op-1", atlasdomain.OpClaim, sentinel(), wire)
	if err != nil {
		t.Fatal(err)
	}
	if reason := e.apply(body, t0); reason != "" {
		t.Fatalf("a claim with a fact this build does not know was dropped as %s", reason)
	}
	v := atlas.Fold(e.live(), t1)
	if len(v.Skipped) != 1 || v.Skipped[0].Subject != checkout {
		t.Errorf("the fold reads %+v, want the claim skipped by name", v)
	}
}

// EVERY CLAIM LANDS ON ITS OWN ENTRY'S SUBJECT, which the applier keys every
// row on: the writer derives the subject from the claim, so the two never
// disagree.
func TestEveryClaimLandsOnItsOwnEntrysSubject(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAtlasLog(t), "data-1")
	n.record("ada", must(t)(atlas.Relates(checkout, atlas.RelationDependsOn, orders, asPerson, writerClock)))
	n.drain()
	_, payload, _, _, err := n.log.At(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := n.verifier.Open(payload)
	rec, err := atlasdomain.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := atlasdomain.DecodeMutation(rec)
	if err != nil {
		t.Fatal(err)
	}
	claim := mutation.(atlasdomain.Claim)
	if rec.Subject != atlasdomain.EntrySubject(checkout) || claim.Target.ID != orders.ID().String() {
		t.Errorf("the claim on %s with target %s landed on %s", checkout, claim.Target.ID, rec.Subject)
	}
	if !claim.ObservedAt.IsZero() {
		t.Error("a person's claim carries the writer's clock: the log's own instant orders people's words")
	}
}
