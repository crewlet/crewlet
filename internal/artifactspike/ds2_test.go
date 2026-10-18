package artifactspike

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
)

// SPIKE DS-2: what a datastore really costs on disk, against what the applier
// meters for it.
//
// Every datastore byte is held on every data node and rides every snapshot and
// backup, and the quotas (`artifacts.limits.data_bytes`, `data_store_bytes`)
// bound the footprint the applier meters (internal/artifactdata/meter.go). If
// the metered figure is far from what the file really holds, a quota means
// something other than what it says. The spike fills one store with ten
// thousand review rows and a week's worth of edits, then measures the bytes
// the store added to the replicated file (live, with its write-ahead log) and
// to a snapshot of it (VACUUM INTO, which is what a backup is), each against
// the same file before the store existed. [TestTheFootprintMeterMatchesTheFile]
// holds the meter to the file on every run, across row shapes.
//
// Run: go test -run '^$' -bench DS2 -benchtime 1x ./internal/artifactspike

const (
	// footprintRows is the store's documents: the contract's 10 000-row
	// store, a large but ordinary sheet.
	footprintRows = 10_000

	// footprintCellEdits and footprintRowEdits are a week of a busy
	// sheet's edits after it was filled: single cells changed, and rows
	// rewritten whole.
	footprintCellEdits = 2_000
	footprintRowEdits  = 500
)

// BenchmarkDS2Footprint measures the store's real bytes and its metered
// footprint.
func BenchmarkDS2Footprint(b *testing.B) {
	for range b.N {
		b.StopTimer()
		br := startBrokers(b, topoSolo)
		br.provisionDataLog(b)
		n := joinData(b, br.queues[0], "node-0")
		id := storeIDs("footprint", 1)[0]
		snapDir := b.TempDir()
		liveBefore := liveBytes(b, n)
		snapBefore := snapshotBytes(b, n, filepath.Join(snapDir, "before.db"))

		b.StartTimer()
		seedSheetAs(b, n, id, footprintRows, unbounded(owner()))
		r := rand.New(rand.NewPCG(2, 2))
		for range footprintCellEdits {
			res, err := n.writer.Write(b.Context(), unbounded(editor("member-01")), id,
				artifacts.DataWrite{Ops: opsOf(r, shapeCell, footprintRows)})
			mustApply(b, "a cell edit", res, err)
		}
		for range footprintRowEdits {
			res, err := n.writer.Write(b.Context(), unbounded(editor("member-02")), id,
				artifacts.DataWrite{Ops: opsOf(r, shapeRow, footprintRows)})
			mustApply(b, "a row rewrite", res, err)
		}
		applied(b, []*dataNode{n})
		b.StopTimer()

		info, err := n.reader.Store(b.Context(), id)
		if err != nil {
			b.Fatalf("read the store's catalogue row: %v", err)
		}
		live := liveBytes(b, n) - liveBefore
		snap := snapshotBytes(b, n, filepath.Join(snapDir, "after.db")) - snapBefore
		content := float64(info.BodyBytes)

		b.ReportMetric(float64(info.Docs), "docs")
		b.ReportMetric(float64(info.Footprint)/mib, "metered-MiB")
		b.ReportMetric(float64(info.DocBytes)/mib, "doc-metered-MiB")
		b.ReportMetric(float64(info.BodyBytes)/mib, "body-MiB")
		b.ReportMetric(float64(info.FieldEntries), "field-entries")
		b.ReportMetric(float64(info.HistoryBytes)/mib, "history-MiB")
		b.ReportMetric(float64(snap)/mib, "snapshot-MiB")
		b.ReportMetric(float64(live)/mib, "live-MiB")
		b.ReportMetric(float64(snap)/float64(info.Footprint), "snapshot/metered")
		if content > 0 {
			// PER MiB OF CONTENT: the bytes a backup carries for each
			// MiB of canonical document bodies the store holds.
			b.ReportMetric(float64(snap)/content, "snapshot-MiB-per-content-MiB")
		}
		if info.Docs > 0 {
			b.ReportMetric(float64(snap)/float64(info.Docs), "snapshot-B/doc")
		}

		// WHERE THE BYTES ARE: each table's share of the snapshot, its
		// indexes included, measured by emptying the tables one at a time
		// and snapshotting again. The store is the spike's own and is
		// never read after this.
		shares := tableShares(b, n, snapDir, attributedTables)
		for _, t := range attributedTables {
			b.ReportMetric(float64(shares[t])/mib, t+"-MiB")
		}
		if info.FieldEntries > 0 {
			b.ReportMetric(float64(shares["artifact_data_fields"])/float64(info.FieldEntries),
				"B/field-entry")
		}
		history := shares["artifact_data_writes"] + shares["artifact_data_touches"]
		if info.HistoryBytes > 0 {
			b.ReportMetric(float64(history)/float64(info.HistoryBytes), "history-disk/metered")
		}
		if docs := shares["artifact_data_docs"] + shares["artifact_data_fields"]; info.DocBytes > 0 {
			b.ReportMetric(float64(docs)/float64(info.DocBytes), "docs-disk/metered")
		}
	}
}

// unbounded lifts a call's stamped datastore quotas to their ceilings: a spike
// that measures what a store costs must not be stopped by the quota that
// counts it (the default per-store bound holds about the ten thousand rows
// DS-2 writes, and no more).
func unbounded(c artifactdata.Call) artifactdata.Call {
	c.Authority.Limits.DataStoreBytes = artifacts.MaxDataStoreBytes
	c.Authority.Limits.DataBytes = artifacts.MaxDataBytes
	return c
}

// TestTheFootprintMeterMatchesTheFile holds the footprint the applier meters
// to the bytes a store's rows take in a snapshot of the replicated file
// (D42, ADR-0039), within DS-2's 10 percent, across the shapes a sheet takes:
// review rows of a few hundred bytes, rows whose long text fills half a page,
// long document ids, a week of edits, pasted blocks and deleted rows held as
// tombstones. A meter that counted bodies alone, or the field index at a
// flat per-entry figure, is off by a factor of two here.
func TestTheFootprintMeterMatchesTheFile(t *testing.T) {
	for _, c := range []struct {
		name         string
		rows         int
		idPad, notes int
	}{
		{"review rows", 1500, 0, 0},
		{"half-page rows with long ids", 600, 40, 1900},
	} {
		t.Run(c.name, func(t *testing.T) {
			br := startBrokers(t, topoSolo)
			br.provisionDataLog(t)
			n := joinData(t, br.queues[0], "node-0")
			id := storeIDs("meter "+c.name, 1)[0]
			dir := t.TempDir()
			before := snapshotBytes(t, n, filepath.Join(dir, "before.db"))

			res, err := n.writer.Configure(t.Context(), unbounded(owner()), id,
				[]artifacts.Collection{reviewSheet()})
			mustApply(t, "configure the sheet", res, err)
			doc := func(k int) string { return docID(k) + strings.Repeat("x", c.idPad) }
			r := rand.New(rand.NewPCG(7, 7))
			row := func(k int) map[string]json.RawMessage {
				f := rowFields(r, k)
				if c.notes > 0 {
					f["notes"] = jsonOf(strings.Repeat("A long review note. ", c.notes/20))
				}
				return f
			}
			write := func(what string, call artifactdata.Call, ops []artifacts.DataOp) {
				res, err := n.writer.Write(t.Context(), unbounded(call), id, artifacts.DataWrite{Ops: ops})
				mustApply(t, what, res, err)
			}
			const batch = 40
			for start := 0; start < c.rows; start += batch {
				ops := make([]artifacts.DataOp, 0, batch)
				for k := start; k < min(start+batch, c.rows); k++ {
					ops = append(ops, artifacts.DataOp{Op: artifacts.DataSet,
						Collection: sheetCollection, Doc: doc(k), Fields: row(k)})
				}
				write("seed", owner(), ops)
			}
			for range c.rows / 5 {
				write("a cell edit", editor("member-01"), []artifacts.DataOp{{Op: artifacts.DataMerge,
					Collection: sheetCollection, Doc: doc(r.IntN(c.rows)),
					Fields: map[string]json.RawMessage{"status": jsonOf("done")}}})
			}
			for range 5 {
				start := r.IntN(c.rows - batch)
				ops := make([]artifacts.DataOp, batch)
				for i := range ops {
					ops[i] = artifacts.DataOp{Op: artifacts.DataMerge, Collection: sheetCollection,
						Doc: doc(start + i), Fields: pasteFields(r, start+i)}
				}
				write("a paste", editor("member-02"), ops)
			}
			for k := 0; k < c.rows; k += 7 {
				write("a delete", editor("member-03"), []artifacts.DataOp{{Op: artifacts.DataDelete,
					Collection: sheetCollection, Doc: doc(k)}})
			}
			applied(t, []*dataNode{n})

			info, err := n.reader.Store(t.Context(), id)
			if err != nil {
				t.Fatalf("read the store's catalogue row: %v", err)
			}
			snap := snapshotBytes(t, n, filepath.Join(dir, "after.db")) - before
			shares := tableShares(t, n, dir, attributedTables)
			docs := shares["artifact_data_docs"] + shares["artifact_data_fields"]
			history := shares["artifact_data_writes"] + shares["artifact_data_touches"]
			for _, m := range []struct {
				what          string
				metered, disk int64
			}{
				{"the documents and their field index", info.DocBytes, docs},
				{"the history and its touches", info.HistoryBytes, history},
				{"the store as a snapshot carries it", info.Footprint, snap},
			} {
				if ratio := float64(m.metered) / float64(m.disk); ratio < 0.9 || ratio > 1.1 {
					t.Errorf("%s: metered %d bytes against %d on disk (%.2f), outside DS-2's 10 percent",
						m.what, m.metered, m.disk, ratio)
				}
			}
			if info.Footprint != info.DocBytes+info.HistoryBytes {
				t.Errorf("the footprint %d is not the documents %d plus the history %d",
					info.Footprint, info.DocBytes, info.HistoryBytes)
			}
		})
	}
}

// attributedTables are the datastore tables a store's rows live in, in the
// order the attribution empties them.
var attributedTables = []string{
	"artifact_data_touches", "artifact_data_writes", "artifact_data_fields", "artifact_data_docs",
}

// tableShares empties each table in turn and answers how many snapshot bytes
// each one held, its indexes included.
func tableShares(tb testing.TB, n *dataNode, dir string, tables []string) map[string]int64 {
	tb.Helper()
	out := map[string]int64{}
	before := snapshotBytes(tb, n, filepath.Join(dir, "share-0.db"))
	for i, table := range tables {
		if err := n.db.Replicated().Tx(tb.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(tb.Context(), `DELETE FROM `+table)
			return err
		}); err != nil {
			tb.Fatalf("empty %s: %v", table, err)
		}
		after := snapshotBytes(tb, n, filepath.Join(dir, fmt.Sprintf("share-%d.db", i+1)))
		out[table] = before - after
		before = after
	}
	return out
}

// liveBytes is the replicated file and its write-ahead log as they stand on
// disk now.
func liveBytes(tb testing.TB, n *dataNode) int64 {
	tb.Helper()
	path := n.db.ReplicatedFile()
	var total int64
	for _, p := range []string{path, path + "-wal"} {
		st, err := os.Stat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			tb.Fatalf("measure %s: %v", p, err)
		}
		total += st.Size()
	}
	return total
}

// snapshotBytes is the size of a snapshot of the replicated file taken now,
// the way a backup takes one.
func snapshotBytes(tb testing.TB, n *dataNode, dest string) int64 {
	tb.Helper()
	db, err := n.db.ReplicatedDB()
	if err != nil {
		tb.Fatalf("open the replicated estate for a snapshot: %v", err)
	}
	info, err := db.Backup(tb.Context(), dest)
	if err != nil {
		tb.Fatalf("snapshot the replicated estate: %v", err)
	}
	st, err := os.Stat(info.Path)
	if err != nil {
		tb.Fatalf("measure the snapshot: %v", err)
	}
	return st.Size()
}
