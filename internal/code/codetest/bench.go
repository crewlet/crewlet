package codetest

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
)

// BenchGitEnv names the git a benchmark's pools run — an absolute path, for
// the git the engine image ships when the machine's own is another — while
// the client stays the git on PATH (gittest's). Unset, the pools run the git
// on PATH too.
//
// BENCHMARKS ONLY, as s3fake.RealBucket's variables are: a test that passed
// or failed on whichever git a developer happened to name would not be a test
// of this tree. And it names only the SERVER's git, because a figure is the
// pool's — upload-pack, receive-pack, pack-objects, index-pack — while a
// person's client is whatever git they run.
const BenchGitEnv = "CREWLET_BENCH_GIT"

// BenchGit is the git BenchGitEnv names, for SoloOptions.GitPath: "" when it
// names none, so the pools run the git on PATH. A path that is not absolute,
// or names nothing a pool can run, fails b naming the variable — a figure
// taken on a git nobody chose is a figure about the wrong git.
func BenchGit(b *testing.B) string {
	b.Helper()
	path := os.Getenv(BenchGitEnv)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		b.Fatalf("%s=%q is not an absolute path to a git binary", BenchGitEnv, path)
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		b.Fatalf("%s=%q names no git binary this process can run (%v)", BenchGitEnv, path, err)
	}
	return path
}

// Quiet keeps a benchmark's output to its figures: every node a topology
// starts logs at info, and an info line in the middle of benchmark output is a
// line benchstat cannot read. Warn and above still print, because one of those
// during a measurement is a finding.
func Quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}
