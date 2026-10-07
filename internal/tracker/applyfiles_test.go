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

// chunkEraPut is a file put exactly as the build before this one encoded it —
// record version 12, its content named as a list of content-addressed chunks
// — taken from that build's own encoder. Such records are in fleets' logs and
// snapshots, and nothing rewrites them.
const chunkEraPut = `{"v":12,"op_id":"0199b4f6-0000-7000-8000-000000000001","subject":{"kind":"file","id":"ENG.cfda805d4bdf824a078ab3c9cd7a1f27"},"op":"patch","created_at":"2026-10-05T09:00:00Z","gen":1,"writer":"node-a","scope":"s/ENG","mutation":{"v":1,"version":0,"project":"ENG","path":"reports/q3.md","content_type":"text/markdown","hash":"c322135151cf0bc395a4d1e2a3e560e6cb0a62057e70e749c4f18cdbb81f32d7","size":10,"chunks":[{"hash":"c322135151cf0bc395a4d1e2a3e560e6cb0a62057e70e749c4f18cdbb81f32d7","size":10}],"created_by":"dev","created_at":"2026-10-05T09:00:00Z","updated_by":"dev","updated_at":"2026-10-05T09:00:00Z"},"actor":"dev","actor_kind":"agent","kind":"file_written"}`

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

// A FILE AN EARLIER BUILD KEPT IN CHUNKS APPLIES AS A LIVE ROW NAMING NO
// OBJECT, on every node alike — the record decoded with its chunk list dropped,
// the row's object NULL, its digest and size kept — so a fleet holding such
// records still applies its whole log and its copies still agree. And the next
// put of the same address, naming an object, replaces it like any other.
func TestAFileAnEarlierBuildKeptInChunksAppliesNamingNoObject(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	var old tracker.MutationRecord
	if err := json.Unmarshal([]byte(chunkEraPut), &old); err != nil {
		t.Fatalf("decode the chunk-era record: %v", err)
	}
	if old.V != 12 || old.V > tracker.RecordVersion {
		t.Fatalf("the fixture is at version %d; this build must read it", old.V)
	}
	at := time.Date(2026, 10, 5, 9, 0, 1, 0, time.UTC)
	if _, err := h.apply(old, at); err != nil {
		t.Fatalf("a chunk-era put did not apply: %v", err)
	}
	id := old.Subject.ID
	row := h.file(id)
	if row.object.Valid || row.hash != "c322135151cf0bc395a4d1e2a3e560e6cb0a62057e70e749c4f18cdbb81f32d7" ||
		row.size != 10 || row.removed {
		t.Fatalf("the chunk-era file applied as %+v; want a live row naming no object", row)
	}
	var file tracker.File
	if err := json.Unmarshal(row.document, &file); err != nil {
		t.Fatalf("decode the row's document: %v", err)
	}
	if _, named := file.Content(); named {
		t.Fatal("a chunk-era file answers content this build cannot read")
	}

	// A NEW PUT AT THE SAME ADDRESS names its object, at version 14.
	o := objstore.Object{Key: objstore.KeyAt(at), Hash: objstore.HashOf([]byte("again")), Size: 5}
	put := tracker.File{V: tracker.DocumentVersion, Project: "ENG", Path: "reports/q3.md",
		Hash: o.Hash, Size: o.Size, Object: o.Key, CreatedAt: at, UpdatedAt: at}
	body, err := json.Marshal(put)
	if err != nil {
		t.Fatal(err)
	}
	rec := old
	rec.OpID, rec.Mutation, rec.V = "0199b4f6-0000-7000-8000-000000000002", body, 0
	encoded, err := rec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.V != 14 {
		t.Fatalf("a put naming an object was stamped %d; a build reading 13 must retain it", rec.V)
	}
	if _, err := h.apply(rec, at.Add(time.Second)); err != nil {
		t.Fatalf("the new put did not apply: %v", err)
	}
	if row := h.file(id); row.object.String != o.Key.String() || row.hash != string(o.Hash) {
		t.Fatalf("the new put applied as %+v, want object %s", row, o.Key)
	}

	// AND A REMOVAL clears the object again, at the base file version.
	removed := put
	removed.Hash, removed.Size, removed.Object = "", 0, objstore.Key{}
	removed.RemovedAt, removed.RemovedBy = &at, "dev"
	if body, err = json.Marshal(removed); err != nil {
		t.Fatal(err)
	}
	rec.OpID, rec.Mutation, rec.V = "0199b4f6-0000-7000-8000-000000000003", body, 0
	if encoded, err = rec.Encode(); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.V != 12 {
		t.Fatalf("a removal was stamped %d; it names no object and stays at 12", rec.V)
	}
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
