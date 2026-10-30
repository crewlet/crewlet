package artifactdata_test

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// digestFromRows recomputes a store's state digest from its rows alone: one
// term per live document and one per collection, summed.
func (n *node) digestFromRows(id artifacts.ID) (canon.Digest, canon.Digest) {
	n.t.Helper()
	var computed, stored canon.Digest
	if err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		var no int64
		var raw []byte
		if err := tx.QueryRowContext(n.t.Context(), `
			SELECT store_no, state_digest FROM artifact_data_stores WHERE artifact_id = ?`,
			id.String()).Scan(&no, &raw); err != nil {
			return err
		}
		var err error
		if stored, err = canon.DigestFrom(raw); err != nil {
			return err
		}
		names := map[int64]string{}
		rows, err := tx.QueryContext(n.t.Context(), `
			SELECT collection_no, name, spec FROM artifact_data_collections WHERE store_no = ?`, no)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c int64
			var name, spec string
			if err := rows.Scan(&c, &name, &spec); err != nil {
				return err
			}
			names[c] = name
			computed = computed.Add(canon.CollectionTerm(name, []byte(spec)))
		}
		if err := rows.Close(); err != nil {
			return err
		}
		docs, err := tx.QueryContext(n.t.Context(), `
			SELECT collection_no, doc_id, body, version FROM artifact_data_docs
			WHERE store_no = ? AND body IS NOT NULL`, no)
		if err != nil {
			return err
		}
		defer func() { _ = docs.Close() }()
		for docs.Next() {
			var c, version int64
			var doc, body string
			if err := docs.Scan(&c, &doc, &body, &version); err != nil {
				return err
			}
			computed = computed.Add(canon.DocumentTerm(names[c], doc, []byte(body), version))
		}
		return docs.Err()
	}); err != nil {
		n.t.Fatalf("recompute the digest: %v", err)
	}
	return computed, stored
}

// churn writes a store through every kind of change a digest must follow:
// creates, field updates, increments, chips, deletes, a refusal, a schema
// change and an undo.
func churn(t *testing.T, n *node) {
	t.Helper()
	jane := person("jane", artifacts.RoleOwner)
	n.configure(jane, reviewSheet())
	n.applied(jane, set("review", "a", map[string]string{"title": `"A"`, "reviewer": `"bob"`}),
		set("review", "b", map[string]string{"title": `"B"`}))
	n.applied(jane, set("tasks", "t1", map[string]string{"n": `1`}))
	n.applied(jane, field(artifacts.DataIncrement, "tasks", "t1", "n", `2`))
	n.applied(person("bob", artifacts.RoleContributor), merge("review", "a", map[string]string{"done": `true`}))
	n.applied(jane, field(artifacts.DataAdd, "review", "b", "labels", `"copy"`))
	n.refused(person("dana", artifacts.RoleViewer), artifacts.DataForbidden,
		set("review", "c", map[string]string{"title": `"C"`}))
	gone := n.applied(jane, artifacts.DataOp{Op: artifacts.DataDelete, Collection: "review", Doc: "b"})
	n.configure(jane, withColumn(reviewSheet(), artifacts.Column{Key: "extra", Type: artifacts.ColumnText}),
		artifacts.Collection{Name: "tasks", Preset: artifacts.PresetShared})
	if _, err := n.writer.Undo(t.Context(), jane, sheet, gone.Seq); err != nil {
		t.Fatalf("undo the delete: %v", err)
	}
}

// withColumn is a collection with one more column.
func withColumn(c artifacts.Collection, col artifacts.Column) artifacts.Collection {
	c.Columns = append(append([]artifacts.Column(nil), c.Columns...), col)
	return c
}

// THE STATE DIGEST IS THE SUM OF ITS DOCUMENTS (and of its collections): after
// every kind of change, the digest the applier kept by subtracting and adding
// terms equals the one recomputed from the rows, so a checkpoint a later build
// takes can be verified against it.
//
// Mutation: drop the subtraction of a changed document's old term in writeDoc
// and the stored digest drifts from the rows on the first update.
func TestTheStateDigestIsTheSumOfItsDocuments(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	churn(t, n)
	computed, stored := n.digestFromRows(sheet)
	if stored.IsZero() {
		t.Fatal("the store's digest is empty after a dozen writes")
	}
	if computed != stored {
		t.Fatalf("the stored digest %s is not the sum of the store's documents and collections %s",
			stored, computed)
	}
	info, err := n.reader.Store(t.Context(), sheet)
	if err != nil {
		t.Fatal(err)
	}
	if info.Digest != stored.String() {
		t.Fatalf("the store reads its digest as %s, and the row holds %s", info.Digest, stored)
	}
}

// EVERY HISTORY ROW CARRIES THE DIGEST AFTER IT: replaying the log one record
// at a time on a fresh node, the digest recomputed from the rows after each
// record is the digest that record's history row recorded, refusals (which
// change nothing) included.
func TestEveryHistoryRowCarriesTheDigestAfterIt(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	n := newNode(t, log, openStore(t, "a.db"), "node-a")
	churn(t, n)
	replay := newNode(t, log, openStore(t, "replay.db"), "node-r")
	end, err := log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	spec := artifactdata.Domain{}.Stream()
	checked := 0
	for seq := uint64(1); seq <= end; seq++ {
		_, payload, storedAt, ok, err := log.At(t.Context(), seq)
		if err != nil || !ok {
			t.Fatalf("read record %d: %v", seq, err)
		}
		body, verdict := replay.verifier.Open(payload)
		if verdict != statelog.Verified {
			t.Fatalf("record %d: %s", seq, verdict)
		}
		replay.applyBody(spec, seq, body, storedAt)
		computed, _ := replay.digestFromRows(sheet)
		var recorded []byte
		pos := statelog.Position{Stream: spec.Name, Seq: seq}
		err = replay.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `
				SELECT digest_after FROM artifact_data_writes WHERE seq = ?`, pos.Packed()).Scan(&recorded)
		})
		if err != nil {
			t.Fatalf("record %d's history row: %v", seq, err)
		}
		if got := hex.EncodeToString(recorded); got != computed.String() {
			t.Fatalf("record %d's history row carries digest %s, and the rows after it sum to %s",
				seq, got, computed)
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("checked %d history rows, which is too few to prove anything", checked)
	}
}

// A CONDITIONAL WRITE IS SKIPPED IDENTICALLY ON EVERY DATA NODE: two nodes race the
// same guarded edit of one row; the log orders them, exactly one applies and
// the other is a conflict, and both nodes, and a third replaying the log from
// nothing, hold the same verdicts and byte-identical tables.
//
// Mutation: make the applier read the guard against a node-local value (the
// applying node's own clock, its own id) and the verdicts part.
func TestAConditionalWriteIsSkippedIdenticallyOnEveryNode(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	a := newNode(t, log, openStore(t, "a.db"), "node-a")
	b := newNode(t, log, openStore(t, "b.db"), "node-b")
	jane, sam := person("jane", artifacts.RoleEditor), person("sam", artifacts.RoleEditor)
	base := a.applied(jane, set("tasks", "t", map[string]string{"title": `"Base"`}))
	b.drain()
	var verdicts []artifacts.DataVerdict
	for i, n := range []*node{a, b} {
		c := jane
		if i == 1 {
			c = sam
		}
		res := n.write(c, guarded(merge("tasks", "t", map[string]string{
			"title": fmt.Sprintf(`"Edit %d"`, i)}), base.Seq))
		verdicts = append(verdicts, res.Verdict)
	}
	if verdicts[0] != artifacts.DataApplied || verdicts[1] != artifacts.DataConflict {
		t.Fatalf("the two guarded edits were %v, want the first applied and the second a conflict", verdicts)
	}
	a.drain()
	b.drain()
	replay := newNode(t, log, openStore(t, "replay.db"), "node-c")
	replay.drain()
	for _, table := range replicatedTables() {
		want := a.table(table)
		if got := b.table(table); got != want {
			t.Errorf("%s differs between the two nodes:\n a: %s\n b: %s", table, want, got)
		}
		if got := replay.table(table); got != want {
			t.Errorf("%s differs on the node that replayed the log:\n a: %s\n c: %s", table, want, got)
		}
	}
	if n := a.count(`SELECT COUNT(*) FROM artifact_data_writes WHERE verdict = 'conflict'`); n != 1 {
		t.Fatalf("%d conflict rows, want 1", n)
	}
}
