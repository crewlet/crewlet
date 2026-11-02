package artifactsdomain_test

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE APPLIER ALONE, over records formed by hand: the cases a writer that keeps
// its rules can never produce, which is exactly what the applier's own checks
// exist for. Each is a refusal the applier makes deterministically, written as
// the record's history row on every data node alike.

// estate is one node's replicated estate and applier, fed records by hand.
type estate struct {
	t       *testing.T
	db      store.ReplicatedHandle
	applier *artifactsdomain.Applier
	seq     uint64
	moved   []artifactsdomain.Moved
}

// newEstate is a fresh estate.
func newEstate(t *testing.T) *estate {
	t.Helper()
	e := &estate{t: t, db: openStore(t, "estate.db")}
	e.applier = artifactsdomain.NewApplier("estate-node", func(m []artifactsdomain.Moved) {
		e.moved = append(e.moved, m...)
	})
	return e
}

// apply applies one record at the next position, stored at at, reporting
// whether a gate dropped it.
func (e *estate) apply(body []byte, at time.Time) statelog.Reason {
	e.t.Helper()
	e.seq++
	env, err := artifactsdomain.Domain{}.Envelope(body)
	if err != nil {
		e.t.Fatalf("envelope: %v", err)
	}
	rec := statelog.Record{
		Envelope: env, Payload: body, StoredAt: at,
		Position: statelog.Position{Stream: artifactsdomain.Domain{}.Stream().Name, Generation: 1, Seq: e.seq},
	}
	var reason statelog.Reason
	if err := e.db.Tx(e.t.Context(), func(tx *sql.Tx) error {
		r, gated, err := e.applier.Gated(e.t.Context(), tx, rec)
		if err != nil || gated {
			reason = r
			return err
		}
		_, err = e.applier.Apply(e.t.Context(), tx, rec, statelog.ApplyOptions{
			Now: at, StoredAt: at, MaxVariables: 999,
		})
		return err
	}); err != nil {
		e.t.Fatalf("apply record %d: %v", e.seq, err)
	}
	e.applier.Committed(e.t.Context())
	return reason
}

// verdict is the history row operation opID wrote on artifact.
func (e *estate) verdict(artifact, opID string) (outcome, reason string, summary artifactsdomain.HistorySummary) {
	e.t.Helper()
	var raw string
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(e.t.Context(), `
			SELECT outcome, reason, summary FROM artifact_history
			WHERE artifact_id = ? AND op_id = ?`, artifact, opID).Scan(&outcome, &reason, &raw)
	}); err != nil {
		e.t.Fatalf("read the verdict of %s on %s: %v", opID, artifact, err)
	}
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		e.t.Fatalf("decode the summary: %v", err)
	}
	return outcome, reason, summary
}

// count is the rows a query counts.
func (e *estate) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(e.t.Context(), query, args...).Scan(&n)
	}); err != nil {
		e.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// handRecord is one record formed by hand on subject, by writer.
func handRecord(t *testing.T, subject artifactsdomain.Subject, op artifactsdomain.OpKind,
	opID string, scope artifactsdomain.ScopeSet, payload any) []byte {

	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := artifactsdomain.Encode(artifactsdomain.MutationRecord{
		RecordEnvelope: artifactsdomain.RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, CreatedAt: monday, Gen: 1,
			Writer: "hand-node", Scope: scope,
		},
		Mutation: body, Actor: "jane", ActorKind: iam.ActorHuman,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}

// handCreate is a one-page create of artifact id holding obj, stamped with lim.
func handCreate(t *testing.T, id, opID string, obj objstore.Object, lim artifacts.Limits) []byte {
	t.Helper()
	auth := suiteAuthority()
	auth.Limits = lim
	return handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindArtifact, ID: id},
		artifactsdomain.OpCreate, opID, artifactsdomain.ScopeSet{Subject: true},
		artifactsdomain.CreatePayload{
			V: 1, Artifact: id, Kind: artifacts.KindHTML, Title: "Hand " + id,
			Owner: "jane", OwnerKind: iam.ActorHuman,
			Version: artifactsdomain.VersionPayload{
				Number: 1, VersionID: uuid.NewString(), Contract: "1.0.0",
				Manifest: artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{{
					Path: "index.html", ContentType: "text/html; charset=utf-8", Object: obj,
				}}},
			},
			Authority: auth,
		})
}

// handPublish is version number of artifact id holding files.
func handPublish(t *testing.T, id, opID string, number int, prune []int, files ...artifacts.Entry) []byte {
	t.Helper()
	return handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindArtifact, ID: id},
		artifactsdomain.OpPublish, opID, artifactsdomain.ScopeSet{Subject: true},
		artifactsdomain.PublishPayload{V: 1, Authority: suiteAuthority(),
			Version: artifactsdomain.VersionPayload{
				Number: number, VersionID: uuid.NewString(), Base: number - 1,
				Contract: "1.0.0", Prune: prune,
				Manifest: artifacts.Manifest{Entry: files[0].Path, Files: files},
			}})
}

// newID is a fresh artifact id.
func newID() string { return artifacts.IDFor(uuid.NewString()).String() }

// A CARRIED OBJECT MUST BE NAMED WHERE IT IS CARRIED FROM: the apply
// writes a carried entry only when its own transaction finds the object named by
// this artifact, with the digest and size the entry claims; otherwise the
// record is refused on every data node and its writer told the paths to upload.
//
// Mutation: drop the carry check in planObjects and the dangling carry applies.
func TestACarriedObjectMustBeNamedWhereItIsCarriedFrom(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	id := newID()
	held := suiteObject("held", "<p>held</p>")
	e.apply(handCreate(t, id, "op-create", held, limits()), monday)

	never := suiteObject("never-uploaded", "<p>gone</p>")
	e.apply(handPublish(t, id, "op-dangling", 2, nil, artifacts.Entry{
		Path: "index.html", ContentType: "text/html; charset=utf-8", Object: never, Carried: true,
	}), monday.Add(time.Minute))
	outcome, reason, summary := e.verdict(id, "op-dangling")
	if outcome != string(artifactsdomain.OutcomeRefused) ||
		reason != string(artifactsdomain.RefusedCarriedObjectGone) {
		t.Fatalf("a carry of an object the artifact never named was %s (%s)", outcome, reason)
	}
	if len(summary.Paths) != 1 || summary.Paths[0] != "index.html" {
		t.Errorf("the refusal names paths %v, want the one to upload again", summary.Paths)
	}
	if got := e.count(`SELECT head_number FROM artifacts WHERE id = ?`, id); got != 1 {
		t.Errorf("the head moved to %d on a refused publish", got)
	}

	// A CARRY CLAIMING OTHER BYTES THAN THE OBJECT HOLDS is refused too: a
	// held key is never named for bytes it does not hold.
	lying := held
	lying.Size++
	e.apply(handPublish(t, id, "op-lying", 2, nil, artifacts.Entry{
		Path: "index.html", ContentType: "text/html; charset=utf-8", Object: lying, Carried: true,
	}), monday.Add(2*time.Minute))
	if outcome, _, _ := e.verdict(id, "op-lying"); outcome != string(artifactsdomain.OutcomeRefused) {
		t.Errorf("a carry claiming another size than the held object applied")
	}

	// AND THE HONEST CARRY APPLIES, naming the same object twice now.
	e.apply(handPublish(t, id, "op-carry", 2, nil, artifacts.Entry{
		Path: "index.html", ContentType: "text/html; charset=utf-8", Object: held, Carried: true,
	}), monday.Add(3*time.Minute))
	if outcome, _, _ := e.verdict(id, "op-carry"); outcome != string(artifactsdomain.OutcomeApplied) {
		t.Fatalf("an honest carry was refused")
	}
	if refs := e.count(`SELECT refs FROM artifact_objects WHERE artifact_id = ? AND object = ?`,
		id, held.Key.String()); refs != 2 {
		t.Errorf("the carried object is named %d times, want 2", refs)
	}
}

// NO CARRY CROSSES AN ARTIFACT: artifact B neither carries A's object
// nor names A's key as its own upload. A company-wide lookup would tell a
// writer whether an artifact they cannot read holds a file.
//
// Mutation: compare artifact_objects without the artifact_id in planObjects or
// drop namedElsewhere, and B's record applies.
func TestNoCarryCrossesAnArtifact(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	a, b := newID(), newID()
	shared := suiteObject("a-page", "<p>a</p>")
	e.apply(handCreate(t, a, "op-a", shared, limits()), monday)
	e.apply(handCreate(t, b, "op-b", suiteObject("b-page", "<p>b</p>"), limits()), monday)

	e.apply(handPublish(t, b, "op-b-carries-a", 2, nil, artifacts.Entry{
		Path: "index.html", ContentType: "text/html; charset=utf-8", Object: shared, Carried: true,
	}), monday.Add(time.Minute))
	if _, reason, _ := e.verdict(b, "op-b-carries-a"); reason != string(artifactsdomain.RefusedCarriedObjectGone) {
		t.Errorf("B carried A's object: refusal %q", reason)
	}
	e.apply(handPublish(t, b, "op-b-uploads-a", 2, nil, artifacts.Entry{
		Path: "index.html", ContentType: "text/html; charset=utf-8", Object: shared,
	}), monday.Add(2*time.Minute))
	if _, reason, _ := e.verdict(b, "op-b-uploads-a"); reason != string(artifactsdomain.RefusedObjectNamedElsewhere) {
		t.Errorf("B named A's key as its own upload: refusal %q", reason)
	}
	if got := e.count(`SELECT COUNT(*) FROM artifact_objects WHERE object = ?`, shared.Key.String()); got != 1 {
		t.Errorf("A's key is named by %d artifacts, want only A", got)
	}
}

// A RETIRED VERSION UNNAMES AN OBJECT ONLY WHEN NO RETAINED VERSION NAMES IT:
// the reference table counts manifest entries, and a row goes, letting the
// collector reclaim the file, at zero and not before.
//
// Mutation: delete an object row on any decrement in applyObjects and the
// object version 3 still carries reads as unnamed.
func TestARetiredVersionUnnamesOnlyWhatNothingRetainedNames(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	id := newID()
	first := suiteObject("v1", "<p>one</p>")
	e.apply(handCreate(t, id, "op-1", first, limits()), monday)
	kept := suiteObject("kept-css", "body{}")
	page2 := suiteObject("v2", "<p>two</p>")
	css := artifacts.Entry{Path: "site.css", ContentType: "text/css; charset=utf-8", Object: kept}
	e.apply(handPublish(t, id, "op-2", 2, nil,
		artifacts.Entry{Path: "index.html", ContentType: "text/html; charset=utf-8", Object: page2},
		css), monday.Add(time.Minute))
	css.Carried = true
	e.apply(handPublish(t, id, "op-3", 3, []int{1, 2},
		artifacts.Entry{Path: "index.html", ContentType: "text/html; charset=utf-8",
			Object: suiteObject("v3", "<p>three</p>")},
		css), monday.Add(2*time.Minute))

	if outcome, reason, _ := e.verdict(id, "op-3"); outcome != string(artifactsdomain.OutcomeApplied) {
		t.Fatalf("the publish that retires 1 and 2 was refused: %s", reason)
	}
	for _, gone := range []objstore.Object{first, page2} {
		if n := e.count(`SELECT COUNT(*) FROM artifact_objects WHERE object = ?`, gone.Key.String()); n != 0 {
			t.Errorf("object %s only retired versions named is still named", gone.Key)
		}
	}
	if refs := e.count(`SELECT refs FROM artifact_objects WHERE object = ?`, kept.Key.String()); refs != 1 {
		t.Errorf("the stylesheet version 3 carries is named %d times, want 1", refs)
	}
	if n := e.count(`SELECT COUNT(*) FROM artifact_versions WHERE artifact_id = ?`, id); n != 1 {
		t.Errorf("%d versions are kept, want only version 3", n)
	}
	var held, usage int64
	held = int64(e.count(`SELECT bytes_held FROM artifacts WHERE id = ?`, id))
	usage = int64(e.count(`SELECT bytes FROM artifact_usage WHERE scope = 'company'`))
	want := kept.Size + int64(len("<p>three</p>"))
	if held != want || usage != want {
		t.Errorf("held %d and usage %d bytes, want %d", held, usage, want)
	}
}

// THE STAMPED QUOTA IS ENFORCED AT THE APPLY: two creates that each passed
// their own decide land in log order, and the one past the stamped limit is
// refused on every data node, naming the Tier A key.
//
// Mutation: drop the artifacts-count check in applyCreate and the second
// create applies.
func TestAQuotaIsEnforcedFromTheStampAtTheApply(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	lim := limits()
	lim.Artifacts = 1
	first, second := newID(), newID()
	e.apply(handCreate(t, first, "op-first", suiteObject("q1", "<p>1</p>"), lim), monday)
	e.apply(handCreate(t, second, "op-second", suiteObject("q2", "<p>2</p>"), lim), monday)

	outcome, reason, summary := e.verdict(second, "op-second")
	if outcome != string(artifactsdomain.OutcomeRefused) ||
		reason != string(artifactsdomain.RefusedQuotaArtifacts) {
		t.Fatalf("the create past the stamped limit was %s (%s)", outcome, reason)
	}
	if summary.Key != artifactsdomain.QuotaKeyArtifacts || summary.Limit != 1 || summary.Usage != 1 {
		t.Errorf("the refusal reads %+v, want the limit, the usage and the Tier A key", summary)
	}
	if n := e.count(`SELECT COUNT(*) FROM artifacts`); n != 1 {
		t.Errorf("%d artifacts exist, want 1", n)
	}
}

// A NARROWING MOVES THE ACCESS EPOCH, and a widening does not: a view ticket
// minted before an unshare, a lowered role, a trash or a transfer is refused by
// the epoch it carries.
//
// Mutation: bump the epoch on every share, or on none, and a row goes red.
func TestANarrowingMovesTheAccessEpoch(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	id := newID()
	subject := artifactsdomain.Subject{Kind: artifactsdomain.KindArtifact, ID: id}
	e.apply(handCreate(t, id, "op-create", suiteObject("epoch", "<p>e</p>"), limits()), monday)
	bob := artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}
	epoch := func() int { return e.count(`SELECT access_epoch FROM artifacts WHERE id = ?`, id) }
	share := func(op string, role artifacts.Role) {
		e.apply(handRecord(t, subject, artifactsdomain.OpShare, op, artifactsdomain.ScopeSet{Subject: true},
			artifactsdomain.SharePayload{V: 1, Share: artifacts.Share{Audience: bob, Role: role,
				GrantedBy: iam.Actor{Name: "jane", Kind: iam.ActorHuman}}}), monday)
	}
	for _, step := range []struct {
		name string
		do   func()
		want int
	}{
		{"granting viewer widens", func() { share("op-viewer", artifacts.RoleViewer) }, 0},
		{"raising to editor widens", func() { share("op-editor", artifacts.RoleEditor) }, 0},
		{"lowering to viewer narrows", func() { share("op-lower", artifacts.RoleViewer) }, 1},
		{"unsharing narrows", func() {
			e.apply(handRecord(t, subject, artifactsdomain.OpUnshare, "op-unshare",
				artifactsdomain.ScopeSet{Subject: true},
				artifactsdomain.UnsharePayload{V: 1, Audience: bob}), monday)
		}, 2},
		{"trashing narrows", func() {
			e.apply(handRecord(t, subject, artifactsdomain.OpTrash, "op-trash",
				artifactsdomain.ScopeSet{Subject: true}, artifactsdomain.StatusPayload{V: 1}), monday)
		}, 3},
		{"restoring widens", func() {
			e.apply(handRecord(t, subject, artifactsdomain.OpRestore, "op-restore",
				artifactsdomain.ScopeSet{Subject: true}, artifactsdomain.StatusPayload{V: 1}), monday)
		}, 3},
		{"transferring narrows", func() {
			e.apply(handRecord(t, subject, artifactsdomain.OpTransfer, "op-transfer",
				artifactsdomain.ScopeSet{Subject: true},
				artifactsdomain.TransferPayload{V: 1, To: "bob", ToKind: iam.ActorHuman}), monday)
		}, 4},
	} {
		step.do()
		if got := epoch(); got != step.want {
			t.Errorf("%s: the access epoch is %d, want %d", step.name, got, step.want)
		}
	}
}

// A PURGE REMOVES EVERY ROW, AND ITS MARKER GATES WHATEVER ARRIVES AFTER IT:
// a redelivered or late record about the artifact applies nowhere, so a replay
// cannot resurrect what an administrator destroyed.
//
// Mutation: skip a table in applyPurge, or leave the asset subject out of
// gatedArtifact, and a row goes red.
func TestAPurgeRemovesEveryRowAndItsMarkerGatesWhatFollows(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	id, other := newID(), newID()
	e.apply(handCreate(t, id, "op-create", suiteObject("purge", "<p>p</p>"), limits()), monday)
	e.apply(handCreate(t, other, "op-other", suiteObject("other", "<p>o</p>"), limits()), monday)
	asset := uuid.NewString()
	e.apply(handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindAsset, ID: id + "." + asset},
		artifactsdomain.OpCreate, "op-asset", artifactsdomain.ScopeSet{Subject: true},
		artifactsdomain.AssetPayload{V: 1, Name: "logo.png", ContentType: "image/png",
			Object: suiteObject("logo", "png"), Authority: suiteAuthority()}), monday)
	e.apply(handRecord(t, artifactsdomain.ReaderSubject("bob"), artifactsdomain.OpPin, "op-pin",
		artifactsdomain.ScopeSet{Terms: []artifactsdomain.ScopeTerm{
			{Kind: artifactsdomain.TermReader, ID: "bob"}, {Kind: artifactsdomain.TermArtifact, ID: id}}},
		artifactsdomain.ReaderPayload{V: 1, Artifact: id}), monday)
	e.apply(handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindArtifact, ID: id},
		artifactsdomain.OpPurge, "op-purge", artifactsdomain.ScopeSet{Subject: true},
		artifactsdomain.StatusPayload{V: 1}), monday)

	for _, table := range []string{"artifacts WHERE id", "artifact_versions WHERE artifact_id",
		"artifact_objects WHERE artifact_id", "artifact_assets WHERE artifact_id",
		"artifact_pins WHERE artifact_id", "artifact_history WHERE artifact_id"} {
		if n := e.count(`SELECT COUNT(*) FROM `+table+` = ?`, id); n != 0 {
			t.Errorf("%s still holds %d rows of the purged artifact", table, n)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM artifacts WHERE id = ?`, other); n != 1 {
		t.Errorf("the purge reached another artifact")
	}
	if n := e.count(`SELECT artifacts FROM artifact_usage`); n != 1 {
		t.Errorf("usage counts %d artifacts after the purge, want 1", n)
	}
	title := "back from the dead"
	if reason := e.apply(handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindArtifact, ID: id},
		artifactsdomain.OpPatch, "op-late", artifactsdomain.ScopeSet{Subject: true},
		artifactsdomain.PatchPayload{V: 1, Title: &title}), monday); reason != statelog.ReasonDeleted {
		t.Errorf("a late patch on the purged artifact was not gated: %q", reason)
	}
	if reason := e.apply(handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindAsset, ID: id + "." + uuid.NewString()},
		artifactsdomain.OpCreate, "op-late-asset", artifactsdomain.ScopeSet{Subject: true},
		artifactsdomain.AssetPayload{V: 1, Name: "x.png", ContentType: "image/png",
			Object: suiteObject("late", "png"), Authority: suiteAuthority()}), monday); reason != statelog.ReasonDeleted {
		t.Errorf("a late asset on the purged artifact was not gated: %q", reason)
	}
	// AND THE PURGE'S OWN RECORD, redelivered, passes its own marker and
	// changes nothing.
	if reason := e.apply(handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindArtifact, ID: id},
		artifactsdomain.OpPurge, "op-purge", artifactsdomain.ScopeSet{Subject: true},
		artifactsdomain.StatusPayload{V: 1}), monday); reason != "" {
		t.Errorf("the purge's own record was gated by its own marker: %q", reason)
	}
	if n := e.count(`SELECT artifacts FROM artifact_usage`); n != 1 {
		t.Errorf("a redelivered purge moved the usage again: %d artifacts", n)
	}
}

// HISTORY IS KEPT A YEAR OF BROKER TIME, pruned by the applier in bounded
// batches as later records apply, in one total order on every data node.
//
// Mutation: drop pruneHistory from Apply and the year-old row survives.
func TestHistoryIsPrunedAfterAYearOfBrokerTime(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	old, fresh := newID(), newID()
	e.apply(handCreate(t, old, "op-old", suiteObject("old", "<p>o</p>"), limits()), monday)
	later := monday.Add(artifacts.HistoryRetention + time.Hour)
	e.apply(handCreate(t, fresh, "op-fresh", suiteObject("fresh", "<p>f</p>"), limits()), later)
	if n := e.count(`SELECT COUNT(*) FROM artifact_history WHERE artifact_id = ?`, old); n != 0 {
		t.Errorf("a history row older than the retention survived a later record")
	}
	if n := e.count(`SELECT COUNT(*) FROM artifact_history WHERE artifact_id = ?`, fresh); n != 1 {
		t.Errorf("the fresh history row is missing")
	}
	if n := e.count(`SELECT COUNT(*) FROM artifacts WHERE id = ?`, old); n != 1 {
		t.Errorf("pruning history removed the artifact itself")
	}
}

// A SOURCE ADDRESS IS HELD BY ONE ARTIFACT: a second create claiming the same
// owner's address is refused at the apply, and a create whose payload claims
// another address than the subject it arbitrated on is refused too.
//
// Mutation: drop the subject recomputation in applySource and the mismatched
// record claims an address it never arbitrated.
func TestASourceAddressIsHeldByOneArtifact(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	claim := func(op, id, key string, subject artifactsdomain.Subject) {
		p := createPayload(id)
		p.Owner, p.SourceKey = "jane", key
		e.apply(handRecord(t, subject, artifactsdomain.OpCreate, op,
			artifactsdomain.ScopeSet{Terms: []artifactsdomain.ScopeTerm{
				{Kind: artifactsdomain.TermSource, ID: subject.ID},
				{Kind: artifactsdomain.TermArtifact, ID: id}}}, p), monday)
	}
	first, second, liar := newID(), newID(), newID()
	address := artifactsdomain.SourceSubject("jane", "reports/q3.html")
	claim("op-first", first, "reports/q3.html", address)
	claim("op-second", second, "reports/q3.html", address)
	claim("op-liar", liar, "reports/q4.html", address)
	if outcome, _, _ := e.verdict(first, "op-first"); outcome != string(artifactsdomain.OutcomeApplied) {
		t.Fatalf("the first claim was refused")
	}
	if _, reason, _ := e.verdict(second, "op-second"); reason != string(artifactsdomain.RefusedSourceTaken) {
		t.Errorf("a second claim on a held address was %q", reason)
	}
	if _, reason, _ := e.verdict(liar, "op-liar"); reason != string(artifactsdomain.RefusedInvalid) {
		t.Errorf("a create claiming another address than its subject was %q", reason)
	}
}

// A CREATE NAMES A KIND THIS BUILD SERVES, at the apply as at the writer: the
// kind table is what every reader of the row draws by, so a row whose kind has
// no row would be an artifact nothing can open. A newer kind arrives under a
// raised record version, which this build defers rather than applies.
//
// Mutation: check only that the kind is non-empty and the unknown kind applies.
func TestACreateOfAKindThisBuildDoesNotServeIsRefused(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	id := newID()
	p := createPayload(id)
	p.Kind = artifacts.Kind("hologram")
	e.apply(handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindArtifact, ID: id},
		artifactsdomain.OpCreate, "op-hologram", artifactsdomain.ScopeSet{Subject: true}, p), monday)
	if _, reason, _ := e.verdict(id, "op-hologram"); reason != string(artifactsdomain.RefusedInvalid) {
		t.Fatalf("a create of an unserved kind was %q", reason)
	}
}
