package sandbox

import (
	"context"
	"os"
	"testing"
)

// tokenPath is a file the launch hands its job the absolute path of, under the
// box's home, and ownPath one whose variable the launch's own environment sets.
const tokenPath, ownPath = ".config/crewlet/token", ".config/crewlet/own"

// homePathsReq is launchReq handing its job tokenPath under CLI_TOKEN_FILE,
// and ownPath under CLI_OWN_FILE, which its own environment also sets.
func homePathsReq(turnID string) LaunchRequest {
	req := launchReq(turnID)
	req.Spec.Env = map[string]string{"CLI_OWN_FILE": "/srv/own"}
	req.HomePaths = map[string]string{"CLI_TOKEN_FILE": tokenPath, "CLI_OWN_FILE": ownPath}
	return req
}

// A JOB IS HANDED EACH PATH JOINED ONTO ITS OWN BOX'S HOME, which differs by
// backend: a direct box's is a directory minted for it on the engine host,
// where the box's own write of the file lands, and a container box's is its
// mount inside the container. A variable the launch's environment sets wins.
// Mutation: join onto the default home, or lay the paths over the launch's
// environment, and this goes red.
func TestAJobIsHandedEachPathJoinedOntoItsOwnBoxsHome(t *testing.T) {
	t.Parallel()
	m, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: NewFakeProvider()},
		Runners:   map[string]Runner{"claude-code": NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	req := homePathsReq("t1")

	direct := mustCreate(t, newDirect(t), Spec{})
	if err := direct.WriteFile(t.Context(), tokenPath, []byte("the token")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	env := jobEnvFor(m, req, direct)
	if held, err := os.ReadFile(env["CLI_TOKEN_FILE"]); err != nil || string(held) != "the token" {
		t.Errorf("a direct box's job was handed %q, which reads %q (%v): want the file the box wrote",
			env["CLI_TOKEN_FILE"], held, err)
	}
	if env["CLI_OWN_FILE"] != "/srv/own" {
		t.Errorf("a direct box's job was handed CLI_OWN_FILE=%q, want the launch's own", env["CLI_OWN_FILE"])
	}

	container, _ := NewContainerBoxAt(t.TempDir())
	env = jobEnvFor(m, req, container)
	if want := "/home/user/" + tokenPath; env["CLI_TOKEN_FILE"] != want {
		t.Errorf("a container box's job was handed %q, want %q", env["CLI_TOKEN_FILE"], want)
	}
	if env["CLI_OWN_FILE"] != "/srv/own" {
		t.Errorf("a container box's job was handed CLI_OWN_FILE=%q, want the launch's own", env["CLI_OWN_FILE"])
	}
	if req.Spec.Env["CLI_TOKEN_FILE"] != "" {
		t.Error("the paths were written into the launch's own environment")
	}
}

// A LAUNCH HANDS ITS JOB THE PATHS IN THE BOX IT GOT, a fresh box and a kept
// one a job continues in alike, through the one construction of the job's
// environment. Mutation: start the job on the launch's environment with the
// telemetry alone, and this goes red.
func TestALaunchHandsItsJobThePathsInTheBoxItGot(t *testing.T) {
	t.Parallel()
	want := map[string]string{"CLI_TOKEN_FILE": DefaultHome + "/" + tokenPath, "CLI_OWN_FILE": "/srv/own"}
	handed := func(t *testing.T, env map[string]string) {
		t.Helper()
		for name, value := range want {
			if env[name] != value {
				t.Errorf("the job was handed %s=%q, want %q", name, env[name], value)
			}
		}
	}

	t.Run("fresh", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRig(t)
		if _, err := rig.launchVia(t.Context(), rig.manager, homePathsReq("t1")); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		started := rig.runner.Started()
		if len(started) != 1 {
			t.Fatalf("%d jobs started, want 1", len(started))
		}
		handed(t, started[0].Env)
	})

	t.Run("kept", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		var relaunched LaunchResult
		run := askAndAnswer(t, rig, false, func(ctx context.Context, _ PendingRun, asked *AskedRun) {
			req, paths := continuing(asked), homePathsReq("t1")
			req.Spec.Env, req.HomePaths = paths.Spec.Env, paths.HomePaths
			res, err := rig.launchVia(ctx, rig.manager, req)
			if err != nil {
				t.Errorf("relaunch: %v", err)
			}
			relaunched = res
		})
		if !relaunched.Reused || relaunched.SandboxID != run.SandboxID {
			t.Fatalf("relaunch = %+v, want it continued in the asking run's box %q", relaunched, run.SandboxID)
		}
		started := rig.runner.Started()
		handed(t, started[len(started)-1].Env)
	})
}
