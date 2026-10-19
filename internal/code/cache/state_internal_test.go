package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

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
