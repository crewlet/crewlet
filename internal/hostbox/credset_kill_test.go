package hostbox_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/hostbox/hostboxtest"
)

// credentialWriterEnv makes this test binary a WRITER of a shared login rather
// than the suite: see [runCredentialWriter]. Its value is the shared
// credential directory.
const credentialWriterEnv = "HOSTBOX_TEST_CREDENTIAL_WRITER"

// TestMain runs the writer when this binary was re-executed as one, and the
// suite otherwise: dispatched here rather than from a test function, so the
// writer is never reported as a test that skipped. Here rather than in package
// hostbox's own tests because the kill matrix imports hostbox, so only the
// external package can run it.
func TestMain(m *testing.M) {
	if dir := os.Getenv(credentialWriterEnv); dir != "" {
		runCredentialWriter(dir)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// killedScript is what the writer writes after its setup: a login of two
// files, then one that drops one of them, then both put back, then the same
// login again, which changes nothing.
var killedScript = hostboxtest.Script{
	Names: []string{"auth.json", "config.toml"},
	Logins: []map[string]string{
		{"auth.json": `{"token":"first sign-in"}`, "config.toml": "provider = \"first\"\n"},
		{"auth.json": `{"token":"second sign-in"}`},
		{"auth.json": `{"token":"first sign-in"}`, "config.toml": "provider = \"first\"\n"},
		{"auth.json": `{"token":"first sign-in"}`, "config.toml": "provider = \"first\"\n"},
	},
}

// runCredentialWriter writes killedScript's logins into dir in turn, each in
// one write under the lock every writer takes, the first as its setup, with
// the matrix's observer on every write.
func runCredentialWriter(dir string) {
	w := hostboxtest.NewWriter()
	ctx := context.Background()
	write := func(login map[string]string) {
		release, err := hostbox.LockCredentials(ctx, dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		files := map[string][]byte{}
		for name, content := range login {
			files[name] = []byte(content)
		}
		err = hostbox.WriteCredentials(dir, killedScript.Names, files, w.Steps())
		release()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	write(killedScript.Logins[0])
	w.Ready()
	for _, login := range killedScript.Logins[1:] {
		write(login)
	}
	w.Done()
}

// A WRITER KILLED AT ANY STEP LEAVES ONE LOGIN OR THE OTHER, NEVER HALF OF
// EACH. A real process writing three logins in turn under the lock, each in a
// write of its own, is killed at every step of every write, one process per
// step, and what the directory holds is judged exactly and then read under the
// same lock as the next process reads it: every time, the login the
// interrupted write committed. Its steps are held to the exact sequence the
// protocol takes, so a step the write stops reporting fails here rather than
// quietly shrinking the matrix.
//
// Mutation: write the files without the journal, and the run reaches no
// commit; report the commit ahead of the journal's rename, and the kill there
// finds no journal; drop the removed step, and the steps differ from the ten
// named.
func TestAKilledWriterNeverLeavesHalfALogin(t *testing.T) {
	t.Parallel()
	stops := hostboxtest.KillAtEveryStep(t, killedScript, func(t *testing.T) hostboxtest.Run {
		dir := filepath.Join(t.TempDir(), "credentials")
		cmd := exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // this test binary
		cmd.Env = append(os.Environ(), credentialWriterEnv+"="+dir)
		return hostboxtest.Run{Cmd: cmd, Dir: dir, Read: func(t *testing.T) map[string]string {
			t.Helper()
			release, err := hostbox.LockCredentials(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			login, err := hostbox.ReadCredentials(dir, killedScript.Names)
			release()
			if err != nil {
				t.Fatalf("reading what the killed writer left: %v", err)
			}
			out := map[string]string{}
			for name, content := range login {
				out[name] = string(content)
			}
			return out
		}}
	})
	var got []string
	for _, stop := range stops {
		got = append(got, fmt.Sprintf("%d %s %s", stop.Write, stop.Step, stop.Name))
	}
	want := []string{
		"1 committed ", "1 written auth.json", "1 removed config.toml", "1 closed ",
		"2 committed ", "2 written auth.json", "2 written config.toml", "2 closed ",
		"3 committed ", "3 closed ",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the writer's steps were %q, want %q", got, want)
	}
}
