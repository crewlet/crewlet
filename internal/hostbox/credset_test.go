package hostbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writerEnv makes this test binary a WRITER of a shared login rather than the
// suite: see [TestMain] and [runWriter].
const writerEnv = "HOSTBOX_TEST_CREDENTIAL_WRITER"

// TestMain runs the writer when this binary was re-executed as one, and the
// suite otherwise — dispatched here rather than from a test function, so the
// writer is never reported as a test that skipped.
func TestMain(m *testing.M) {
	if dir := os.Getenv(writerEnv); dir != "" {
		runWriter(dir)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// twoLogins are the logins the killed writer alternates between: two files
// each, every file different, so any mix of the two is told apart from both.
var twoLogins = []map[string][]byte{
	{"auth.json": []byte(`{"token":"first sign-in"}`), "config.toml": []byte("provider = \"first\"\n")},
	{"auth.json": []byte(`{"token":"second sign-in"}`), "config.toml": []byte("provider = \"second\"\n")},
}

var twoLoginNames = []string{"auth.json", "config.toml"}

// runWriter writes the two logins in turn, for ever, each under the lock every
// writer takes, and says "ready" once the first has landed — so the case that
// kills it kills a writer in its loop rather than one still starting.
func runWriter(dir string) {
	ctx := context.Background()
	for i := 0; ; i++ {
		release, err := LockCredentials(ctx, dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := WriteCredentials(dir, twoLoginNames, twoLogins[i%2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		release()
		if i == 0 {
			fmt.Println("ready")
		}
	}
}

// writeRaw puts files in a directory directly, as the state a writer left.
func writeRaw(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// readLogin is ReadCredentials over the two names, as strings.
func readLogin(t *testing.T, dir string) (map[string]string, error) {
	t.Helper()
	login, err := ReadCredentials(dir, twoLoginNames)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, content := range login {
		out[name] = string(content)
	}
	return out, nil
}

// ONE JOURNAL, BESIDE THE DIRECTORY, for the reason the lock is: nothing that
// copies the directory carries a write in progress with it.
func TestTheCredentialJournalSitsBesideItsDirectory(t *testing.T) {
	t.Parallel()
	for dir, want := range map[string]string{
		"/state/credentials":   "/state/credentials.pending",
		"/state/credentials/":  "/state/credentials.pending",
		"/state/./credentials": "/state/credentials.pending",
	} {
		if got := CredentialJournal(dir); got != want {
			t.Errorf("CredentialJournal(%q) = %q, want %q", dir, got, want)
		}
	}
}

// A WRITE THAT STOPS PART WAY IS NEVER READ AS A LOGIN, AND IS FINISHED BY THE
// NEXT READ. The second file cannot be written (a directory stands where it
// goes), so the write stops with the first file in place: the shape a full disk
// or a killed process leaves. Every read while the obstacle stands refuses
// rather than answering with the one file, and the first read after it is
// gone puts the whole login in place and answers with it.
//
// Mutation: write the files without the journal, and the read answers with the
// first file alone.
func TestAWriteThatStopsPartWayIsFinishedByTheNextRead(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	obstacle := filepath.Join(dir, "config.toml")
	writeRaw(t, filepath.Join(obstacle, "in-the-way"), map[string]string{"x": "x"})

	err := WriteCredentials(dir, twoLoginNames, twoLogins[0])
	if err == nil {
		t.Fatal("a write whose second file could not be written reported success")
	}
	var unapplied *UnappliedError
	if !errors.As(err, &unapplied) || !Committed(err) {
		t.Fatalf("the write = %v, want an UnappliedError: its journal landed, so it is committed", err)
	}
	if got, err := readLogin(t, dir); err == nil {
		t.Fatalf("a read of a write that stopped part way answered %v rather than refusing", got)
	}

	if err := os.RemoveAll(obstacle); err != nil {
		t.Fatal(err)
	}
	got, err := readLogin(t, dir)
	if err != nil {
		t.Fatalf("ReadCredentials: %v", err)
	}
	if want := map[string]string{"auth.json": `{"token":"first sign-in"}`,
		"config.toml": "provider = \"first\"\n"}; !maps.Equal(got, want) {
		t.Fatalf("the read answered %v, want the whole committed login %v", got, want)
	}
	if _, err := os.Stat(CredentialJournal(dir)); !os.IsNotExist(err) {
		t.Errorf("the journal of a finished write is still there: %v", err)
	}
	for name, body := range got {
		if raw, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(raw) != body {
			t.Errorf("%s on disk = %q (%v), want the finished write's %q", name, raw, err, body)
		}
	}
}

// A WRITE THAT FAILS BEFORE ITS JOURNAL LANDS WRITES NOTHING, and says it is
// not committed: the caller's steps after a write must not follow it.
func TestAWriteThatFailsBeforeItsJournalWritesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "credentials")
	writeRaw(t, dir, map[string]string{"auth.json": "kept"})
	// A directory where the journal goes: its rename cannot land.
	writeRaw(t, filepath.Join(CredentialJournal(dir), "in-the-way"), map[string]string{"x": "x"})

	err := WriteCredentials(dir, twoLoginNames, twoLogins[1])
	if err == nil || Committed(err) {
		t.Fatalf("a write whose journal could not land = %v, want a failure that did not commit", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil || string(raw) != "kept" {
		t.Errorf("auth.json = %q (%v); a write that never committed touched the login", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); !os.IsNotExist(err) {
		t.Errorf("a write that never committed wrote config.toml: %v", err)
	}
}

// A WRITER KILLED AT ANY INSTANT LEAVES ONE LOGIN OR THE OTHER, NEVER HALF OF
// EACH. A real process writing two logins in turn under the lock is killed at a
// random moment, again and again, and what the directory holds afterwards is
// read under the same lock: every time, exactly one of the two logins. Most
// kills land inside a write — between its journal and its last file — which is
// counted, so a run in which none did certifies nothing and fails.
//
// Mutation: write the files without the journal, and some kill leaves one
// login's auth.json beside the other's config.toml.
func TestAKilledWriterNeverLeavesHalfALogin(t *testing.T) {
	t.Parallel()
	const trials = 40
	midWrite := 0
	for trial := range trials {
		dir := filepath.Join(t.TempDir(), "credentials")
		cmd := exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // this test binary
		cmd.Env = append(os.Environ(), writerEnv+"="+dir)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("trial %d: the writer did not start (%q, %v): %s", trial, line, err, stderr.String())
		}
		time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()

		if _, err := os.Stat(CredentialJournal(dir)); err == nil {
			midWrite++
		}
		release, err := LockCredentials(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readLogin(t, dir)
		release()
		if err != nil {
			t.Fatalf("trial %d: reading what the killed writer left: %v", trial, err)
		}
		whole := false
		for _, login := range twoLogins {
			want := map[string]string{}
			for name, content := range login {
				want[name] = string(content)
			}
			whole = whole || maps.Equal(got, want)
		}
		if !whole {
			t.Fatalf("trial %d: a writer killed mid-write left %v — part of one login and part of "+
				"the other", trial, got)
		}
	}
	t.Logf("%d of %d kills landed inside a write", midWrite, trials)
	if midWrite == 0 {
		t.Fatalf("none of %d kills landed inside a write, so the case certified nothing", trials)
	}
}

// A WRITE REPLACES ONE IT FINDS STANDING, unapplied — which is how a logout
// discards a login that was never put in place, even one that CANNOT be put in
// place — and carries over only what that write decided about files outside
// its own names, so nothing it committed is lost.
//
// The standing write stopped because auth.json could not be written there (an
// empty directory stands in its place, which a removal takes and a rename onto
// refuses), so a logout that finished it first would stop at the same place
// and remove nothing.
//
// Mutation: finish the standing write before replacing it, and the logout
// fails; drop the standing write's other decisions, and the file outside the
// logout's names is never written.
func TestAWriteReplacesOneLeftStanding(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	writeRaw(t, dir, map[string]string{"config.toml": "old config"})
	if err := os.Mkdir(filepath.Join(dir, "auth.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(credentialJournal{
		Version: journalVersion,
		Write: map[string][]byte{
			"auth.json": []byte("new auth"), "config.toml": []byte("new config"),
			"elsewhere.json": []byte("outside the logout's names"),
		},
		Remove: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CredentialJournal(dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// The logout: every name it knows, none of them held.
	if err := WriteCredentials(dir, twoLoginNames, nil); err != nil {
		t.Fatalf("WriteCredentials: %v", err)
	}
	got, err := readLogin(t, dir)
	if err != nil {
		t.Fatalf("ReadCredentials: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("after the logout the login holds %v", got)
	}
	kept, err := os.ReadFile(filepath.Join(dir, "elsewhere.json"))
	if err != nil || string(kept) != "outside the logout's names" {
		t.Errorf("elsewhere.json = %q (%v); a decision the logout did not make was dropped", kept, err)
	}
}

// A JOURNAL THIS BUILD CANNOT FINISH IS REFUSED, by every read and every write,
// naming itself and what discarding it means: finished as far as it could be
// read, it would put a login nobody wrote in front of every seat.
func TestAJournalThisBuildCannotFinishIsRefused(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"not json":        "{",
		"another shape":   `{"version":1,"write":{},"remove":[],"rename":{}}`,
		"another version": `{"version":2,"write":{},"remove":[]}`,
		"a path":          `{"version":1,"write":{"../escape":"eA=="},"remove":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "credentials")
			writeRaw(t, dir, map[string]string{"auth.json": "a"})
			if err := os.WriteFile(CredentialJournal(dir), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, readErr := ReadCredentials(dir, twoLoginNames)
			writeErr := WriteCredentials(dir, twoLoginNames, twoLogins[0])
			for verb, err := range map[string]error{"read": readErr, "write": writeErr} {
				if err == nil || !strings.Contains(err.Error(), CredentialJournal(dir)) ||
					!strings.Contains(err.Error(), "remove it to discard that write") {
					t.Errorf("a %s over a journal it cannot finish = %v, want a refusal naming it and the way out",
						verb, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "..", "escape")); !os.IsNotExist(err) {
				t.Errorf("a journal naming a path wrote outside the directory: %v", err)
			}
		})
	}
}

// WHAT IS NO LOGIN IS NEVER WRITTEN AS ONE: a file past the bound, a file
// outside the names the login may hold, a name that is a path — each refused
// before anything lands.
func TestAWriteOfWhatIsNoLoginIsRefused(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		names []string
		login map[string][]byte
	}{
		"too large": {twoLoginNames, map[string][]byte{
			"auth.json": make([]byte, MaxCredentialBytes+1)}},
		"outside its names": {twoLoginNames, map[string][]byte{"stray.json": []byte("x")}},
		"a path":            {[]string{"../auth.json"}, map[string][]byte{"../auth.json": []byte("x")}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "credentials")
			if err := WriteCredentials(dir, c.names, c.login); err == nil || Committed(err) {
				t.Fatalf("WriteCredentials = %v, want a refusal that committed nothing", err)
			}
			if _, err := os.Stat(CredentialJournal(dir)); !os.IsNotExist(err) {
				t.Errorf("a refused write left a journal: %v", err)
			}
			if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 0 {
				t.Errorf("a refused write left %v", entries)
			}
		})
	}
}

// A FILE THAT IS NO LOGIN FILE IS AN ERROR, NOT AN ABSENCE: read as nothing, a
// directory where a login file goes would hand a seat the login without it.
func TestALoginFileThatIsNotAFileIsAnError(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	writeRaw(t, dir, map[string]string{"auth.json": "a"})
	writeRaw(t, filepath.Join(dir, "config.toml"), map[string]string{"x": "x"})
	if got, err := readLogin(t, dir); err == nil {
		t.Fatalf("a directory under a login file's name read as %v", got)
	}
}
