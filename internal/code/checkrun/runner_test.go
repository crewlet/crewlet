package checkrun_test

import (
	"errors"
	"go/build"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/checkrun"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/config"
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
	for !box.Closed() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !box.Closed() {
		t.Error("the box of an ended check was never closed")
	}
}

// A CHECK'S BOX HOLDS NO MODEL'S CREDENTIAL: its environment is EXACTLY the
// company's check secrets its definition names — resolved — what the check is,
// and the code host's base and the check's own token with git's configuration
// for them; no secret it did not name, no coding agent's sign-in, no login's
// files and no commit identity.
//
// Mutation: hand the box every code.checks.env entry, or the process's own
// model keys — the unnamed secret or ANTHROPIC_API_KEY reaches it, red.
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
	if spec.CodingAgent != "" || len(spec.CredentialFiles) > 0 || len(spec.Repos) > 0 || len(spec.Mounts) > 0 {
		t.Fatalf("a check's box was made with an agent %q, %d login files, %d run grants, %d mounts",
			spec.CodingAgent, len(spec.CredentialFiles), len(spec.Repos), len(spec.Mounts))
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
		"CREWLET_CHECK_API": "https://git.example.com/box/code/web", sandbox.GitURLVar: gitBase} {
		if got := spec.Env[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if tok := spec.Env[sandbox.GitTokenVar]; !strings.HasPrefix(tok, "chk-web-unit-1-") {
		t.Errorf("the box's token is %q, want its check's", tok)
	}
	for _, exec := range box.Execs() {
		for name := range exec.Env {
			if strings.Contains(name, "ANTHROPIC") || strings.Contains(name, "CLAUDE") {
				t.Errorf("a command of the check's box was run with %s", name)
			}
		}
	}
	f.container.end(box, 0, "")
	f.ended(key)
}

// A CHECK SPENDS NO TOKENS: nothing it runs is a model's, so the runner holds
// no seam an LLM's spend or a budget is charged through — it imports none of
// the packages that charge one — and what an ended attempt records of its cost
// is its duration alone.
//
// Mutation: import the LLM provider layer or the spend ledger into the runner,
// red.
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
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	key := f.queue(oidOf("d"), def("unit", time.Minute))
	box := f.container.box(t)
	f.await(key, "named its box", func(r code.CheckRow) bool { return r.Box != "" })
	time.Sleep(30 * time.Millisecond)
	f.container.end(box, 0, "")
	if row := f.ended(key); row.DurationMs <= 0 {
		t.Errorf("an ended check's duration = %d ms, want the time its run took", row.DurationMs)
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
// Mutation: poll the box alone, never the row — the box runs on, red.
func TestACancelledRowStopsItsBox(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	key := f.queue(oidOf("f"), def("unit", time.Minute))
	box := f.container.box(t)
	f.await(key, "named its box", func(r code.CheckRow) bool { return r.Box == box.ID() })
	res, err := f.d.Served().PublishChecks(t.Context(), code.ChecksRequest{OpID: newOpID("code_check_cancel"),
		Finished: &code.CheckFinished{CheckKey: key, State: code.CheckStateCancelled,
			Reason: code.CheckReasonSuperseded, FinishedAt: time.Now().UTC()}})
	if err != nil || res.Refusal != "" {
		t.Fatalf("cancel the running attempt: (%+v, %v)", res, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !box.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("the box of a cancelled attempt was never closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(10 * fast.Poll)
	if row, _ := f.row(key); row.State != code.CheckStateCancelled || row.Reason != code.CheckReasonSuperseded {
		t.Errorf("the cancelled attempt's row = %+v, want its cancellation kept", row)
	}
}

// GIT.MAX_CHECKS BOUNDS CONTAINER CELLS: a runner of two container slots runs
// two of five queued container checks and leaves three queued; each that ends
// frees a slot the next takes; and a remote (e2b) check runs beside them,
// taking none.
//
// Mutation: claim whatever is queued, slots or not — five boxes at once, red.
func TestMaxChecksBoundsContainerCells(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	r := f.runner(f.options("git-a", 2, &order{nodes: []string{"git-a"}}))
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
	time.Sleep(20 * fast.Walk)
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
// Mutation: claim at every rank at once — the runner-up takes attempts the
// home would, red.
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
	ranked := &order{nodes: []string{"git-home", "git-next"}}
	f.runner(f.options("git-home", 1, ranked))
	next := f.options("git-next", 1, ranked)
	next.Boxes = func() (checkrun.Boxes, bool) { return m, true }
	f.runner(next)
	first := f.queue(oidOf("9"), def("unit", time.Minute))
	if row := f.await(first, "claimed", func(r code.CheckRow) bool { return r.Runner != "" }); row.Runner !=
		"git-home" {
		t.Fatalf("the first attempt was claimed by %s, want its home", row.Runner)
	}
	// THE HOME'S ONE SLOT IS HELD: the next attempt is its runner-up's, a
	// stagger after it was queued.
	queued := time.Now()
	second := f.queue(oidOf("b"), def("unit", time.Minute))
	row := f.await(second, "claimed", func(r code.CheckRow) bool { return r.Runner != "" })
	if row.Runner != "git-next" || time.Since(queued) < fast.Stagger {
		t.Fatalf("with the home full the attempt was claimed by %s after %s, want git-next after a stagger "+
			"(%s)", row.Runner, time.Since(queued), fast.Stagger)
	}
	f.container.end(f.container.box(t), 0, "")
	other.end(other.box(t), 0, "")
}

// A RUNNER NEEDS THE CODE HOST'S BASE as a box dials it: New refuses one that
// does not end in /box/git, since its /box/code sibling is where a check reads
// its commit without a clone.
func TestARunnerNeedsTheBoxGitBase(t *testing.T) {
	t.Parallel()
	o := checkrun.Options{NodeID: "git-a", Estate: estate{}, Order: &order{},
		Boxes:   func() (checkrun.Boxes, bool) { return nil, false },
		Company: func() (checkrun.Company, bool) { return checkrun.Company{}, false }, Tokens: tokens{}}
	for _, base := range []string{"", "https://git.example.com", "https://git.example.com/git"} {
		o.GitBase = base
		if _, err := checkrun.New(o); !errors.Is(err, checkrun.ErrOptions) {
			t.Errorf("New with GitBase %q = %v, want ErrOptions", base, err)
		}
	}
	o.GitBase = gitBase
	if _, err := checkrun.New(o); err != nil {
		t.Errorf("New with GitBase %q: %v", gitBase, err)
	}
}
