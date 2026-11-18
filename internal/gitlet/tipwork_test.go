package gitlet_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// TIP CHECKS RUN ON A PERSON'S DIRECT LANDING (ADR-0098): a person's push onto
// a protected branch whose gate requires a check lies above its green row, and
// the home's `checks_due` listing names the repository and, on its first page,
// the branch's tip work: no green yet, the required check, and the push's
// landing from A to Y, ending its record. Once unit failed there and the
// bisect's broken record names the landing, the landing node announces ONE
// code_target_broken naming ada's push and unit, and the branch owes nothing
// more; nor does the same push under a gate requiring no check, green as it lands.
//
// Mutation: dueCheckWork skips dueTips (ada's landing never checked), list no
// repository for its tips alone (the backstop never reaches it), or announce
// no broken landing: red.
func TestTipChecksRunOnAPersonsDirectLanding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		gate *gitlet.MergeGate
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
			s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
			s.Protections[0].DirectPush, s.Protections[0].Gate = gitlet.DirectPersons, tc.gate
			repo := n.ensure("web", s, t1).RepoID
			n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A"))))
			got, err := c.Propose(t.Context(), proposal(repo, update(mainRef, oidOf("A"), oidOf("Y"))))
			if err != nil || len(got.Answer.Commands) != 1 || got.Answer.Commands[0].Result != gitlet.ResultOK {
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
			n.checked(repo, oidOf("Y"), mainRef, "unit", 1, gitlet.CheckStateFailed)
			res, err := c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
				p.Broken = &gitlet.Broken{RepoID: repo, TargetRef: mainRef, Position: newest.Position,
					Ordinal: newest.Ordinal, OID: oidOf("Y"), Checks: []string{"unit"}}
			}))
			requireRecorded(t, res, err)
			waitAnnouncedOf(t, stream, "code_target_broken", 1)
			ev := stream.of("code_target_broken")[0]
			broken, ok := ev.Data.(*types.CodeTargetBroken)
			if !ok || broken.RepoName != "web" || broken.Target != mainRef || broken.Commit != oidOf("Y") ||
				broken.Kind != string(gitlet.LandedPush) || broken.ChangeID != "" || broken.LandedBy != "ada" ||
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
func tipsDue(t *testing.T, n *node, repo string) ([]string, []gitlet.TipDue) {
	t.Helper()
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	repos, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChecksDue,
		ChecksDue: &gitlet.ChecksDueQuery{RefreshBefore: harnessAt}, Fresh: fresh})
	if err != nil {
		t.Fatalf("the due repositories: %v", err)
	}
	work, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChecksDue,
		ChecksDue: &gitlet.ChecksDueQuery{RepoID: repo, RefreshBefore: harnessAt}, Fresh: fresh})
	if err != nil {
		t.Fatalf("the due work of %s: %v", repo, err)
	}
	return repos.DueRepos, work.DueTips
}

// A TIP WINDOW KEEPS ITS NEWEST, AND CALLS A GAP ONLY A GAP (ADR-0098): tip
// work reads at most its window's bound of landings above the green (two here,
// MaxTipLandings shipped), then its newest, the tip a dispatch reaches. Three
// pushes are one past the window, and that one is the newest: all three read
// with no gap, so a bisect may name the newest. A fourth leaves one landing
// unread between: the oldest two and the newest read, the newest ending its
// record, and More set, which keeps a bisect from naming a tip across the gap.
//
// Mutation: say More whenever the window is cut (the three read with a gap),
// or drop the newest past the window (the fourth push never dispatched): red.
func TestATipWindowKeepsItsNewestAndCallsAGapOnlyAGap(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	gitlet.SetTipWindow(n.reader, 2)
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = gitlet.DirectPersons, queueGate()
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A"))))
	land := func(from, to string) {
		t.Helper()
		got, err := c.Propose(t.Context(), proposal(repo, update(mainRef, oidOf(from), oidOf(to))))
		if err != nil || len(got.Answer.Commands) != 1 || got.Answer.Commands[0].Result != gitlet.ResultOK {
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

// A GREEN MOVES IN ONE RECORD WHATEVER LIES BENEATH IT (ADR-0098): a landing
// is green by its place, never a mark of its own, so a green record moves the
// one row however many unproven landings a red tip left. Four unproven pushes
// land on main; once unit passed on the newest, a green record naming it is
// RECORDED (never refused for the landings beneath), moves main's green row
// onto it, and leaves the target owing nothing and its repository unlisted.
//
// Mutation: refuse a green past a cap on the landings beneath it (record_full),
// or leave the target's row of tip work where a green moves past every landing
// (the repository still listed): red.
func TestAGreenMovesInOneRecordWhateverLiesBeneathIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = gitlet.DirectPersons, queueGate()
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A"))))
	from := "A"
	for _, to := range []string{"B", "C", "D", "E"} {
		got, err := c.Propose(t.Context(), proposal(repo, update(mainRef, oidOf(from), oidOf(to))))
		if err != nil || len(got.Answer.Commands) != 1 || got.Answer.Commands[0].Result != gitlet.ResultOK {
			t.Fatalf("ada's push from %s to %s answered %+v (%v), want it landed", from, to, got.Answer, err)
		}
		from = to
	}
	_, tips := tipsDue(t, n, repo)
	if len(tips) != 1 || len(tips[0].Landings) != 4 {
		t.Fatalf("web owes tip work %+v, want main's four landings", tips)
	}
	newest := tips[0].Newest()
	n.checked(repo, newest.OID, mainRef, "unit", 1, gitlet.CheckStatePassed)
	res, err := c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
		p.Green = &gitlet.Green{RepoID: repo, TargetRef: mainRef, OID: newest.OID, Through: newest.Position}
	}))
	requireRecorded(t, res, err)
	if commit, position := n.greenOf(repo, mainRef); commit != oidOf("E") || position != newest.Position.Packed() {
		t.Fatalf("main's green row is at %s (%d), want E at its landing's %d", commit, position,
			newest.Position.Packed())
	}
	if repos, tips := tipsDue(t, n, repo); len(tips) != 0 || slices.Contains(repos, repo) {
		t.Fatalf("a target green past every landing owes tip work %+v (listed: %v), want none", tips, repos)
	}
}

// A TIP ROUND IN FLIGHT IS READ WHEREVER IT IS (ADR-0098): the home dispatches
// a target's newest landing only once no tip round on another of its commits
// is queued or running, so tip work names that round's commit, read by its own
// index, past the window too. Three pushes past a window of one read B and D,
// C in the gap: an attempt queued on D, the newest, is no round it waits for;
// one running on C, unread by the window, is.
//
// Mutation: leave the newest's own commit in the round's read (D named), or
// read no round at all (C never named): red.
func TestATipRoundInFlightIsReadWhereverItIs(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	gitlet.SetTipWindow(n.reader, 1)
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = gitlet.DirectPersons, queueGate()
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A"))))
	from := "A"
	for _, to := range []string{"B", "C", "D"} {
		got, err := c.Propose(t.Context(), proposal(repo, update(mainRef, oidOf(from), oidOf(to))))
		if err != nil || len(got.Answer.Commands) != 1 || got.Answer.Commands[0].Result != gitlet.ResultOK {
			t.Fatalf("ada's push from %s to %s answered %+v (%v), want it landed", from, to, got.Answer, err)
		}
		from = to
	}
	inFlight := func() string {
		t.Helper()
		_, tips := tipsDue(t, n, repo)
		if len(tips) != 1 || !tips[0].More || tips[0].Newest().OID != oidOf("D") {
			t.Fatalf("web owes tip work %+v, want main's read past a gap to D", tips)
		}
		return tips[0].InFlight
	}
	n.checked(repo, oidOf("D"), mainRef, "unit", 1, gitlet.CheckStateQueued)
	if got := inFlight(); got != "" {
		t.Fatalf("main's round in flight is %s with only its newest's own attempt queued, want none", got)
	}
	n.checked(repo, oidOf("C"), mainRef, "unit", 1, gitlet.CheckStateRunning)
	if got := inFlight(); got != oidOf("C") {
		t.Fatalf("main's round in flight is %q with C's tip attempt running past the window, want C", got)
	}
}

// A TARGET PAST THE PAGE IS ANSWERED ON THE NEXT PAGE (ADR-0098): main and
// release each owe tip work, and a page holds one target's. The first page
// answers main (the target that fills a page is answered, never dropped) and
// ends there; the next, asked after it, answers release; the third answers no
// tip and ends, so the home's turn answers both, each once.
//
// Mutation: ask whether the page is full before answering the target that
// fills it (main never answered), or restart whatever the page before ended at
// (main answered again, release never): red.
func TestATargetPastThePageIsAnsweredOnTheNextPage(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = gitlet.DirectPersons, queueGate()
	release := s.Protections[0]
	release.Pattern = "refs/heads/release"
	s.Protections = append(s.Protections, release)
	repo := n.ensure("web", s, t1).RepoID
	for _, target := range []string{mainRef, "refs/heads/release"} {
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(target, gitlet.ZeroOID, oidOf("A"))))
		got, err := c.Propose(t.Context(), proposal(repo, update(target, oidOf("A"), oidOf("B"))))
		if err != nil || len(got.Answer.Commands) != 1 || got.Answer.Commands[0].Result != gitlet.ResultOK {
			t.Fatalf("ada's push onto %s answered %+v (%v), want it landed", target, got.Answer, err)
		}
	}
	gitlet.SetPageLimits(n.reader, 1, gitlet.MaxPageBytes)
	var answered []string
	after := ""
	for page := 1; ; page++ {
		if page > 4 {
			t.Fatalf("web's tip work was still paging after three pages, answered %v", answered)
		}
		a, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChecksDue,
			ChecksDue: &gitlet.ChecksDueQuery{RepoID: repo, RefreshBefore: harnessAt, TipsAfter: after},
			Fresh:     statelog.Freshness{Level: statelog.ReadStale}})
		if err != nil {
			t.Fatalf("page %d of web's check work: %v", page, err)
		}
		for _, tip := range a.DueTips {
			answered = append(answered, tip.TargetRef)
		}
		if a.TipsNext == "" {
			break
		}
		after = a.TipsNext
	}
	if !slices.Equal(answered, []string{mainRef, "refs/heads/release"}) {
		t.Fatalf("web's pages answered the tip work of %v, want main's and then release's, each once", answered)
	}
}
