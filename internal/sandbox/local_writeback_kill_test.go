//go:build unix

package sandbox_test

import (
	"bufio"
	"bytes"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// A BOX'S TEARDOWN KILLED AT ANY INSTANT LEAVES ONE WHOLE LOGIN. A process
// running local boxes one after another — each seeded with the shared login,
// each run rewriting both of its files to the other login, each teardown
// writing them back — is killed at a random moment, again and again, and what
// the shared directory holds is then read as the next reader reads it: every
// time, one login or the other. Written back a file at a time, a teardown
// killed between its two files left one sign-in's token beside the other's
// config.toml. Most kills land inside a write, which is counted, so a run in
// which none did certifies nothing and fails.
//
// Mutation: write the box's refreshed files back one at a time, and some kill
// leaves half of each login.
func TestABoxTeardownKilledPartWayNeverLeavesHalfALogin(t *testing.T) {
	t.Parallel()
	const trials = 30
	midWrite := 0
	for trial := range trials {
		dir := filepath.Join(t.TempDir(), "credentials")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range writeBackLogins[1] {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // this test binary
		cmd.Env = append(os.Environ(), writeBackWriterEnv+"="+dir, writeBackStateEnv+"="+t.TempDir())
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("trial %d: the boxes did not start (%q, %v): %s", trial, line, err, stderr.String())
		}
		time.Sleep(time.Duration(rand.IntN(30_000)) * time.Microsecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()

		if _, err := os.Stat(hostbox.CredentialJournal(dir)); err == nil {
			midWrite++
		}
		release, err := hostbox.LockCredentials(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		login, err := hostbox.ReadCredentials(dir, []string{"auth.json", "config.toml"})
		release()
		if err != nil {
			t.Fatalf("trial %d: reading what the killed teardown left: %v", trial, err)
		}
		got := map[string]string{}
		for name, content := range login {
			got[name] = string(content)
		}
		if !slices.ContainsFunc(writeBackLogins, func(w map[string]string) bool { return maps.Equal(got, w) }) {
			t.Fatalf("trial %d: a teardown killed part way left %v — not one whole login", trial, got)
		}
	}
	t.Logf("%d of %d kills landed inside a write", midWrite, trials)
	if midWrite == 0 {
		t.Fatalf("none of %d kills landed inside a write, so the case certified nothing", trials)
	}
}
