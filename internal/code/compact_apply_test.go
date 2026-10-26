package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE COMPACTION'S APPLY AND DECIDE, case by case, over one node's estate with
// no broker between: records built here, and the decide run in a snapshot of
// the rows a case seeds.

// outputOf is output n of compaction cid, a bundle of objects objects whose
// key was minted at minted — as a builder lays one out ([LayBundle]).
func outputOf(cid string, n int, objects int64, minted time.Time) PackRef {
	return PackRef{
		PackID: DeriveCompactionPackID(cid, n), Object: objstore.KeyAt(minted),
		Hash:         objstore.HashOf([]byte(fmt.Sprint(cid, n))),
		Size:         32*objects + IndexBytesFor(objects) + ReverseBytesFor(objects),
		IndexBytes:   IndexBytesFor(objects),
		ReverseBytes: ReverseBytesFor(objects),
		GitChecksum:  oid(fmt.Sprint("output", cid, n)), Objects: objects,
	}
}

// compactOf is a compaction record on repository id retiring retire into add.
func compactOf(t testing.TB, id, cid string, retire []string, add ...PackRef) built {
	return rec(t, RepoSubject(id), OpCompact, Scope{RepoPath(id)}, Compaction{
		RepoID: id, CompactionID: cid, Node: "node-a", Seen: codeAt(1), Retire: retire, Add: add,
	})
}

// A COMPACTION RETIRES ITS INPUTS AND ADDS ITS OUTPUTS, in one record: the
// inputs' rows leave code_packs for code_packs_retired, keeping every column
// they had and stamped with the record's position and broker instant; the
// outputs join code_packs as live packs of kind `compact`; and the
// repository's live-pack counters move by exactly that. Applied again — a
// second record of the same compaction, which the ledger would have collapsed
// — it finds nothing to retire and nothing to add, and moves nothing.
//
// Mutation: drop the apply's finding that a record applied already, and the
// record applied again moves the repository's version though it moved no
// row; stamp the retired rows with the position their packs were added at,
// and they read as retired long before the compaction.
func TestACompactionRetiresItsInputsAndAddsItsOutputs(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 5)
	packsBefore := e.num(`SELECT live_packs FROM code_repos WHERE id = 'repo-a'`)
	bytesBefore := e.num(`SELECT live_bytes FROM code_repos WHERE id = 'repo-a'`)
	objectsBefore := e.num(`SELECT live_objects FROM code_repos WHERE id = 'repo-a'`)
	retire := []string{"repo-a-pack-000000", "repo-a-pack-000001", "repo-a-pack-000002"}
	cid := statelog.NewOpID(applyClock, "")
	out := outputOf(cid, 0, 3, applyClock)
	compaction := compactOf(t, "repo-a", cid, retire, out)

	at := codeAt(e.next)
	if rows := e.applyCounting(compaction); rows != 3+3+1+1 || rows > compactDecidedRows(3, 1, nil) {
		t.Fatalf("the compaction reported %d rows, want its three retirements, three removals, one "+
			"output and the counters — at most compactDecidedRows(3, 1, nil) = %d", rows,
			compactDecidedRows(3, 1, nil))
	}
	for _, id := range retire {
		if n := e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = 'repo-a' AND pack_id = ?`, id); n != 0 {
			t.Errorf("input %s is still a live pack", id)
		}
		if got := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = 'repo-a' AND pack_id = ?
			AND part = 'pack' AND retired_position = ? AND retired_at = ? AND kind = 'push'
			AND added_position = 1 AND size = 10 AND objects = 1 AND object = ?`,
			id, at.Packed(), store.EncodeTime(applyClock), packOf(id).Object.String()); got != 1 {
			t.Errorf("input %s is not retired with its own columns at the compaction's position and instant", id)
		}
	}
	if got := e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = 'repo-a' AND pack_id = ? AND part = 'pack'
		AND kind = 'compact' AND state = 'live' AND added_position = ? AND size = ? AND objects = 3
		AND object = ?`, out.PackID, at.Packed(), out.Size, out.Object.String()); got != 1 {
		t.Fatalf("the output is not a live pack of kind compact added at the compaction's position")
	}
	counters := func() [4]int64 {
		return [4]int64{e.num(`SELECT live_packs FROM code_repos WHERE id = 'repo-a'`),
			e.num(`SELECT live_bytes FROM code_repos WHERE id = 'repo-a'`),
			e.num(`SELECT live_objects FROM code_repos WHERE id = 'repo-a'`),
			e.num(`SELECT version FROM code_repos WHERE id = 'repo-a'`)}
	}
	want := [4]int64{packsBefore - 3 + 1, bytesBefore - 3*10 + out.Size, objectsBefore - 3 + 3, at.Packed()}
	if got := counters(); got != want {
		t.Fatalf("live packs, bytes, objects and version are %v after the compaction, want %v", got, want)
	}
	// THE COUNTERS ARE THE ROWS, before and after.
	if got, rows := counters()[0], e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = 'repo-a'`); got != rows {
		t.Fatalf("live_packs says %d and code_packs holds %d", got, rows)
	}

	// THE SAME COMPACTION AGAIN, at a later position the version guard
	// does not stop: nothing left to retire, its output already there.
	if rows := e.applyCounting(compactOf(t, "repo-a", cid, retire, out)); rows != 0 {
		t.Fatalf("the compaction applied again reported %d rows, want none", rows)
	}
	if got := counters(); got != want {
		t.Fatalf("the compaction applied again moved the counters from %v to %v", want, got)
	}
	if n := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = 'repo-a'`); n != 3 {
		t.Fatalf("%d retired rows after the compaction applied again, want the three", n)
	}
}

// A COMPACTION THAT NAMES A PACK TWICE, OR ONE ALREADY GONE, COUNTS WHAT IT
// FINDS: a record no committer of this build forms is still applied as the
// rows it finds and writes, so the counters stay the rows' and the log does not
// stop — an input named twice moves once, an input another record took first
// is passed over, and an output named twice is written once.
//
// Mutation: retire by the record's list rather than the rows found, and the
// input named twice is counted out twice.
func TestACompactionCountsTheRowsItFinds(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 4)
	cid := statelog.NewOpID(applyClock, "")
	out := outputOf(cid, 0, 2, applyClock)
	e.apply(compactOf(t, "repo-a", cid,
		[]string{"repo-a-pack-000000", "repo-a-pack-000000", "repo-a-pack-000001", "repo-a-gone"}, out, out))
	if got, rows := e.num(`SELECT live_packs FROM code_repos WHERE id = 'repo-a'`),
		e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = 'repo-a'`); got != rows || rows != 3 {
		t.Fatalf("live_packs says %d and code_packs holds %d, want both 3: two inputs out, one output in",
			got, rows)
	}
	if got, sum := e.num(`SELECT live_objects FROM code_repos WHERE id = 'repo-a'`),
		e.num(`SELECT SUM(objects) FROM code_packs WHERE repo_id = 'repo-a'`); got != sum {
		t.Fatalf("live_objects says %d and the rows hold %d", got, sum)
	}
	if n := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = 'repo-a'`); n != 2 {
		t.Fatalf("%d retired rows, want the two inputs found", n)
	}
}

// compactionDecidedIn runs one round of the compaction's decide over e's rows at now.
func compactionDecidedIn(t *testing.T, e *estate, c Compaction, now time.Time) (compactDecision, error) {
	t.Helper()
	var d compactDecision
	err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		var err error
		d, err = decideCompaction(context.Background(), tx, c, DefaultDeletedRetention, now)
		return err
	})
	return d, err
}

// A COMPACTION OUTPUT PAST THE BUNDLE RULE IS REFUSED, and one inside it is
// decided whatever its pack section: the decide holds every output to
// [PackRef.Check] — the bundle WHOLE, its pack and both indexes, within
// MaxPackBytes, the one rule every holder of a row applies — never to the cut a
// builder cuts its packs at, which an output of a dense roll's scaled cut or a
// delta written whole may pass by a little.
//
// Mutation: hold an output to CompactPackBytes, and the output at the cut and
// a mebibyte is refused.
func TestACompactionOutputPastTheBundleRuleIsRefused(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 2)
	retire := []string{"repo-a-pack-000000", "repo-a-pack-000001"}
	for name, tc := range map[string]struct {
		size    int64
		refusal Refusal
	}{
		"a bundle a byte past MaxPackBytes":        {MaxPackBytes + 1, RefusalPackTooLarge},
		"a bundle a mebibyte past the nominal cut": {CompactPackBytes + 1<<20, ""},
	} {
		cid := statelog.NewOpID(applyClock, "")
		out := outputOf(cid, 0, 1, applyClock)
		out.Size = tc.size
		d, err := compactionDecidedIn(t, e, Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a",
			Seen: codeAt(1), Retire: retire, Add: []PackRef{out}}, applyClock)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d.refusal != tc.refusal || (tc.refusal != "" && !strings.Contains(d.detail, out.PackID)) {
			t.Errorf("%s: the decide answered %q (%q), want %q naming the output", name, d.refusal, d.detail,
				tc.refusal)
		}
	}
}

// AN OUTPUT UPLOADED TOO LONG AGO IS REFUSED UPLOAD_EXPIRED, by the push's one
// rule and sentence: a key minted objstore.RecordWithin before the decision may
// already have been judged unnamed and deleted by a collection, so no record
// may name it.
//
// Its compaction was planned long before either output's upload began, which
// is nothing to the rule: an output is judged by its own key.
//
// Mutation: drop the rule from the compaction's decide, and an output whose
// key is twelve hours old is decided.
func TestACompactionOutputUploadedTooLongAgoIsRefused(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 2)
	retire := []string{"repo-a-pack-000000", "repo-a-pack-000001"}
	cid := statelog.NewOpID(applyClock.Add(-objstore.RecordWithin-time.Hour), "")
	for name, tc := range map[string]struct {
		minted  time.Time
		refusal Refusal
	}{
		"minted RecordWithin before the decision": {applyClock.Add(-objstore.RecordWithin), RefusalUploadExpired},
		"minted a moment before it":               {applyClock.Add(-time.Second), ""},
	} {
		d, err := compactionDecidedIn(t, e, Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a",
			Seen: codeAt(1), Retire: retire, Add: []PackRef{outputOf(cid, 0, 1, tc.minted)}}, applyClock)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d.refusal != tc.refusal {
			t.Errorf("%s: the decide answered %q (%q), want %q", name, d.refusal, d.detail, tc.refusal)
		}
		if tc.refusal != "" && !strings.Contains(d.detail, "or more before this decision; the next pass builds it again") {
			t.Errorf("%s: the refusal says %q, not the push's sentence with the compaction's remedy", name,
				d.detail)
		}
	}
}

// A COMPACTION PAST THE ROW BUDGET IS REFUSED BEFORE ITS RECORD: the committer
// takes no more than MaxCompactInputs inputs and MaxCompactOutputs outputs —
// Validate refuses one more of either, naming it — and the largest compaction
// it takes fits one record whole, in rows (compactDecidedRows within
// MaxRecordRows, the framework's transaction budget) and in bytes. So does
// the widest PUBLICATION it takes — those inputs and outputs, and a layer of
// MaxPacksPerRepo packs in MaxLayerParts parts replacing MaxPublishedLayers −
// 1 layers, its record under 200 KiB — and one more replaced layer or one more
// part is refused, naming it.
//
// Mutation: raise MaxCompactInputs past the row budget, and the largest
// compaction the committer takes no longer fits its record; let Validate take
// as many replaced layers as a chain holds, and the arm naming layer.replaces
// passes.
func TestACompactionPastTheRowBudgetIsRefusedBeforeItsRecord(t *testing.T) {
	t.Parallel()
	if rows := compactDecidedRows(MaxCompactInputs, MaxCompactOutputs, nil); rows > MaxRecordRows {
		t.Fatalf("the largest compaction the committer takes writes %d rows, past MaxRecordRows (%d)",
			rows, MaxRecordRows)
	}
	largest := func(inputs, outputs int) CompactionProposal {
		cid := statelog.NewOpID(time.Now(), "")
		retire := make([]string, inputs)
		for i := range retire {
			retire[i] = statelog.DeriveOpID(time.Now(), "", "input", fmt.Sprint(i))
		}
		slices.Sort(retire)
		add := make([]PackRef, outputs)
		for i := range add {
			add[i] = outputOf(cid, i, int64(i+1), time.Now())
		}
		return CompactionProposal{Compaction: Compaction{
			RepoID: "0190f3c4-0000-7000-8000-00000000c0de", CompactionID: cid, Node: "node-a",
			Seen:   statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 7},
			Retire: retire, Add: add,
		}}
	}
	p := largest(MaxCompactInputs, MaxCompactOutputs)
	bytes, err := p.measure()
	if err != nil {
		t.Fatalf("the largest compaction the committer takes is refused: %v", err)
	}
	// ITS RECORD whole: the envelope a committer stamps around it.
	decision, err := recordFor(statelog.Stamp{Writer: "node-a", Gen: 1}, RepoSubject(p.Compaction.RepoID),
		OpCompact, []string{RepoPath(p.Compaction.RepoID)}, p.Compaction.CompactionID,
		Actor{ID: "node-a", Kind: ActorSystem}, p.Compaction, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := withinCompaction(decision, p.Compaction); err != nil {
		t.Fatalf("the largest compaction's record (%d bytes, %d of them the compaction) does not fit: %v",
			len(decision.Payload), bytes, err)
	}
	for name, tc := range map[string]struct {
		p     CompactionProposal
		field string
	}{
		"one input more":  {largest(MaxCompactInputs+1, 1), "retire"},
		"one output more": {largest(MaxCompactOutputs+1, MaxCompactOutputs+1), "add"},
	} {
		if err := tc.p.Validate(); !errors.Is(err, ErrInvalidProposal) ||
			!strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%s: Validate = %v, want ErrInvalidProposal naming %s", name, err, tc.field)
		}
	}

	// THE WIDEST PUBLICATION, every bound at once.
	widest := func() CompactionProposal {
		w := largest(MaxCompactInputs, MaxCompactOutputs)
		packs := make([]string, 0, MaxPacksPerRepo)
		for _, out := range w.Compaction.Add {
			packs = append(packs, out.GitChecksum)
		}
		for i := len(packs); i < MaxPacksPerRepo; i++ {
			packs = append(packs, oid(fmt.Sprint("covered", i)))
		}
		slices.Sort(packs)
		replaces := make([]string, MaxPublishedLayers-1)
		for i := range replaces {
			replaces[i] = oid(fmt.Sprint("replaced", i))
		}
		parts := make([]LayerPart, MaxLayerParts)
		for i := range parts {
			parts[i] = LayerPart{Object: objstore.KeyAt(time.Now()), Hash: objstore.HashOf([]byte(fmt.Sprint(i))),
				Size: LayerPartBytes}
		}
		w.Compaction.Layer = &LayerRef{LayerID: oid("widest"), BaseID: oid("base"), Replaces: replaces,
			GraphID: oid("graph"), Packs: packs, Parts: parts,
			MidxBytes: MaxLayerParts*LayerPartBytes - 2<<20, BitmapBytes: 1 << 20, GraphBytes: 1 << 20,
			Objects: 300_000_000, Commits: 90_000_000, Git: "2.56.0", TookMs: 3_600_000}
		return w
	}
	w := widest()
	bytes, err = w.measure()
	if err != nil {
		t.Fatalf("the widest publication the committer takes is refused: %v", err)
	}
	decision, err = recordFor(statelog.Stamp{Writer: "node-a", Gen: 1}, RepoSubject(w.Compaction.RepoID),
		OpCompact, []string{RepoPath(w.Compaction.RepoID)}, w.Compaction.CompactionID,
		Actor{ID: "node-a", Kind: ActorSystem}, w.Compaction, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := withinCompaction(decision, w.Compaction); err != nil {
		t.Fatalf("the widest publication's record (%d bytes, %d of them the compaction) does not fit: %v",
			len(decision.Payload), bytes, err)
	}
	if len(decision.Payload) >= 200<<10 {
		t.Errorf("the widest publication's record is %d bytes, past the 200 KiB it is sized at",
			len(decision.Payload))
	}
	if rows := compactDecidedRows(MaxCompactInputs, MaxCompactOutputs, w.Compaction.Layer); rows != 2_609 ||
		rows > MaxRecordRows {
		t.Errorf("the widest publication writes %d rows, want the 2 609 compactDecidedRows' doc states, under "+
			"MaxRecordRows (%d)", rows, MaxRecordRows)
	}
	t.Logf("the widest publication's record is %d bytes of the %d a record may be", len(decision.Payload),
		MaxRecordBytes)
	for name, tc := range map[string]struct {
		edit  func(*Compaction)
		field string
	}{
		"one replaced layer more": {func(c *Compaction) {
			c.Layer.Replaces = append(c.Layer.Replaces, oid("one more"))
		}, "layer.replaces:"},
		"one part more": {func(c *Compaction) {
			c.Layer.Parts = append(c.Layer.Parts, c.Layer.Parts[0])
		}, "layer: " + ErrInvalidLayer.Error() + ": parts:"},
		"an output the layer does not cover": {func(c *Compaction) {
			c.Layer.Packs = slices.DeleteFunc(c.Layer.Packs, func(s string) bool { return s == c.Add[3].GitChecksum })
		}, "add[3].git_checksum:"},
	} {
		p := widest()
		tc.edit(&p.Compaction)
		if err := p.Validate(); !errors.Is(err, ErrInvalidProposal) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: Validate = %v, want ErrInvalidProposal naming %s", name, err, tc.field)
		}
	}
	// AND A LAYER-ONLY PUBLICATION is a proposal, where a compaction with no
	// inputs is none.
	only := widest()
	only.Compaction.Retire, only.Compaction.Add = nil, nil
	if err := only.Validate(); err != nil {
		t.Errorf("a publication with no repack: %v", err)
	}
	only.Compaction.Layer = nil
	if err := only.Validate(); !errors.Is(err, ErrInvalidProposal) || !strings.Contains(err.Error(), "retire:") {
		t.Errorf("a compaction with no inputs: %v, want ErrInvalidProposal naming retire", err)
	}
}
