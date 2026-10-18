package artifactspike

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE WORKLOAD: a review sheet as a team or an agent builds one, with the
// columns a model-made tracker reaches for. Every datastore spike writes this
// collection, so DS-1's rates, DS-2's bytes and S7's latencies describe one
// shape of document.

// sheetCollection is the collection's name.
const sheetCollection = "review"

// reviewSheet is the collection: twelve columns across the types a sheet
// offers, which is what a field index row per column costs.
func reviewSheet() artifacts.Collection {
	five := 5.0
	return artifacts.Collection{
		Name: sheetCollection, Preset: artifacts.PresetShared,
		Columns: []artifacts.Column{
			{Key: "title", Type: artifacts.ColumnText, Required: true, Primary: true},
			{Key: "status", Type: artifacts.ColumnStatus, Options: []artifacts.Option{
				{ID: "todo", Label: "To do", Category: "todo"},
				{ID: "doing", Label: "In progress", Category: "in_progress"},
				{ID: "done", Label: "Done", Category: "done"},
			}},
			{Key: "owner", Type: artifacts.ColumnPerson},
			{Key: "priority", Type: artifacts.ColumnSelect, Options: []artifacts.Option{
				{ID: "p0", Label: "P0"}, {ID: "p1", Label: "P1"},
				{ID: "p2", Label: "P2"}, {ID: "p3", Label: "P3"},
			}},
			{Key: "estimate", Type: artifacts.ColumnNumber},
			{Key: "due", Type: artifacts.ColumnDate},
			{Key: "done", Type: artifacts.ColumnCheckbox},
			{Key: "notes", Type: artifacts.ColumnLongText},
			{Key: "link", Type: artifacts.ColumnURL},
			{Key: "score", Type: artifacts.ColumnRating, Max: &five},
			{Key: "labels", Type: artifacts.ColumnMultiSelect, Options: []artifacts.Option{
				{ID: "copy", Label: "Copy"}, {ID: "design", Label: "Design"},
				{ID: "eng", Label: "Engineering"}, {ID: "legal", Label: "Legal"},
			}},
			{Key: "reviewer", Type: artifacts.ColumnPerson},
		},
	}
}

// docID is the sheet's kth row.
func docID(k int) string { return fmt.Sprintf("r%05d", k) }

// rowFields is a whole row: every column, the notes a sentence or two long.
func rowFields(r *rand.Rand, k int) map[string]json.RawMessage {
	statuses := []string{"todo", "doing", "done"}
	labels := []string{"copy", "design", "eng", "legal"}
	notes := strings.Repeat("Checked against the brief and the launch copy. ", 1+r.IntN(3))
	return map[string]json.RawMessage{
		"title":    jsonOf(fmt.Sprintf("Review item %05d", k)),
		"status":   jsonOf(statuses[r.IntN(len(statuses))]),
		"owner":    jsonOf(fmt.Sprintf("member-%02d", r.IntN(len(spikeSeats)))),
		"priority": jsonOf(fmt.Sprintf("p%d", r.IntN(4))),
		"estimate": json.RawMessage(fmt.Sprintf("%d.5", r.IntN(13))),
		"due":      jsonOf(fmt.Sprintf("2026-%02d-%02d", 1+r.IntN(12), 1+r.IntN(28))),
		"done":     json.RawMessage(fmt.Sprint(r.IntN(2) == 1)),
		"notes":    jsonOf(strings.TrimSpace(notes)),
		"link":     jsonOf(fmt.Sprintf("https://example.com/items/%d", k)),
		"score":    json.RawMessage(fmt.Sprint(1 + r.IntN(5))),
		"labels":   jsonOf([]string{labels[r.IntN(2)], labels[2+r.IntN(2)]}),
		"reviewer": jsonOf(fmt.Sprintf("member-%02d", r.IntN(len(spikeSeats)))),
	}
}

// pasteFields is the six columns a pasted block of rows fills.
func pasteFields(r *rand.Rand, k int) map[string]json.RawMessage {
	full := rowFields(r, k)
	out := map[string]json.RawMessage{}
	for _, key := range []string{"title", "status", "owner", "priority", "estimate", "due"} {
		out[key] = full[key]
	}
	return out
}

// shape is the shape of one write.
type shape string

const (
	// shapeCell is one person changing one cell: a merge of one field.
	shapeCell shape = "cell"

	// shapeRow is a whole row edited at once, as a form saves it: a merge
	// of every column but the long text, which is written whole only under
	// the version its writer edited (a guarded write) or appended to.
	shapeRow shape = "row"

	// shapeBulk is a pasted block or an agent's batch over existing rows:
	// the largest write a store takes ([artifacts.MaxBatchOps] merges of six
	// fields).
	shapeBulk shape = "bulk"
)

// opsOf is one write of the given shape on a sheet of rows rows.
func opsOf(r *rand.Rand, s shape, rows int) []artifacts.DataOp {
	switch s {
	case shapeCell:
		statuses := []string{"todo", "doing", "done"}
		return []artifacts.DataOp{{Op: artifacts.DataMerge, Collection: sheetCollection,
			Doc: docID(r.IntN(rows)), Fields: map[string]json.RawMessage{
				"status": jsonOf(statuses[r.IntN(len(statuses))])}}}
	case shapeRow:
		k := r.IntN(rows)
		fields := rowFields(r, k)
		delete(fields, "notes")
		return []artifacts.DataOp{{Op: artifacts.DataMerge, Collection: sheetCollection,
			Doc: docID(k), Fields: fields}}
	case shapeBulk:
		start := r.IntN(max(rows-artifacts.MaxBatchOps, 1))
		return pasteOps(r, start, artifacts.MaxBatchOps)
	}
	panic("artifactspike: unknown write shape " + string(s))
}

// pasteOps is n rows from start, the paste's six columns merged over each.
func pasteOps(r *rand.Rand, start, n int) []artifacts.DataOp {
	out := make([]artifacts.DataOp, n)
	for i := range out {
		k := start + i
		out[i] = artifacts.DataOp{Op: artifacts.DataMerge, Collection: sheetCollection,
			Doc: docID(k), Fields: pasteFields(r, k)}
	}
	return out
}

// seedSheet configures the sheet on store id and fills it with rows full rows,
// through node n as the artifact's owner, failing the spike on anything but
// `applied`.
func seedSheet(tb testing.TB, n *dataNode, id artifacts.ID, rows int) {
	tb.Helper()
	seedSheetAs(tb, n, id, rows, owner())
}

// seedSheetAs is [seedSheet] as the caller as.
func seedSheetAs(tb testing.TB, n *dataNode, id artifacts.ID, rows int, as artifactdata.Call) {
	tb.Helper()
	res, err := n.writer.Configure(tb.Context(), as, id, []artifacts.Collection{reviewSheet()})
	mustApply(tb, "configure the sheet", res, err)
	r := rand.New(rand.NewPCG(uint64(rows), 1))
	for start := 0; start < rows; start += artifacts.MaxBatchOps {
		count := min(artifacts.MaxBatchOps, rows-start)
		ops := make([]artifacts.DataOp, count)
		for i := range ops {
			k := start + i
			ops[i] = artifacts.DataOp{Op: artifacts.DataSet, Collection: sheetCollection,
				Doc: docID(k), Fields: rowFields(r, k)}
		}
		res, err := n.writer.Write(tb.Context(), as, id, artifacts.DataWrite{Ops: ops})
		mustApply(tb, fmt.Sprintf("seed rows %d..%d", start, start+count-1), res, err)
	}
}

// mustApply fails the spike unless a write applied.
func mustApply(tb testing.TB, what string, res artifactdata.WriteResult, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatalf("%s: %v", what, err)
	}
	if res.Verdict != artifacts.DataApplied {
		tb.Fatalf("%s: verdict %q (%s), want applied", what, res.Verdict, res.Reason)
	}
}

// jsonOf is v's JSON, for a value the spike itself built.
func jsonOf(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("artifactspike: encode %v: %v", v, err))
	}
	return b
}

// storeIDs are n artifacts' ids, distinct per spike run.
func storeIDs(run string, n int) []artifacts.ID {
	out := make([]artifacts.ID, n)
	for i := range out {
		out[i] = artifacts.IDFor(fmt.Sprintf("spike %s store %d", run, i))
	}
	return out
}
