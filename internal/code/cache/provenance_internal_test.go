package cache

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"runtime"
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
	if _, err := mergeRuns(&buf, []runFile{a, b}, base-1); err != nil {
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
	h, err := mergeRuns(&buf, []runFile{a, b}, floor)
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

// A MERGE IS THE RUN ITS KEPT ROWS BUILD, byte for byte: of any number of
// adjacent runs, over histories whose pushes share refs across the runs and
// move refs of their own, land several pushes at one position — two of them
// moving one ref, which their chain orders — bring a commit back that an
// earlier push introduced, and file their refs under a handful of keys, so
// every key is shared by many names; at any floor, from below the first run
// to inside the last. The merge streams one key at a time, and the run built
// whole from the rows above the floor is what it must write.
//
// Mutation: keep a ref whose every move is at or below the floor (refGroups
// keeping every name) — the merged run names refs its rows do not move.
func TestAMergeIsTheRunItsKeptRowsBuild(t *testing.T) {
	t.Parallel()
	few := func(ref string) [runKeyBytes]byte {
		sum := sha256.Sum256([]byte(ref))
		return [runKeyBytes]byte{sum[0] % 4}
	}
	base := statelog.Position{Stream: topics.CodeLogStream, Generation: 2, Seq: 1}.Packed()
	for seed := range uint64(40) {
		rng := rand.New(rand.NewPCG(seed, 7))
		rows, records := historyOf(rng, base, 120)
		for _, inputs := range []int{2, 3, 5, 8} {
			// Cut the history at record boundaries into inputs runs.
			cuts := []int{0}
			for _, at := range rng.Perm(len(records) - 1)[:inputs-1] {
				cuts = append(cuts, at+1)
			}
			cuts = append(cuts, len(records))
			slices.Sort(cuts)
			var runs []runFile
			from := base - 1
			for k := range inputs {
				lo, hi := records[cuts[k]], len(rows)
				if cuts[k+1] < len(records) {
					hi = records[cuts[k+1]]
				}
				through := rows[hi-1].Position.Packed()
				runs = append(runs, runKeyed(t, from, through, rows[lo:hi], few))
				from = through
			}
			floor := base - 1 + int64(rng.IntN(int(from-base+1)))
			var kept []code.IntroducedRow
			for _, r := range rows {
				if r.Position.Packed() > floor {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				continue
			}
			var got bytes.Buffer
			if _, err := mergeRuns(&got, runs, floor); err != nil {
				t.Fatalf("seed %d, %d runs, floor %d: %v", seed, inputs, floor, err)
			}
			built, err := buildRunKeyed(max(base-1, floor), from, kept, few)
			if err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if _, err := built.write(&want); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				g, _ := openRunBytes(got.Bytes())
				t.Fatalf("seed %d: the merge of %d runs above %d is %d bytes (%+v), and the run its %d kept rows "+
					"build is %d (%+v)", seed, inputs, floor, got.Len(), g.h, len(kept), want.Len(), built.header())
			}
		}
	}
}

// historyOf is a synthetic history of n pushes from base: records of one to
// three pushes at one position, each push moving one or two refs — one of a
// few shared by the whole history, or one of its own — two pushes of a
// record now and then moving one ref one after the other, and now and then
// introducing a commit an earlier push introduced. It answers the rows in
// (position, push id) order, and the index of each record's first row.
func historyOf(rng *rand.Rand, base int64, n int) ([]code.IntroducedRow, []int) {
	value := map[string]string{}
	oid := 0
	fresh := func() string {
		oid++
		return oidOf(1_000_000 + oid)
	}
	move := func(ref string, forced bool) code.RefMove {
		old := value[ref]
		if old == "" {
			old = code.ZeroOID
		}
		m := code.RefMove{Ref: ref, Old: old, New: fresh(), Forced: forced && old != code.ZeroOID}
		if rng.IntN(20) == 0 && old != code.ZeroOID {
			m.New = code.ZeroOID // a deletion
		}
		if m.New == code.ZeroOID {
			delete(value, ref)
		} else {
			value[ref] = m.New
		}
		return m
	}
	var rows []code.IntroducedRow
	var records []int
	var introduced []string
	position := base
	for len(rows) < n {
		records = append(records, len(rows))
		size := 1 + rng.IntN(3)
		var group []code.IntroducedRow
		chained := ""
		if size > 1 && rng.IntN(3) == 0 {
			chained = fmt.Sprintf("refs/heads/shared-%d", rng.IntN(3))
		}
		for range size {
			i := len(rows) + len(group)
			row := synthetic(base, i, "")
			row.Position = statelog.Unpack(topics.CodeLogStream, position)
			row.Moves = nil
			switch {
			case chained != "":
				row.Moves = append(row.Moves, move(chained, rng.IntN(4) == 0))
			case rng.IntN(2) == 0:
				row.Moves = append(row.Moves, move(fmt.Sprintf("refs/heads/shared-%d", rng.IntN(3)), rng.IntN(4) == 0))
			default:
				row.Moves = append(row.Moves, move(fmt.Sprintf("refs/heads/own-%d", i), false))
			}
			if rng.IntN(3) == 0 {
				row.Moves = append(row.Moves, move(fmt.Sprintf("refs/tags/t-%d", i), false))
			}
			row.Introduced = []string{fresh()}
			if len(introduced) > 0 && rng.IntN(5) == 0 {
				row.Introduced = append(row.Introduced, introduced[rng.IntN(len(introduced))])
			}
			introduced = append(introduced, row.Introduced[0])
			row.Count = len(row.Introduced)
			group = append(group, row)
		}
		// A record's rows are read in push id order; the moves' chain says
		// which landed first.
		slices.SortFunc(group, func(a, b code.IntroducedRow) int { return strings.Compare(a.PushID, b.PushID) })
		rows = append(rows, group...)
		position += 1 + int64(rng.IntN(3))
	}
	return rows, records
}

// A MERGE HOLDS ONE KEY'S REFS, NOT A RUN'S. The refs of two runs — every
// name either holds, a repository whose every push moves a ref of its own
// holding as many as it has pushes — are merged a key at a time, so what a
// merge allocates does not grow with them: four times the refs cost the merge
// no more than a megabyte more. Held whole by name, as the first merge held
// them, the refs of 40 000 pushes cost some 20 MB, 519 bytes a ref.
//
// Not parallel: it reads what the process allocated across the merge, which
// a case running beside it would add to.
//
// Mutation: hold every input's names in maps across the merge (mergeRuns'
// first shape) — four times the refs allocate some 15 MB more.
func TestAMergeHoldsOneKeysRefsNotARunsRefs(t *testing.T) {
	alloc := func(pushes int) uint64 {
		t.Helper()
		base := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 1}.Packed()
		var runs []runFile
		for k := range 2 {
			rows := make([]code.IntroducedRow, 0, pushes)
			for i := k * pushes; i < (k+1)*pushes; i++ {
				rows = append(rows, synthetic(base, i, fmt.Sprintf("refs/heads/seat/%07d", i)))
			}
			runs = append(runs, runOf(t, base+int64(k*pushes)-1, base+int64((k+1)*pushes)-1, rows))
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, err := mergeRuns(io.Discard, runs, base-1); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := alloc(5_000), alloc(20_000)
	if large > small+1<<20 {
		t.Errorf("a merge of 40 000 refs allocated %d bytes, and of 10 000 %d: what a merge holds grows with "+
			"its refs", large, small)
	}
}

// THE MERGE POLICY KEEPS AN INDEX SMALL AND MERGES IT SELDOM. An index's runs
// merged as the backfill merges them, to quiescence after every run written,
// by mergeDue's rule alone, in three shapes: 100 000 folds of one push each —
// a repository whose every push is synced on its own, a run a fold; 100 000
// folds of one to twenty, a burst's; and a history read back below the index,
// 1 200 pages of 7 500 pushes, with 20 000 one-push folds above it. Merged a
// pair at a time whenever two neighbours were within a factor of two, the
// one-push folds merged once a push, four syncs and a state write each on
// every node holding the copy, and the bursts — runs of every size, each
// beside one it is not within a factor of — filled the index to its bound,
// where every fold's merge rewrote whatever pair was cheapest, some 200
// times a push. Eight balanced runs a merge, each shape merges at most once
// in four pushes, rewrites each push under 2·log₂ n times, and holds its
// index eight runs clear of MaxProvenanceRuns.
//
// Mutation: merge the cheapest eight runs whether or not they are balanced
// (mergeDue's balance test dropped) — a large run is rewritten for every few
// small ones, thousands of times a push.
func TestTheMergePolicyKeepsAnIndexSmallAndMergesItSeldom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		steps int
		// add answers the run step i adds, and whether it lands below the
		// index (a history page) rather than above (a fold).
		add func(rng *rand.Rand, i int) (int64, bool)
	}{
		{"a push a fold", 100_000, func(*rand.Rand, int) (int64, bool) { return 1, false }},
		{"bursts of up to twenty", 100_000, func(rng *rand.Rand, _ int) (int64, bool) {
			return 1 + rng.Int64N(20), false
		}},
		{"a history read below folds", 1_200 + 20_000, func(_ *rand.Rand, i int) (int64, bool) {
			if i < 1_200 {
				return 7_500, true
			}
			return 1, false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(1, 2))
			rec := &provenanceState{}
			var merges, written, pushes, most int64
			for i := range tc.steps {
				n, below := tc.add(rng, i)
				run := provenanceRun{N: uint64(i + 1), Pushes: n}
				if below {
					rec.Runs = append([]provenanceRun{run}, rec.Runs...)
				} else {
					rec.Runs = append(rec.Runs, run)
				}
				pushes += n
				for {
					lo, hi := mergeDue(rec, uint64(1)<<len(rec.Runs)-1)
					if hi == 0 {
						break
					}
					merged := provenanceRun{N: rec.Runs[lo].N}
					for _, r := range rec.Runs[lo:hi] {
						merged.Pushes += r.Pushes
					}
					rec.Runs = slices.Replace(rec.Runs, lo, hi, merged)
					merges++
					written += merged.Pushes
				}
				most = max(most, int64(len(rec.Runs)))
			}
			perPush := float64(merges) / float64(pushes)
			rewrites := float64(written) / float64(pushes)
			t.Logf("%d runs written of %d pushes: %d merges, %.3f a push; each push rewritten %.1f times; at "+
				"most %d runs, %d at the end", tc.steps, pushes, merges, perPush, rewrites, most, len(rec.Runs))
			if perPush > 0.25 {
				t.Errorf("%d pushes merged %d times, %.2f a push: want at most one in four", pushes, merges, perPush)
			}
			if bound := 2 * math.Log2(float64(pushes)); rewrites > bound {
				t.Errorf("each push was rewritten %.1f times on average, past 2·log₂ n = %.1f", rewrites, bound)
			}
			if most > MaxProvenanceRuns-ProvenanceMergeWidth {
				t.Errorf("the index held %d runs, within %d of MaxProvenanceRuns", most, ProvenanceMergeWidth)
			}
		})
	}
}

// THE BOUND UNMAPS THE RUN LET GO LONGEST AGO, AND NEVER ONE HELD. A mapping
// past the node's bound on mapped runs makes room by unmapping an idle one —
// the one its last reader let go before every other's — so the runs of the
// copies people are reading stay mapped; a mapping a reader holds is never
// taken from under it, so a bound every reader holds whole refuses the next
// mapping rather than exceed itself; and a run its index stops naming is
// unmapped at its last release, never kept idle for a reader that will not
// come.
//
// Mutation: evict from the idle list's head (the most recently let go) — the
// first run let go is still held, and the last is gone.
func TestTheBoundUnmapsTheRunLetGoLongestAgo(t *testing.T) {
	t.Parallel()
	rm := newRunMaps(2)
	mapped := func(n uint64) *mappedRun {
		t.Helper()
		if err := rm.reserve(); err != nil {
			t.Fatalf("reserving run %d's mapping under a bound with room: %v", n, err)
		}
		m := &mappedRun{n: n}
		if !rm.hold(m) {
			t.Fatalf("a fresh mapping of run %d could not be held", n)
		}
		return m
	}
	first, second := mapped(1), mapped(2)
	if rm.let(first) || rm.let(second) {
		t.Fatal("a run its index still names was unmapped at its last release, not kept for the next reader")
	}
	third := mapped(3)
	switch {
	case rm.hold(first):
		t.Error("past the bound, the run let go first is still mapped")
	case !rm.hold(second):
		t.Error("past the bound, the run let go last was unmapped before the one let go first")
	}
	if got := rm.count(); got != 2 {
		t.Errorf("the node holds %d runs mapped under a bound of 2", got)
	}
	// Both held now: the bound refuses a third rather than take one.
	if err := rm.reserve(); !errors.Is(err, errMapsFull) {
		t.Errorf("a mapping past a bound its readers hold whole = %v, want errMapsFull", err)
	}
	if rm.let(third) {
		t.Fatal("a run still named was unmapped at its last release")
	}
	if err := rm.reserve(); err != nil {
		t.Errorf("a mapping once a reader let one go = %v, want room", err)
	}
	if rm.hold(third) {
		t.Error("the idle run the new mapping made room with is still mapped")
	}
	// A run its index no longer names goes at its last release.
	if rm.forget(second) {
		t.Error("a held run was unmapped as its index stopped naming it")
	}
	if !rm.let(second) {
		t.Error("a run its index no longer names was kept idle at its last release")
	}
	if rm.hold(second) {
		t.Error("a run its index no longer names was held again")
	}
}

// EVERY PUSH PRINCIPAL AND WAY HAS A RUN CODE. A run names a push's way and
// its principal's kind by a code of one table each, so a kind or a way a later
// build adds to code's closed sets is a code here — or a run could not be
// written for a push of it, and its round's fold would leave the index a gap.
// Each code is distinct and none is zero, and a push of each round-trips — a
// seat's with the run behind it, a coding box's run and coding agent among
// it, each field at its bound and unlike the others, so a field written under
// another's tag, or a length past one byte's uvarint, reads back wrong.
//
// Mutations: drop the agent's code — a seat's push builds no run; write no
// work key into a run (pushPayload skipping tag 5) — a seat's push reads back
// without it; write no run id (pushPayload skipping tag 7) — a box's push
// reads back as one no coding run made.
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
			switch k {
			case code.PrincipalOperator:
				row.Principal.OperatorID = "pat:" + strings.Repeat("x", 300)
			case code.PrincipalAgent:
				row.Principal.Run = code.RunProvenance{
					TurnID:      strings.Repeat("t", code.MaxProvenanceBytes),
					WorkBackend: strings.Repeat("b", code.MaxProvenanceBytes),
					WorkKey:     strings.Repeat("k", code.MaxProvenanceBytes),
					Model:       strings.Repeat("m", code.MaxProvenanceBytes),
					RunID:       "0192f00d-0000-7000-8000-00000000c0de",
					CodingAgent: strings.Repeat("a", code.MaxProvenanceBytes),
				}
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
