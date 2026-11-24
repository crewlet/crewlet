package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexCompressedFixture is [codexRolloutFixture] as Codex 0.171.0's
// local_thread_store_compression wrote it: the fixture aged 8 days in a
// CODEX_HOME, compressed at the next Codex start.
const codexCompressedFixture = "testdata/codex/0.171.0/rollout.jsonl.zst"

// codexFixtureThread is the thread [codexRolloutFixture] records.
const codexFixtureThread = "01a20695-4ebf-7ed3-874f-b28aac7c7b49"

// materialize runs codex-materialize with args, and returns its exit status
// and stderr.
func materialize(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var stderr bytes.Buffer
	code, once := runOnce(context.Background(), append([]string{"-adapter", "codex-materialize"}, args...),
		func(string) string { return "" }, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if !once {
		t.Fatal("codex-materialize is not a one-shot adapter")
	}
	return code, stderr.String()
}

// compressedSessions is a sessions directory holding body as the compressed
// rollout of thread, dated where Codex dates it, its mtime at, and is the
// directory and the compressed file's path.
func compressedSessions(t *testing.T, thread string, body []byte, at time.Time) (string, string) {
	t.Helper()
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "2026", "11", "21")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	z := filepath.Join(dir, "rollout-2026-11-21T18-33-59-"+thread+".jsonl.zst")
	if err := os.WriteFile(z, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(z, at, at); err != nil {
		t.Fatal(err)
	}
	return sessions, z
}

// leftovers is every file under dir whose name starts with a dot: a temporary
// file a materialize left behind.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".") && path != dir {
			out = append(out, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A ROLLOUT A PERSON'S CODEX COMPRESSED IS WRITTEN PLAIN AS CODEX WOULD WRITE
// IT: the bytes Codex compressed, under the name less `.zst`, its mode and
// mtime kept (Codex's 7-day clock reads the mtime), the `.zst` removed and no
// temporary file left; another thread's compressed rollout is not touched.
// Read compressed, the gate's renew and every pack would read no line.
//
// Mutation: skip the decode, drop the Chtimes or the Chmod, keep the `.zst`,
// or match any thread, and a check below fails.
func TestARolloutAPersonsCodexCompressedIsWrittenPlainAsCodexWouldWriteIt(t *testing.T) {
	t.Parallel()
	plain, err := os.ReadFile(codexRolloutFixture)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := os.ReadFile(codexCompressedFixture)
	if err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	sessions, z := compressedSessions(t, codexFixtureThread, compressed, aged)
	other := filepath.Join(filepath.Dir(z), "rollout-2026-11-21T18-33-59-01a20695-0000-7000-8000-000000000000.jsonl.zst")
	if err = os.WriteFile(other, compressed, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, said := materialize(t, "-sessions", sessions, "-thread", codexFixtureThread); code != exitDone {
		t.Fatalf("codex-materialize exited %d: %s", code, said)
	}
	got, err := os.ReadFile(strings.TrimSuffix(z, ".zst"))
	if err != nil {
		t.Fatalf("no plain rollout was written: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("the plain rollout is %d bytes, want the %d Codex compressed", len(got), len(plain))
	}
	info, err := os.Stat(strings.TrimSuffix(z, ".zst"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.ModTime().Equal(aged) {
		t.Errorf("the plain rollout is mode %v at %v, want 0600 at %v", info.Mode().Perm(), info.ModTime(), aged)
	}
	if _, err = os.Stat(z); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the compressed rollout is left beside the plain one")
	}
	if _, err = os.Stat(other); err != nil {
		t.Errorf("another thread's compressed rollout was touched: %v", err)
	}
	if left := leftovers(t, sessions); len(left) != 0 {
		t.Errorf("left behind %q", left)
	}
}

// A PLAIN ROLLOUT BESIDE A COMPRESSED ONE WINS: Codex writes it before it
// removes the `.zst`, so it is the newer, and it is never overwritten; the
// stale `.zst` is removed, since a resume would take it over a plain copy
// elsewhere (measured on 0.171.0).
//
// Mutation: decode over the plain file, or keep the `.zst`, and a check fails.
func TestAPlainCodexRolloutBesideACompressedOneWins(t *testing.T) {
	t.Parallel()
	compressed, err := os.ReadFile(codexCompressedFixture)
	if err != nil {
		t.Fatal(err)
	}
	sessions, z := compressedSessions(t, codexFixtureThread, compressed, time.Now())
	newer := []byte(`{"type":"session_meta","payload":{"id":"` + codexFixtureThread + `"}}` + "\n")
	if err = os.WriteFile(strings.TrimSuffix(z, ".zst"), newer, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, said := materialize(t, "-sessions", sessions, "-thread", codexFixtureThread); code != exitDone {
		t.Fatalf("codex-materialize exited %d: %s", code, said)
	}
	if got, _ := os.ReadFile(strings.TrimSuffix(z, ".zst")); !bytes.Equal(got, newer) {
		t.Errorf("the plain rollout became %d bytes, want the plain file kept", len(got))
	}
	if _, err = os.Stat(z); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the stale compressed rollout is left for a resume to take")
	}
}

// A COMPRESSED ROLLOUT THAT DOES NOT DECODE IS LEFT AS IT WAS, the helper
// exiting 1 and saying which, so the gate starts a session of its own rather
// than resume one Codex cannot read; nothing half written is left under the
// plain name or beside it. A command line naming no sessions directory or a
// thread a pattern could misread, or one with -config, is refused.
//
// Mutation: publish before the decode finishes, or exit 0 on a decode error,
// and a check fails.
func TestACompressedCodexRolloutThatDoesNotDecodeIsLeftAsItWas(t *testing.T) {
	t.Parallel()
	sessions, z := compressedSessions(t, codexFixtureThread, []byte("\x28\xb5\x2f\xfdnot a frame"), time.Now())
	code, said := materialize(t, "-sessions", sessions, "-thread", codexFixtureThread)
	if code != exitError || !strings.Contains(said, z) {
		t.Errorf("codex-materialize exited %d saying %q, want %d naming %s", code, said, exitError, z)
	}
	if _, err := os.Stat(z); err != nil {
		t.Errorf("the compressed rollout was removed: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(z, ".zst")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a plain rollout was written from what did not decode")
	}
	if left := leftovers(t, sessions); len(left) != 0 {
		t.Errorf("left behind %q", left)
	}
	for name, args := range map[string][]string{
		"no sessions":    {"-thread", codexFixtureThread},
		"no thread":      {"-sessions", sessions},
		"a pattern":      {"-sessions", sessions, "-thread", "*"},
		"with a -config": {"-sessions", sessions, "-thread", codexFixtureThread, "-config", "x.json"},
	} {
		if code, _ := materialize(t, args...); code != exitUsage {
			t.Errorf("%s: exited %d, want %d", name, code, exitUsage)
		}
	}
}
