package home_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// landedAt is a landing of its own record at seq, from base to commit, ending
// that record, holding checks.
func landedAt(seq uint64, base, commit string, checks ...gitlet.TipCheck) gitlet.TipLanding {
	return gitlet.TipLanding{Position: statelog.Position{Stream: topics.CodeLogStream, Seq: seq}, Ordinal: 1,
		Base: base, OID: commit, Last: true, Checks: checks}
}

// ran is attempt 1 of name, ended in state.
func ran(name string, state gitlet.CheckState) gitlet.TipCheck {
	return gitlet.TipCheck{Name: name, Attempt: 1, State: state}
}

// tipTurn runs one turn of e's check work and answers every check write and
// every committer record it asked, and every commit whose checks file it read.
func tipTurn(t *testing.T, e *checkEstate) ([]gitlet.ChecksRequest, []gitlet.QueueProposal, []string) {
	t.Helper()
	e.mu.Lock()
	e.writes, e.records, e.read = nil, nil, nil
	e.mu.Unlock()
	if err := home.CheckWork(t.Context(), checkLoop(t, e), repoA, statelog.Position{}); err != nil {
		t.Fatalf("the check work: %v", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.writes), slices.Clone(e.records), slices.Clone(e.read)
}

// A TIP CHECK IS DEFINED AT ITS LANDINGS' BASE (ADR-0098, M5-D15): above main's
// green G lie X, which rewrites the checks file so unit runs `true`, and T on it.
// The tip work reads the file at G alone, never at a landing: unit is queued on T
// alone (coalesced), a tip check under G's definition and file, based at G. Once
// unit passed on T, main's green is asked onto T at the record that landed it.
// Where G's file defines no unit, unit is errored on T, whatever T's file says.
//
// Mutation: read the checks file at the tip (X's `true`, the landing judging
// itself), red; or refuse a tip's attempt errored at its dispatch
// (gitlet.CheckQueued.Validate), red.
func TestATipCheckIsDefinedAtItsLandingsBase(t *testing.T) {
	t.Parallel()
	green, rewrite, tip := oid("1"), oid("2"), oid("3")
	e := newCheckEstate()
	e.files[green] = checksFile("unit")
	rogue := strings.Replace(checksFile("unit"), "make unit", "true", 1)
	e.files[rewrite], e.files[tip] = rogue, rogue
	main := gitlet.TipDue{TargetRef: "refs/heads/main", Required: []string{"unit"},
		Green: &gitlet.GreenRow{RepoID: repoA, TargetRef: "refs/heads/main", CommitOID: green,
			Position: statelog.Position{Stream: topics.CodeLogStream, Seq: 9}},
		Landings: []gitlet.TipLanding{landedAt(10, green, rewrite), landedAt(11, rewrite, tip)}}
	e.tips = []gitlet.TipDue{main}

	writes, records, read := tipTurn(t, e)
	queued := queuedOf(writes)
	unit, ok := queued["unit"]
	switch {
	case !slices.Equal(read, []string{green}):
		t.Fatalf("the tip work read the checks files of %v, want the green's alone", read)
	case len(writes) != 1 || !ok || len(records) != 0:
		t.Fatalf("the tip work asked %d check writes (%v) and %d records, want unit queued once", len(writes),
			queued, len(records))
	case unit.OID != tip || unit.Purpose != gitlet.PurposeTip || unit.Target != "refs/heads/main" ||
		unit.Attempt != 1 || !unit.Required || unit.Reason != gitlet.QueueDispatch:
		t.Fatalf("unit is queued as %+v, want attempt 1 of main's tip, a required tip check", unit)
	case unit.Def == nil || unit.Def.Run != "make unit" || unit.DefsOID != blobOf(e.files[green]) ||
		unit.Base != green:
		t.Fatalf("unit runs %+v under file %s based at %s, want the green's `make unit`, its file and the green",
			unit.Def, unit.DefsOID, unit.Base)
	}

	main.Landings[1].Checks = []gitlet.TipCheck{ran("unit", gitlet.CheckStatePassed)}
	e.tips = []gitlet.TipDue{main}
	writes, records, _ = tipTurn(t, e)
	if len(writes) != 0 || len(records) != 1 || records[0].Green == nil {
		t.Fatalf("a passed tip asked %d check writes and records %+v, want one green", len(writes), records)
	}
	if g := records[0].Green; g.OID != tip || g.Through != main.Landings[1].Position ||
		g.TargetRef != "refs/heads/main" || g.RepoID != repoA {
		t.Fatalf("the green asked is %+v, want main onto its tip at the record that landed it", g)
	}

	// A BASE DEFINING NO SUCH CHECK errors it on the tip, saying so, rather
	// than leave the gate waiting on a check nobody can run — even where the
	// tip's own file defines it.
	e.files[green] = checksFile("lint")
	main.Landings[1].Checks = nil
	e.tips = []gitlet.TipDue{main}
	writes, _, _ = tipTurn(t, e)
	queued = queuedOf(writes)
	if unit := queued["unit"]; len(writes) != 1 || unit.Def != nil || unit.Purpose != gitlet.PurposeTip ||
		!strings.Contains(unit.Invalid, "defines no check unit") {
		t.Fatalf("a base defining no unit queued %+v, want unit errored on the tip for want of a definition",
			queued)
	}

	// A BRANCH ITS FIRST LANDING CREATED has no base file: unit is errored on the
	// tip in a person's words, naming the branch and what happened to it, never
	// the landing rows' "first landing above its green".
	created := gitlet.TipDue{TargetRef: "refs/heads/main", Required: []string{"unit"},
		Landings: []gitlet.TipLanding{landedAt(10, gitlet.ZeroOID, tip)}}
	e.tips = []gitlet.TipDue{created}
	writes, _, _ = tipTurn(t, e)
	queued = queuedOf(writes)
	if unit := queued["unit"]; len(writes) != 1 || unit.Def != nil ||
		!strings.HasPrefix(unit.Invalid, "main has had no commit whose required checks passed since a push or a "+
			"merge created it") || strings.Contains(unit.Invalid, "landing above") {
		t.Fatalf("a branch its first landing created queued %+v, want unit errored saying a push or a merge "+
			"created main", queued)
	}
}

// A RED TIP IS BISECTED OVER LANDING ROWS: four landings, each its own record, lie
// above main's green; on the tip unit failed, lint passed, and e2e, required but
// undefined by the green's checks file, errored, holding nothing back. Each turn
// reads only the rows and probes unit alone, a bisect under the green's file, midway
// between the last landing unit passed on and the first it failed on: the second,
// then the first. With unit passed on the first and failed on the second, the SECOND
// is recorded broken for unit; a broken landing is recorded nothing again, and no
// turn asks main's green past a red tip.
//
// Mutation: probe the landing just below the tip each round (a linear walk, the
// third probed first), red; or wait on the check the base cannot define, red.
func TestARedTipIsBisectedOverLandingRows(t *testing.T) {
	t.Parallel()
	green := oid("9") // never oid("0"): that is gitlet.ZeroOID, a branch with no base file
	commits := []string{oid("1"), oid("2"), oid("3"), oid("4")}
	e := newCheckEstate()
	e.files[green] = checksFile("unit", "lint")
	main := gitlet.TipDue{TargetRef: "refs/heads/main", Required: []string{"e2e", "lint", "unit"},
		Green: &gitlet.GreenRow{RepoID: repoA, TargetRef: "refs/heads/main", CommitOID: green,
			Position: statelog.Position{Stream: topics.CodeLogStream, Seq: 9}}}
	base := green
	for i, c := range commits {
		main.Landings = append(main.Landings, landedAt(uint64(10+i), base, c))
		base = c
	}
	main.Landings[3].Checks = []gitlet.TipCheck{
		{Name: "e2e", Attempt: 1, State: gitlet.CheckStateErrored, Reason: gitlet.CheckReasonDefsInvalid},
		ran("lint", gitlet.CheckStatePassed), ran("unit", gitlet.CheckStateFailed)}

	// probed is the one landing a turn queues unit on, as a bisect probe of
	// unit alone.
	probed := func(round string) string {
		t.Helper()
		e.tips = []gitlet.TipDue{main}
		writes, records, _ := tipTurn(t, e)
		queued := queuedOf(writes)
		unit, ok := queued["unit"]
		if len(writes) != 1 || !ok || len(records) != 0 || unit.Purpose != gitlet.PurposeBisect ||
			unit.Def == nil || unit.Def.Run != "make unit" || unit.DefsOID != blobOf(e.files[green]) {
			t.Fatalf("%s: the bisect asked %d check writes %v and records %+v, want unit alone probed under the "+
				"green's file", round, len(writes), queued, records)
		}
		return unit.OID
	}
	if got := probed("round 1"); got != commits[1] {
		t.Fatalf("round 1 probed %s, want the second landing, midway", got[:7])
	}
	main.Landings[1].Checks = []gitlet.TipCheck{ran("unit", gitlet.CheckStateFailed)}
	if got := probed("round 2"); got != commits[0] {
		t.Fatalf("round 2 probed %s, want the first landing, midway below the second", got[:7])
	}
	main.Landings[0].Checks = []gitlet.TipCheck{ran("unit", gitlet.CheckStatePassed)}
	e.tips = []gitlet.TipDue{main}
	writes, records, _ := tipTurn(t, e)
	if len(writes) != 0 || len(records) != 1 || records[0].Broken == nil {
		t.Fatalf("a bisect down to adjacent landings asked %d check writes and records %+v, want one broken",
			len(writes), records)
	}
	if b := records[0].Broken; b.OID != commits[1] || b.Position != main.Landings[1].Position || b.Ordinal != 1 ||
		!slices.Equal(b.Checks, []string{"unit"}) || b.TargetRef != "refs/heads/main" {
		t.Fatalf("the broken asked is %+v, want the second landing broken by unit", b)
	}

	main.Landings[1].Broken = true
	e.tips = []gitlet.TipDue{main}
	if writes, records, _ := tipTurn(t, e); len(writes) != 0 || len(records) != 0 {
		t.Fatalf("a culprit already named asked %d check writes and records %+v, want nothing", len(writes),
			records)
	}
}

// A TIP WAITS FOR THE ROUND IN FLIGHT (ADR-0098): main's tip round runs on X, and
// T, holding no attempt, has landed on it since. The turn dispatches nothing, so a
// target landed on faster than its checks run keeps one round in flight, not one a
// landing. With no round in flight, the same rows dispatch unit on T.
//
// Mutation: dispatch the newest whatever is in flight (unit queued on T beside X's
// round), red.
func TestATipWaitsForTheRoundInFlight(t *testing.T) {
	t.Parallel()
	green, earlier, tip := oid("1"), oid("2"), oid("3")
	e := newCheckEstate()
	e.files[green] = checksFile("unit")
	main := gitlet.TipDue{TargetRef: "refs/heads/main", Required: []string{"unit"},
		Green: &gitlet.GreenRow{RepoID: repoA, TargetRef: "refs/heads/main", CommitOID: green,
			Position: statelog.Position{Stream: topics.CodeLogStream, Seq: 9}},
		Landings: []gitlet.TipLanding{landedAt(10, green, earlier, ran("unit", gitlet.CheckStateRunning)),
			landedAt(11, earlier, tip)},
		InFlight: earlier}
	e.tips = []gitlet.TipDue{main}
	if writes, records, _ := tipTurn(t, e); len(writes) != 0 || len(records) != 0 {
		t.Fatalf("a tip landed while a round runs asked %d check writes (%v) and records %+v, want it to wait",
			len(writes), queuedOf(writes), records)
	}

	main.Landings[0].Checks, main.InFlight = []gitlet.TipCheck{ran("unit", gitlet.CheckStatePassed)}, ""
	e.tips = []gitlet.TipDue{main}
	writes, records, _ := tipTurn(t, e)
	if unit, ok := queuedOf(writes)["unit"]; len(writes) != 1 || !ok || len(records) != 0 || unit.OID != tip ||
		unit.Purpose != gitlet.PurposeTip {
		t.Fatalf("with no round in flight the tip work asked %d check writes (%v) and records %+v, want unit "+
			"queued on the tip", len(writes), queuedOf(writes), records)
	}
}

// EVERY TARGET'S TIP IS WORKED WITHIN ONE TURN: main's and release's windows each
// fill a page of the check work. The one turn reads on past main's page and queues
// unit on both tips: a target past the first page is never left to a later pass,
// which would read the same first page again.
//
// Mutation: stop the turn at the page the heads' cursor ends (release never read), red.
func TestEveryTargetsTipIsWorkedWithinOneTurn(t *testing.T) {
	t.Parallel()
	green := oid("1")
	e := newCheckEstate()
	e.files[green] = checksFile("unit")
	e.tipsPerPage = 1
	for i, target := range []string{"refs/heads/main", "refs/heads/release"} {
		e.tips = append(e.tips, gitlet.TipDue{TargetRef: target, Required: []string{"unit"},
			Green: &gitlet.GreenRow{RepoID: repoA, TargetRef: target, CommitOID: green,
				Position: statelog.Position{Stream: topics.CodeLogStream, Seq: 9}},
			Landings: []gitlet.TipLanding{landedAt(uint64(10+i), green, oid(string(rune('2'+i))))}})
	}
	writes, _, _ := tipTurn(t, e)
	var tips []string
	for _, w := range writes {
		if q := w.Queued; q != nil && q.Name == "unit" && q.Purpose == gitlet.PurposeTip {
			tips = append(tips, q.Target)
		}
	}
	if !slices.Equal(tips, []string{"refs/heads/main", "refs/heads/release"}) {
		t.Fatalf("one turn queued unit on the tips of %v, want main's and release's", tips)
	}
}

// A CULPRIT PAST THE WINDOW IS WARNED OF ONCE A TIP: main's tip failed unit and the
// landing before it in the window passed it, but the window was read past a gap, so
// no culprit can be named. The gap stands while nothing moves and every check record
// asks a turn, so two turns warn code_tip_culprit_past_the_window ONCE; a new tip
// past the same gap is warned of afresh.
//
// Mutation: warn on every turn that finds the gap, or forget the tip warned of
// whatever the turn found: two warnings for one tip, red.
func TestACulpritPastTheWindowIsWarnedOfOnceATip(t *testing.T) {
	t.Parallel()
	green := oid("1")
	e := newCheckEstate()
	e.files[green] = checksFile("unit")
	e.logs = &lines{}
	main := gitlet.TipDue{TargetRef: "refs/heads/main", Required: []string{"unit"}, More: true,
		Green: &gitlet.GreenRow{RepoID: repoA, TargetRef: "refs/heads/main", CommitOID: green,
			Position: statelog.Position{Stream: topics.CodeLogStream, Seq: 9}},
		Landings: []gitlet.TipLanding{landedAt(10, green, oid("2"), ran("unit", gitlet.CheckStatePassed)),
			landedAt(400, oid("7"), oid("3"), ran("unit", gitlet.CheckStateFailed))}}
	e.tips = []gitlet.TipDue{main}
	l := checkLoop(t, e)
	warned := func(turns int) int {
		t.Helper()
		for range turns {
			if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
				t.Fatalf("the check work: %v", err)
			}
		}
		return strings.Count(e.logs.String(), "code_tip_culprit_past_the_window")
	}
	if n := warned(2); n != 1 {
		t.Fatalf("two turns past the same gap warned %d times, want once", n)
	}
	main.Landings[1] = landedAt(401, oid("3"), oid("4"), ran("unit", gitlet.CheckStateFailed))
	e.mu.Lock()
	e.tips = []gitlet.TipDue{main}
	e.mu.Unlock()
	if n := warned(1); n != 2 {
		t.Fatalf("a new tip past the gap brought the warnings to %d, want a second", n)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.writes) != 0 || len(e.records) != 0 {
		t.Fatalf("a culprit past the window asked %d check writes and records %+v, want nothing",
			len(e.writes), e.records)
	}
}
