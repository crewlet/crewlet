//go:build unix

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// containerLocal is a container-placement provider over a runtime that shares
// the box's home the way a local daemon does: Create's mount proof passes, a
// removal and an unpause succeed, nothing is paused and no container is
// listed as live. No real runtime is involved.
func containerLocal(t *testing.T) *Local {
	t.Helper()
	state := t.TempDir()
	return &Local{
		opts:    LocalOptions{Placement: Container, Image: "img"},
		root:    state,
		runtime: stubRuntime(t, state, true),
	}
}

// THE ENGINE'S RECORDS ABOUT A CONTAINER BOX ARE NOT ITS JOB'S TO WRITE. A
// container's job writes its whole home, which is the one directory mounted
// into it; the records of which logins the box was seeded with, and the env
// file each exec rewrites, are what the engine acts on as the engine host's
// user. They used to sit in the home's .crewlet/, so a job could name any host
// file as the "shared login" a box file was copied back over at teardown, or
// make the env file a link the next exec followed out of the box.
//
// Mutation: put the records back under the home's .crewlet/, and the victim
// is overwritten — by the next exec, and again by the teardown.
func TestAContainerJobCannotRewriteWhatTheEngineKnowsAboutItsBox(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	host := t.TempDir()
	shared := filepath.Join(host, "login.json")
	victim := filepath.Join(host, "victim")
	writeFile(t, shared, `{"token":"first"}`)
	writeFile(t, victim, "the engine host's own file")

	local := containerLocal(t)
	box, err := local.Create(ctx, Spec{
		Env:             map[string]string{"ANTHROPIC_API_KEY": "sk-ant-..."},
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := layoutOf(t, box).home()

	// What a job inside the container can write in its own home, at the
	// names the engine's records had there.
	writeFile(t, filepath.Join(home, ".crewlet", "credentials.json"), `{"payload":"`+victim+`"}`)
	writeFile(t, filepath.Join(home, "payload"), "written by the box")
	env := filepath.Join(home, ".crewlet", "env")
	if err := os.Remove(env); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, env); err != nil {
		t.Fatal(err)
	}
	// And a login the run refreshed, which is what is written back.
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), `{"token":"rotated"}`)

	// An exec rewrites the env file; its exit status is the stub's, and
	// beside the point.
	if _, err := box.Exec(ctx, "true", ExecOptions{}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	// Torn down by id, as the pause reaper and every reconnected teardown do.
	if err := local.Kill(ctx, box.ID()); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	if got := readFile(t, victim); got != "the engine host's own file" {
		t.Fatalf("a host file the box named was overwritten with %q", got)
	}
	if got := readFile(t, shared); got != `{"token":"rotated"}` {
		t.Fatalf("shared login = %q; want the refresh the run made written back", got)
	}
}

// A RECORD THAT IS NOT A REGULAR FILE IS ANSWERED AT ONCE, by every path that
// reads one. Each reconnect reads the box's credential map before it answers —
// the poll's Attach, the collection's Connect, the pause reaper's Kill and the
// orphan reaper — and a direct box's teardown and liveness read its job
// record; a plain open of a named pipe there waited for a writer for good, and
// nothing cancels an open. An unusable map degrades to no write-back, said in a
// warning; an unusable job record is the unknown answer, which keeps the box.
//
// Mutation: read either record with os.ReadFile, and the pipe cases hang.
func TestABoxRecordThatIsNotARegularFileIsAnsweredAtOnce(t *testing.T) {
	t.Parallel()
	kinds := map[string]func(t *testing.T, path string){
		"named pipe": func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		},
		"dangling link": func(t *testing.T, path string) {
			if err := os.Symlink(path+".nowhere", path); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		},
	}
	providers := map[string]func(t *testing.T) *Local{
		"direct":    newDirect,
		"container": containerLocal,
	}
	records := map[string]func(boxLayout) string{
		"credential map": boxLayout.credentialsFile,
		"job record":     boxLayout.pidFile,
	}
	ops := map[string]func(t *testing.T, local *Local, box Sandbox) func() error{
		"Attach": func(t *testing.T, local *Local, box Sandbox) func() error {
			return func() error { _, err := local.Attach(t.Context(), box.ID()); return err }
		},
		"Connect": func(t *testing.T, local *Local, box Sandbox) func() error {
			return func() error { _, err := local.Connect(t.Context(), box.ID()); return err }
		},
		"Kill": func(t *testing.T, local *Local, box Sandbox) func() error {
			return func() error { return local.Kill(t.Context(), box.ID()) }
		},
		"the orphan reaper": func(t *testing.T, local *Local, box Sandbox) func() error {
			ageBox(t, box, 2*time.Hour)
			return func() error { local.reapOrphans(t.Context(), time.Minute); return nil }
		},
	}
	for placement, provider := range providers {
		for record, pathOf := range records {
			if placement == "container" && record == "job record" {
				// A container box records no job of its own: the runtime
				// is asked about the container instead.
				continue
			}
			for kind, create := range kinds {
				for op, prepare := range ops {
					t.Run(placement+"/"+record+"/"+kind+"/"+op, func(t *testing.T) {
						t.Parallel()
						local := provider(t)
						host := filepath.Join(t.TempDir(), "login.json")
						writeFile(t, host, `{"token":"first"}`)
						box, err := local.Create(t.Context(), Spec{
							CredentialFiles: map[string]string{".claude/.credentials.json": host},
						})
						if err != nil {
							t.Fatalf("Create: %v", err)
						}
						layout := layoutOf(t, box)
						path := pathOf(layout)
						if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
							t.Fatal(err)
						}
						create(t, path)
						call := prepare(t, local, box)

						answered := make(chan error, 1)
						go func() { answered <- call() }()
						select {
						case err := <-answered:
							if err != nil {
								t.Fatalf("%s with a %s for its %s = %v; want an answer, not a failure",
									op, kind, record, err)
							}
						case <-time.After(10 * time.Second):
							// A blocked open is never released, so the
							// test binary is what ends it.
							t.Fatalf("%s with a %s for its %s has not answered in 10s", op, kind, record)
						}
						if op == "the orphan reaper" {
							_, statErr := os.Stat(layout.root)
							kept := statErr == nil
							// An unreadable job record is the unknown answer,
							// and a box whose liveness is unknown is kept.
							if want := record == "job record"; kept != want {
								t.Fatalf("the reaper kept the box = %v; want %v (%v)", kept, want, statErr)
							}
						}
					})
				}
			}
		}
	}
}

// A POLL THAT MEETS SUCH A BOX STILL LETS THE WAITER STOP. The poll's task
// held one of the waiter's slots and the run's busy registration while its
// open waited, and Stop waits for every task the loop started — so engine
// shutdown never completed, and sixteen such boxes stopped every poll on the
// node.
//
// Mutation: read the credential map with os.ReadFile, and the poll never
// reaches its runner.
func TestAWaiterStopsAfterAPollMeetsABoxWithAPipeForARecord(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	path := layoutOf(t, box).credentialsFile()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	polled := make(chan struct{}, 1)
	rig.runner.PollFunc = func(context.Context, Sandbox) (bool, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return false, nil
	}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: local},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.current.Store(manager)
	rig.seedRunning("t-pipe", box.ID())

	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
		Interval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	waiter.Start(t.Context())
	select {
	case <-polled:
	case <-time.After(10 * time.Second):
		t.Fatal("the poll never reached the runner: reaching the box blocked on its record")
	}
	stopped := make(chan struct{})
	go func() { waiter.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Waiter.Stop has not returned in 10s")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
