package cache

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RUN FORMAT (provrun.go) AND THE VIEW OVER IT (provenance.go), read back
// from bytes in memory: no cache, no git — a run is a function of the push
// rows it is built from.

// runPusher is who the suite's synthetic pushes are made by.
var runPusher = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"}

// synthetic is push i of a synthetic history of one ref: at position base+i,
// moving ref from commit i to commit i+1, introducing commit i+1.
func synthetic(base int64, i int, ref string) code.IntroducedRow {
	return code.IntroducedRow{
		PushedBy: code.PushedBy{
			PushID:    uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("push-%d", i))).String(),
			Position:  statelog.Unpack(topics.CodeLogStream, base+int64(i)),
			At:        time.Unix(1_800_000_000+int64(i), 0).UTC(),
			Principal: runPusher, Via: code.ViaGit,
		},
		Moves:      []code.RefMove{{Ref: ref, Old: oidOf(i), New: oidOf(i + 1)}},
		Introduced: []string{oidOf(i + 1)}, Count: 1,
	}
}

// oidOf is synthetic commit i's id; commit 0 is the zero id, a create's old.
func oidOf(i int) string { return fmt.Sprintf("%040x", i) }

// runOf is the run of rows over (from, through], written and read back.
func runOf(t testing.TB, from, through int64, rows []code.IntroducedRow) runFile {
	t.Helper()
	return runKeyed(t, from, through, rows, refKey)
}

// runKeyed is runOf with every ref filed under the key keyOf answers.
func runKeyed(t testing.TB, from, through int64, rows []code.IntroducedRow,
	keyOf func(string) [runKeyBytes]byte) runFile {
	t.Helper()
	built, err := buildRunKeyed(from, through, rows, keyOf)
	if err != nil {
		t.Fatalf("building a run: %v", err)
	}
	var buf bytes.Buffer
	if _, err := built.write(&buf); err != nil {
		t.Fatalf("writing a run: %v", err)
	}
	f, err := openRunBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("reading a run back: %v", err)
	}
	f.keyOf = keyOf
	return f
}

// MOVES DECODE ONLY WHAT IS PROBED. A landed-in search over the busiest
// branch of the large model — hundreds of thousands of moves in the window —
// probes a few dozen: the view over the runs is found by a binary search a run,
// its length, its segments, its gap and every move's position are read off
// fixed-width records, and a move and its push are decoded only when At asks
// for that one. Over 100 000 moves in three runs: nothing decoded for the
// view, its segments, its gaps and a binary search of its positions, and one
// decode an At.
//
// Mutation: decode the range into a slice as Moves cuts it — the count is
// 100 000.
func TestMovesDecodesOnlyWhatIsProbed(t *testing.T) {
	t.Parallel()
	const n, ref = 100_000, "refs/heads/main"
	base := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 1}.Packed()
	rows := make([]code.IntroducedRow, n)
	for i := range n {
		rows[i] = synthetic(base, i, ref)
	}
	cuts := []int{0, 30_000, 60_000, n}
	var runs []runFile
	for k := range 3 {
		lo, hi := cuts[k], cuts[k+1]
		runs = append(runs, runOf(t, base+int64(lo)-1, base+int64(hi)-1, rows[lo:hi]))
	}
	var decodes atomic.Int64
	p := Provenance{runs: runs, from: base - 1, indexed: true, decodes: &decodes}
	v := p.Moves(ref, statelog.Unpack(topics.CodeLogStream, base+n))
	if v.Len() != n {
		t.Fatalf("the view holds %d moves, want %d", v.Len(), n)
	}
	if segs := v.Segments(); !slices.Equal(segs, []int{0}) {
		t.Errorf("an unforced history's segments are %v, want the one beginning at its first move", segs)
	}
	v.Gaps()
	target := base + 77_777
	lo, hi := 0, v.Len()
	for lo < hi {
		mid := (lo + hi) / 2
		if v.Position(mid).Packed() < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if got := decodes.Load(); got != 0 {
		t.Fatalf("the view, its segments, its gaps and a binary search of its positions decoded %d moves, want none",
			got)
	}
	for i := range 17 {
		if m := v.At(lo + i); m.New != oidOf(77_777+i+1) || m.By.PushID != rows[77_777+i].PushID {
			t.Fatalf("move %d is %+v, want push %d's", lo+i, m, 77_777+i)
		}
	}
	if got := decodes.Load(); got != 17 {
		t.Errorf("17 probes decoded %d moves, want 17", got)
	}
}

// TWO REFS SHARING A KEY KEEP THEIR OWN MOVES. A run files a ref's moves under
// the first eight bytes of its name's SHA-256, and two refs of one repository
// share them about once in a billion at the most refs a repository holds: each
// is then an ordinal of the key, found by its name, so its moves stay its own
// — in a run, in a merge of two, and for a ref the run holds no move of.
//
// Mutation: find a ref by its key alone (refOrdinal answering the key's first
// ordinal) — b's view holds a's moves.
func TestTwoRefsSharingAKeyKeepTheirOwnMoves(t *testing.T) {
	t.Parallel()
	same := func(string) [runKeyBytes]byte { return [runKeyBytes]byte{1, 2, 3} }
	base := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 10}.Packed()
	var first, second []code.IntroducedRow
	for i := range 8 {
		ref := []string{"refs/heads/a", "refs/heads/b"}[i%2]
		row := synthetic(base, i, ref)
		if i < 4 {
			first = append(first, row)
		} else {
			second = append(second, row)
		}
	}
	a := runKeyed(t, base-1, base+3, first, same)
	b := runKeyed(t, base+3, base+7, second, same)
	var buf bytes.Buffer
	if _, err := mergeRuns(&buf, a, b, base-1); err != nil {
		t.Fatal(err)
	}
	merged, err := openRunBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	merged.keyOf = same
	for _, tc := range []struct {
		name string
		runs []runFile
	}{{"two runs", []runFile{a, b}}, {"their merge", []runFile{merged}}} {
		p := Provenance{runs: tc.runs, from: base - 1, indexed: true}
		at := statelog.Unpack(topics.CodeLogStream, base+8)
		for _, want := range []struct {
			ref   string
			moves []int
		}{{"refs/heads/a", []int{0, 2, 4, 6}}, {"refs/heads/b", []int{1, 3, 5, 7}}, {"refs/heads/c", nil}} {
			v := p.Moves(want.ref, at)
			var got []int
			for i := range v.Len() {
				m := v.At(i)
				if m.Ref != want.ref {
					t.Errorf("%s: %s's view holds a move of %s", tc.name, want.ref, m.Ref)
				}
				got = append(got, int(v.Position(i).Packed()-base))
			}
			if !slices.Equal(got, want.moves) {
				t.Errorf("%s: %s's moves are at %v, want %v", tc.name, want.ref, got, want.moves)
			}
		}
	}
}

// A RUN ANSWERS WHAT IT WAS BUILT FROM: every commit its pushes introduced
// answers the push — the newest of two listing one — with the principal and
// the way the row named; and the moves of one ref that one record's pushes
// landed, decided one after another by group commit, come back in the order
// they landed, which their values spell, whatever order their push ids sort
// in.
//
// Mutation: order a record's moves by their push ids (chainMoves keeping the
// pushes' order) — the second landing comes back first.
func TestARunAnswersWhatItWasBuiltFrom(t *testing.T) {
	t.Parallel()
	base := statelog.Position{Stream: topics.CodeLogStream, Generation: 2, Seq: 100}.Packed()
	at := statelog.Unpack(topics.CodeLogStream, base)
	seat := code.Principal{Kind: code.PrincipalAgent, Seat: "eng-lead"}
	// Two pushes at one position, both moving main: the one whose id sorts
	// first landed SECOND (its old is the other's new).
	idLow, idHigh := "00000000-0000-4000-8000-000000000001", "ffffffff-ffff-4fff-bfff-ffffffffffff"
	rows := []code.IntroducedRow{
		{PushedBy: code.PushedBy{PushID: idLow, Position: at, At: time.Unix(10, 0).UTC(), Principal: seat,
			Via: code.ViaGit},
			Moves:      []code.RefMove{{Ref: "refs/heads/main", Old: oidOf(2), New: oidOf(3)}},
			Introduced: []string{oidOf(3), oidOf(9)}},
		{PushedBy: code.PushedBy{PushID: idHigh, Position: at, At: time.Unix(9, 0).UTC(), Principal: runPusher,
			Via: code.ViaGit},
			Moves:      []code.RefMove{{Ref: "refs/heads/main", Old: oidOf(1), New: oidOf(2), Forced: true}},
			Introduced: []string{oidOf(2), oidOf(9)}},
	}
	p := Provenance{runs: []runFile{runOf(t, base-1, base, rows)}, from: base - 1, indexed: true}
	v := p.Moves("refs/heads/main", at)
	if v.Len() != 2 {
		t.Fatalf("main's view holds %d moves, want 2", v.Len())
	}
	if first, second := v.At(0), v.At(1); first.By.PushID != idHigh || second.By.PushID != idLow ||
		!first.Forced || second.Forced {
		t.Errorf("main's moves came back as %+v then %+v, want the forced move to commit 2 first", first, second)
	}
	for oid, want := range map[string]code.PushedBy{oidOf(3): rows[0].PushedBy, oidOf(2): rows[1].PushedBy} {
		got, ok := p.Commit(oid)
		if !ok || got != want {
			t.Errorf("commit %s is answered by %+v (%t), want %+v", oid[:7], got, ok, want)
		}
	}
	// Listed by both: the newer in (position, push id) order, the run's last.
	if got, _ := p.Commit(oidOf(9)); got.PushID != idHigh {
		t.Errorf("a commit both pushes list is answered by %s, want the newer of the two, %s", got.PushID, idHigh)
	}
	if _, ok := p.Commit(oidOf(7)); ok {
		t.Error("a commit no push introduced is answered by a push")
	}
}

// A MERGE KEEPS EVERY PUSH ABOVE ITS FLOOR, AND NOTHING BELOW. Two adjacent
// runs merge into one that answers exactly what the two answered above the
// floor it is given — the rows' horizon — with each push renumbered and every
// move and commit pointing at it, and nothing at or below the floor.
//
// Mutation: renumber the second run's pushes from zero (base[1] = 0) — its
// moves name the first run's pushes.
func TestAMergeKeepsEveryPushAboveItsFloor(t *testing.T) {
	t.Parallel()
	const ref = "refs/heads/main"
	base := statelog.Position{Stream: topics.CodeLogStream, Generation: 3, Seq: 1}.Packed()
	var rows []code.IntroducedRow
	for i := range 10 {
		rows = append(rows, synthetic(base, i, ref))
	}
	a := runOf(t, base-1, base+4, rows[:5])
	b := runOf(t, base+4, base+9, rows[5:])
	floor := base + 2 // pushes 0, 1 and 2 are at or below it
	var buf bytes.Buffer
	h, err := mergeRuns(&buf, a, b, floor)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := openRunBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if h.pushes != 7 || merged.h.from != floor || merged.h.through != base+9 {
		t.Errorf("the merge holds %d pushes over (%d, %d], want 7 over (%d, %d]", h.pushes, merged.h.from,
			merged.h.through, floor, base+9)
	}
	p := Provenance{runs: []runFile{merged}, from: floor, indexed: true}
	for i, row := range rows {
		got, ok := p.Commit(oidOf(i + 1))
		switch {
		case i <= 2 && ok:
			t.Errorf("push %d, at or below the floor, still answers commit %d", i, i+1)
		case i > 2 && (!ok || got.PushID != row.PushID):
			t.Errorf("commit %d is answered by %q (%t), want push %d's %s", i+1, got.PushID, ok, i, row.PushID)
		}
	}
	v := p.Moves(ref, statelog.Unpack(topics.CodeLogStream, base+9))
	if v.Len() != 7 {
		t.Fatalf("the merge's view holds %d moves, want 7", v.Len())
	}
	for i := range v.Len() {
		if m := v.At(i); m.By.PushID != rows[i+3].PushID || m.New != oidOf(i+4) {
			t.Errorf("the merge's move %d is %+v, want push %d's", i, m, i+3)
		}
	}
}

// EVERY PUSH PRINCIPAL AND WAY HAS A RUN CODE. A run names a push's way and
// its principal's kind by a code of one table each, so a kind or a way a later
// build adds to code's closed sets is a code here — or a run could not be
// written for a push of it, and its round's fold would leave the index a gap.
// Each code is distinct and none is zero, and a push of each round-trips.
//
// Mutation: drop the agent's code — a seat's push builds no run.
func TestEveryPushPrincipalAndWayHasARunCode(t *testing.T) {
	t.Parallel()
	seen := map[uint8]bool{}
	for _, k := range code.PrincipalKinds() {
		c, ok := runKindCodes[k]
		if !ok || c == 0 || seen[c] {
			t.Errorf("principal kind %q has run code %d (%t): every kind needs a distinct non-zero one", k, c, ok)
		}
		seen[c] = true
	}
	seen = map[uint8]bool{}
	for _, v := range code.PushVias() {
		c, ok := runViaCodes[v]
		if !ok || c == 0 || seen[c] {
			t.Errorf("push way %q has run code %d (%t): every way needs a distinct non-zero one", v, c, ok)
		}
		seen[c] = true
	}
	for _, k := range code.PrincipalKinds() {
		for _, v := range code.PushVias() {
			row := synthetic(1, 0, "refs/heads/main")
			row.Via, row.Principal = v, code.Principal{Kind: k, Seat: "eng-lead"}
			if k == code.PrincipalOperator {
				row.Principal.OperatorID = "pat:" + strings.Repeat("x", 300)
			}
			payload, err := pushPayload(row)
			if err != nil {
				t.Errorf("a push by a %s, %s: %v", k, v, err)
				continue
			}
			if got, err := decodePush(payload); err != nil || got != row.PushedBy {
				t.Errorf("a push by a %s, %s, read back as %+v (%v), want %+v", k, v, got, err, row.PushedBy)
			}
		}
	}
}

// AN INDEX'S RECORD NO WRITE LEAVES IS REFUSED, so a state file holding one is
// no state this cache wrote: runs out of order, overlapping, in the gap, past
// the range or empty; a gap outside the range or an empty one; a number taken
// twice or past Next; more runs than MaxProvenanceRuns; bytes that are not the
// runs'.
//
// Mutation: drop the overlap case from check — the overlapping record passes.
func TestAnIndexsRecordNoWriteLeavesIsRefused(t *testing.T) {
	t.Parallel()
	valid := func() *provenanceState {
		return &provenanceState{From: 10, Through: 50, GapFrom: 20, GapThrough: 30, Next: 4, Runs: []provenanceRun{
			{N: 1, Bytes: 100, From: 10, Through: 20, Pushes: 3},
			{N: 3, Bytes: 50, From: 30, Through: 50, Pushes: 1},
		}}
	}
	if err := valid().check(150); err != nil {
		t.Fatalf("a valid record: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(s *provenanceState)
		bytes  int64
	}{
		{"a range that runs backwards", func(s *provenanceState) { s.From = 60 }, 150},
		{"a gap outside the range", func(s *provenanceState) { s.GapThrough = 70 }, 150},
		{"an empty gap left named", func(s *provenanceState) { s.GapFrom, s.GapThrough = 25, 25 }, 150},
		{"overlapping runs", func(s *provenanceState) { s.Runs[1].From = 15; s.GapFrom, s.GapThrough = 0, 0 }, 150},
		{"a run in the gap", func(s *provenanceState) { s.Runs[1].From = 25 }, 150},
		{"a run past the range", func(s *provenanceState) { s.Runs[1].Through = 60 }, 150},
		{"a run of no pushes", func(s *provenanceState) { s.Runs[0].Pushes = 0 }, 150},
		{"a number twice", func(s *provenanceState) { s.Runs[1].N = 1 }, 150},
		{"a number past Next", func(s *provenanceState) { s.Next = 3 }, 150},
		{"bytes not the runs'", func(*provenanceState) {}, 151},
		{"more runs than the bound", func(s *provenanceState) {
			s.GapFrom, s.GapThrough, s.From, s.Through, s.Runs = 0, 0, 0, 1000, nil
			for i := range MaxProvenanceRuns + 1 {
				s.Runs = append(s.Runs, provenanceRun{N: uint64(i + 1), Bytes: 1, From: int64(i), Through: int64(i + 1),
					Pushes: 1})
			}
			s.Next = uint64(MaxProvenanceRuns + 2)
		}, int64(MaxProvenanceRuns + 1)},
	} {
		s := valid()
		tc.change(s)
		if err := s.check(tc.bytes); err == nil {
			t.Errorf("a record with %s passed", tc.name)
		}
	}
}
