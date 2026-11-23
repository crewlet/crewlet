package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// The repositories the cases give a run, valid for a coding executor
// ([RunRepo.ValidateFor], [CloneCoding]).
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

// withRepos is req given repos as the engine gives a carried-on round its
// job's grants: on the spec, with the floor their resolution read. The run,
// not the launch, clones them.
func withRepos(t *testing.T, req LaunchRequest, repos ...RunRepo) LaunchRequest {
	t.Helper()
	req.Spec.Repos = repos
	req.CodeFloor = "code@1:40"
	return req
}

// A LAUNCH WRITES THE JOB'S GRANTS, ITS FLOOR AND WHOM ITS PUSHES ARE MADE FOR
// ON THE ROW BEFORE ITS BOX EXISTS: a box's request to Gitlet is admitted
// against the row and can come before the launch returns. The launch floor is
// the floor the job opened at.
//
// Mutations: leave Repos off the run BeginLaunch is handed — the row at the
// box's creation carries none, red; leave whom its pushes are made for off it
// — the row at the box's creation works for nobody, red.
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
	designer := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "designer"}
	req.CodeCeiling, req.CodeWorkingFor = "designer", designer
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
	if !reflect.DeepEqual(witness.seen.CodeWorkingFor, designer) {
		t.Fatalf("at the box's creation the row works for %+v, want %+v", witness.seen.CodeWorkingFor, designer)
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
// WRITTEN: too many repositories, one that is not a RunRepo, one named twice by
// name or id (two grants, two clones), a floor that does not go with its
// repositories, and a colleague's ceiling or whom its pushes are made for on a
// run given none. No row is stored and no box made.
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
		{"whom its pushes are made for with no repositories", func(t *testing.T) LaunchRequest {
			r := launchReq("t1")
			r.CodeWorkingFor = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "designer"}
			return r
		}, "has nobody its pushes are made for"},
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

// THE LAUNCH'S OWN ENVIRONMENT REACHES EVERY SETUP STEP AND THE RUN.
// [LaunchRequest.RunEnv] is asked once with the row's launch id; its
// variables reach the operator's setup and the harness over an operator's
// value; its Gitlet credential is written into the box, named to the run by
// its path there and in no variable. Telemetry reaches the run alone. The
// spec's sign-in names reach the run. The caller's spec keeps its own map. A
// launch runs only the operator's steps (the run clones a grant) and its
// environment block names no checkout.
//
// Mutations, each red: provision with req.Spec rather than the launch's (the
// setup commands lack the launch's variable); put the credential in the
// environment; write no file, or name none to the run.
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
	req.Spec.Env = map[string]string{"OPERATOR": "op", GitURLVar: "the operator's"}
	req.Spec.SignInEnv = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}
	specEnv := maps.Clone(req.Spec.Env)
	req.Setup = []SetupStep{{Name: "operator", Commands: []string{"echo provisioned"}, Brief: "Provisioned."}}
	var asked []string
	req.RunEnv = func(launchID string, _ EngineHost) (LaunchEnv, error) {
		asked = append(asked, launchID)
		return LaunchEnv{Env: map[string]string{GitURLVar: "base-for-" + launchID},
			GitAccess: "token-for-" + launchID}, nil
	}

	res, err := rig.launchVia(t.Context(), manager, req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	launch := rig.get("t1").LaunchID
	if !slices.Equal(asked, []string{launch}) {
		t.Fatalf("RunEnv was asked for %v, want once, for the row's job %q", asked, launch)
	}
	token, base := "token-for-"+launch, "base-for-"+launch

	execs := rig.provider.Box(res.SandboxID).Execs()
	var ran []string
	for _, e := range execs {
		if e.Command == boxFactsScript {
			// THE LAUNCH'S READ OF THE BOX needs no credential and gets none.
			if len(e.Env) != 0 {
				t.Errorf("the box's read was handed an environment: %v", e.Env)
			}
			continue
		}
		ran = append(ran, firstLine(e.Command))
		if got := e.Env[GitURLVar]; got != base {
			t.Errorf("setup command %q ran with %s=%q, want the job's %q", firstLine(e.Command), GitURLVar,
				got, base)
		}
		for name, value := range e.Env {
			if strings.Contains(value, token) {
				t.Errorf("setup command %q ran with the credential in %s", firstLine(e.Command), name)
			}
		}
		if e.Env["OPERATOR"] != "op" {
			t.Errorf("setup command %q lost the operator's environment: %v", firstLine(e.Command), e.Env)
		}
		if _, ok := e.Env["OTEL_EXPORTER_OTLP_ENDPOINT"]; ok {
			t.Errorf("setup command %q was handed the run's telemetry endpoint, minted for the job's start",
				firstLine(e.Command))
		}
	}
	if want := []string{"echo provisioned"}; !slices.Equal(ran, want) {
		t.Fatalf("the box ran %q, want the operator's setup alone: %q", ran, want)
	}

	started := rig.runner.Started()
	if len(started) != 1 {
		t.Fatalf("started %d runs, want 1", len(started))
	}
	run := started[0]
	box := rig.provider.Box(res.SandboxID)
	if run.Env[GitURLVar] != base || run.Env["OPERATOR"] != "op" ||
		run.Env[GitAccessFileVar] != box.Home()+"/"+GitAccessFile {
		t.Errorf("the run's environment is %v, want the job's base, the operator's variable and the "+
			"credential's file", run.Env)
	}
	for name, value := range run.Env {
		if strings.Contains(value, token) {
			t.Errorf("the run's variable %s holds its credential, which only its file holds", name)
		}
	}
	if got, err := box.ReadFile(t.Context(), GitAccessFile); err != nil || string(got) != token {
		t.Errorf("the box's credential file holds %q (%v), want the job's %q", got, err, token)
	}

	if !strings.HasPrefix(run.Env["OTEL_EXPORTER_OTLP_ENDPOINT"], "https://engine.internal/otlp/") {
		t.Errorf("the run was not handed its telemetry endpoint: %v", run.Env)
	}
	if !slices.Equal(run.SignInEnv, req.Spec.SignInEnv) {
		t.Errorf("the run's sign-in names are %v, want the spec's %v", run.SignInEnv, req.Spec.SignInEnv)
	}
	if !maps.Equal(req.Spec.Env, specEnv) {
		t.Errorf("the caller's spec environment became %v, want %v untouched", req.Spec.Env, specEnv)
	}
	// THE ENVIRONMENT BLOCK IS THE OPERATOR'S STEPS', and names no checkout
	// the box does not hold.
	if !strings.Contains(run.System, "Provisioned.") || strings.Contains(run.System, "is checked out at") {
		t.Errorf("the environment block is not the operator's steps alone:\n%s", run.System)
	}
}

// A LAUNCH WRITES THE BOX'S GIT AS THE BOX IT MAKES DIALS IT: [LaunchRequest.RunEnv]
// is asked, before the box exists, with the name the engine host answers to in
// the cell the launch places the box in, so the documented single-host base
// (http://localhost:8000/box/git) reaches a container box's setup and run under
// its runtime's alias, the credential helper scoped to that URL, while a direct
// box in the same catalogue keeps it as written.
//
// Mutation: ask RunEnv with "" whatever the cell, or with the catalogue
// default's name — the container box is handed its own loopback, red.
func TestALaunchWritesTheBoxsGitAsTheBoxDialsIt(t *testing.T) {
	t.Parallel()
	const loopback = "http://localhost:8000/box/git"
	for _, tc := range []struct {
		cell Placement
		host EngineHost
		want string
	}{
		{Container, "host.containers.internal", "http://host.containers.internal:8000/box/git"},
		{Direct, "", loopback},
	} {
		t.Run(string(tc.cell), func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRig(t)
			cells := map[Placement]*FakeProvider{Direct: rig.provider, Container: NewFakeProvider()}
			cells[Container].HostAlias = "host.containers.internal"
			manager, err := NewManager(ManagerOptions{DefaultPlacement: Direct,
				Providers: map[Placement]Provider{Direct: cells[Direct], Container: cells[Container]},
				Runners:   map[string]Runner{"claude-code": rig.runner}})
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			req := launchReq("t1")
			req.Spec.Placement = tc.cell
			req.Setup = []SetupStep{{Name: "operator", Commands: []string{"echo provisioned"}}}
			var asked []EngineHost
			req.RunEnv = func(_ string, host EngineHost) (LaunchEnv, error) {
				asked = append(asked, host)
				return BoxGit{Base: loopback, EngineHost: host, User: "run", Token: "t"}.Launch(nil)
			}
			res, err := rig.launchVia(t.Context(), manager, req)
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if !slices.Equal(asked, []EngineHost{tc.host}) {
				t.Fatalf("RunEnv was asked with %q, want once, with the %s cell's %q", asked, tc.cell, tc.host)
			}
			scope := GitConfigParameter("credential."+tc.want+"/.helper", "")
			dialled := func(what string, env map[string]string) {
				if env[GitURLVar] != tc.want || !strings.Contains(env[GitConfigParametersVar], scope) {
					t.Errorf("%s ran with %s=%q and %s=%q, want %q and its helper scoped to it", what, GitURLVar,
						env[GitURLVar], GitConfigParametersVar, env[GitConfigParametersVar], tc.want)
				}
			}
			box := cells[tc.cell].Box(res.SandboxID)
			if box == nil {
				t.Fatalf("the premise: box %s is the %s cell's", res.SandboxID, tc.cell)
			}
			setup := 0
			for _, e := range box.Execs() {
				if e.Command != boxFactsScript {
					setup++
					dialled("setup command "+firstLine(e.Command), e.Env)
				}
			}
			started := rig.runner.Started()
			if setup == 0 || len(started) != 1 {
				t.Fatalf("the box ran %d setup commands and %d runs, want the operator's step and one run",
					setup, len(started))
			}
			dialled("the run", started[0].Env)
		})
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
	receiver, err := NewOtelReceiver(OtelReceiverOptions{BaseURL: "https://engine.internal", Tokens: tokens,
		TokenTTL: time.Hour})
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
	req.RunEnv = func(string, EngineHost) (LaunchEnv, error) { return LaunchEnv{}, refused }
	_, err := rig.launchVia(t.Context(), rig.manager, req)
	if !errors.Is(err, refused) {
		t.Fatalf("Launch = %v, want the environment's failure", err)
	}
	if rig.provider.Box("box-1") != nil || len(rig.runner.Started()) != 0 {
		t.Fatal("a launch whose environment failed made a box or started a run")
	}
	rig.finished("t1")
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

// resumeInItsBox puts the rig's run t1 where a send-back or an answered
// question relaunches it: the job over, its turn resumed, the row still naming
// its box ([PriorBox.ContinuesIn]). The next launch of t1 in the same cell and
// CLI is given that box back.
func resumeInItsBox(t *testing.T, rig *waiterRig) {
	t.Helper()
	if err := rig.pending.SetStatus(t.Context(), "t1", StatusResumed, Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
}
