package cliagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// testProfile is a minimal profile whose credential and volatile paths are
// what the workspace tests exercise.
func testProfile(t *testing.T) cliprofile.Profile {
	t.Helper()
	return cliprofile.Profile{
		Binary:          "fake",
		CompleteArgs:    []string{"-p"},
		Output:          cliprofile.OutputText,
		CredentialPaths: []string{".fake/creds.json"},
		VolatilePaths:   []string{".fake/sessions", ".fake/history.jsonl"},
	}
}

func newWorkspace(t *testing.T) *Workspace {
	t.Helper()
	dir := t.TempDir()
	ws, err := Shared(dir, "fake-cli", testProfile(t))
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	t.Cleanup(func() { forgetWorkspace(dir) })
	return ws
}

// The isolation the rest of the backend depends on: two seats must not share
// a home, or seven seats on one subscription read each other's transcripts.
func TestSeatsDoNotShareAHome(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	a, err := ws.Acquire(t.Context(), "sarah-chen", "call-1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	b, err := ws.Acquire(t.Context(), "marcus-rivera", "call-2", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if a.Home == b.Home {
		t.Fatalf("two seats share the home %q", a.Home)
	}
	if a.Work == b.Work {
		t.Fatalf("two calls share the working directory %q", a.Work)
	}
	if err := a.Release(t.Context()); err != nil {
		t.Errorf("Release: %v", err)
	}
	if err := b.Release(t.Context()); err != nil {
		t.Errorf("Release: %v", err)
	}
}

// Volatile state is deleted before and after every call. A second, invisible
// memory inside the CLI would make turns non-reproducible and carry one
// task's context into the next.
func TestVolatileStateIsPrunedAroundACall(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	first, err := ws.Acquire(t.Context(), "dev", "call-1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	session := filepath.Join(first.Home, ".fake", "sessions", "transcript.json")
	if err := os.MkdirAll(filepath.Dir(session), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte("last turn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(session); !os.IsNotExist(err) {
		t.Fatalf("the transcript survived the release: %v", err)
	}

	// And again on the way in, for state a crashed call left behind.
	if err := os.MkdirAll(filepath.Dir(session), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte("crashed"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := ws.Acquire(t.Context(), "dev", "call-2", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = second.Release(t.Context()) }()
	if _, err := os.Stat(session); !os.IsNotExist(err) {
		t.Fatalf("a crashed call's transcript survived the next acquire: %v", err)
	}
}

// WHAT A CALL LEAVES IN TMPDIR GOES WITH THE SEAT'S OTHER LEFTOVERS, for every
// CLI, whatever its profile lists. The engine points TMPDIR inside the seat's
// home and every CLI writes there, so a profile that forgot the directory let
// its home grow by what each call left. And the directory is there, private
// and empty, when the next call starts: a CLI handed a TMPDIR that does not
// exist fails its first temporary file.
func TestTheSeatsTemporaryDirectoryIsPrunedAndRemade(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	first, err := ws.Acquire(t.Context(), "dev", "call-1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	tmp := childEnv(ws.profile, first, nil, Auth{})["TMPDIR"]
	if rel, err := filepath.Rel(first.Home, tmp); err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("TMPDIR = %q is not inside the seat home %q", tmp, first.Home)
	}
	if err := checkPrivateEmptyDir(tmp); err != nil {
		t.Fatalf("at the first call: %v", err)
	}
	left := filepath.Join(tmp, "claude-1000", "fswatch-probe-1")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := ws.Acquire(t.Context(), "dev", "call-2", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = second.Release(t.Context()) }()
	if err := checkPrivateEmptyDir(tmp); err != nil {
		t.Errorf("at the next call: %v", err)
	}
}

// checkPrivateEmptyDir says what is wrong with dir as a fresh TMPDIR.
func checkPrivateEmptyDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s is %v, want a 0700 directory", dir, info.Mode())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s holds %s, left by an earlier call", dir, entries[0].Name())
	}
	return nil
}

// Batched sub-agents belong to the SAME agent and run in parallel, so the
// second call into a seat must not prune under the first. Pruning there would
// delete the parent's live session mid-call.
func TestASecondCallIntoASeatDoesNotPruneUnderTheFirst(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	parent, err := ws.Acquire(t.Context(), "dev", "parent", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	live := filepath.Join(parent.Home, ".fake", "sessions", "live.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}

	child, err := ws.Acquire(t.Context(), "dev", "subagent", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if child.Home != parent.Home {
		t.Errorf("a sub-agent got its own home %q, want the parent's %q", child.Home, parent.Home)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the parent's live session was pruned by its own sub-agent: %v", err)
	}

	// The sub-agent finishing is not the seat going quiet.
	if err := child.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the parent's live session was pruned when its sub-agent finished: %v", err)
	}
	if err := parent.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("the session survived the seat going quiet: %v", err)
	}
}

// The login is seeded in and a REFRESHED one is synced back out. Discarding
// the file the CLI rewrote logs the whole fleet out at the next expiry,
// because most vendors rotate the refresh token with the access token.
func TestARefreshedLoginIsSyncedBackToTheSharedDirectory(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	shared := filepath.Join(ws.CredentialsDir(), "creds.json")
	if err := os.MkdirAll(ws.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte(`{"token":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	checkout, err := ws.Acquire(t.Context(), "dev", "call", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	seeded := filepath.Join(checkout.Home, ".fake", "creds.json")
	got, err := os.ReadFile(seeded)
	if err != nil {
		t.Fatalf("the login was not seeded: %v", err)
	}
	if string(got) != `{"token":"old"}` {
		t.Fatalf("seeded %q", got)
	}

	// The CLI refreshes in place.
	if err := os.WriteFile(seeded, []byte(`{"token":"new"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkout.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	after, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != `{"token":"new"}` {
		t.Fatalf("the shared login is %q, want the refreshed one — the fleet is now "+
			"logged out at the next expiry", after)
	}
}

// A missing login is NOT a failure to acquire. The call then fails with the
// vendor's own "not authenticated", which names the CLI; refusing here would
// take a company down at boot over one provider's credentials.
func TestAMissingLoginDoesNotRefuseTheCheckout(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	checkout, err := ws.Acquire(t.Context(), "dev", "call", nil)
	if err != nil {
		t.Fatalf("Acquire with no login: %v", err)
	}
	if err := checkout.Release(t.Context()); err != nil {
		t.Errorf("Release: %v", err)
	}
	if hasLogin(t, ws) {
		t.Error("HasLogin reported a login that was never written")
	}
}

// Profile paths are operator-overridable and prune() calls RemoveAll on what
// they resolve to, so one escaping the seat home is a config that deletes the
// engine user's dotfiles.
func TestAVolatilePathCannotEscapeTheSeatHome(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{"../../.ssh", "..", "/etc"} {
		if _, err := cliprofile.UnderRoot("/var/lib/crewlet/seats/dev/home", rel); err == nil {
			t.Errorf("underRoot accepted %q", rel)
		}
	}
	if _, err := cliprofile.UnderRoot("/var/lib/crewlet/seats/dev/home", ".fake/sessions"); err != nil {
		t.Errorf("a legitimate relative path was refused: %v", err)
	}
}

// TWO CLIs ON ONE STATE DIRECTORY NEVER MEET EACH OTHER'S FILES. Each keeps
// its share under <state dir>/<harness>: its own login, its own seat homes. Keyed
// by the directory alone, the second CLI was refused for the life of the
// process, so moving a providers.llm key from claude-code to codex produced a
// revision every running node refused while a restarted one applied it — and
// had it been let through, codex would have seeded its seats from, and pruned,
// a login it cannot read.
//
// Mutation: key the registry by the directory alone and root every workspace
// at it, and the two CLIs share one login here.
func TestTwoCLIsOnOneStateDirectoryKeepSeparateWorkspaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	claude, err := Shared(dir, "claude-code", testProfile(t))
	if err != nil {
		t.Fatalf("Shared(claude-code): %v", err)
	}
	codex, err := Shared(dir, "codex", testProfile(t))
	if err != nil {
		t.Fatalf("a second CLI on one state directory was refused: %v", err)
	}
	if claude == codex {
		t.Fatal("two CLIs on one state directory got one workspace")
	}
	for ws, agent := range map[*Workspace]string{claude: "claude-code", codex: "codex"} {
		if want := filepath.Join(dir, agent); ws.Root() != want {
			t.Errorf("%s keeps its files in %q, want %q", agent, ws.Root(), want)
		}
		if ws.StateDir() != dir {
			t.Errorf("%s reports state dir %q, want %q", agent, ws.StateDir(), dir)
		}
	}
	if claude.CredentialsDir() == codex.CredentialsDir() {
		t.Fatalf("two CLIs share the login directory %q", claude.CredentialsDir())
	}

	writeCreds(t, &Provider{ws: claude}, `{"token":"claude-login"}`)
	co, err := codex.Acquire(t.Context(), "dev", "call", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if strings.HasPrefix(co.Home, claude.Root()+string(filepath.Separator)) {
		t.Errorf("codex's seat home %q is inside Claude Code's share %q", co.Home, claude.Root())
	}
	if _, err := os.Stat(filepath.Join(co.Home, ".fake", "creds.json")); err == nil {
		t.Error("codex's seat home was seeded with Claude Code's login")
	}
	if err := co.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := readCreds(t, &Provider{ws: claude}); got != `{"token":"claude-login"}` {
		t.Errorf("Claude Code's login after a codex call = %q, want it untouched", got)
	}
}

// SWITCHING AN ENTRY BACK FINDS ITS OLD LOGIN. A key moved from claude-code to
// codex and back opens the directory Claude Code left: nothing the codex
// period did reached it, so the operator does not sign in again.
func TestSwitchingBackToACLIFindsItsLoginIntact(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	claude, err := Shared(dir, "claude-code", testProfile(t))
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	writeCreds(t, &Provider{ws: claude}, `{"token":"R0"}`)

	// The entry moves to codex, which runs a call and is signed in there.
	forgetWorkspace(dir)
	codex, err := Shared(dir, "codex", testProfile(t))
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	writeCreds(t, &Provider{ws: codex}, `{"token":"codex-login"}`)
	co, err := codex.Acquire(t.Context(), "dev", "call", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := co.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// And back, in a fresh process.
	forgetWorkspace(dir)
	back, err := Shared(dir, "claude-code", testProfile(t))
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	if !hasLogin(t, back) {
		t.Fatal("switching back to Claude Code found no login")
	}
	seat, err := back.Acquire(t.Context(), "dev", "call-after", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = seat.Release(t.Context()) }()
	raw, err := os.ReadFile(filepath.Join(seat.Home, ".fake", "creds.json"))
	if err != nil {
		t.Fatalf("the seat home was not seeded with the old login: %v", err)
	}
	if string(raw) != `{"token":"R0"}` {
		t.Errorf("the seat home holds %q, want Claude Code's own login", raw)
	}
}

// AN AGENT NAMES ONE DIRECTORY, and a name that could climb out of the state
// directory, or that two CLIs could both resolve to, is refused rather than
// written under.
func TestAnAgentThatIsNotOnePathElementIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	for _, agent := range []string{"", ".", "..", "../escape", "a/b", "codex/"} {
		if _, err := Shared(dir, agent, testProfile(t)); err == nil {
			t.Errorf("agent %q was given a workspace", agent)
		}
	}
}

// The same CLI over one directory is the SUPPORTED case — it is how per-phase
// models work off a single login — and both entries must get the one
// workspace, or each would prune the other's live seat homes.
//
// Mutation: key the registry by anything more than the resolved directory and
// the CLI, and the two entries get two workspaces.
func TestTwoEntriesOnOneDirectoryShareOneWorkspace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	opus, err := Shared(dir, "claude-code", testProfile(t))
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	sonnet, err := Shared(filepath.Join(dir, "."), "claude-code", testProfile(t))
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	if opus != sonnet {
		t.Fatal("two entries on one directory got two workspaces, so each will prune the other")
	}
}

// The in-flight count is read and written from several goroutines at once,
// which is what -race is run against.
func TestConcurrentCallsIntoOneSeatAreSafe(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			checkout, err := ws.Acquire(t.Context(), "dev", string(rune('a'+i%16)), nil)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			if err := checkout.Release(t.Context()); err != nil {
				t.Errorf("Release: %v", err)
			}
		})
	}
	wg.Wait()
}

// Release is called from a defer that can also run after an error path, so
// releasing twice must not drive the in-flight count negative and prune under
// a live call.
func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	checkout, err := ws.Acquire(t.Context(), "dev", "call", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := checkout.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := checkout.Release(t.Context()); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	live, err := ws.Acquire(t.Context(), "dev", "next", nil)
	if err != nil {
		t.Fatalf("Acquire after a double release: %v", err)
	}
	if err := live.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// A handle comes from a vendor's API, so it must not be able to escape the
// state directory or overrun NAME_MAX — while staying recognisable to an
// operator looking at the directory.
func TestSeatSlugIsSafeAndRecognisable(t *testing.T) {
	t.Parallel()
	// A canonical handle — which is all org.ValidHandle admits — renders
	// bare, so the common case stays readable.
	if got := seatSlug("sarah-chen"); got != "sarah-chen" {
		t.Errorf("seatSlug(%q) = %q, want it unchanged", "sarah-chen", got)
	}
	for _, in := range []string{
		"Sarah Chen", "../../etc", "", "///", "a/b", strings.Repeat("x", 200),
	} {
		got := seatSlug(in)
		if got == "" {
			t.Errorf("seatSlug(%q) = \"\"", in)
		}
		if strings.ContainsAny(got, "/\\.") {
			t.Errorf("seatSlug(%q) = %q, which can escape the directory", in, got)
		}
		if len(got) > 255 {
			t.Errorf("seatSlug(%q) is %d bytes, past NAME_MAX", in, len(got))
		}
	}
}

// A CALL THAT WAS NEVER PREPARED LEAVES NO SCRATCH DIRECTORY. It has no
// Checkout to Release, so a working directory an error path in Acquire left
// behind is one nothing removes — one per failing call for as long as the
// failure lasts. Only a failed seed used to clean up; a failed prune and a
// failed work-scope settings file did not.
func TestAFailedAcquireLeavesNoWorkingDirectory(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		profile func(*cliprofile.Profile)
		// breakIt makes the step fail, given the seat dir and the call's
		// working directory; it runs before Acquire.
		breakIt func(t *testing.T, ws *Workspace, seatDir, work string)
	}{
		"the prune fails": {
			breakIt: func(t *testing.T, _ *Workspace, seatDir, _ string) {
				// A FILE where a volatile path's parent should be is
				// what RemoveAll cannot get through, even as root.
				blocker := filepath.Join(seatDir, "home", ".fake")
				if err := os.MkdirAll(filepath.Dir(blocker), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(blocker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		"the seed fails": {
			breakIt: func(t *testing.T, ws *Workspace, _, _ string) {
				if err := os.MkdirAll(filepath.Join(ws.CredentialsDir(), "creds.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		"the working directory's settings file fails": {
			profile: func(p *cliprofile.Profile) {
				p.SeedFiles = []cliprofile.SeedFile{{Path: ".vendor/project.json", In: cliprofile.SeedWork, Content: "{}"}}
			},
			breakIt: func(t *testing.T, _ *Workspace, _, work string) {
				if err := os.MkdirAll(work, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(work, ".vendor"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			profile := testProfile(t)
			if c.profile != nil {
				c.profile(&profile)
			}
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			ws, err := Shared(dir, "fake-cli", profile)
			if err != nil {
				t.Fatalf("Shared: %v", err)
			}
			work := filepath.Join(ws.SeatDir("dev"), "work", seatSlug("call"))
			c.breakIt(t, ws, ws.SeatDir("dev"), work)
			if co, err := ws.Acquire(t.Context(), "dev", "call", nil); err == nil {
				_ = co.Release(t.Context())
				t.Fatal("the broken step did not fail the acquire")
			}
			if _, err := os.Stat(work); !os.IsNotExist(err) {
				t.Errorf("a failed acquire left its working directory behind (%v)", err)
			}
		})
	}
}

// AND INJECTIVE. This directory is a seat's HOME — its coding-CLI session,
// its history, its credentials — so two seats that land on one slug get one
// home, which is the cross-seat read this package exists to prevent. The
// rendering used to collide two ways: every unsafe character folded to '-',
// and the result was cut at 64 with nothing bounding a handle's length.
func TestSeatSlugNeverCollides(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 64)
	seen := map[string]string{}
	for _, in := range []string{
		// Folded apart from each other under the old rendering.
		"sarah-chen", "Sarah Chen", "sarah.chen", "sarah/chen", "SARAH-CHEN",
		"a/b", "a.b", "a b", "a-b",
		// Shared a 64-character prefix under the old rendering.
		long + "-one", long + "-two", long + strings.Repeat("y", 40),
		// Both degenerate inputs previously became the literal "seat".
		"", "///", "..", "../../etc", "../../var",
	} {
		got := seatSlug(in)
		if prev, dup := seen[got]; dup {
			t.Errorf("seatSlug(%q) and seatSlug(%q) both give %q — one HOME for two seats",
				prev, in, got)
		}
		seen[got] = in
	}
}
