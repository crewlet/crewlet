package store

import (
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A CHUNK ROW WRITTEN WHEN ROWS CARRIED A GROUP READS ITS SLOT AFTER 0024.
//
// Replicated migration 0024 replaced `tracker_file_chunks.pg` with `slot`, and
// it had to CARRY every row across: nothing re-applies the file records behind
// them, and a row lost here is a chunk the object store's collector reads as
// named by nothing and deletes. So the case stands a database up at 0023,
// writes rows in that shape — with the group the old applier wrote beside each
// — and lets the real migrator take it forward.
//
// THE SLOT IS JUDGED AGAINST [objstore.Hash.Slot], the function the applier
// writes every row after 0024 with — so what this proves is that a row carried
// across and a row written fresh file one chunk at the same slot, which is the
// only agreement a pass over a slot range needs. The addresses are chosen for
// the hex arithmetic: every digit from 0 to f in every one of the four places
// the slot is read from, both ends of the range, and real content hashes.
func TestAChunkRowFromTheGroupEraReadsItsSlot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "replicated.db")
	old := migratedThrough(t, EstatePartition, path, "0023_a_project_keeps_files.sql")

	const digits = "0123456789abcdef"
	chunks := []objstore.Hash{
		objstore.Hash(strings.Repeat("0", 64)),
		objstore.Hash(strings.Repeat("f", 64)),
		objstore.HashOf([]byte("a quarterly plan")),
		objstore.HashOf([]byte("the second chunk of it")),
	}
	// Sixteen rotations of the alphabet, so place p of rotation k holds
	// digit k+p: each place sees every digit once.
	for k := range len(digits) {
		lead := (digits + digits)[k : k+4]
		chunks = append(chunks, objstore.Hash(lead+strings.Repeat(digits[k:k+1], 60)))
	}
	for i, h := range chunks {
		if !h.Valid() {
			t.Fatalf("fixture %d, %q, is not a content address — the backfill is "+
				"only claimed for the rows an applier could have written", i, h)
		}
		// TWO FILES, so the primary key the rows are carried under is
		// exercised with a repeated seq as well as a repeated file.
		file := []string{"ENG.a", "ENG.b"}[i%2]
		if _, err := old.ExecContext(t.Context(), `
			INSERT INTO tracker_file_chunks (file_id, seq, chunk, size, pg)
			VALUES (?, ?, ?, ?, ?)`,
			file, i/2, string(h), 1000+i, groupEraPG(t, h)); err != nil {
			_ = old.Close()
			t.Fatalf("write a row in 0023's shape: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenEstate(t.Context(), EstatePartition, path, Options{})
	if err != nil {
		t.Fatalf("migrate the group-era database forward: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	applied, err := db.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied, "0024_a_chunk_row_names_its_slot.sql") {
		t.Fatalf("0024 did not run on a database at 0023: applied %v", applied)
	}

	type row struct {
		chunk objstore.Hash
		size  int
		slot  int
	}
	got := map[string]row{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT file_id, seq, chunk, size, slot FROM tracker_file_chunks`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var file string
			var seq int
			var r row
			if err := rows.Scan(&file, &seq, &r.chunk, &r.size, &r.slot); err != nil {
				return err
			}
			got[file+"/"+strconv.Itoa(seq)] = r
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the migrated rows: %v", err)
	}
	if len(got) != len(chunks) {
		t.Fatalf("%d rows were written at 0023 and %d came through 0024 — a row "+
			"lost here is a chunk the collector deletes", len(chunks), len(got))
	}
	for i, h := range chunks {
		key := []string{"ENG.a", "ENG.b"}[i%2] + "/" + strconv.Itoa(i/2)
		r, ok := got[key]
		switch {
		case !ok:
			t.Errorf("the row for %s (%s) did not come through", h, key)
		case r.chunk != h || r.size != 1000+i:
			t.Errorf("%s came through as chunk %s, size %d; want %s, %d",
				key, r.chunk, r.size, h, 1000+i)
		case r.slot != h.Slot():
			t.Errorf("%s was backfilled at slot %d and the applier files it at %d "+
				"— a pass over its group reads the other range, and the collector "+
				"sees the chunk named by nothing", h, r.slot, h.Slot())
		}
	}

	// THE SHAPE 0024 LEAVES: no group column and no index over one, the
	// slot index in its place, the aside table gone — and a slot that has
	// NO DEFAULT, so a writer that forgot it is refused rather than filed
	// at slot 0, which is a real slot and the one a default would pick.
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var columns []string
		rows, err := tx.QueryContext(t.Context(),
			`SELECT name FROM pragma_table_info('tracker_file_chunks')`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			columns = append(columns, name)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if slices.Contains(columns, "pg") || !slices.Contains(columns, "slot") {
			t.Errorf("tracker_file_chunks has columns %v, want slot and no pg", columns)
		}
		for name, want := range map[string]bool{
			"tracker_file_chunks_slot_idx": true,
			"tracker_file_chunks_pg_idx":   false,
			"tracker_file_chunks_by_group": false,
			"tracker_file_chunks":          true,
		} {
			var n int
			if err := tx.QueryRowContext(t.Context(),
				`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil {
				return err
			}
			if (n == 1) != want {
				t.Errorf("%q is in the schema %d times, want present=%v", name, n, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read the schema back: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO tracker_file_chunks (file_id, seq, chunk, size)
			VALUES ('ENG.c', 0, ?, 1)`, string(chunks[0]))
		return err
	}); err == nil {
		t.Error("a chunk row with no slot was accepted — the column carries a " +
			"default, so a writer that forgets it files its chunk at a slot " +
			"nothing chose")
	}
}

// groupEraPG is the placement group 0023's applier stored beside a chunk: its
// address's first four bytes, big-endian, modulo the 256 groups a map then
// had. Restated rather than imported because the function that computed it is
// gone with the fixed group count — and the value matters only for being the
// real shape of an old row, since 0024 reads the slot off the chunk itself.
func groupEraPG(t *testing.T, h objstore.Hash) int {
	t.Helper()
	raw, err := hex.DecodeString(string(h[:8]))
	if err != nil {
		t.Fatal(err)
	}
	return int(binary.BigEndian.Uint32(raw) % 256)
}
