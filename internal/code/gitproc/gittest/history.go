package gittest

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

// A SOURCE REPOSITORY'S HISTORY, for every benchmark whose figure follows
// the shape of a history and not only its bytes: what an index costs — the
// commit-graph reads every commit, the bitmap selects and walks commits —
// follows the commits as much as the objects, and so does what a push into a
// large repository or a clone of one costs. ONE GENERATOR, so the cache's
// index figures and the host's push figures are taken over the same
// history.
//
// Every commit changes historyFilesPerCommit files of one directory of
// historyDirFiles: a commit object, the root tree, its directory's tree and
// four blobs, so commits are a seventh of the objects and trees two sevenths,
// and a gigabyte of history is about 400 000 commits — the hundreds of
// thousands a source repository of that size has. Each file's versions are a
// chain of deltas: a session writes every version of each file in turn
// (fast-import deltas a blob against the one before it in the stream), each
// rewriting a historyWindow of the file with seeded random bytes, then the
// session's commits, every file's version v before any file's v+1. The count
// of files is what the size scales.
const (
	// HistorySessions is how many fast-import sessions a History is
	// written in — and so how many pushes a benchmark that pushes it a
	// session at a time makes.
	HistorySessions       = 64
	historyVersions       = 20
	historyFileBytes      = 4 << 10
	historyWindow         = 192
	historyDirFiles       = 48 // a multiple of historyFilesPerCommit: no commit spans two directories
	historyFilesPerCommit = 4
)

// LargeEnv is the variable that, set to 1, runs the benchmark arms whose
// fixture takes minutes and gigabytes — a History of gigabytes, thousands of
// pushes. A default `make bench` skips them, saying so under -v.
const LargeEnv = "CREWLET_BENCH_LARGE"

// Large skips b unless LargeEnv is 1, naming what the arm would build — the
// one reader of the variable, so every package's large arms are turned on by
// the same setting.
func Large(b *testing.B, builds string) {
	b.Helper()
	if os.Getenv(LargeEnv) != "1" {
		b.Skipf("%s=1 runs this arm, whose fixture is %s", LargeEnv, builds)
	}
}

// historyBytesPerFile is about what one file adds to one session's pack — its
// first version whole, the rest as deltas of a little over the window each,
// and its share of the session's commits and trees — so a size's history
// holds about its size: measured, 99.4 MB of packs at 100 MB and 1.004 GB at
// 1 GB.
const historyBytesPerFile = 11_800

// historyIdent and historyEpoch are every History commit's committer and the
// first one's time, each later commit a second on, so a History is the same
// commits on every run.
const (
	historyIdent = "Crewlet Bench <bench@example.com>"
	historyEpoch = 1_767_225_600 // 2026-01-01T00:00:00Z
)

// After is the date (GIT_AUTHOR_DATE, GIT_COMMITTER_DATE) for the n-th commit
// made on top of a History of commits: a second past its last commit, and n
// more. A Client stamps every commit at one instant, the History's first, so
// a commit it made on top of a History would be older than its parent — a
// skewed clock, which git's date-ordered walks read as a reason to walk the
// whole history: `merge-base --is-ancestor` and `rev-list --not` took 2.5 s
// each over the gigabyte's 455 680 commits for a push of one commit.
func After(commits []string, n int) string {
	return fmt.Sprintf("@%d +0000", historyEpoch+len(commits)+n)
}

// Skewed is the date (GIT_AUTHOR_DATE, GIT_COMMITTER_DATE) for a commit made
// on top of a History by a client whose clock is behind: the day before the
// History's first commit, so the commit is older than its parent and than
// every commit under it — what a person's skewed clock makes, and what a
// Client's own fixed instant made before After existed. git's walks by date
// read it as a reason to walk the whole history.
func Skewed() string {
	return fmt.Sprintf("@%d +0000", historyEpoch-86_400)
}

// historySeed keys a History's random bytes and draws: fixed, so a size's
// history is the same bytes on every run — and the one BenchmarkMidx's
// figures were first measured over.
var historySeed = [32]byte{3}

// History fast-imports a history of about size bytes into a repository the
// engine's way (Empty) of its own, and answers the repository and main's
// commits, oldest first. The history is written in HistorySessions sessions
// of equal commits, so a benchmark pushes it a session at a time by pushing
// every HistorySessions-th commit.
func History(t testing.TB, c *Client, size int64) (string, []string) {
	t.Helper()
	files := max(1, int(size/HistorySessions/historyBytesPerFile))
	cur := make([][]byte, files)
	for f := range cur {
		cur[f] = make([]byte, historyFileBytes)
	}
	bytes := rand.NewChaCha8(historySeed)
	draw := rand.New(bytes)
	src := Empty(t)
	for s := range HistorySessions {
		FastImport(t, c, src, historyStream(s, s*historyCommits(files), cur, bytes, draw))
	}
	return src, strings.Fields(c.Run(t, src, "rev-list", "--reverse", "main"))
}

// historyCommits is how many commits a session over files files makes.
func historyCommits(files int) int {
	return historyVersions * ((files + historyFilesPerCommit - 1) / historyFilesPerCommit)
}

// historyStream is the fast-import stream of session s over cur, every file's
// content as the session before left it. Its commits are the history's
// first-th and on, each stamped historyEpoch and its number.
func historyStream(s, first int, cur [][]byte, bytes *rand.ChaCha8, draw *rand.Rand) func(*bufio.Writer) error {
	return func(w *bufio.Writer) error {
		for f := range cur {
			for v := range historyVersions {
				if s == 0 && v == 0 {
					_, _ = bytes.Read(cur[f])
				} else {
					off := draw.IntN(historyFileBytes - historyWindow)
					_, _ = bytes.Read(cur[f][off : off+historyWindow])
				}
				fmt.Fprintf(w, "blob\nmark :%d\ndata %d\n", f*historyVersions+v+1, historyFileBytes)
				_, _ = w.Write(cur[f])
				if err := w.WriteByte('\n'); err != nil {
					return err
				}
			}
		}
		n := first
		for v := range historyVersions {
			for lo := 0; lo < len(cur); lo += historyFilesPerCommit {
				msg := fmt.Sprintf("session %d version %d files %d\n", s, v, lo)
				fmt.Fprintf(w, "commit refs/heads/main\ncommitter %s %d +0000\ndata %d\n%s", historyIdent,
					historyEpoch+n, len(msg), msg)
				if s > 0 && n == first {
					_, _ = w.WriteString("from refs/heads/main^0\n")
				}
				for f := lo; f < min(lo+historyFilesPerCommit, len(cur)); f++ {
					fmt.Fprintf(w, "M 100644 :%d d%03d/f%05d\n", f*historyVersions+v+1, f/historyDirFiles, f)
				}
				if err := w.WriteByte('\n'); err != nil {
					return err
				}
				n++
			}
		}
		return nil
	}
}
