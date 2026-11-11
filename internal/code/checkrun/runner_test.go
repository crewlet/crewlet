package checkrun_test

import (
	"errors"
	"go/build"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/checkrun"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// A CHECK ENDS BY ITS COMMAND'S EXIT: the runner claims the queued attempt,
// makes its box in the company's cell, names the box on the row, runs the
// command in its checkout and records it passed for status 0 — its summary
// alone — and failed for any other, with the last of its output; and closes
// the box either way.
//
// Mutation: record every exit passed — the failing command's row says
// passed, red.
func TestACheckEndsByItsCommandsExit(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.runner(f.options("git-a", 2, &order{nodes: []string{"git-a"}}))

	pass := f.queue(oidOf("a"), def("unit", time.Minute))
	box := f.container.box(t)
	running := f.await(pass, "named its box", func(r code.CheckRow) bool { return r.Box != "" })
	if running.Runner != "git-a" || running.Placement != code.PlacementContainer || running.Box != box.ID() {
		t.Fatalf("the running row = %+v, want git-a's container box %s", running, box.ID())
	}
	f.container.end(box, 0, "ok\n")
	if row := f.ended(pass); row.State != code.CheckStatePassed || row.Reason != code.CheckReasonExit ||
		row.ExitCode == nil || *row.ExitCode != 0 || !strings.Contains(row.Summary, "exited 0") {
		t.Fatalf("a command exiting 0 = %+v, want passed by its exit, its summary saying so", row)
	}
	if bg := box.Background(); len(bg) != 1 || !strings.Contains(bg[0], "cd 'web' && sh -c 'make unit'") {
		t.Fatalf("the box ran %q, want the check's command in its checkout", bg)
	}

	fail := f.queue(oidOf("b"), def("unit", time.Minute))
	box = f.container.box(t)
	f.await(fail, "named its box", func(r code.CheckRow) bool { return r.Box == box.ID() })
	f.container.end(box, 2, "building\nFAIL: TestAThing\n")
	row := f.ended(fail)
	if row.State != code.CheckStateFailed || row.ExitCode == nil || *row.ExitCode != 2 {
		t.Fatalf("a command exiting 2 = %+v, want failed by its exit", row)
	}
	if tail := f.detail(fail).Tail; !strings.Contains(tail, "FAIL: TestAThing") {
		t.Errorf("the failed run's tail = %q, want its output's end", tail)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !box.Closed() && time.Now().Before(deadline) { //fixedwait:measure a poll's own deadline: whether the box closed is asserted after it
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
	if !box.Closed() {
		t.Error("the box of an ended check was never closed")
	}
}

// A CHECK'S BOX HOLDS NO MODEL'S CREDENTIAL: its environment is EXACTLY the
// company's check secrets its definition names — resolved — what the check is,
// and Gitlet's base and the check's own token with git's configuration
// for them; no secret it did not name, no harness's sign-in, no login's
// files and no commit identity.
//
// Mutation: hand the box every code.checks.env entry, or the process's own
// model keys — the unnamed secret or ANTHROPIC_API_KEY reaches it, red; run
// the check's command with ANTHROPIC_API_KEY — the command was run with it,
// red.
func TestACheckBoxHoldsNoModelCredential(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-the-engine's-own")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "the-engine's-plan")
	f := newFleet(t)
	f.company.Code.Checks.Env = map[string]string{"NPM_TOKEN": "${NPM_SECRET}", "DEPLOY_KEY": "never-named"}
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))

	d := def("unit", time.Minute)
	d.Env = []string{"NPM_TOKEN"}
	key := f.queue(oidOf("c"), d)
	box := f.container.box(t)
	spec := f.container.spec(0)
	if spec.Harness != "" || len(spec.CredentialFiles) > 0 || len(spec.Repos) > 0 {
		t.Fatalf("a check's box was made with an agent %q, %d login files, %d run grants",
			spec.Harness, len(spec.CredentialFiles), len(spec.Repos))
	}
	allowed := append([]string{"NPM_TOKEN", sandbox.GitURLVar, sandbox.GitTokenVar, "GIT_CONFIG_COUNT"},
		config.ReservedCheckEnv()...)
	for name, value := range spec.Env {
		switch {
		case strings.HasPrefix(name, "GIT_CONFIG_KEY_"):
			if value == "user.name" || value == "user.email" {
				t.Errorf("a check's box is given a commit identity (%s): it acts as nobody", value)
			}
		case strings.HasPrefix(name, "GIT_CONFIG_VALUE_"):
		case !slices.Contains(allowed, name):
			t.Errorf("a check's box holds %s, which nothing gives it", name)
		}
	}
	if got := spec.Env["NPM_TOKEN"]; got != "npm-from-the-environment" {
		t.Errorf("NPM_TOKEN = %q, want the named secret resolved", got)
	}
	for name, want := range map[string]string{"CREWLET_CHECK_REPO": "web", "CREWLET_CHECK_SHA": key.OID,
		"CREWLET_CHECK_NAME": "unit", "CREWLET_CHECK_ATTEMPT": "1",
		"CREWLET_CHECK_API": codeBase + "/web", sandbox.GitURLVar: gitBase} {
		if got := spec.Env[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if tok := spec.Env[sandbox.GitTokenVar]; !strings.HasPrefix(tok, "chk-web-unit-1-") {
		t.Errorf("the box's token is %q, want its check's", tok)
	}
	f.container.end(box, 0, "")
	f.ended(key)
	// Read once the attempt has ENDED, never once its box is made: the box
	// is handed to the case before the runner sets it up and long before it
	// starts the command, so a read between sees only what ran by then.
	if bg := box.Background(); len(bg) != 1 {
		t.Fatalf("the premise: the box ran the check's command once, it ran %q", bg)
	}
	for _, exec := range box.Execs() {
		for name := range exec.Env {
			if strings.Contains(name, "ANTHROPIC") || strings.Contains(name, "CLAUDE") {
				t.Errorf("a command of the check's box was run with %s", name)
			}
		}
	}
}

// A CHECK SPENDS NO TOKENS: nothing it runs is a model's, so the runner holds
// no seam an LLM's spend or a budget is charged through — it imports none of
// the packages that charge one — and what an ended attempt records of its cost
// is its duration alone.
//
// THE RUNNER'S CLOCK IS THE CASE'S, frozen an hour into tomorrow (past the
// attempt's queuing, so it falls due at once) and moved only by the case, a
// second and a half while the command runs: the duration is the span between
// the runner's own two readings, claim and ending, so it is exactly what the
// case moved, and no wall time has to pass for it.
//
// Mutation: import the LLM provider layer or the spend ledger into the runner,
// red; record no duration, or read it off the wall clock rather than the
// runner's, red.
func TestACheckSpendsNoTokens(t *testing.T) {
	t.Parallel()
	p, err := build.Default.ImportDir(filepath.Join(sourcetree.Root(t), "internal", "code", "checkrun"), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range p.Imports {
		for _, charged := range []string{"/internal/providers/llm", "/internal/auxspend", "/internal/costlimit",
			"/internal/usage", "/internal/agent/"} {
			if strings.Contains(imp, charged) {
				t.Errorf("the check runner imports %s: a check charges no model's spend", imp)
			}
		}
	}
	f := newFleet(t)
	clock := &clock{at: time.Now().UTC().Truncate(24 * time.Hour).Add(25 * time.Hour)}
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Now = clock.now
	f.runner(o)
	key := f.queue(oidOf("d"), def("unit", time.Minute))
	box := f.container.box(t)
	f.await(key, "named its box", func(r code.CheckRow) bool { return r.Box != "" })
	const took = 1500 * time.Millisecond
	clock.advance(took)
	f.container.end(box, 0, "")
	if row := f.ended(key); row.DurationMs != took.Milliseconds() {
		t.Errorf("an ended check's duration = %d ms, want the %d ms its run took on the runner's clock",
			row.DurationMs, took.Milliseconds())
	}
}

// A LOSING CLAIM STARTS NO BOX: two runners that each take themselves for the
// attempt's first rank claim it at once, the repository's checks writer
// refuses the second `claimed`, and only the holder makes a box — one box for
// the attempt, whichever node wins.
//
// Mutation: make the box before the claim (create-then-claim) — two boxes,
// red.
func TestALosingClaimStartsNoBox(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	other := newCell()
	m, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.Container: other},
		DefaultPlacement: sandbox.Container,
		Runners:          map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a", "git-b"}}))
	b := f.options("git-b", 1, &order{nodes: []string{"git-b", "git-a"}})
	b.Boxes = func() (checkrun.Boxes, bool) { return m, true }
	f.runner(b)
	for i, oid := range []string{oidOf("1"), oidOf("2"), oidOf("3"), oidOf("4")} {
		key := f.queue(oid, def("unit", time.Minute))
		row := f.await(key, "named its box", func(r code.CheckRow) bool { return r.Box != "" })
		if made := f.container.count() + other.count(); made != i+1 {
			t.Fatalf("after %d checks %d boxes were made (git-a %d, git-b %d): a lost claim made one", i+1, made,
				f.container.count(), other.count())
		}
		winner := f.container
		if row.Runner == "git-b" {
			winner = other
		}
		winner.end(winner.box(t), 0, "")
		f.ended(key)
	}
}

// A DEAD RUNNER'S CHECK RUNS AT MOST THREE ATTEMPTS: a runner that dies
// holding its claim — a crash at CheckAfterClaim, no box made and no ending
// recorded — and leaves the repository's git order has its attempt ended
// `errored runner_lost` by the repository's home; the home queues it again
// (what its checks worker does, m5-k5, stood in for here), a runner dies on it
// again, and past the third attempt a requeue is refused attempts_exhausted.
//
// Mutation: sweep only rows past their timeout — the lost runner's attempt is
// never ended, red.
func TestADeadRunnersCheckRunsAtMostThreeAttempts(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.faults.Arm(codefault.CheckAfterClaim, codefault.Action{Err: errors.New("the runner died")})
	f.runner(f.options("git-dead", 1, &order{nodes: []string{"git-dead"}}))
	gone := &order{nodes: []string{"git-home"}}
	sweep, err := checkrun.NewSweeper(checkrun.SweeperOptions{Estate: estate{f.d}, Order: gone,
		Boxes: func() (checkrun.Boxes, bool) { return f.manager, true }, Poll: fast.Poll})
	if err != nil {
		t.Fatal(err)
	}
	d := def("unit", time.Minute)
	key := f.queue(oidOf("e"), d)
	for attempt := 1; ; attempt++ {
		key.Attempt = attempt
		f.await(key, "claimed by the runner that dies", func(r code.CheckRow) bool {
			return r.State == code.CheckStateRunning && r.Runner == "git-dead"
		})
		repos, _, err := sweep.Due(t.Context(), statelogZero, "")
		if err != nil || !slices.Contains(repos, f.repo) {
			t.Fatalf("the home's listing = (%v, %v), want web, which holds a running check", repos, err)
		}
		if err := sweep.Work(t.Context(), f.repo, statelogZero); err != nil {
			t.Fatalf("the home's sweep: %v", err)
		}
		row := f.ended(key)
		if row.State != code.CheckStateErrored || row.Reason != code.CheckReasonRunnerLost {
			t.Fatalf("attempt %d of a dead runner's check = %+v, want errored runner_lost", attempt, row)
		}
		next := key
		next.Attempt++
		res, err := f.queueAs(next, d, code.QueueRequeue)
		if attempt < code.AutoRequeueAttempts {
			if err != nil || res.Refusal != "" {
				t.Fatalf("the requeue of attempt %d = (%+v, %v)", attempt, res, err)
			}
			continue
		}
		if err != nil || res.Refusal != code.CheckRefusalAttemptsExhausted {
			t.Fatalf("a requeue past attempt %d = (%+v, %v), want attempts_exhausted", attempt, res, err)
		}
		break
	}
	if n := f.container.count(); n != 0 {
		t.Errorf("a runner that died at its claim made %d boxes", n)
	}
}

// A CANCELLED ROW STOPS ITS BOX: an attempt ended by somebody else while it
// runs — its head moved on — is found ended at the runner's next poll, its box
// closed, and nothing more recorded: the row keeps the cancellation.
//
// The runner's clock is the case's, frozen an hour into tomorrow: past the
// attempt's queuing, so it falls due at once, and never past its one-minute
// timeout, so its run ends by its row's cancellation or not at all. And the
// run has returned once its slot is free (Runner.run gives it back last), so
// whatever it records is recorded by then.
//
// Mutation: poll the box alone, never the row — the box runs on, red; record
// the run's own ending over the row's — a measurement, which replaces the
// cancellation — red.
func TestACancelledRowStopsItsBox(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	clock := &clock{at: time.Now().UTC().Truncate(24 * time.Hour).Add(25 * time.Hour)}
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Now = clock.now
	r := f.runner(o)
	key := f.queue(oidOf("f"), def("unit", time.Minute))
	box := f.container.box(t)
	f.await(key, "named its box", func(r code.CheckRow) bool { return r.Box == box.ID() })
	res, err := f.d.Served().PublishChecks(t.Context(), code.ChecksRequest{OpID: newOpID("code_check_cancel"),
		Finished: &code.CheckFinished{CheckKey: key, State: code.CheckStateCancelled,
			Reason: code.CheckReasonSuperseded, FinishedAt: clock.now()}})
	if err != nil || res.Refusal != "" {
		t.Fatalf("cancel the running attempt: (%+v, %v)", res, err)
	}
	eventually(t, "the cancelled attempt's run returned", func() bool {
		return r.Held(config.PlacementContainer) == 0
	})
	if !box.Closed() {
		t.Error("the run of a cancelled attempt returned with its box open")
	}
	if row, _ := f.row(key); row.State != code.CheckStateCancelled || row.Reason != code.CheckReasonSuperseded {
		t.Errorf("the cancelled attempt's row = %+v, want its cancellation kept", row)
	}
}

// GIT.MAX_CHECKS BOUNDS CONTAINER CELLS: a runner of two container slots runs
// two of five queued container checks and leaves three queued; each that ends
// frees a slot the next takes; and a remote (e2b) check runs beside them,
// taking none. The three are read as queued once a walk begun after the
// remote check's claim, and after every run started has made its box, has
// ended (walks.walked): that check, queued last, was claimed by a walk that
// took the queue oldest first, so it had considered every container check,
// and the walk after it considered what was left.
//
// Mutation: claim whatever is queued, slots or not — five boxes at once, red;
// take a container slot past max_checks on the walks after the first — the
// walk after the claims takes a third, red.
func TestMaxChecksBoundsContainerCells(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	o := f.options("git-a", 2, &order{nodes: []string{"git-a"}})
	var w walks
	w.counting(&o)
	r := f.runner(o)
	var keys []code.CheckKey
	for _, s := range []string{"1", "2", "3", "4", "5"} {
		keys = append(keys, f.queue(oidOf(s), def("unit", time.Minute)))
	}
	remote := def("lint", time.Minute)
	remote.RunIn = code.CellE2B
	lint := f.queue(oidOf("6"), remote)
	first, second := f.container.box(t), f.container.box(t)
	f.e2b.box(t)
	f.await(lint, "run remotely", func(row code.CheckRow) bool { return row.Placement == code.PlacementE2B })
	w.walked(t, r, 1)
	if n := f.container.count(); n != 2 || r.Held(config.PlacementContainer) != 2 {
		t.Fatalf("a runner of two slots made %d container boxes and holds %d", n, r.Held(config.PlacementContainer))
	}
	queued := 0
	for _, key := range keys {
		if row, _ := f.row(key); row.State == code.CheckStateQueued {
			queued++
		}
	}
	if queued != 3 {
		t.Fatalf("%d of five container checks wait queued, want three", queued)
	}
	f.container.end(first, 0, "")
	third := f.container.box(t)
	if n := f.container.count(); n != 3 {
		t.Fatalf("a slot freed made %d container boxes in all, want three", n)
	}
	f.container.end(second, 0, "")
	f.container.end(third, 0, "")
}

// A TIMED OUT CHECK FAILS: a command still running past its check's own
// timeout is stopped — its box closed — and its attempt FAILED `timed_out`,
// a measurement of the commit, with the last of its output.
//
// Mutation: end it errored — infrastructure's — and the row says errored, red.
func TestATimedOutCheckFails(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	key := f.queue(oidOf("7"), def("slow", time.Second))
	box := f.container.box(t)
	box.Put(box.Home()+"/.crewlet-check/output", "still going\n")
	row := f.ended(key)
	if row.State != code.CheckStateFailed || row.Reason != code.CheckReasonTimedOut || row.ExitCode != nil {
		t.Fatalf("a command past its timeout = %+v, want failed timed_out", row)
	}
	if tail := f.detail(key).Tail; !strings.Contains(tail, "still going") {
		t.Errorf("the timed out run's tail = %q, want its output's end", tail)
	}
	if !box.Closed() {
		t.Error("a timed out check's box was left open")
	}
}

// A CHECK NAMING A CELL THE COMPANY DOES NOT ALLOW RUNS NOWHERE: the node
// whose turn it is ends it `errored defs_invalid`, saying which cell and what
// the company allows, and makes no box.
func TestACheckInACellNotAllowedEndsUnrun(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.company.Code.Checks.AllowRunIn = nil
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	d := def("unit", time.Minute)
	d.RunIn = code.CellE2B
	key := f.queue(oidOf("8"), d)
	row := f.ended(key)
	if row.State != code.CheckStateErrored || row.Reason != code.CheckReasonDefsInvalid ||
		!strings.Contains(row.Summary, "does not allow") {
		t.Fatalf("a check in a cell not allowed = %+v, want errored defs_invalid saying why", row)
	}
	if n := f.e2b.count() + f.container.count(); n != 0 {
		t.Errorf("%d boxes were made for a check no cell runs", n)
	}
}

// A RUNNER RANKS AN ATTEMPT BY ITS REPOSITORY'S GIT ORDER: the node at rank
// one claims only once the attempt has waited a stagger, so the home — rank
// zero — claims first wherever it can; and a home with no free cell leaves
// the attempt to the runner-up a stagger later.
//
// THE RUNNERS' CLOCK IS THE CASE'S: a rank falls due by the clock alone, so
// the home's turn is never cut short by a runner too loaded to walk within a
// stagger of the wall clock — which handed the first attempt to the
// runner-up, rightly, while the home had not walked yet — and the
// runner-up's comes only when the case moves the clock a stagger on.
//
// Mutation: claim at every rank at once — the runner-up takes the attempt
// short of its stagger, red.
func TestTheHomeClaimsFirstAndItsRunnerUpAStaggerLater(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	other := newCell()
	m, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.Container: other},
		DefaultPlacement: sandbox.Container,
		Runners:          map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{}
	nodes := []string{"git-home", "git-next"}
	home := f.options("git-home", 1, &order{nodes: nodes})
	home.Now = clk.now
	f.runner(home)
	ranked := &countedOrder{order: order{nodes: nodes}}
	next := f.options("git-next", 1, ranked)
	next.Boxes = func() (checkrun.Boxes, bool) { return m, true }
	next.Now = clk.now
	f.runner(next)

	first := f.queue(oidOf("9"), def("unit", time.Minute))
	clk.set(f.queuedAt(first))
	if row := f.await(first, "claimed", func(r code.CheckRow) bool { return r.Runner != "" }); row.Runner !=
		"git-home" {
		t.Fatalf("the first attempt was claimed by %s, want its home", row.Runner)
	}
	// THE HOME'S ONE SLOT IS HELD: the next attempt is its runner-up's, once
	// it has waited a stagger — and not at any walk before. A third walk
	// ranking it begins only once the second has ended, so by then two
	// walks have ranked it a millisecond short, the second wholly after the
	// clock moved, and a claim either made is on the row.
	second := f.queue(oidOf("b"), def("unit", time.Minute))
	queued := f.queuedAt(second)
	clk.set(queued.Add(fast.Stagger - time.Millisecond))
	walked := ranked.asked.Load()
	early := func() {
		if row, _ := f.row(second); row.Runner != "" {
			t.Fatalf("with the home full the attempt was claimed by %s a millisecond short of a stagger (%s)",
				row.Runner, fast.Stagger)
		}
	}
	eventually(t, "three walks of the runner-up ranking the attempt", func() bool {
		early()
		return ranked.asked.Load() >= walked+3
	})
	early()
	clk.set(queued.Add(fast.Stagger))
	if row := f.await(second, "claimed", func(r code.CheckRow) bool { return r.Runner != "" }); row.Runner !=
		"git-next" {
		t.Fatalf("with the home full the attempt was claimed by %s once it had waited a stagger, want git-next",
			row.Runner)
	}
	f.container.end(f.container.box(t), 0, "")
	other.end(other.box(t), 0, "")
}

// countedOrder is a git order that counts the walks ranking an attempt by it:
// a runner asks its order once a walk for each repository it holds a queued
// attempt of (Runner.rank).
type countedOrder struct {
	order
	asked atomic.Int64
}

func (o *countedOrder) GitOrder(repo string) ([]string, error) {
	o.asked.Add(1)
	return o.order.GitOrder(repo)
}

// A RUNNER NEEDS BOTH OF A BOX'S BASES: Gitlet's, the one a box clones from,
// which New refuses unless it ends in /box/git, and the one a check reads its
// commit at without a clone, which New refuses unset — a box told neither
// would fail its checkout or its first read with nothing to say why.
func TestARunnerNeedsBothBoxBases(t *testing.T) {
	t.Parallel()
	o := wholeOptions()
	for _, base := range []string{"", "https://git.example.com", "https://git.example.com/git"} {
		o.GitBase = base
		if _, err := checkrun.New(o); !errors.Is(err, checkrun.ErrOptions) {
			t.Errorf("New with GitBase %q = %v, want ErrOptions", base, err)
		}
	}
	o = wholeOptions()
	o.CodeBase = ""
	if _, err := checkrun.New(o); !errors.Is(err, checkrun.ErrOptions) || !strings.Contains(err.Error(), "CodeBase") {
		t.Errorf("New with no CodeBase = %v, want ErrOptions naming CodeBase", err)
	}
	if _, err := checkrun.New(wholeOptions()); err != nil {
		t.Errorf("New with GitBase %q and CodeBase %q: %v", gitBase, codeBase, err)
	}
}

// wholeOptions are a runner's options with every one New requires.
func wholeOptions() checkrun.Options {
	return checkrun.Options{NodeID: "git-a", Estate: estate{}, Order: &order{},
		Boxes:   func() (checkrun.Boxes, bool) { return nil, false },
		Company: func() (checkrun.Company, bool) { return checkrun.Company{}, false }, Tokens: tokens{},
		Leases: memory.New(), Minutes: memory.NewFleet(), GitBase: gitBase, CodeBase: codeBase}
}

// A RUNNER'S TIMING IS WHOLE OR UNSET: a cadence of zero is no cadence — a
// slot renewed every zero seconds panics its ticker — so New refuses a Timing
// with any cadence unset, and takes a whole one or none (Production).
func TestARunnerTakesAWholeTimingOrNone(t *testing.T) {
	t.Parallel()
	noRenew := fast
	noRenew.Renew = 0
	for name, timing := range map[string]checkrun.Timing{
		"a poll alone": {Poll: time.Second}, "every cadence but the renewal": noRenew,
	} {
		o := wholeOptions()
		o.Timing = timing
		if _, err := checkrun.New(o); !errors.Is(err, checkrun.ErrOptions) {
			t.Errorf("New with %s = %v, want ErrOptions", name, err)
		}
	}
	for name, timing := range map[string]checkrun.Timing{"none": {}, "a whole one": fast} {
		o := wholeOptions()
		o.Timing = timing
		if _, err := checkrun.New(o); err != nil {
			t.Errorf("New with %s: %v", name, err)
		}
	}
}

// A RUNNER NEEDS THE COMPANY'S COORDINATION STORE: the leases a remote box's
// slot is held on and the counter its minutes are charged to, without which no
// remote cap could hold — New refuses either absent, naming it.
func TestARunnerNeedsTheCoordinationStore(t *testing.T) {
	t.Parallel()
	for name, drop := range map[string]func(o *checkrun.Options){
		"Leases":  func(o *checkrun.Options) { o.Leases = nil },
		"Minutes": func(o *checkrun.Options) { o.Minutes = nil },
	} {
		o := wholeOptions()
		drop(&o)
		if _, err := checkrun.New(o); !errors.Is(err, checkrun.ErrOptions) || !strings.Contains(err.Error(), name) {
			t.Errorf("New without %s = %v, want ErrOptions naming it", name, err)
		}
	}
}
