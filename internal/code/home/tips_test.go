package home_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// landedAt is a landing of its own record at seq, from base to commit, ending
// that record, holding checks.
func landedAt(seq uint64, base, commit string, checks ...code.TipCheck) code.TipLanding {
	return code.TipLanding{Position: statelog.Position{Stream: topics.CodeLogStream, Seq: seq}, Ordinal: 1,
		Base: base, OID: commit, Last: true, Checks: checks}
}

// ran is attempt 1 of name, ended in state.
func ran(name string, state code.CheckState) code.TipCheck {
	return code.TipCheck{Name: name, Attempt: 1, State: state}
}

// tipTurn runs one turn of e's check work and answers every check write and
// every committer record it asked, and every commit whose checks file it read.
func tipTurn(t *testing.T, e *checkEstate) ([]code.ChecksRequest, []code.QueueProposal, []string) {
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

// A TIP CHECK IS DEFINED AT ITS LANDINGS' BASE (ADR-0068, M5-D15): main's
// green is at G; two landings lie above it — X, which rewrites the checks file
// so unit runs `true`, then T on top of it. The tip work reads the checks file
// at G, the base of the first landing above the green, and never at a
// landing: unit is queued on T alone (coalesced — X, no longer the newest, is
// never dispatched), as a tip check, under G's definition and G's file, its
// diff based at G. Once unit passed on T, main's green row is asked onto T, at
// the record that landed it. And where G's file defines no unit, unit is
// errored on T saying so — T's own file defining it changes nothing.
//
// Mutation: read the checks file at the tip — unit is queued under X's
// `true`, the landing judging itself, red; or refuse a tip's attempt errored
// at its dispatch (code.CheckQueued.Validate) — red.
func TestATipCheckIsDefinedAtItsLandingsBase(t *testing.T) {
	t.Parallel()
	green, rewrite, tip := oid("1"), oid("2"), oid("3")
	e := newCheckEstate()
	e.files[green] = checksFile("unit")
	rogue := strings.Replace(checksFile("unit"), "make unit", "true", 1)
	e.files[rewrite], e.files[tip] = rogue, rogue
	main := code.TipDue{TargetRef: "refs/heads/main", Required: []string{"unit"},
		Green: &code.GreenRow{RepoID: repoA, TargetRef: "refs/heads/main", CommitOID: green,
			Position: statelog.Position{Stream: topics.CodeLogStream, Seq: 9}},
		Landings: []code.TipLanding{landedAt(10, green, rewrite), landedAt(11, rewrite, tip)}}
	e.tips = []code.TipDue{main}

	writes, records, read := tipTurn(t, e)
	queued := queuedOf(writes)
	unit, ok := queued["unit"]
	switch {
	case !slices.Equal(read, []string{green}):
		t.Fatalf("the tip work read the checks files of %v, want the green's alone", read)
	case len(writes) != 1 || !ok || len(records) != 0:
		t.Fatalf("the tip work asked %d check writes (%v) and %d records, want unit queued once", len(writes),
			queued, len(records))
	case unit.OID != tip || unit.Purpose != code.PurposeTip || unit.Target != "refs/heads/main" ||
		unit.Attempt != 1 || !unit.Required || unit.Reason != code.QueueDispatch:
		t.Fatalf("unit is queued as %+v, want attempt 1 of main's tip, a required tip check", unit)
	case unit.Def == nil || unit.Def.Run != "make unit" || unit.DefsOID != blobOf(e.files[green]) ||
		unit.Base != green:
		t.Fatalf("unit runs %+v under file %s based at %s, want the green's `make unit`, its file and the green",
			unit.Def, unit.DefsOID, unit.Base)
	}

	main.Landings[1].Checks = []code.TipCheck{ran("unit", code.CheckStatePassed)}
	e.tips = []code.TipDue{main}
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
	e.tips = []code.TipDue{main}
	writes, _, _ = tipTurn(t, e)
	queued = queuedOf(writes)
	if unit := queued["unit"]; len(writes) != 1 || unit.Def != nil || unit.Purpose != code.PurposeTip ||
		!strings.Contains(unit.Invalid, "defines no check unit") {
		t.Fatalf("a base defining no unit queued %+v, want unit errored on the tip for want of a definition",
			queued)
	}
}

// A RED TIP IS BISECTED OVER LANDING ROWS: four landings lie above main's
// green, each its own record, and unit failed on the tip while lint passed
// there — and e2e, which main's gate requires and the green's checks file
// does not define, errored on the tip for want of a definition, which no turn
// can change while the green stays where it is, so it holds nothing back.
// Each turn reads only the rows: it probes the landing midway between
// the last one unit passed on (the green, at first) and the first it failed
// on — the second landing, then the first — queuing unit alone, as a bisect
// under the green's file; with unit passed on the first and failed on the
// second, it records the SECOND landing broken for unit. A landing already
// broken is recorded nothing again, and no turn ever asks main's green row
// past a red tip.
//
// Mutation: probe the landing just below the tip each round — a linear walk
// back, the third landing probed first, red; or wait on the check the base
// cannot define rather than bisect beside it — nothing probed, red.
func TestARedTipIsBisectedOverLandingRows(t *testing.T) {
	t.Parallel()
	green := oid("9") // never oid("0"): that is code.ZeroOID, a branch with no base file
	commits := []string{oid("1"), oid("2"), oid("3"), oid("4")}
	e := newCheckEstate()
	e.files[green] = checksFile("unit", "lint")
	main := code.TipDue{TargetRef: "refs/heads/main", Required: []string{"e2e", "lint", "unit"},
		Green: &code.GreenRow{RepoID: repoA, TargetRef: "refs/heads/main", CommitOID: green,
			Position: statelog.Position{Stream: topics.CodeLogStream, Seq: 9}}}
	base := green
	for i, c := range commits {
		main.Landings = append(main.Landings, landedAt(uint64(10+i), base, c))
		base = c
	}
	main.Landings[3].Checks = []code.TipCheck{
		{Name: "e2e", Attempt: 1, State: code.CheckStateErrored, Reason: code.CheckReasonDefsInvalid},
		ran("lint", code.CheckStatePassed), ran("unit", code.CheckStateFailed)}

	// probed is the one landing a turn queues unit on, as a bisect probe of
	// unit alone.
	probed := func(round string) string {
		t.Helper()
		e.tips = []code.TipDue{main}
		writes, records, _ := tipTurn(t, e)
		queued := queuedOf(writes)
		unit, ok := queued["unit"]
		if len(writes) != 1 || !ok || len(records) != 0 || unit.Purpose != code.PurposeBisect ||
			unit.Def == nil || unit.Def.Run != "make unit" || unit.DefsOID != blobOf(e.files[green]) {
			t.Fatalf("%s: the bisect asked %d check writes %v and records %+v, want unit alone probed under the "+
				"green's file", round, len(writes), queued, records)
		}
		return unit.OID
	}
	if got := probed("round 1"); got != commits[1] {
		t.Fatalf("round 1 probed %s, want the second landing, midway", got[:7])
	}
	main.Landings[1].Checks = []code.TipCheck{ran("unit", code.CheckStateFailed)}
	if got := probed("round 2"); got != commits[0] {
		t.Fatalf("round 2 probed %s, want the first landing, midway below the second", got[:7])
	}
	main.Landings[0].Checks = []code.TipCheck{ran("unit", code.CheckStatePassed)}
	e.tips = []code.TipDue{main}
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
	e.tips = []code.TipDue{main}
	if writes, records, _ := tipTurn(t, e); len(writes) != 0 || len(records) != 0 {
		t.Fatalf("a culprit already named asked %d check writes and records %+v, want nothing", len(writes),
			records)
	}
}
