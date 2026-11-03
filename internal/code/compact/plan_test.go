package compact_test

import (
	"crypto/sha1" //nolint:gosec // a fixture's 40 hex digits, named after a pack id: no security property.
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// row is a live pack row named id, of objects objects and a pack section of
// packBytes, with the indexes a version-2 index and a version-1 reverse index
// of that count take: what a plan weighs (code.PackRow, as the cache's live
// set answers it).
func row(id string, objects, packBytes int64) code.PackRow {
	idx, rev := code.IndexBytesFor(objects), code.ReverseBytesFor(objects)
	return code.PackRow{PackID: id, Size: packBytes + idx + rev, IndexBytes: idx, ReverseBytes: rev,
		GitChecksum: checksumOf(id), Objects: objects}
}

// checksumOf is a git checksum named after id: forty hex digits no other id
// shares.
func checksumOf(id string) string {
	sum := sha1.Sum([]byte(id)) //nolint:gosec // see the import.
	return hex.EncodeToString(sum[:])
}

// inputIDs is every row id a plan's inputs retire, sorted.
func inputIDs(p compact.Planned) []string {
	var ids []string
	for _, in := range p.Inputs {
		ids = append(ids, in...)
	}
	slices.Sort(ids)
	return ids
}

// A FULL PACK IS NEVER MERGED AGAIN: neither one whose pack section is at
// seven eighths of the cut nor one whose whole bundle is, though its pack
// section is short of it — what a dense roll's scaled cut writes — however
// LIGHT it is, so the geometric split, which weighs a pack by its objects,
// would otherwise roll a few large blobs up with the small packs below them.
// And a repository holding nothing but full packs is never due, whatever their
// number: what a later published layer names stays valid until an expunge.
//
// Mutation: drop the full filter from plan, and the light full packs are rolled
// up with the small ones, and the repository of full packs is due.
func TestThePlanNeverTakesAFullPack(t *testing.T) {
	t.Parallel()
	full := []code.PackRow{
		row("full-by-pack", 4, code.FullPackBytes),
		row("full-by-pack-past", 3, code.FullPackBytes+1),
	}
	// Full by its bundle alone: a pack section a mebibyte short of the
	// threshold, and indexes over enough objects to take the bundle past it.
	byBundle := row("full-by-bundle", 1, code.FullPackBytes-(1<<20))
	byBundle.IndexBytes += 1 << 20
	byBundle.Size += 1 << 20
	full = append(full, byBundle)
	for _, f := range full {
		if !f.Full() {
			t.Fatalf("the fixture's %s is not full by code.PackRow.Full", f.PackID)
		}
	}
	// Past the floor by two, so the roll is due by the small packs alone.
	var live []code.PackRow
	var small []string
	for i := range code.CompactMinMerge + 2 {
		id := fmt.Sprintf("small-%02d", i)
		live = append(live, row(id, 10, 1<<20))
		small = append(small, id)
	}
	live = append(live, full...)
	p := compact.PlanOf(live, compact.InputByteCap(code.CompactPackBytes), code.CompactPackBytes)
	if !p.Due {
		t.Fatalf("%d small packs beside three full ones planned not due: %+v", len(small), p)
	}
	if got := inputIDs(p); !slices.Equal(got, small) {
		t.Errorf("the plan merges %v, want the %d small packs %v and no full one", got, len(small), small)
	}

	var onlyFull []code.PackRow
	for i := range code.CompactTargetPacks + 10 {
		onlyFull = append(onlyFull, row(fmt.Sprintf("full-%03d", i), 2, code.FullPackBytes))
	}
	if p := compact.PlanOf(onlyFull, compact.InputByteCap(code.CompactPackBytes), code.CompactPackBytes); p.Due ||
		len(p.Inputs) > 0 {
		t.Errorf("a repository of %d full packs planned %+v, want not due", len(onlyFull), p)
	}
}

// THE PLAN LEAVES EVERY PUBLISHED PACK OUT: a node's chain loads only over
// every pack a layer of it names, so the decide refuses a compaction retiring
// one stale — and the plan never builds one: the packs a layer names are out
// of the roll before it is split or counted against the target, so a roll's
// small packs that a layer covers and two more are no compaction, where the
// same packs unpublished are one.
//
// Mutation: plan over every live pack, and the published packs are merged.
func TestThePlanLeavesEveryPublishedPackOut(t *testing.T) {
	t.Parallel()
	n := code.CompactMinMerge + 2
	var live []code.PackRow
	published := map[string]bool{}
	for i := range n {
		id := fmt.Sprintf("p-%02d", i)
		live = append(live, row(id, 100, 10<<10))
		if i < code.CompactMinMerge {
			published[checksumOf(id)] = true
		}
	}
	cap := compact.InputByteCap(code.CompactPackBytes)
	if p := compact.PlanOver(live, published, cap, code.CompactPackBytes); p.Due {
		t.Errorf("%d packs %d of which a layer covers planned the merge of %v, want nothing due", n,
			code.CompactMinMerge, inputIDs(p))
	}
	if p := compact.PlanOver(live, nil, cap, code.CompactPackBytes); !p.Due || len(inputIDs(p)) != n {
		t.Errorf("the same %d packs published nowhere planned due=%v over %v, want the %d merged (the premise)", n,
			p.Due, inputIDs(p), n)
	}
}

// A REPOSITORY PAST THE TARGET ALWAYS SHRINKS: one whose packs already form a
// geometric progression rolls up nothing by git's split, and past
// code.CompactTargetPacks non-full packs the two lightest are merged anyway,
// so its pack count falls by one at least at every poll — while at the target
// exactly, with nothing to roll, it is not due.
//
// Mutation: drop the target rule, and the progression of sixty is never due.
func TestARepositoryPastTheTargetAlwaysShrinks(t *testing.T) {
	t.Parallel()
	// A PROGRESSION BY OBJECTS, every pack a kibibyte whatever its count —
	// weights doubling sixty times are no repository's, and indexes sized
	// by them would make every heavy pack full by its bundle, which is
	// another rule's case.
	progression := func(n int) []code.PackRow {
		out := make([]code.PackRow, n)
		weight := int64(1)
		for i := range out {
			out[i] = code.PackRow{PackID: fmt.Sprintf("p-%03d", i), Size: 1 << 10,
				GitChecksum: checksumOf(fmt.Sprintf("p-%03d", i)), Objects: weight}
			weight *= code.CompactFactor
		}
		return out
	}
	for _, c := range []struct {
		packs int
		due   bool
	}{{packs: 60, due: true}, {packs: code.CompactTargetPacks + 1, due: true}, {packs: code.CompactTargetPacks}} {
		live := progression(c.packs)
		p := compact.PlanOf(live, compact.InputByteCap(code.CompactPackBytes), code.CompactPackBytes)
		if p.Due != c.due {
			t.Errorf("a progression of %d packs planned due=%v, want %v", c.packs, p.Due, c.due)
			continue
		}
		if !c.due {
			continue
		}
		if got, want := inputIDs(p), []string{"p-000", "p-001"}; !slices.Equal(got, want) {
			t.Errorf("a progression of %d packs merges %v, want the two lightest %v", c.packs, got, want)
		}
	}
	if split := compact.GeometricSplit([]int64{1, 2, 4, 8}, code.CompactFactor); split != 0 {
		t.Errorf("git's split of a progression rolls %d packs up, want none", split)
	}
}

// A ROLL IS CAPPED AT ITS OUTPUTS: whatever a repository's packs, one
// compaction merges at most compact.InputByteCap of their pack bytes — half
// code.MaxCompactOutputs cuts, the lightest first, since a cut writes outputs
// a little under it and a delta whose base went to an earlier output whole —
// and at most code.MaxCompactInputs of their rows. And a proposal of more
// outputs than one compaction writes is refused before any committer decides
// it, naming them.
//
// Mutation: drop the byte cap, and the forty packs of 0.8 GiB are merged in
// one roll; drop the row cap, and the 1 100 rows are.
func TestARollIsCappedAtItsOutputs(t *testing.T) {
	t.Parallel()
	const packBytes = 4 << 28 / 5 // 0.8 GiB: not full, and ten of them fit the cap
	var heavy []code.PackRow
	for i := range 40 {
		heavy = append(heavy, row(fmt.Sprintf("heavy-%02d", i), 1_000, packBytes))
	}
	byteCap := compact.InputByteCap(code.CompactPackBytes)
	p := compact.PlanOf(heavy, byteCap, code.CompactPackBytes)
	if !p.Due {
		t.Fatalf("forty packs of 0.8 GiB planned not due")
	}
	if got, want := len(p.Inputs), int(byteCap/packBytes); got != want {
		t.Errorf("forty packs of 0.8 GiB: the roll merges %d, want the %d whose pack bytes fit %d", got, want,
			byteCap)
	}
	if got := inputIDs(p); len(got) > 0 && got[0] != "heavy-00" {
		t.Errorf("the capped roll begins at %s, want the lightest (the first by id among equals)", got[0])
	}

	var many []code.PackRow
	for i := range 1_100 {
		many = append(many, row(fmt.Sprintf("one-%04d", i), 1, 1<<10))
	}
	if p := compact.PlanOf(many, byteCap, code.CompactPackBytes); len(inputIDs(p)) != code.MaxCompactInputs {
		t.Errorf("1 100 packs of one object: the roll merges %d rows, want %d", len(inputIDs(p)),
			code.MaxCompactInputs)
	}

	// TWO FILES NAMING MORE ROWS THAN ONE COMPACTION RETIRES are no roll: a
	// plan never names more rows than its record may, even where the two
	// lightest alone would, or every poll would build what its committer
	// refuses.
	var dup []code.PackRow
	for _, f := range []string{"a", "b"} {
		for i := range code.MaxCompactInputs/2 + 1 {
			r := row(fmt.Sprintf("dup-%s-%04d", f, i), 1, 1<<10)
			r.GitChecksum = checksumOf("file-" + f)
			dup = append(dup, r)
		}
	}
	for i := range code.CompactMinMerge {
		dup = append(dup, row(fmt.Sprintf("other-%d", i), 1, 1<<10))
	}
	if p := compact.PlanOf(dup, byteCap, code.CompactPackBytes); len(inputIDs(p)) > code.MaxCompactInputs {
		t.Errorf("a roll of files named by %d rows retires %d, past the %d one record may", len(dup),
			len(inputIDs(p)), code.MaxCompactInputs)
	}

	// THE COMMITTER'S OWN LIMIT on what a roll may write, held before a byte
	// is stored.
	id := statelog.NewOpID(time.Now(), "")
	retire := make([]string, code.MaxCompactOutputs+2)
	for i := range retire {
		retire[i] = fmt.Sprintf("in-%02d", i)
	}
	add := make([]code.PackRef, code.MaxCompactOutputs+1)
	for i := range add {
		add[i] = row(code.DeriveOutputPackID(id, i), 1_000, 1<<20).Ref()
	}
	err := code.CompactionProposal{Compaction: code.Compaction{
		RepoID: "0193b4f2-7c7a-7e3a-9f0e-3c8b2a1d4e5f", CompactionID: id, Node: "git-a",
		Seen: statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9}, Retire: retire, Add: add,
	}}.ValidateUnstored()
	if err == nil || !strings.Contains(err.Error(), "add") {
		t.Errorf("a proposal of %d outputs = %v, want refused naming add", len(add), err)
	}
}
