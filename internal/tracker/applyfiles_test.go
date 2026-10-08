package tracker_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fileRow is one file's row as the applier wrote it.
type fileRow struct {
	object   sql.NullString
	hash     string
	size     int64
	removed  bool
	document []byte
}

func (h *applyHarness) file(id string) fileRow {
	h.t.Helper()
	var r fileRow
	var removedAt sql.NullInt64
	if err := h.db.Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT object, hash, size, removed_at, document FROM tracker_files WHERE id = ?`,
			id).Scan(&r.object, &r.hash, &r.size, &removedAt, &r.document)
	}); err != nil {
		h.t.Fatalf("read file %s: %v", id, err)
	}
	r.removed = removedAt.Valid
	return r
}

// A PUT NAMES ITS OBJECT ON THE ROW, AND A REMOVAL CLEARS IT: the row's
// object is what keeps the bytes alive past the collector, so a put that
// applied without it would lose its file a day later, and a removal that kept
// it would keep bytes nobody can reach.
func TestAPutNamesItsObjectAndARemovalClearsIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Date(2026, 10, 5, 9, 0, 1, 0, time.UTC)
	o := objstore.Object{Key: objstore.KeyAt(at), Hash: objstore.HashOf([]byte("again")), Size: 5}
	put := tracker.File{V: tracker.DocumentVersion, Project: "ENG", Path: "reports/q3.md",
		Hash: o.Hash, Size: o.Size, Object: o.Key, CreatedAt: at, UpdatedAt: at}
	// stamped is rec with its mutation set to file, as this build's encoder
	// writes it and a node decodes it.
	stamped := func(rec tracker.MutationRecord, opID string, file tracker.File) tracker.MutationRecord {
		t.Helper()
		body, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		rec.OpID, rec.Mutation, rec.V = opID, body, 0
		encoded, err := rec.Encode()
		if err != nil {
			t.Fatal(err)
		}
		var back tracker.MutationRecord
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatal(err)
		}
		return back
	}
	base := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			Subject: tracker.FileSubject(put.Project, put.Path), Op: tracker.OpPatch,
			CreatedAt: at, Gen: 1, Writer: "node-a",
			Scope: tracker.ScopeSet{Subject: true, Container: put.Project},
		},
		Kind: tracker.ChangeFileWritten, Actor: "dev", ActorKind: tracker.AuthorAgent,
	}
	rec := stamped(base, "0199b4f6-0000-7000-8000-000000000002", put)
	if _, err := h.apply(rec, at.Add(time.Second)); err != nil {
		t.Fatalf("the put did not apply: %v", err)
	}
	id := rec.Subject.ID
	if row := h.file(id); row.object.String != o.Key.String() || row.hash != string(o.Hash) ||
		row.size != o.Size || row.removed {
		t.Fatalf("the put applied as %+v, want object %s", row, o.Key)
	}

	removed := put
	removed.Hash, removed.Size, removed.Object = "", 0, objstore.Key{}
	removed.RemovedAt, removed.RemovedBy = &at, "dev"
	rec = stamped(base, "0199b4f6-0000-7000-8000-000000000003", removed)
	if strings.Contains(string(rec.Mutation), `"object"`) {
		t.Fatalf("a removal carries an object: %s", rec.Mutation)
	}
	if _, err := h.apply(rec, at.Add(2*time.Second)); err != nil {
		t.Fatalf("the removal did not apply: %v", err)
	}
	if row := h.file(id); row.object.Valid || !row.removed {
		t.Fatalf("the removal applied as %+v, want a removed row naming no object", row)
	}
}
