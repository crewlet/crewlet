package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// noPushes reads every run as having pushed nothing: the coordinators built
// with it run no repository on the code host (the cases below read one).
type noPushes struct{}

func (noPushes) Pushed(context.Context, sandbox.PendingRun) ([]sandbox.PushedRef, int, error) {
	return nil, 0, nil
}

func (noPushes) Changes(_ context.Context, _ sandbox.PendingRun, pushed []sandbox.PushedRef,
	_ bool) ([]sandbox.PushedRef, []sandbox.OpenedChange, error) {
	return pushed, nil, nil
}

// pagedReader is the code host's read answering a scripted page per call, and
// recording every query it was asked.
type pagedReader struct {
	pages []code.ReadAnswer
	err   error
	asked []code.ReadQuery
}

func (r *pagedReader) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	r.asked = append(r.asked, q)
	if r.err != nil {
		return code.ReadAnswer{}, r.err
	}
	if len(r.asked) > len(r.pages) {
		return code.ReadAnswer{}, fmt.Errorf("asked for page %d of %d", len(r.asked), len(r.pages))
	}
	return r.pages[len(r.asked)-1], nil
}

// Change and GitRead are no question the push record's reader asks.
func (r *pagedReader) Change(context.Context, code.ChangeProposal) (code.ChangeResult, error) {
	return code.ChangeResult{}, errors.New("the push record's reader asked for a change write")
}

func (r *pagedReader) GitRead(context.Context, code.GitQuery) (code.GitAnswer, error) {
	return code.GitAnswer{}, errors.New("the push record's reader asked for a read by object id")
}

// readerOf is a runPushes over r.
func readerOf(r pushReader) runPushes {
	return runPushes{code: func() (pushReader, error) { return r, nil }}
}

// The repositories, objects and positions the cases read.
const (
	runWebID  = "0190d8f0-0000-7000-8000-00000000000a"
	runAPIID  = "0190d8f0-0000-7000-8000-00000000000b"
	runDocsID = "0190d8f0-0000-7000-8000-00000000000c"
	runLaunch = "0190d8f0-0000-7000-8000-0000000000f1"
)

var (
	runLaunchAt = statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 40}
	firstPageAt = statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 90}
)

// oid is a whole object id of one repeated hex digit.
func oidOf(digit string) string { return strings.Repeat(digit, 40) }

// pushedRun is a run given web and api to push to and docs to read, launched
// at runLaunchAt.
func pushedRun() sandbox.PendingRun {
	return sandbox.PendingRun{
		TurnID: "t1", LaunchID: runLaunch, AgentHandle: "swe", CodeLaunch: runLaunchAt.String(),
		Repos: []sandbox.RunRepo{
			{Repo: "web", RepoID: runWebID, Branch: "seats/swe/ENG-42", Access: sandbox.RepoWrite},
			{Repo: "docs", RepoID: runDocsID, Branch: "seats/swe/ENG-42", Access: sandbox.RepoRead},
			{Repo: "api", RepoID: runAPIID, Branch: "seats/swe/ENG-42", Access: sandbox.RepoWrite},
		},
	}
}

// pushRow is a push row of repo whose commands are cmds.
func pushRow(id, repo string, cmds ...code.Command) code.PushRow {
	return code.PushRow{PushID: id, RepoID: repo, Commands: cmds}
}

// cmdOK is a command the decide took, moving ref from one object to another;
// cmdNG one it refused.
func cmdOK(ref, from, to string) code.Command {
	return code.Command{Ref: ref, Old: from, New: to, Result: code.ResultOK}
}

func cmdNG(ref, from, to string) code.Command {
	return code.Command{Ref: ref, Old: from, New: to, Result: code.ResultNG, Reason: "stale"}
}

// A RUN GIVEN ITS MOST REPOSITORIES HAS ITS PUSHES READ WHOLE: the bound a
// run's repositories are admitted to — run_sandbox's `repos` and the launch's
// grants, sandbox.MaxRunRepos — never passes the bound a `run_pushes` read may
// name, code.MaxRunRepos, which the code host refuses a read past. The two are
// spelled in two packages that do not import each other, and a raise of the
// first alone would refuse the "what it pushed" read of every collected run
// given more, surfacing only as the sentence that its push record was unread.
// Here, the one package that imports both.
//
// Mutation: sandbox.MaxRunRepos raised to 9 → red.
func TestARunGivenItsMostRepositoriesHasItsPushesReadWhole(t *testing.T) {
	t.Parallel()
	if sandbox.MaxRunRepos > code.MaxRunRepos {
		t.Errorf("a run may be given %d repositories (sandbox.MaxRunRepos) and a read of its pushes may name "+
			"at most %d (code.MaxRunRepos): a run given its most would have its pushes refused",
			sandbox.MaxRunRepos, code.MaxRunRepos)
	}
	// AND THE READ THE COORDINATOR MAKES FOR SUCH A RUN NAMES THEM ALL: every
	// repository it may push to, none dropped to fit.
	run := sandbox.PendingRun{TurnID: "t1", LaunchID: runLaunch, AgentHandle: "swe",
		CodeLaunch: runLaunchAt.String()}
	for i := range sandbox.MaxRunRepos {
		run.Repos = append(run.Repos, sandbox.RunRepo{Repo: fmt.Sprintf("r%d", i),
			RepoID: fmt.Sprintf("0190d8f0-0000-7000-8000-0000000001%02x", i), Branch: "seats/swe/ENG-42",
			Access: sandbox.RepoWrite})
	}
	reader := &pagedReader{pages: []code.ReadAnswer{{}}}
	if _, _, err := readerOf(reader).Pushed(t.Context(), run); err != nil {
		t.Fatalf("read the pushes of a run given %d repositories: %v", len(run.Repos), err)
	}
	if len(reader.asked) != 1 || len(reader.asked[0].RunPushes.Repos) != sandbox.MaxRunRepos {
		t.Errorf("the read of a run given %d repositories asked %+v, want one read naming every one",
			sandbox.MaxRunRepos, reader.asked)
	}
}

// WHAT A RUN PUSHED IS READ FROM ITS LAUNCH'S PUSH ROWS, every page to the
// last: the first LINEARIZABLE — one barrier a collected run, so "it pushed
// nothing" is never a stale answer — and every later one at session at the
// first page's position, so no node behind that snapshot answers one. The
// repositories are the run's own it may push to, by id in its order, read
// above its launch; each ref collapses to the old value of its first push and
// the new value of its last, in the order the run first pushed it; a command
// the decide refused moved nothing; and a ref is named as the run was given
// its repository.
//
// Mutations: list every push rather than collapse — four web entries, red;
// read every page linearizable — the second page's level, red; keep refused
// commands — the ng ref listed, red; name the repository by id — red.
func TestRunPushesReadsTheRunsLaunchInOrder(t *testing.T) {
	t.Parallel()
	branch := "refs/heads/seats/swe/ENG-42"
	reader := &pagedReader{pages: []code.ReadAnswer{
		{
			Pushes: []code.PushRow{
				pushRow("p1", runWebID, cmdOK(branch, code.ZeroOID, oidOf("1")), cmdNG("refs/heads/main", oidOf("a"), oidOf("b"))),
				pushRow("p2", runWebID, cmdOK(branch, oidOf("1"), oidOf("2")), cmdOK("refs/heads/seats/swe/spike", code.ZeroOID,
					oidOf("9"))),
				pushRow("p3", runWebID, cmdOK(branch, oidOf("2"), oidOf("3"))),
			},
			Next: "0/91/p3", Position: firstPageAt, Level: statelog.ReadLinearizable,
		},
		{
			Pushes: []code.PushRow{
				pushRow("p4", runAPIID, cmdOK("refs/heads/seats/swe/old", oidOf("4"), code.ZeroOID)),
			},
			Position: firstPageAt, Level: statelog.ReadSession,
		},
	}}
	refs, more, err := readerOf(reader).Pushed(t.Context(), pushedRun())
	if err != nil {
		t.Fatalf("Pushed: %v", err)
	}
	want := []sandbox.PushedRef{
		{Repo: "web", Ref: branch, Old: code.ZeroOID, New: oidOf("3"), Pushes: 3},
		{Repo: "web", Ref: "refs/heads/seats/swe/spike", Old: code.ZeroOID, New: oidOf("9"), Pushes: 1},
		{Repo: "api", Ref: "refs/heads/seats/swe/old", Old: oidOf("4"), New: code.ZeroOID, Pushes: 1},
	}
	if !slices.Equal(refs, want) || more != 0 {
		t.Errorf("read %+v and %d more, want %+v", refs, more, want)
	}

	if len(reader.asked) != 2 {
		t.Fatalf("asked %d pages, want the two the code host answered", len(reader.asked))
	}
	first, second := reader.asked[0], reader.asked[1]
	wantQuery := code.RunPushesQuery{Run: runLaunch, Repos: []string{runWebID, runAPIID}, After: runLaunchAt}
	for i, q := range reader.asked {
		if q.Kind != code.ReadRunPushes || q.RunPushes == nil || q.RunPushes.Run != wantQuery.Run ||
			!slices.Equal(q.RunPushes.Repos, wantQuery.Repos) || q.RunPushes.After != wantQuery.After {
			t.Errorf("page %d asked %s %+v, want run_pushes %+v — the repositories the run may push to, "+
				"in its order, above its launch", i+1, q.Kind, q.RunPushes, wantQuery)
		}
	}
	if first.Fresh.Level != statelog.ReadLinearizable || first.Cursor != "" {
		t.Errorf("the first page was asked at %q from cursor %q, want linearizable from the start",
			first.Fresh.Level, first.Cursor)
	}
	if second.Fresh.Level != statelog.ReadSession || second.Fresh.Floor(topics.CodeLogStream) != firstPageAt ||
		second.Cursor != "0/91/p3" {
		t.Errorf("the second page was asked at %q floored at %s from cursor %q, want session at the first "+
			"page's %s from its cursor", second.Fresh.Level, second.Fresh.Floor(topics.CodeLogStream),
			second.Cursor, firstPageAt)
	}

	// A RUN GIVEN NOTHING TO PUSH TO IS READ AS NOTHING, with no read.
	idle := pushedRun()
	idle.Repos = idle.Repos[1:2]
	silent := &pagedReader{err: errors.New("asked")}
	if refs, more, err := readerOf(silent).Pushed(t.Context(), idle); err != nil || refs != nil || more != 0 ||
		len(silent.asked) != 0 {
		t.Errorf("a run given only a repository to read = %v, %d, %v after %d reads; want nothing, unread",
			refs, more, err, len(silent.asked))
	}
}

// A READER HOLDS NO MORE REFS THAN A RESULT CAN LIST, whatever the run pushed:
// a box chooses how many refs it moves, and the reader's memory follows what a
// record can carry. The refs past sandbox.MaxPushedRefs are counted, never
// held, each once for every push that moved it; the held ones still take
// their later pushes.
//
// Mutation: drop the cap — every ref held, red.
func TestRunPushesHoldsAtMostTheRefsAResultCanList(t *testing.T) {
	t.Parallel()
	var cmds []code.Command
	for i := range sandbox.MaxPushedRefs + 5 {
		cmds = append(cmds, cmdOK(fmt.Sprintf("refs/heads/seats/swe/%d", i), code.ZeroOID, oidOf("1")))
	}
	reader := &pagedReader{pages: []code.ReadAnswer{{
		Pushes: []code.PushRow{
			pushRow("p1", runWebID, cmds...),
			// A held ref moved again, and a counted one moved again.
			pushRow("p2", runWebID, cmdOK("refs/heads/seats/swe/0", oidOf("1"), oidOf("2")),
				cmdOK(fmt.Sprintf("refs/heads/seats/swe/%d", sandbox.MaxPushedRefs), oidOf("1"), oidOf("2"))),
		},
		Position: firstPageAt,
	}}}
	refs, more, err := readerOf(reader).Pushed(t.Context(), pushedRun())
	if err != nil {
		t.Fatalf("Pushed: %v", err)
	}
	if len(refs) != sandbox.MaxPushedRefs || more != 6 {
		t.Fatalf("held %d refs and counted %d more, want %d held and the 5 past them counted with the "+
			"second push of one", len(refs), more, sandbox.MaxPushedRefs)
	}
	if refs[0].New != oidOf("2") || refs[0].Pushes != 2 {
		t.Errorf("the first ref = %+v, want its second push taken", refs[0])
	}
}

// A RECORD THE READER CANNOT READ IS AN ERROR, never "nothing pushed": no code
// host here, a launch position it cannot read or on another log, a read the
// code host refused, a page that did not move, a first page answered at no
// position of the code log, and a row of a repository the run was not given.
func TestRunPushesRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	page := func(a code.ReadAnswer) []code.ReadAnswer { return []code.ReadAnswer{a, a} }
	for name, tc := range map[string]struct {
		run    func(*sandbox.PendingRun)
		reader *pagedReader
		want   string
	}{
		"an unreadable launch": {run: func(r *sandbox.PendingRun) { r.CodeLaunch = "nowhere" },
			reader: &pagedReader{}, want: "launch position"},
		"a launch on another log": {run: func(r *sandbox.PendingRun) { r.CodeLaunch = "CREWLET_PAGES_LOG@1:4" },
			reader: &pagedReader{}, want: "another log"},
		"a refused read": {reader: &pagedReader{err: code.ErrNotServing}, want: "not serving"},
		"a page that does not move": {reader: &pagedReader{pages: page(code.ReadAnswer{Next: "0/91/p3",
			Position: firstPageAt})}, want: "again"},
		"a first page at no position": {reader: &pagedReader{pages: page(code.ReadAnswer{Next: "0/91/p3"})},
			want: "no position of the code log"},
		"another repository's row": {reader: &pagedReader{pages: page(code.ReadAnswer{Position: firstPageAt,
			Pushes: []code.PushRow{pushRow("p1", runDocsID, cmdOK("refs/heads/x", code.ZeroOID, oidOf("1")))}})},
			want: "not given"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := pushedRun()
			if tc.run != nil {
				tc.run(&run)
			}
			refs, _, err := readerOf(tc.reader).Pushed(t.Context(), run)
			if err == nil || !strings.Contains(err.Error(), tc.want) || refs != nil {
				t.Fatalf("Pushed = %v, %v; want a refusal saying %q and no refs", refs, err, tc.want)
			}
		})
	}

	// A node that serves no code host says why.
	bare := &Engine{}
	if _, _, err := bare.codeRunPushes().Pushed(t.Context(), pushedRun()); err == nil ||
		!strings.Contains(err.Error(), "does not serve the company's code host") {
		t.Fatalf("a node with no code host = %v, want it said", err)
	}
}

// THE RESUMED EXECUTOR'S RECORD CARRIES THE RUN'S WHOLE ACCOUNT: the box off
// the run's row, the refs its report named and what its push record says it
// pushed, each as the resume handed it over.
//
// Mutation: drop the push record from runRecordOf, or from accountOf — red.
func TestAResumedRecordCarriesTheRunsWholeAccount(t *testing.T) {
	t.Parallel()
	run := pushedRun()
	run.CodingAgent, run.SandboxID = "claude-code", "box-1"
	pushed := []sandbox.PushedRef{{Repo: "web", Ref: "refs/heads/seats/swe/ENG-42", Old: code.ZeroOID,
		New: oidOf("3"), Pushes: 3}}
	got := runRecordOf(run, accountOf(sandbox.ResumeRequest{
		Run: run, DeliveredRefs: []string{"https://example.com/pr/9"}, DeliveredRefsElided: 1,
		Pushed: pushed, PushedElided: 2, PushedUnread: "",
	}))
	want := []types.PushedRefRecord{{Repo: "web", Ref: "refs/heads/seats/swe/ENG-42", Old: code.ZeroOID,
		New: oidOf("3"), Pushes: 3}}
	if got.CodingAgent != "claude-code" || got.SandboxID != "box-1" || got.LaunchID != runLaunch ||
		!slices.Equal(got.DeliveredRefs, []string{"https://example.com/pr/9"}) || got.RefsElided != 1 ||
		!slices.Equal(got.Pushed, want) || got.PushedElided != 2 || got.PushedUnread != "" {
		t.Errorf("the resumed record = %+v, want the run's box and its whole account", got)
	}
	unread := runRecordOf(run, accountOf(sandbox.ResumeRequest{Run: run, PushedUnread: "the code host is down"}))
	if unread.PushedUnread != "the code host is down" || unread.Pushed != nil {
		t.Errorf("an unread record = %+v, want the reason and no refs", unread)
	}
}
