package code_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// TIP CHECKS RUN ON A PERSON'S DIRECT LANDING (ADR-0068): a person's own push
// onto a protected branch whose gate requires a check lands with no check
// having measured it, so it lies above the branch's green row — and the
// home's `checks_due` listing names the repository and, on its first page,
// the branch's tip work: no green yet, the check its gate requires, and the
// push's landing, from main's A to ada's Y, ending its record — what the home
// dispatches the tip's checks from. Once unit failed there and the bisect's
// broken record names the landing, the node that landed the record announces
// ONE code_target_broken naming ada's push and unit, and the branch owes its
// home nothing more. The same push onto a branch whose gate requires no check
// is green as it lands, and owes its home nothing.
//
// Mutation: answer no tip work on a repository's first page (dueCheckWork
// skips dueTips) — ada's landing is never checked, red; list no repository
// for its tips alone — the backstop never reaches it, red; or announce no
// broken landing, red.
func TestTipChecksRunOnAPersonsDirectLanding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		gate *code.MergeGate
		owed bool
	}{
		{"a gate requiring a check", queueGate(), true},
		{"a gate requiring no check", mergeGate(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			n.chart = testChart()
			c, stream := mergeStream(t, n)
			// NO CHANGE on the repository, so nothing but its tip lists it.
			s := suiteSettings()
			s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
			s.Protections[0].DirectPush, s.Protections[0].Gate = code.DirectPersons, tc.gate
			repo := n.ensure("web", s, t1).RepoID
			n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A"))))
			got, err := c.Propose(t.Context(), proposal(repo, update(mainRef, oidOf("A"), oidOf("Y"))))
			if err != nil || len(got.Answer.Commands) != 1 || got.Answer.Commands[0].Result != code.ResultOK {
				t.Fatalf("ada's push answered %+v (%v), want it landed", got.Answer, err)
			}
			repos, tips := tipsDue(t, n, repo)
			if !tc.owed {
				if len(tips) != 0 || slices.Contains(repos, repo) {
					t.Fatalf("a branch green as it landed owes tip work %+v (listed: %v), want none", tips, repos)
				}
				return
			}
			if !slices.Contains(repos, repo) {
				t.Fatalf("the due check work lists %v, want web for main's tip", repos)
			}
			if len(tips) != 1 {
				t.Fatalf("web owes tip work %+v, want main's alone", tips)
			}
			tip := tips[0]
			newest := tip.Newest()
			switch {
			case tip.TargetRef != mainRef || tip.Green != nil || !slices.Equal(tip.Required, []string{"unit"}):
				t.Fatalf("main's tip work is %+v, want main with no green row requiring unit", tip)
			case tip.More || newest.OID != oidOf("Y") || newest.Base != oidOf("A") || !newest.Last ||
				newest.Broken || len(newest.Checks) != 0:
				t.Fatalf("main's newest landing reads %+v, want ada's push from A to Y, its record's last, "+
					"unchecked", newest)
			case tip.Base() != tip.Landings[0].Base:
				t.Fatalf("main's tip checks are defined at %s, want its first landing's base", tip.Base())
			}

			// ITS BISECT NAMES IT: unit failed on Y, so the landing is
			// recorded broken, announced once, and owes its home nothing
			// more until another lands.
			n.checked(repo, oidOf("Y"), mainRef, "unit", 1, code.CheckStateFailed)
			res, err := c.ProposeQueue(t.Context(), queueRecord(func(p *code.QueueProposal) {
				p.Broken = &code.Broken{RepoID: repo, TargetRef: mainRef, Position: newest.Position,
					Ordinal: newest.Ordinal, OID: oidOf("Y"), Checks: []string{"unit"}}
			}))
			requireRecorded(t, res, err)
			waitAnnouncedOf(t, stream, "code_target_broken", 1)
			ev := stream.of("code_target_broken")[0]
			broken, ok := ev.Data.(*types.CodeTargetBroken)
			if !ok || broken.RepoName != "web" || broken.Target != mainRef || broken.Commit != oidOf("Y") ||
				broken.Kind != string(code.LandedPush) || broken.ChangeID != "" || broken.LandedBy != "ada" ||
				!slices.Equal(broken.Checks, []string{"unit"}) {
				t.Fatalf("the broken landing announced %+v, want ada's push onto main broken by unit", ev.Data)
			}
			if repos, tips := tipsDue(t, n, repo); len(tips) != 0 || slices.Contains(repos, repo) {
				t.Fatalf("a branch whose only landing is broken owes tip work %+v (listed: %v), want none",
					tips, repos)
			}
		})
	}
}

// tipsDue is the repositories n's rows list as owing check work, and repo's
// tip work, as its home reads them.
func tipsDue(t *testing.T, n *node, repo string) ([]string, []code.TipDue) {
	t.Helper()
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	repos, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadChecksDue,
		ChecksDue: &code.ChecksDueQuery{RefreshBefore: harnessAt}, Fresh: fresh})
	if err != nil {
		t.Fatalf("the due repositories: %v", err)
	}
	work, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadChecksDue,
		ChecksDue: &code.ChecksDueQuery{RepoID: repo, RefreshBefore: harnessAt}, Fresh: fresh})
	if err != nil {
		t.Fatalf("the due work of %s: %v", repo, err)
	}
	return repos.DueRepos, work.DueTips
}

// A TIP WINDOW KEEPS ITS NEWEST, AND CALLS A GAP ONLY A GAP (ADR-0068): a
// target's tip work reads at most its window's bound of landings above its
// green — two here, where the shipped bound is MaxTipLandings — and then its
// newest, the tip a dispatch reaches. Three of ada's pushes onto main are one
// more than the window holds, and that one is the newest: it follows the
// window with no landing between, so the work reads all three, oldest first,
// with no gap, and a bisect may name the newest as it would any landing. A
// fourth push leaves one landing unread between the window and the newest:
// the work reads the oldest two and the newest, ending its record, and says
// More, which is what keeps a bisect from naming a tip across that gap.
//
// Mutation: say More whenever the window is cut — the three read with a gap,
// red; or drop the newest past the window — the fourth push never
// dispatched, red.
func TestATipWindowKeepsItsNewestAndCallsAGapOnlyAGap(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	code.SetTipWindow(n.reader, 2)
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = code.DirectPersons, queueGate()
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A"))))
	land := func(from, to string) {
		t.Helper()
		got, err := c.Propose(t.Context(), proposal(repo, update(mainRef, oidOf(from), oidOf(to))))
		if err != nil || len(got.Answer.Commands) != 1 || got.Answer.Commands[0].Result != code.ResultOK {
			t.Fatalf("ada's push from %s to %s answered %+v (%v), want it landed", from, to, got.Answer, err)
		}
	}
	// read is main's tip work: whether it says More, and the commit of each
	// landing it read, oldest first, each base the commit before it.
	read := func() (bool, []string) {
		t.Helper()
		_, tips := tipsDue(t, n, repo)
		if len(tips) != 1 || tips[0].TargetRef != mainRef {
			t.Fatalf("web owes tip work %+v, want main's alone", tips)
		}
		if newest := tips[0].Newest(); !newest.Last {
			t.Fatalf("main's newest landing %+v does not end its record", newest)
		}
		var commits []string
		for _, l := range tips[0].Landings {
			commits = append(commits, l.OID)
		}
		return tips[0].More, commits
	}

	land("A", "B")
	land("B", "C")
	land("C", "D")
	if more, commits := read(); more || !slices.Equal(commits, []string{oidOf("B"), oidOf("C"), oidOf("D")}) {
		t.Fatalf("one landing past a window of two reads %v (more: %t), want B, C and D with no gap", commits,
			more)
	}
	land("D", "E")
	if more, commits := read(); !more || !slices.Equal(commits, []string{oidOf("B"), oidOf("C"), oidOf("E")}) {
		t.Fatalf("two landings past a window of two read %v (more: %t), want B and C, then E past a gap",
			commits, more)
	}
}
