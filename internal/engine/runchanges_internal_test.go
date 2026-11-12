package engine

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// changeHost is Gitlet as a collection asks it of the changes a run's
// branches head: each repository's live changes by branch, an open that
// lands one — numbered from 7 — unless its branch heads one, and a read by
// object id answering one commit's subject; every question recorded. Set to
// answer opens together, it holds each until that many are being asked at
// once ([changeHost.meet]).
type changeHost struct {
	mu       sync.Mutex
	heads    map[pushKey]gitlet.BranchChange
	next     int64
	subject  string
	reads    []gitlet.ReadQuery
	writes   []gitlet.ChangeProposal
	gits     []gitlet.GitQuery
	together int
	arrived  int
	met      chan struct{}
}

func newChangeHost() *changeHost {
	return &changeHost{heads: map[pushKey]gitlet.BranchChange{}, next: 7, subject: "Retry the upload once"}
}

func (h *changeHost) Read(_ context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads = append(h.reads, q)
	if q.Kind != gitlet.ReadBranchChanges || q.BranchChanges == nil {
		return gitlet.ReadAnswer{}, fmt.Errorf("%w: a collection read kind %s", gitlet.ErrInvalid, q.Kind)
	}
	out := gitlet.ReadAnswer{Position: firstPageAt, Level: q.Fresh.Level}
	for _, repo := range q.BranchChanges.Repos {
		for _, ref := range repo.Refs {
			if bc, ok := h.heads[pushKey{repo: repo.RepoID, ref: ref}]; ok {
				bc.RepoID = repo.RepoID
				out.BranchChanges = append(out.BranchChanges, bc)
			}
		}
	}
	return out, nil
}

// pushRecordAt is where the cases' push records were read: their one barrier,
// which every read of the run's changes is floored at.
var pushRecordAt = statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 85}

// recordOf is pushed as a push record read at pushRecordAt.
func recordOf(pushed []sandbox.PushedRef) sandbox.PushRecord {
	return sandbox.PushRecord{Refs: pushed, At: pushRecordAt.String()}
}

// atTheRecord reports whether q was read at session floored at the push
// record's position — no barrier of its own.
func atTheRecord(q gitlet.ReadQuery) bool {
	return q.Fresh.Level == statelog.ReadSession && q.Fresh.Floor(topics.CodeLogStream) == pushRecordAt
}

func (h *changeHost) Change(ctx context.Context, p gitlet.ChangeProposal) (gitlet.ChangeResult, error) {
	if err := h.meet(ctx); err != nil {
		return gitlet.ChangeResult{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writes = append(h.writes, p)
	o := p.Open
	key := pushKey{repo: o.RepoID, ref: o.SourceRef}
	if live, ok := h.heads[key]; ok {
		return gitlet.ChangeResult{Refusal: gitlet.ChangeRefusalChangeExists,
			Detail: fmt.Sprintf("%s already heads change %d", o.SourceRef, live.Number)}, nil
	}
	h.heads[key] = gitlet.BranchChange{Ref: o.SourceRef, ChangeID: o.ChangeID, Number: h.next}
	h.next++
	return gitlet.ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: o.ChangeID, Number: h.next - 1,
		Version: 1, Position: firstPageAt}, nil
}

func (h *changeHost) GitRead(_ context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gits = append(h.gits, q)
	return gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Log: &gitlet.LogAnswer{
		Commits: []gitlet.LogCommit{{OID: q.Log.Rev, Subject: h.subject}}}}, nil
}

// meet holds an open, where the host is set to answer opens together, until
// that many have arrived — or ctx ends, answered as the write's failure.
func (h *changeHost) meet(ctx context.Context) error {
	h.mu.Lock()
	if h.together == 0 {
		h.mu.Unlock()
		return nil
	}
	if h.arrived++; h.arrived == h.together {
		close(h.met)
	}
	met := h.met
	h.mu.Unlock()
	select {
	case <-met:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// asked is what the host was asked, copied under its lock.
func (h *changeHost) asked() ([]gitlet.ReadQuery, []gitlet.ChangeProposal, []gitlet.GitQuery) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reads), slices.Clone(h.writes), slices.Clone(h.gits)
}

// openingRun is pushedRun asking to open a change from web's branch into
// main, charged to a native task, its launch begun at an instant of its own.
func openingRun() sandbox.PendingRun {
	run := pushedRun()
	run.Repos[0].Open = &sandbox.OpenChange{Target: "refs/heads/main"}
	run.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: "0190d8f0-5555-7000-8000-0123456789ab",
		Key: "ENG-42", Project: "ENG"}
	run.Launch.StartedAt = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	return run
}

// A RUN OPENS ITS CHANGE ONCE, AT ITS COLLECTION (M4-decisions D33): a run that
// asked to open a change from its branch, and pushed it, has the change
// opened by the collection — as the seat it ran for, from that branch into
// the target its launch resolved, titled by the subject of the commit it
// delivered, carrying the work item it was charged to and naming its turn and
// its run, decided at or past the read that found the branch heading none —
// under an operation that is a step of the launch's own, so the same
// collection asked again opens nothing twice and names the change it opened
// as its own; and the ref it was opened from names it. A collection not asked
// to open opens nothing, and a run that pushed nothing to its branch is told
// so.
//
// Mutation: mint the open's operation at the collection (a fresh id each
// time) — the second collection's open is another operation, and the change
// the first opened is named as not this run's, red.
func TestARunOpensItsChangeOnceAtCollect(t *testing.T) {
	t.Parallel()
	run := openingRun()
	branch := "refs/heads/seats/swe/ENG-42"
	pushed := []sandbox.PushedRef{{Repo: "web", Ref: branch, Old: gitlet.ZeroOID, New: oidOf("1"), Pushes: 2}}
	host := newChangeHost()

	named, opened, err := readerOf(host).Changes(t.Context(), run, recordOf(pushed), true)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	want := []sandbox.OpenedChange{{Repo: "web", Branch: "seats/swe/ENG-42", Change: "web!7", Opened: true}}
	if !reflect.DeepEqual(opened, want) {
		t.Fatalf("what became of the run's ask: %+v, want %+v", opened, want)
	}
	if len(named) != 1 || named[0].Change != "web!7" {
		t.Fatalf("the ref the change was opened from is %+v, want it naming web!7", named)
	}
	reads, writes, gits := host.asked()
	if len(reads) != 1 || !atTheRecord(reads[0]) {
		t.Fatalf("the collection read %+v, want one read of web's branches at session at its push record's %s",
			reads, pushRecordAt)
	}
	if len(gits) != 1 || gits[0].Log == nil || gits[0].Log.Rev != oidOf("1") || gits[0].Asker.Seat != "swe" {
		t.Fatalf("the collection titled the change by %+v, want one read of the delivered commit as swe", gits)
	}
	op := openOpID(run, runWebID)
	wantOpen := gitlet.ChangeProposal{OpID: op, Asker: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe"},
		Seen: firstPageAt, Open: &gitlet.ChangeOpen{RepoID: runWebID, ChangeID: gitlet.OpenChangeID(op),
			SourceRef: branch, TargetRef: "refs/heads/main", Title: "Retry the upload once",
			WorkItem: run.WorkItem, TurnID: "t1", RunID: runLaunch}}
	if len(writes) != 1 || !reflect.DeepEqual(writes[0], wantOpen) {
		t.Fatalf("the collection asked %+v, want ONE open %+v", writes, wantOpen)
	}
	if err := statelog.CheckCallerOpID(op); err != nil {
		t.Fatalf("the open's operation %q is no operation id the engine accepts: %v", op, err)
	}

	// THE SAME COLLECTION ASKED AGAIN, on this node or another, opens
	// nothing twice: the branch heads the change this launch opened.
	_, again, err := readerOf(host).Changes(t.Context(), run, recordOf(pushed), true)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("the collection asked again: %+v, %v; want %+v", again, err, want)
	}
	if _, writes, _ := host.asked(); len(writes) != 1 {
		t.Fatalf("the collection asked again wrote %d opens, want the first alone", len(writes))
	}

	// ANOTHER LAUNCH of the turn's run is another operation: it names the
	// change the first opened as one its branch already headed.
	relaunched := run
	relaunched.LaunchID = "0190d8f0-0000-7000-8000-0000000000f2"
	if _, other, err := readerOf(host).Changes(t.Context(), relaunched, recordOf(pushed), true); err != nil ||
		len(other) != 1 || other[0].Opened || other[0].Change != "web!7" {
		t.Fatalf("a relaunch's collection: %+v, %v; want web!7 named as the branch's, not opened", other, err)
	}

	// NOT ASKED TO OPEN — a run that did not finish — it opens nothing.
	quiet := newChangeHost()
	if _, none, err := readerOf(quiet).Changes(t.Context(), run, recordOf(pushed), false); err != nil || none != nil {
		t.Fatalf("a collection not asked to open answered %+v, %v", none, err)
	}
	if _, writes, _ := quiet.asked(); len(writes) != 0 {
		t.Fatalf("a collection not asked to open wrote %+v", writes)
	}

	// A RUN THAT PUSHED NOTHING TO ITS BRANCH proposes nothing.
	idle := newChangeHost()
	_, unopened, err := readerOf(idle).Changes(t.Context(), run, recordOf(nil), true)
	if err != nil || len(unopened) != 1 || unopened[0].Change != "" ||
		!strings.Contains(unopened[0].Unopened, "pushed nothing") {
		t.Fatalf("a run that pushed nothing: %+v, %v; want it told no change was opened", unopened, err)
	}
	if _, writes, _ := idle.asked(); len(writes) != 0 {
		t.Fatalf("a run that pushed nothing asked to open %+v", writes)
	}
}

// A RUN'S OPEN IS HELD TO WHOEVER WOKE ITS TURN (M4-decisions D30, U40 (a)):
// a run whose launching turn a colleague seat woke — its row naming that seat
// (sandbox.PendingRun.CodeCeiling), which the box's admission holds every
// push of the run to — asks its open at collection carrying that colleague as
// the write's ceiling, as a code_open_change of the same turn would, so the
// repository's committer judges the open for the colleague from its own chart
// when it decides it, never from the launch's one resolution. A run no
// colleague bounds asks it for its seat alone.
//
// Mutation: leave the ceiling off the open (openOne) — the colleague's run
// asks its open as the seat alone, red.
func TestARunsOpenIsHeldToWhoeverWokeItsTurn(t *testing.T) {
	t.Parallel()
	branch := "refs/heads/seats/swe/ENG-42"
	pushed := []sandbox.PushedRef{{Repo: "web", Ref: branch, Old: gitlet.ZeroOID, New: oidOf("1"), Pushes: 1}}
	for _, tc := range []struct {
		name, colleague string
		want            *gitlet.WriteCeiling
	}{
		{"a colleague's ask", "designer", &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: "designer"}},
		{"nobody's ask", "", nil},
	} {
		run := openingRun()
		run.CodeCeiling = tc.colleague
		host := newChangeHost()
		if _, opened, err := readerOf(host).Changes(t.Context(), run, recordOf(pushed), true); err != nil ||
			len(opened) != 1 || !opened[0].Opened {
			t.Fatalf("%s: the collection answered %+v, %v; want its change opened", tc.name, opened, err)
		}
		_, writes, _ := host.asked()
		if len(writes) != 1 {
			t.Fatalf("%s: the collection asked %+v, want one open", tc.name, writes)
		}
		if got := writes[0].Ceiling; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: the open was asked for %+v, want %+v", tc.name, got, tc.want)
		}
		if got := writes[0].Asker; got != (gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe"}) {
			t.Errorf("%s: the open was asked by %+v, want the run's seat", tc.name, got)
		}
	}
}

// A RUN'S CHANGE IS TITLED WHATEVER ITS BRANCH IS NAMED: a run that delivered
// a commit with no subject asks the open titled by its branch's name, made a
// title as a push's change is (gitlet.DeliveredTitle) — and a branch git accepts
// may hold U+0085, a C1 control character an open refuses in a title as a
// line break — so the ask is never left unopened for a title nobody wrote.
//
// Mutation: title it by the branch's name raw — the open carries U+0085, red.
func TestARunsChangeIsTitledWhateverItsBranchIsNamed(t *testing.T) {
	t.Parallel()
	run := openingRun()
	run.Repos[0].Branch = "seats/swe/a\u0085b"
	branch := "refs/heads/" + run.Repos[0].Branch
	host := newChangeHost()
	host.subject = ""
	pushed := []sandbox.PushedRef{{Repo: "web", Ref: branch, Old: gitlet.ZeroOID, New: oidOf("1"), Pushes: 1}}
	if _, opened, err := readerOf(host).Changes(t.Context(), run, recordOf(pushed), true); err != nil || len(opened) != 1 ||
		!opened[0].Opened {
		t.Fatalf("a run that delivered a commit with no subject to a branch holding U+0085: %+v, %v", opened, err)
	}
	_, writes, _ := host.asked()
	if len(writes) != 1 || writes[0].Open == nil {
		t.Fatalf("the collection asked %+v, want one open", writes)
	}
	if title := writes[0].Open.Title; title != "seats/swe/a b" || gitlet.TitleProblem(title) != "" {
		t.Fatalf("the open is titled %q, want its branch's name made a title, %q", title, "seats/swe/a b")
	}
}

// A RUN'S ASKS ARE OPENED AT ONCE: each ask is its own repository's, under its
// own operation and decided by that repository's committer, so a collection
// asks every open together and waits for its slowest — never for the sum of
// writes that may each wait for their record to apply while the run's seat is
// held — and each ref it delivered names the change opened from it.
//
// Mutation: open the asks one after another — the first open waits for a
// second that never comes until the case's deadline, unopened, red.
func TestARunsAsksAreOpenedAtOnce(t *testing.T) {
	t.Parallel()
	run := openingRun()
	run.Repos[2].Open = &sandbox.OpenChange{Target: "refs/heads/main", Title: "Fix the api"}
	branch := "refs/heads/seats/swe/ENG-42"
	pushed := []sandbox.PushedRef{
		{Repo: "web", Ref: branch, Old: gitlet.ZeroOID, New: oidOf("1"), Pushes: 1},
		{Repo: "api", Ref: branch, Old: gitlet.ZeroOID, New: oidOf("2"), Pushes: 1},
	}
	host := newChangeHost()
	host.together, host.met = 2, make(chan struct{})
	// THE DEADLINE IS ONLY EVER REACHED BY OPENS ASKED ONE AFTER ANOTHER,
	// whose first waits for a second that is not asked until it answers: asked
	// together, both are answered as soon as both have arrived.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	named, opened, err := readerOf(host).Changes(ctx, run, recordOf(pushed), true)
	if err != nil || len(opened) != 2 {
		t.Fatalf("Changes = %+v, %v; want both asks answered", opened, err)
	}
	for i, repo := range []string{"web", "api"} {
		if o := opened[i]; o.Repo != repo || !o.Opened || !strings.HasPrefix(o.Change, repo+"!") ||
			named[i].Change != o.Change {
			t.Errorf("%s's ask: %+v, its ref %+v; want a change of %s opened and its ref naming it", repo, o,
				named[i], repo)
		}
	}
	if _, writes, _ := host.asked(); len(writes) != 2 {
		t.Errorf("the collection asked %d opens, want one a repository", len(writes))
	}
}

// A DELIVERED REF NAMES ITS CHANGE (M4-decisions D33): every branch a collected
// run pushed is named by the live change it heads — the run's own, or one
// anybody opened — in ONE read of every repository's branches, at session at
// the push record's position, so the collection pays the record's barrier
// and no second; a tag, a deleted branch and a branch heading no change name
// none, and a repository the run was given to read is never asked. A run that
// pushed more branches than one read answers is read in as few as hold them,
// each at the record's position, every branch named.
//
// Mutations: read the changes linearizable — the read's level, red; read
// each repository on its own — two reads, red; name a deleted branch by the
// change its name heads — red; drop the branches past one read — the last
// branch's change unnamed, red.
func TestADeliveredRefNamesItsChange(t *testing.T) {
	t.Parallel()
	host := newChangeHost()
	host.heads[pushKey{repo: runWebID, ref: "refs/heads/seats/swe/ENG-42"}] = gitlet.BranchChange{
		Ref: "refs/heads/seats/swe/ENG-42", ChangeID: "0190d8f0-6666-7000-8000-0123456789ab", Number: 42}
	host.heads[pushKey{repo: runAPIID, ref: "refs/heads/fix"}] = gitlet.BranchChange{Ref: "refs/heads/fix",
		ChangeID: "0190d8f0-7777-7000-8000-0123456789ab", Number: 5}
	host.heads[pushKey{repo: runAPIID, ref: "refs/heads/old"}] = gitlet.BranchChange{Ref: "refs/heads/old",
		ChangeID: "0190d8f0-8888-7000-8000-0123456789ab", Number: 6}
	pushed := []sandbox.PushedRef{
		{Repo: "web", Ref: "refs/heads/seats/swe/ENG-42", Old: oidOf("1"), New: oidOf("2"), Pushes: 1},
		{Repo: "web", Ref: "refs/tags/v1", Old: gitlet.ZeroOID, New: oidOf("2"), Pushes: 1},
		{Repo: "api", Ref: "refs/heads/fix", Old: oidOf("3"), New: oidOf("4"), Pushes: 1},
		{Repo: "api", Ref: "refs/heads/old", Old: oidOf("5"), New: gitlet.ZeroOID, Pushes: 1},
		{Repo: "api", Ref: "refs/heads/spike", Old: gitlet.ZeroOID, New: oidOf("6"), Pushes: 1},
	}
	named, opened, err := readerOf(host).Changes(t.Context(), pushedRun(), recordOf(pushed), true)
	if err != nil || opened != nil {
		t.Fatalf("Changes = %+v, %v; want no ask answered", opened, err)
	}
	want := []string{"web!42", "", "api!5", "", ""}
	for i, ref := range named {
		if ref.Change != want[i] {
			t.Errorf("%s %s names change %q, want %q", ref.Repo, ref.Ref, ref.Change, want[i])
		}
	}
	reads, _, _ := host.asked()
	if len(reads) != 1 || !atTheRecord(reads[0]) {
		t.Fatalf("the collection read %+v, want ONE read at session at its push record's %s", reads, pushRecordAt)
	}
	wantRepos := []gitlet.RepoBranches{{RepoID: runWebID, Refs: []string{"refs/heads/seats/swe/ENG-42"}},
		{RepoID: runAPIID, Refs: []string{"refs/heads/fix", "refs/heads/spike"}}}
	if got := reads[0].BranchChanges.Repos; !reflect.DeepEqual(got, wantRepos) {
		t.Errorf("the read named %+v, want each repository's live branches, in the run's order: %+v", got,
			wantRepos)
	}

	// MORE BRANCHES THAN ONE READ ANSWERS: read in two, every branch named.
	many := newChangeHost()
	var lots []sandbox.PushedRef
	for i := range gitlet.MaxRefsPerQuery + 1 {
		lots = append(lots, sandbox.PushedRef{Repo: "web", Ref: fmt.Sprintf("refs/heads/b%d", i),
			Old: gitlet.ZeroOID, New: oidOf("1"), Pushes: 1})
	}
	last := lots[len(lots)-1].Ref
	many.heads[pushKey{repo: runWebID, ref: last}] = gitlet.BranchChange{Ref: last,
		ChangeID: "0190d8f0-9999-7000-8000-0123456789ab", Number: 9}
	named, _, err = readerOf(many).Changes(t.Context(), pushedRun(), recordOf(lots), false)
	if err != nil || named[len(named)-1].Change != "web!9" {
		t.Fatalf("a run that pushed %d branches: its last named %+v, %v; want web!9", len(lots),
			named[len(named)-1], err)
	}
	reads, _, _ = many.asked()
	asked := 0
	for _, q := range reads {
		n := 0
		for _, repo := range q.BranchChanges.Repos {
			n += len(repo.Refs)
		}
		if n > gitlet.MaxRefsPerQuery || !atTheRecord(q) {
			t.Errorf("a read named %d branches at %+v, want at most %d at the push record's position", n,
				q.Fresh, gitlet.MaxRefsPerQuery)
		}
		asked += n
	}
	if len(reads) != 2 || asked != len(lots) {
		t.Errorf("a run that pushed %d branches was read in %d reads naming %d, want two naming every one",
			len(lots), len(reads), asked)
	}
}
