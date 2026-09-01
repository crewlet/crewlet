package cliagent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testProfile is a minimal profile whose credential and volatile paths are
// what the workspace tests exercise.
func testProfile(t *testing.T) Profile {
	t.Helper()
	return Profile{
		Binary:          "fake",
		CompleteArgs:    []string{"-p"},
		Output:          OutputText,
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
	ws := newWorkspace(t)
	a, err := ws.Acquire("sarah-chen", "call-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	b, err := ws.Acquire("marcus-rivera", "call-2")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if a.Home == b.Home {
		t.Fatalf("two seats share the home %q", a.Home)
	}
	if a.Work == b.Work {
		t.Fatalf("two calls share the working directory %q", a.Work)
	}
	if err := a.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if err := b.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
}

// Volatile state is deleted before and after every call. A second, invisible
// memory inside the CLI would make turns non-reproducible and carry one
// task's context into the next.
func TestVolatileStateIsPrunedAroundACall(t *testing.T) {
	ws := newWorkspace(t)
	first, err := ws.Acquire("dev", "call-1")
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
	if err := first.Release(); err != nil {
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
	second, err := ws.Acquire("dev", "call-2")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = second.Release() }()
	if _, err := os.Stat(session); !os.IsNotExist(err) {
		t.Fatalf("a crashed call's transcript survived the next acquire: %v", err)
	}
}

// Batched sub-agents belong to the SAME agent and run in parallel, so the
// second call into a seat must not prune under the first. Pruning there would
// delete the parent's live session mid-call.
func TestASecondCallIntoASeatDoesNotPruneUnderTheFirst(t *testing.T) {
	ws := newWorkspace(t)
	parent, err := ws.Acquire("dev", "parent")
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

	child, err := ws.Acquire("dev", "subagent")
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
	if err := child.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the parent's live session was pruned when its sub-agent finished: %v", err)
	}
	if err := parent.Release(); err != nil {
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
	ws := newWorkspace(t)
	shared := filepath.Join(ws.CredentialsDir(), "creds.json")
	if err := os.MkdirAll(ws.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte(`{"token":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	checkout, err := ws.Acquire("dev", "call")
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
	if err := checkout.Release(); err != nil {
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
	ws := newWorkspace(t)
	checkout, err := ws.Acquire("dev", "call")
	if err != nil {
		t.Fatalf("Acquire with no login: %v", err)
	}
	if err := checkout.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if ws.HasLogin() {
		t.Error("HasLogin reported a login that was never written")
	}
}

// Profile paths are operator-overridable and prune() calls RemoveAll on what
// they resolve to, so one escaping the seat home is a config that deletes the
// engine user's dotfiles.
func TestAVolatilePathCannotEscapeTheSeatHome(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{"../../.ssh", "..", "/etc"} {
		if _, err := underRoot("/var/lib/crewlet/seats/dev/home", rel); err == nil {
			t.Errorf("underRoot accepted %q", rel)
		}
	}
	if _, err := underRoot("/var/lib/crewlet/seats/dev/home", ".fake/sessions"); err != nil {
		t.Errorf("a legitimate relative path was refused: %v", err)
	}
}

// One state directory is one login and one set of homes, which only works for
// the same CLI: two different ones disagree about which files are credentials
// and each would prune the other's login.
func TestTwoDifferentCLIsCannotShareAStateDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	if _, err := Shared(dir, "claude-code", testProfile(t)); err != nil {
		t.Fatalf("Shared: %v", err)
	}
	_, err := Shared(dir, "codex", testProfile(t))
	if err == nil {
		t.Fatal("two CLIs were allowed to share one state directory")
	}
	if !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("error %q does not name the CLI already there", err)
	}
}

// The same CLI over one directory is the SUPPORTED case — it is how per-phase
// models work off a single login — and both entries must get the one
// workspace, or each would prune the other's live seat homes.
func TestTwoEntriesOnOneDirectoryShareOneWorkspace(t *testing.T) {
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
	ws := newWorkspace(t)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			checkout, err := ws.Acquire("dev", string(rune('a'+i%16)))
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			if err := checkout.Release(); err != nil {
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
	ws := newWorkspace(t)
	checkout, err := ws.Acquire("dev", "call")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := checkout.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := checkout.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	live, err := ws.Acquire("dev", "next")
	if err != nil {
		t.Fatalf("Acquire after a double release: %v", err)
	}
	if err := live.Release(); err != nil {
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
