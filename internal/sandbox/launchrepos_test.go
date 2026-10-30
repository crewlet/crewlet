package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// The repositories the cases give a run, valid by [RunRepo.ValidateFor] a
// coding run ([CloneCoding]).
const (
	webRepoID = "0190d8f0-0000-7000-8000-00000000000a"
	apiRepoID = "0190d8f0-0000-7000-8000-00000000000b"
	runBase   = "0123456789abcdef0123456789abcdef01234567"
)

// granted is a repository a run is given to write, at runBase, on the seat's
// branch for its work item, with a cone.
func granted(name, id string) RunRepo {
	return RunRepo{
		Repo: name, RepoID: id, Base: runBase, Branch: "seats/swe/ENG-7",
		Access: RepoWrite, Paths: []string{"src"},
	}
}

// withRepos is req given repos the way the engine gives a run them: on the
// spec, with the steps that clone them — the version check, then a coding
// recipe for each — and the floor their resolution read.
func withRepos(t *testing.T, req LaunchRequest, repos ...RunRepo) LaunchRequest {
	t.Helper()
	req.Spec.Repos = repos
	req.CodeFloor = "code@1:40"
	req.Clone = []SetupStep{GitVersionStep()}
	for _, r := range repos {
		step, err := CloneRecipe(r, CloneCoding)
		if err != nil {
			t.Fatalf("CloneRecipe(%s): %v", r.Repo, err)
		}
		req.Clone = append(req.Clone, step)
	}
	return req
}

// A LAUNCH WRITES THE JOB'S GRANTS AND ITS FLOOR ON THE ROW BEFORE ITS BOX
// EXISTS: the row is what a box's request to the code host is admitted
// against, and that request can come before the launch returns. The launch
// floor is the floor the job opened at.
//
// Mutation: leave Repos off the run BeginLaunch is handed — the row at the
// box's creation carries none, red.
func TestALaunchWritesItsGrantsOnTheRowBeforeItsBoxExists(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	witness := &grantWitness{FakeProvider: rig.provider, store: rig.pending, turnID: "t1"}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: witness},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	req := withRepos(t, launchReq("t1"), granted("web", webRepoID), granted("api", apiRepoID))
	if _, err := rig.launchVia(t.Context(), manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !witness.checked {
		t.Fatal("the launch never created a box, so the row was not read at its creation")
	}
	if !slices.EqualFunc(witness.seen.Repos, req.Spec.Repos, reposEqual) {
		t.Fatalf("at the box's creation the row grants %+v, want %+v", witness.seen.Repos, req.Spec.Repos)
	}
	if witness.seen.CodeFloor != "code@1:40" || witness.seen.CodeLaunch != "code@1:40" {
		t.Fatalf("at the box's creation the row's floor is %q and its launch floor %q, want code@1:40 for both",
			witness.seen.CodeFloor, witness.seen.CodeLaunch)
	}
}

// reposEqual compares two grants field by field, the cone in order.
func reposEqual(a, b RunRepo) bool {
	return a.Repo == b.Repo && a.RepoID == b.RepoID && a.Base == b.Base && a.Branch == b.Branch &&
		a.Access == b.Access && slices.Equal(a.Paths, b.Paths)
}

// grantWitness is a provider that reads the run's row at the moment it is
// asked for a box.
type grantWitness struct {
	*FakeProvider
	store   PendingStore
	turnID  string
	checked bool
	seen    PendingRun
}

func (w *grantWitness) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	run, found, err := w.store.Get(ctx, w.turnID)
	if err != nil {
		return nil, err
	}
	w.checked = found
	w.seen = run
	return w.FakeProvider.Create(ctx, spec)
}

// A LAUNCH WHOSE GRANTS ITS ROW COULD NOT CARRY IS REFUSED BEFORE ANYTHING IS
// WRITTEN: more repositories than a run is given, one that is not a RunRepo,
// one named twice — by its name or by its id, either giving one repository two
// grants and one checkout two clones — a floor that does not go with its
// repositories, and a colleague's ceiling on a run given none, which would
// bound nothing. Nothing is on the store and no box was made.
//
// Mutation: drop checkRepos from Launch — the duplicate and floor rows open a
// row, red.
func TestALaunchRefusesGrantsItsRowCouldNotCarry(t *testing.T) {
	t.Parallel()
	many := make([]RunRepo, MaxRunRepos+1)
	for i := range many {
		many[i] = granted(fmt.Sprintf("repo%d", i), fmt.Sprintf("0190d8f0-0000-7000-8000-%012x", i+1))
	}
	badBranch := granted("web", webRepoID)
	badBranch.Branch = "-x"
	for _, tc := range []struct {
		name string
		req  func(t *testing.T) LaunchRequest
		want string
	}{
		{"more repositories than a run is given", func(t *testing.T) LaunchRequest {
			return withRepos(t, launchReq("t1"), many[:MaxRunRepos]...).withSpecRepos(many)
		}, "at most 8 repositories"},
		{"a repository its grammar refuses", func(t *testing.T) LaunchRequest {
			return launchReq("t1").withSpecRepos([]RunRepo{badBranch}).withFloor("code@1:40")
		}, "not a branch name"},
		{"one repository named twice", func(t *testing.T) LaunchRequest {
			return withRepos(t, launchReq("t1"), granted("web", webRepoID), granted("web", apiRepoID))
		}, "given twice"},
		{"one repository's id given twice", func(t *testing.T) LaunchRequest {
			return withRepos(t, launchReq("t1"), granted("web", webRepoID), granted("api", webRepoID))
		}, "given twice"},
		{"repositories with no floor", func(t *testing.T) LaunchRequest {
			return withRepos(t, launchReq("t1"), granted("web", webRepoID)).withFloor("")
		}, "needs the code floor"},
		{"a floor with no repositories", func(t *testing.T) LaunchRequest {
			return launchReq("t1").withFloor("code@1:40")
		}, "has no code floor"},
		{"a colleague's ceiling with no repositories", func(t *testing.T) LaunchRequest {
			r := launchReq("t1")
			r.CodeCeiling = "designer"
			return r
		}, "no colleague's ceiling on them"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRig(t)
			_, err := rig.launchVia(t.Context(), rig.manager, tc.req(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Launch = %v, want a refusal saying %q", err, tc.want)
			}
			if run, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
				t.Fatalf("a refused launch left a row (%v, %v): %+v", found, err, run)
			}
			if box := rig.provider.Box("box-1"); box != nil {
				t.Fatal("a refused launch made a box")
			}
		})
	}
}

// withSpecRepos is r with its spec's repositories set and nothing else.
func (r LaunchRequest) withSpecRepos(repos []RunRepo) LaunchRequest {
	r.Spec.Repos = repos
	return r
}

// withFloor is r with its code floor set and nothing else.
func (r LaunchRequest) withFloor(floor string) LaunchRequest {
	r.CodeFloor = floor
	return r
}

// THE LAUNCH'S OWN ENVIRONMENT REACHES EVERY SETUP STEP AND THE RUN. A box is
// given its repositories by setup steps that clone them with the job's own
// credential, which the engine mints for the launch the store has just named
// ([LaunchRequest.RunEnv]): asked once, with the row's launch id, its
// variables are in the environment of the operator's setup command, of every
// clone step and of the coding agent, winning over an operator's value of the
// same name, and the clone steps run after the operator's. The run's telemetry
// is minted at the job's start and reaches the run alone. The spec the caller
// built keeps its own map.
//
// Mutation: provision with req.Spec rather than the launch's — the setup
// commands' environment lacks the token, red.
func TestTheLaunchEnvironmentReachesSetupAndTheRun(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	receiver := testReceiver(t)
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: rig.provider},
		Runners:   map[string]Runner{"claude-code": rig.runner},
		Telemetry: receiver,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	req := withRepos(t, launchReq("t1"), granted("web", webRepoID))
	req.Spec.TimeoutSec = 900
	req.Spec.Env = map[string]string{"OPERATOR": "op", "CREWLET_GIT_TOKEN": "the operator's"}
	specEnv := maps.Clone(req.Spec.Env)
	req.Setup = []SetupStep{{Name: "operator", Commands: []string{"echo provisioned"}, Brief: "Provisioned."}}
	var asked []string
	req.RunEnv = func(launchID string) (map[string]string, error) {
		asked = append(asked, launchID)
		return map[string]string{"CREWLET_GIT_TOKEN": "token-for-" + launchID}, nil
	}

	res, err := rig.launchVia(t.Context(), manager, req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	launch := rig.get("t1").LaunchID
	if !slices.Equal(asked, []string{launch}) {
		t.Fatalf("RunEnv was asked for %v, want once, for the row's job %q", asked, launch)
	}
	token := "token-for-" + launch

	execs := rig.provider.Box(res.SandboxID).Execs()
	var ran []string
	for _, e := range execs {
		ran = append(ran, firstLine(e.Command))
		if got := e.Env["CREWLET_GIT_TOKEN"]; got != token {
			t.Errorf("setup command %q ran with CREWLET_GIT_TOKEN=%q, want the job's %q", firstLine(e.Command),
				got, token)
		}
		if e.Env["OPERATOR"] != "op" {
			t.Errorf("setup command %q lost the operator's environment: %v", firstLine(e.Command), e.Env)
		}
		if _, ok := e.Env["OTEL_EXPORTER_OTLP_ENDPOINT"]; ok {
			t.Errorf("setup command %q was handed the run's telemetry endpoint, minted for the job's start",
				firstLine(e.Command))
		}
	}
	want := []string{"echo provisioned", firstLine(req.Clone[0].Commands[0]), firstLine(req.Clone[1].Commands[0])}
	if !slices.Equal(ran, want) {
		t.Fatalf("the box ran %q, want the operator's setup then the clone steps: %q", ran, want)
	}

	started := rig.runner.Started()
	if len(started) != 1 {
		t.Fatalf("started %d runs, want 1", len(started))
	}
	run := started[0]
	if run.Env["CREWLET_GIT_TOKEN"] != token || run.Env["OPERATOR"] != "op" {
		t.Errorf("the run's environment is %v, want the job's token and the operator's variable", run.Env)
	}
	if !strings.HasPrefix(run.Env["OTEL_EXPORTER_OTLP_ENDPOINT"], "https://engine.internal/otlp/") {
		t.Errorf("the run was not handed its telemetry endpoint: %v", run.Env)
	}
	if !maps.Equal(req.Spec.Env, specEnv) {
		t.Errorf("the caller's spec environment became %v, want %v untouched", req.Spec.Env, specEnv)
	}
	// THE ENVIRONMENT BLOCK NAMES THE CHECKOUT, after the operator's own
	// steps, in the order the box was given them.
	setupAt := strings.Index(run.System, "Provisioned.")
	cloneAt := strings.Index(run.System, "Repository `web` is checked out at `./web`")
	if setupAt < 0 || cloneAt < setupAt {
		t.Errorf("the environment block does not name the checkout after the operator's steps:\n%s", run.System)
	}
}

// firstLine is a command's first line, what a case names a step's command by.
func firstLine(command string) string {
	line, _, _ := strings.Cut(command, "\n")
	return line
}

// testReceiver is a telemetry receiver minting endpoints under
// https://engine.internal.
func testReceiver(t *testing.T) *OtelReceiver {
	t.Helper()
	tokens, err := NewOtelTokens(OtelTokenOptions{Domain: OtelKeyDomain, Material: runtoken.OneKey("k1", "material")})
	if err != nil {
		t.Fatalf("NewOtelTokens: %v", err)
	}
	receiver, err := NewOtelReceiver(OtelReceiverOptions{BaseURL: "https://engine.internal", Tokens: tokens})
	if err != nil {
		t.Fatalf("NewOtelReceiver: %v", err)
	}
	return receiver
}

// A LAUNCH WHOSE ENVIRONMENT CANNOT BE MADE IS ABANDONED, as every failure past
// the row is: no box is made for a job whose credentials could not be minted,
// the row it opened is ended and the seat given back.
//
// Mutation: ignore RunEnv's error — the box is made and the run started, red.
func TestALaunchWhoseEnvironmentFailsIsAbandoned(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	req := withRepos(t, launchReq("t1"), granted("web", webRepoID))
	refused := errors.New("the keyring cannot sign")
	req.RunEnv = func(string) (map[string]string, error) { return nil, refused }
	_, err := rig.launchVia(t.Context(), rig.manager, req)
	if !errors.Is(err, refused) {
		t.Fatalf("Launch = %v, want the environment's failure", err)
	}
	if rig.provider.Box("box-1") != nil || len(rig.runner.Started()) != 0 {
		t.Fatal("a launch whose environment failed made a box or started a run")
	}
	rig.finished("t1")
}

// A FAILED CLONE ABANDONS THE LAUNCH, on a fresh box and on a reused one alike:
// a box whose git is below the floor fails the version step with its own
// sentence, named, and the box is reclaimed; a clone that fails on a reused box
// is the launch's failure too, its box killed and its row ended, because a brief
// promising a checkout the box lacks must never reach a run. A reused box is
// given the clone steps alone — the operator's setup survived with its disk.
//
// Mutation: skip the clone steps on a reused box — the second launch starts its
// run with no checkout, red.
func TestAFailedCloneAbandonsTheLaunch(t *testing.T) {
	t.Parallel()
	t.Run("a fresh box", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRig(t)
		failing := &scriptedBoxes{FakeProvider: rig.provider, exec: func(cmd string) ExecResult {
			if strings.HasPrefix(cmd, "command -v git") {
				return ExecResult{ExitCode: 3, Stderr: "git 2.39.3 (Apple Git-145) in this box is below 2.40.0"}
			}
			return ExecResult{}
		}}
		manager, err := NewManager(ManagerOptions{
			Providers: map[Placement]Provider{Direct: failing},
			Runners:   map[string]Runner{"claude-code": rig.runner},
		})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		_, err = rig.launchVia(t.Context(), manager, withRepos(t, launchReq("t1"), granted("web", webRepoID)))
		var setup *SetupError
		if !errors.As(err, &setup) || setup.Step != "git-version" || !setup.Exited || setup.Exit != 3 ||
			!strings.Contains(setup.Detail, "below 2.40.0") {
			t.Fatalf("Launch = %v, want the version step's refusal with its sentence", err)
		}
		if box := rig.provider.Box("box-1"); box == nil || !box.Closed() {
			t.Fatal("the box whose git is below the floor was not reclaimed")
		}
		if len(rig.runner.Started()) != 0 {
			t.Fatal("a run was started in a box whose clone failed")
		}
		rig.finished("t1")
	})
	t.Run("a reused box", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRig(t)
		first := withRepos(t, launchReq("t1"), granted("web", webRepoID))
		first.Setup = []SetupStep{{Name: "operator", Commands: []string{"echo provisioned"}}}
		res, err := rig.launchVia(t.Context(), rig.manager, first)
		if err != nil {
			t.Fatalf("the first launch: %v", err)
		}
		box := rig.provider.Box(res.SandboxID)
		before := len(box.Execs())

		second := withRepos(t, launchReq("t1"), granted("web", webRepoID), granted("api", apiRepoID))
		second.Setup = first.Setup
		second.ReuseBox = res.SandboxID
		if _, err := rig.launchVia(t.Context(), rig.manager, second); err != nil {
			t.Fatalf("the second launch: %v", err)
		}
		var reran []string
		for _, e := range box.Execs()[before:] {
			reran = append(reran, firstLine(e.Command))
		}
		want := []string{firstLine(second.Clone[0].Commands[0]), firstLine(second.Clone[1].Commands[0]),
			firstLine(second.Clone[2].Commands[0])}
		if !slices.Equal(reran, want) {
			t.Fatalf("the reused box ran %q, want its clone steps alone: %q", reran, want)
		}

		// AND A CLONE THAT FAILS THERE ENDS THE LAUNCH.
		box.ExecFunc = func(_ context.Context, cmd string, _ ExecOptions) (ExecResult, error) {
			if strings.Contains(firstLine(cmd), "'api'/.git") {
				return ExecResult{ExitCode: 128, Stderr: "fatal: repository not found"}, nil
			}
			return ExecResult{}, nil
		}
		third := second
		third.Clone = slices.Clone(second.Clone)
		_, err = rig.launchVia(t.Context(), rig.manager, third)
		var setup *SetupError
		if !errors.As(err, &setup) || setup.Step != "clone api" || setup.Exit != 128 {
			t.Fatalf("Launch = %v, want the api clone's failure", err)
		}
		if !slices.Contains(rig.provider.KilledIDs(), res.SandboxID) {
			t.Fatalf("the reused box whose clone failed was not killed: %v", rig.provider.KilledIDs())
		}
		if got := len(rig.runner.Started()); got != 2 {
			t.Fatalf("%d runs were started, want the first two alone", got)
		}
		rig.finished("t1")
	})
}

// scriptedBoxes is a provider whose every box answers its commands through
// exec.
type scriptedBoxes struct {
	*FakeProvider
	exec func(cmd string) ExecResult
}

func (p *scriptedBoxes) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	box, err := p.FakeProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	box.(*FakeSandbox).ExecFunc = func(_ context.Context, cmd string, _ ExecOptions) (ExecResult, error) {
		return p.exec(cmd), nil
	}
	return box, nil
}
