package host_test

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// manualClock is a clock a case moves by hand.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// oneResync is how long a re-sync bucket takes to earn one back: a day over
// the census's seat term.
const oneResync = 24 * time.Hour / 125

// A BOX PAST ITS RE-SYNCS IS ANSWERED AT ITS FLOOR: a box asking for an object
// nobody pushed takes one barrier a request until its run's sixteen are spent;
// the seventeenth appends none and is answered in upload-pack's own form,
// naming the object, the repository and when the run's re-syncs come back. A
// second run of the same seat finds its own bucket full and its SEAT's spent
// (the seat is the scope, M3-decisions U36), and a person's fetch, charged to
// nobody, still takes its barrier. Once the refill has earned one, the run
// takes one barrier again.
//
// Mutation: no bucket (boxResync answering nil) — the seventeenth request
// appends a barrier — red.
func TestABoxPastItsResyncsIsAnsweredAtItsFloor(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	var lines logs
	clock := &manualClock{t: time.Now()}
	c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: rec.wrap}, Logger: lines.logger(),
		ResyncClock: clock.now})
	node := c.Fleet.Nodes()[0]
	runs := c.Fleet.Runs()
	first, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
	missing := randomOID()
	fetch := func(ref host.BoxRunRef) (barriers int, answer string) {
		t.Helper()
		before := rec.linearizable()
		resp, body := boxDo(t, c, 0, codetest.RepoName, boxUploadPack, host.BoxUser, runs.Token(ref), v2,
			fetchWants([]string{missing}))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("a box's fetch of %s answered %d %q", missing, resp.StatusCode, short(body))
		}
		return rec.linearizable() - before, body
	}

	for i := range code.ResyncBurst {
		if n, body := fetch(first); n == 0 || !strings.Contains(body, "not our ref") {
			t.Fatalf("re-sync %d of the run's %d took %d barriers and answered %q, want its one and git's own "+
				"not our ref", i+1, code.ResyncBurst, n, short(body))
		}
	}
	n, body := fetch(first)
	want := "ERR " + missing + " is not in " + codetest.RepoName + " at the position this run has been shown"
	if n != 0 || !strings.Contains(body, want) ||
		!strings.Contains(body, "(not re-judged at linearizable: this run's re-syncs are spent until ") {
		t.Errorf("past its %d re-syncs the run's fetch took %d barriers and answered %q, want none and %q naming "+
			"when its re-syncs come back", code.ResyncBurst, n, short(body), want)
	}
	if got := lines.count("code_box_resyncs_spent", missing); got != 1 {
		t.Errorf("logged code_box_resyncs_spent %d times, want once", got)
	}

	second, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
	if n, body := fetch(second); n != 0 || !strings.Contains(body, "this seat's re-syncs are spent until ") {
		t.Errorf("a second run of the seat took %d barriers and answered %q, want its seat's bucket spent",
			n, short(body))
	}
	before := rec.linearizable()
	if resp, body := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants([]string{missing})); resp.StatusCode !=
		http.StatusOK || rec.linearizable() == before {
		t.Errorf("a person's fetch past the boxes' re-syncs answered %d %q with %d barriers, want one",
			resp.StatusCode, short(body), rec.linearizable()-before)
	}

	clock.advance(oneResync)
	if n, _ := fetch(first); n == 0 {
		t.Errorf("once the refill had earned one, the run's fetch took %d barriers, want one", n)
	}
	if n, _ := fetch(first); n != 0 {
		t.Errorf("the refill's one re-sync was taken twice: the next took %d barriers", n)
	}
	if got := counted(node.Metrics(), metrics.CodeBoxResyncs, "taken"); got != code.ResyncBurst+1 {
		t.Errorf("%s{outcome=taken} = %d, want %d", metrics.CodeBoxResyncs, got, code.ResyncBurst+1)
	}
	if got := counted(node.Metrics(), metrics.CodeBoxResyncs, "spent"); got != 3 {
		t.Errorf("%s{outcome=spent} = %d, want 3", metrics.CodeBoxResyncs, got)
	}
}

// A SEAT HAS ONE TERM ON A NODE, ITS BOXES' AND ITS TOOLS' TOGETHER: a seat
// whose coding box spent its seat's re-syncs on a node, asking for an object
// nobody pushed, has its turn's read of the same object on that node answered
// at its floor — no barrier, saying the SEAT's re-syncs are spent — because
// the code host's boxes and the query server's reads draw on the node's ONE
// set of buckets (code.NewResyncs), and the census gives the seat one term
// however its work reaches the log's head.
//
// Mutation: the code host keeping its boxes' buckets apart from the query
// server's — the turn's read takes a barrier at the head — red.
func TestASeatsBoxAndItsTurnShareItsTermOnANode(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: rec.wrap},
		ResyncClock: (&manualClock{t: time.Now()}).now})
	node := c.Fleet.Nodes()[0]
	runs := c.Fleet.Runs()
	ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
	missing := randomOID()
	for i := range code.ResyncBurst {
		before := rec.linearizable()
		resp, body := boxDo(t, c, 0, codetest.RepoName, boxUploadPack, host.BoxUser, runs.Token(ref), v2,
			fetchWants([]string{missing}))
		if resp.StatusCode != http.StatusOK || rec.linearizable() == before || !strings.Contains(body, "not our ref") {
			t.Fatalf("re-sync %d of the box's %d answered %d %q with %d barriers, want one and git's own not "+
				"our ref", i+1, code.ResyncBurst, resp.StatusCode, short(body), rec.linearizable()-before)
		}
	}

	before := rec.linearizable()
	_, err := node.GitRead(t.Context(), code.GitQuery{Kind: code.GitTree, RepoID: c.Rows().Repo.ID, Home: true,
		Asker: code.Principal{Kind: code.PrincipalAgent, Seat: codetest.FixtureAlice}, Turn: "turn-1",
		Tree: &code.TreeQuery{Rev: missing}})
	if !errors.Is(err, code.ErrObjectNotFound) ||
		!strings.Contains(err.Error(), "(not re-judged at linearizable: this seat's re-syncs are spent until ") ||
		rec.linearizable() != before {
		t.Errorf("the seat's turn's read of %s on the node its box spent the seat's re-syncs on = %v with %d "+
			"barriers, want it answered at its floor, saying the seat's re-syncs are spent, with none", missing,
			err, rec.linearizable()-before)
	}
}

// A BARRIER A BOX TAKES RAISES ITS FLOOR: a box fetching, through a git node
// whose copy is behind, a commit a person pushed through another takes one
// barrier, and its run's floor is raised to the copy it was served from, which
// every later request of the run, through any node, syncs to at its session.
//
// Mutation: no raise at the missing want — the floor stays at the launch's —
// red.
func TestABoxsBarrierRaisesItsFloor(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2, WrapEstate: rec.wrap}})
	runs := c.Fleet.Runs()
	ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
	launched := runs.Floor(ref)
	_, _ = boxDo(t, c, 1, codetest.RepoName, boxUploadPack, host.BoxUser, runs.Token(ref), v2, lsRefs())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Clone(c.URL(0))
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(p, work, "main")

	before := rec.linearizable()
	if resp, body := boxDo(t, c, 1, codetest.RepoName, boxUploadPack, host.BoxUser, runs.Token(ref), v2,
		fetchWants([]string{head})); resp.StatusCode != http.StatusOK || strings.Contains(body, "ERR ") {
		t.Fatalf("the box's fetch of %s through the node behind answered %d %q", head, resp.StatusCode, short(body))
	}
	if rec.linearizable() == before {
		t.Fatal("the premise: the node behind served the push's commit with no barrier")
	}
	if raised, err := launched.Before(runs.Floor(ref)); err != nil || !raised {
		t.Errorf("the run's floor is %s after its fetch's barrier, want past its launch's %s (%v)",
			runs.Floor(ref), launched, err)
	}
}

// A BOX'S ADMISSION JUDGES A REFUSAL AGAIN ONLY WHILE ITS RE-SYNCS LAST: a run
// given a repository since deleted is told so after one barrier a request —
// the session's answer would refuse, so it is resolved again at the log's
// head — until its sixteen are spent; then the session's answer stands, with
// no barrier.
//
// Mutation: the second resolve taken without the bucket — the seventeenth
// request resolves at linearizable — red.
func TestABoxsRefusedRepositoryIsJudgedAgainWhileItsResyncsLast(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: rec.wrap},
		ResyncClock: (&manualClock{t: time.Now()}).now})
	runs := c.Fleet.Runs()
	ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
	c.Fleet.Remove(t, codetest.RepoName)
	resolves := func() int {
		n := 0
		for _, r := range rec.seen("resolve") {
			if r.level == statelog.ReadLinearizable {
				n++
			}
		}
		return n
	}
	for i := range code.ResyncBurst + 1 {
		before := resolves()
		_, body := boxDo(t, c, 0, codetest.RepoName, boxFetchAdvert, host.BoxUser, runs.Token(ref), "", "")
		if !strings.Contains(body, "deleted") {
			t.Fatalf("request %d of a run whose repository is deleted answered %q, want it told so", i+1,
				short(body))
		}
		judged := resolves() - before
		switch {
		case i < code.ResyncBurst && judged == 0:
			t.Errorf("request %d resolved %d times at the log's head, want once", i+1, judged)
		case i == code.ResyncBurst && judged != 0:
			t.Errorf("past the run's re-syncs request %d resolved %d times at the log's head, want none", i+1,
				judged)
		}
	}
}

// A BOX PAST ITS REQUESTS IN FLIGHT IS BUSY: with sixteen of one run's
// requests held on a node, its seventeenth is answered 503 run_busy at once,
// with a Retry-After and before its run's row is read; another run's request
// is served meanwhile; and once the sixteen have ended, the run is served
// again.
//
// Mutation: no cap (holdRun always granting) — the seventeenth is admitted and
// held with the others — red.
func TestABoxPastItsInFlightIsBusy(t *testing.T) {
	t.Parallel()
	var lines logs
	c, _ := boxCase(t, codetest.SoloOptions{Logger: lines.logger()})
	node := c.Fleet.Nodes()[0]
	runs := c.Fleet.Runs()
	busy, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
	other, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Clone(c.URL(0))
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(p, work, "main")
	_ = p.LsRemote(c.URL(0), 0)

	// EVERY `ls-refs` OF THE RUN IS HELD IN ITS FLOOR'S RAISE — the push
	// above put the repository past the run's floor — until released, and
	// released however the case ends, before the fleet shuts down.
	raised, release := make(chan struct{}, host.MaxRunRequests), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let)
	runs.OnRaise(func(codetest.Raise) error {
		raised <- struct{}{}
		<-release
		return nil
	})
	client := node.Client()
	answered := make(chan reply, host.MaxRunRequests)
	for range host.MaxRunRequests {
		req := boxRequest(t, c, 0, codetest.RepoName, boxUploadPack, host.BoxUser, runs.Token(busy), v2, lsRefs())
		go func() { answered <- boxSend(req, client) }()
	}
	for i := range host.MaxRunRequests {
		select {
		case <-raised:
		case a := <-answered:
			t.Fatalf("request %d of the run answered %d %v before its raise was released", i+1, a.status, a.err)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of the run's %d requests reached their raise in ten seconds", i,
				host.MaxRunRequests)
		}
	}

	reads := runs.Reads()
	last := make(chan reply, 1)
	req := boxRequest(t, c, 0, codetest.RepoName, boxUploadPack, host.BoxUser, runs.Token(busy), v2, lsRefs())
	go func() { last <- boxSend(req, client) }()
	select {
	case a := <-last:
		if a.status != http.StatusServiceUnavailable || a.header.Get("Retry-After") == "" ||
			!strings.Contains(a.body, "this run has 16 requests in flight on this node") {
			t.Errorf("the run's seventeenth request answered %d %q (Retry-After %q), want 503 run_busy", a.status,
				short(a.body), a.header.Get("Retry-After"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run's seventeenth request was admitted with its sixteen in flight")
	}
	if got := runs.Reads() - reads; got != 0 {
		t.Errorf("the busy request read the run's row %d times, want none", got)
	}
	if n := lines.count("code_box_unavailable", `"reason":"run_busy"`); n != 1 {
		t.Errorf("logged run_busy %d times, want once", n)
	}
	if got := counted(node.Metrics(), metrics.CodeBoxInflightRefused, ""); got != 1 {
		t.Errorf("%s = %d, want 1", metrics.CodeBoxInflightRefused, got)
	}
	if resp, body := boxDo(t, c, 0, codetest.RepoName, boxFetchAdvert, host.BoxUser, runs.Token(other), v2,
		""); resp.StatusCode != http.StatusOK {
		t.Errorf("another run's request answered %d %q while the first held its sixteen", resp.StatusCode,
			short(body))
	}

	let()
	for range host.MaxRunRequests {
		if a := <-answered; a.status != http.StatusOK {
			t.Errorf("a held request answered %d %v once released", a.status, a.err)
		}
	}
	eventually(t, "the run's places given back", func() bool { return host.RunRequests(node.Host(), busy.LaunchID) == 0 })
	if resp, body := boxDo(t, c, 0, codetest.RepoName, boxUploadPack, host.BoxUser, runs.Token(busy), v2,
		lsRefs()); resp.StatusCode != http.StatusOK {
		t.Errorf("the run's request once its sixteen ended answered %d %q", resp.StatusCode, short(body))
	}
}

// A RUN'S REQUESTS IN FLIGHT ARE TWICE ITS REPOSITORIES: spelled in this
// package, which imports no sandbox, and held to sandbox's bound here.
//
// Mutation: MaxRunRequests at 8 — red.
func TestARunsRequestsInFlightAreTwiceItsRepositories(t *testing.T) {
	t.Parallel()
	if host.MaxRunRequests != 2*sandbox.MaxRunRepos {
		t.Errorf("host.MaxRunRequests = %d, want twice sandbox.MaxRunRepos (%d)", host.MaxRunRequests,
			sandbox.MaxRunRepos)
	}
}
