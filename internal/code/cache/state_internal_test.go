package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A ROUND COMPARES EVERY FIELD OF A REPOSITORY'S ROW but the position it was
// read at (sameRepo): a row that changed only in a field it skipped is a round
// that writes no state, so the state a restart reads names the row as it was.
// Each field of code.RepoRef is set alone, by reflection, so a field the row
// gains is compared or this fails naming it: a list kept by hand beside the
// row is one every new field of it slips past.
//
// Mutation: drop DropEpoch from sameRepo, and this fails naming it.
func TestARoundComparesEveryFieldOfARepositorysRow(t *testing.T) {
	t.Parallel()
	row := reflect.TypeFor[code.RepoRef]()
	for i := range row.NumField() {
		var changed code.RepoRef
		field := reflect.ValueOf(&changed).Elem().Field(i)
		if !setSome(field) {
			t.Fatalf("RepoRef.%s is a %s this case cannot set: teach setSome its kind", row.Field(i).Name,
				field.Type())
		}
		same := sameRepo(code.RepoRef{}, changed)
		switch name := row.Field(i).Name; {
		case name == "Position" && !same:
			t.Error("two reads of one row at two positions are found different")
		case name != "Position" && same:
			t.Errorf("two rows differing in %s alone are found the same", name)
		}
	}
}

// TWO READS OF ONE GATED ROW ARE ONE ROW. Every round reads its repository's
// row afresh, and a rule's merge gate is a pointer, so two reads of a rule
// carrying one are two pointers: compared by ==, a gated repository's row
// would read as changed at every round, and every round would write its state
// again. Two decodes of one row compare the same, and the same row with one
// field of a gate changed does not.
//
// Mutation: compare the rules with slices.Equal in sameRepo — the two reads
// are found different, red.
func TestTwoReadsOfOneGatedRowAreTheSame(t *testing.T) {
	t.Parallel()
	row := code.RepoRef{ID: "repo-1", Name: "web", Protections: []code.Protection{{
		Pattern: "refs/heads/main", DirectPush: code.DirectPersons, Gate: &code.MergeGate{
			MergeBy: code.MergeByMaintainers, Approvals: code.ApprovalsOne, Checks: []string{"unit"},
			Landing: code.Landing{Policy: code.LandingPreLand, Depth: 4, Batch: 4}, DeleteSource: code.DeleteSeats,
		},
	}}}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var one, two code.RepoRef
	if err := json.Unmarshal(raw, &one); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &two); err != nil {
		t.Fatal(err)
	}
	if !sameRepo(one, two) {
		t.Error("two reads of one gated repository's row are found different: every round would write its state")
	}
	two.Protections[0].Gate.Checks = []string{"lint", "unit"}
	if sameRepo(one, two) {
		t.Error("two rows whose gates require different checks are found the same")
	}
}

// THE WIDEST STATE THIS CACHE WRITES FITS ITS READ, with half of it again to
// spare: a repository whose every rule is at the domain's bounds — a 255-byte
// pattern and a merge gate of code.MaxGateBytes, every byte of the pattern one
// JSON spells as six — and whose description is spelled the same way, beside
// every bounded list of a state at its bound. A state past maxStateBytes is
// refused as unreadable, and its repository would start cold at every boot.
//
// The gates are counted at their bound rather than built: each one a record
// may carry is at most MaxGateBytes of JSON, so the widest state is the state
// with none plus a gate member of that width a rule.
func TestTheWidestStateFitsItsRead(t *testing.T) {
	t.Parallel()
	wide := func(n int) string { return strings.Repeat("&", n) }
	maxPosition := statelog.Position{Stream: code.Domain{}.Stream().Name, Generation: ^uint32(0), Seq: ^uint64(0)}
	const most = int64(^uint64(0) >> 1)
	row := code.RepoRef{
		ID: "0190f3c4-0000-7000-8000-000000000001", Name: strings.Repeat("a", 63),
		Status: code.RepoStatusArchived, ManagedBy: code.ManagedConfig, Kind: code.RepoProject,
		Owner: code.Owner{Kind: code.OwnerUnit, Ref: wide(64)}, Visibility: code.VisibilityCompany,
		DefaultBranch: wide(gitref.MaxRefNameBytes - len("refs/heads/")), Description: wide(code.MaxDescriptionBytes),
		DeletedAt: time.Now(), LastActivityAt: time.Now(), Refs: most, LivePacks: most, LiveBytes: most,
		LiveObjects: most, DropEpoch: most, Position: maxPosition,
	}
	for i := range code.MaxProtectionsPerRepo {
		head := fmt.Sprintf("refs/heads/%02d", i)
		row.Protections = append(row.Protections, code.Protection{Pattern: head + wide(gitref.MaxRefNameBytes-len(head)),
			AllowForce: true, AllowDelete: true, DirectPush: code.DirectPersons})
	}
	layers := make([]string, code.MaxPublishedLayers)
	for i := range layers {
		layers[i] = strings.Repeat("f", 40)
	}
	runs := make([]provenanceRun, MaxProvenanceRuns)
	for i := range runs {
		runs[i] = provenanceRun{N: ^uint64(0), Bytes: most, From: most, Through: most, Pushes: most, Commits: most,
			Moves: most}
	}
	cohorts := make([]memoCohort, maxStateCohorts)
	for i := range cohorts {
		cohorts[i] = memoCohort{N: ^uint64(0), Bytes: most}
	}
	st := state{
		Format: stateFormat, RepoID: row.ID, StreamCreated: time.Now(), Position: maxPosition, Repo: row,
		RefsDigest: strings.Repeat("f", 64), Refs: int(most), Packs: int(most), DiskBytes: most, DroppingBytes: most,
		Maintained: maintainedState{LastRun: time.Now(), Took: time.Duration(most)}, LeavingSince: maxPosition,
		Derived: map[DerivedName]derivedState{},
	}
	for _, d := range DerivedFiles() {
		entry := derivedState{Bytes: most, Covers: int(most), Step: "x"}
		switch d.Name {
		case DerivedProvenance:
			entry.Provenance = &provenanceState{Stream: time.Now(), From: most, Through: most, GapFrom: most,
				GapThrough: most, Complete: true, Oldest: most, OldestAt: time.Now(), Next: ^uint64(0), Runs: runs}
		case DerivedMemo:
			entry.Cohorts = cohorts
		case DerivedLayers, DerivedGraphLayers:
			entry.Layers, entry.Tip, entry.TipOver = layers, strings.Repeat("f", 40), strings.Repeat("f", 32)
		}
		st.Derived[d.Name] = entry
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	widest := len(raw) + code.MaxProtectionsPerRepo*(len(`,"gate":`)+code.MaxGateBytes)
	if widest+widest/2 > maxStateBytes {
		t.Errorf("the widest state is %d bytes: maxStateBytes (%d) is less than half again of it, and a state "+
			"past it is unreadable", widest, maxStateBytes)
	}
}

// setSome sets v, a zero value, to another value of its type — a struct
// through its first exported field it can set — and reports whether it could.
func setSome(v reflect.Value) bool {
	if v.Type() == reflect.TypeFor[time.Time]() {
		v.Set(reflect.ValueOf(time.Unix(1, 0)))
		return true
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() && setSome(v.Field(i)) {
				return true
			}
		}
		return false
	default:
		return false
	}
	return true
}

// A DERIVED FILE NO RECORDED WRITE MADE COVERS NOTHING, whatever its entry's
// Covers says: what an index covers is the maintainer's reason not to write
// one (indexDue, plan's plainDue) and the order its pass takes repositories
// in, so an entry no write made — a part found beside no main file, or one
// read from a state — that covered packs would leave a copy unindexed with
// nothing to say so. The rule lives in the accessor every reader asks, so no
// path that sets an entry can break it; state.check refuses such a state
// besides (TestAStateOfAnotherFormatIsDiscarded).
//
// Mutation: make covers return Covers whatever its Step — the first row reads
// three packs covered.
func TestADerivedFileNoWriteMadeCoversNothing(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		d    derivedState
		want int
	}{
		{"made by no write", derivedState{Bytes: 400, Covers: 3}, 0},
		{"the round's index", derivedState{Bytes: 400, Covers: 3, Step: StepIndex}, 3},
		{"the maintainer's bitmap", derivedState{Bytes: 400, Covers: 3, Step: StepBitmap}, 3},
	} {
		inc := &incarnation{derived: map[DerivedName]derivedState{DerivedMidx: row.d}}
		if got := inc.covered(); got != row.want {
			t.Errorf("%s: an index recorded as %+v covers %d packs, want %d", row.name, row.d, got, row.want)
		}
	}
	if got := (&incarnation{}).covered(); got != 0 {
		t.Errorf("no index covers %d packs, want none", got)
	}
}

// BenchmarkReadState is the part of a warm boot that is each repository's
// own: one readState — the open, the read, the decode and the check — of a
// state recording both derived files, as the maintainer leaves a served copy.
// BenchmarkBoot times a whole boot, whose figure a shared machine's load
// moves by more than one state's read; this is the per-repository term on
// its own, the one a change to the state's format moves (cache doc.go,
// "Measured").
func BenchmarkReadState(b *testing.B) {
	dir := b.TempDir()
	id := "b1c2d3e4-f5a6-4b7c-8d9e-0f1a2b3c4d5e"
	raw, err := json.Marshal(state{
		Format:        stateFormat,
		RepoID:        id,
		StreamCreated: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		Position:      statelog.Position{Stream: topics.CodeLogStream, Generation: 2, Seq: 4711},
		Repo: code.RepoRef{
			ID: id, Name: "payments", Status: code.RepoStatusActive, DefaultBranch: "trunk",
			Protections: []code.Protection{{Pattern: "refs/heads/trunk", DirectPush: code.DirectNone}},
			Refs:        12, LivePacks: 100, LiveBytes: 9000,
		},
		RefsDigest: strings.Repeat("ab", 32),
		Refs:       12,
		Packs:      100,
		DiskBytes:  9100,
		Derived: map[DerivedName]derivedState{
			DerivedMidx:  {Bytes: 400, Covers: 100, Step: StepBitmap},
			DerivedGraph: {Bytes: 120, Covers: 100, Step: StepGraph},
		},
		Maintained: maintainedState{LastRun: time.Date(2026, 10, 1, 9, 5, 0, 0, time.UTC), Took: 1500 * time.Millisecond},
	})
	if err != nil {
		b.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, stateName), raw, 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if st, err := readState(dir, id); err != nil || st == nil {
			b.Fatalf("readState = %v, %v", st, err)
		}
	}
}

// A TAIL IS KNOWN BY ITS PACKS, WHATEVER THEIR ORDER (tailDigest): the node's
// tip persists the digest of the tail it covered, and a boot, a full round and
// a swap each count the tail afresh — so the same packs counted in any order
// read the same digest, a pack counted in and out again leaves it where it
// was, a pack more or less is another tail, and the digest a state records
// reads back as itself and nothing else does.
//
// Mutation: let flip add a pack's bytes rather than XOR them — the pack
// counted in and out again leaves the digest moved.
func TestATailIsKnownByItsPacksWhateverTheirOrder(t *testing.T) {
	t.Parallel()
	packs := []livePack{
		{checksum: strings.Repeat("1", 40), size: 10, objects: 3},
		{checksum: "0123456789abcdef0123456789abcdef01234567", size: 20, objects: 5},
		{checksum: strings.Repeat("e", 40), size: 30, objects: 7},
	}
	count := func(order ...int) tailCount {
		var c tailCount
		for _, i := range order {
			c.add(packs[i], 1)
		}
		return c
	}
	whole := count(0, 1, 2)
	if reversed := count(2, 1, 0); reversed != whole {
		t.Errorf("the same packs counted in two orders read %+v and %+v", whole, reversed)
	}
	less := whole
	less.add(packs[1], -1)
	if less.digest == whole.digest || less != count(0, 2) {
		t.Errorf("the tail less a pack reads %+v, want %+v and another digest than %+v", less, count(0, 2), whole)
	}
	less.add(packs[1], 1)
	if less != whole {
		t.Errorf("the pack counted in again reads %+v, want the whole tail's %+v", less, whole)
	}
	var none tailCount
	none.add(packs[0], 1)
	none.add(packs[0], -1)
	if none != (tailCount{}) {
		t.Errorf("a pack counted in and out of an empty tail leaves %+v", none)
	}
	if got, err := parseTailDigest(whole.digest.String()); err != nil || got != whole.digest {
		t.Errorf("the recorded digest %s reads back as %x, %v", whole.digest, got, err)
	}
	for _, bad := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 40), strings.Repeat("A", 32),
		strings.Repeat("g", 32)} {
		if _, err := parseTailDigest(bad); err == nil {
			t.Errorf("%q reads as a tail's digest", bad)
		}
	}
}
