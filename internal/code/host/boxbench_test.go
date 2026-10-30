package host_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CODING BOX'S GIT, MEASURED (ADR-0063): what the two clone recipes that
// fetch cost a box and the git node it clones from (BenchmarkBoxClone), what
// the box's own git costs once its checkout is made and which of git's
// box-side settings earn a place in the engine's box git environment
// (BenchmarkBoxWorktree), and what a box borrowing a node's copy would save
// (BenchmarkBoxDissociate). Every box here is a box as the engine makes one:
// a run launched on the fleet's run store given the repository, its token
// minted for that launch, its requests through /box/git/ with the run's
// credential, and its commands run as a box runs them — `sh -c`, under
// EXACTLY the environment the engine's launch gives it (sandbox.BoxGit over a
// HOME with no configuration, as a template's box has, and the commit
// hook's values) — never the gittest client's, whose gc.auto=0 and
// maintenance.auto=false would hide the gc a box's own git does. The node
// is warm: every pack in its maintainer's index with the bitmap, and a
// commit-graph written (warmBox), before any timer. Every figure is over
// codetest's memobj; the package doc's measured section and
// docs/concepts/code-sandbox.md's "What a clone costs" carry them, with the
// decisions taken from them.

// boxSeat is the seat every benchmark box's run is launched for, and
// boxBranch the branch its run works on.
const (
	boxSeat   = codetest.FixtureAlice
	boxBranch = "seats/" + codetest.FixtureAlice + "/ENG-7"
)

// boxSize is a history BenchmarkBoxClone and BenchmarkBoxDissociate clone: a
// gittest.History of bytes, pushed a session at a time; large is an arm
// only CREWLET_BENCH_LARGE runs.
type boxSize struct {
	name  string
	bytes int64
	large bool
}

// boxSizes are the histories a box clones: a hundred mebibytes — 138 files in
// three directories over 44 800 commits — and the gigabyte BenchmarkPushSmall
// pushes, 1 421 files in thirty directories over 455 680 commits.
var boxSizes = []boxSize{{"100MB", 100 << 20, false}, {"1GB", 1 << 30, true}}

// boxCone is the cone an arm's checkout narrows to: none, or ONE DIRECTORY
// — the first, which every history and tree of gittest holds.
func boxCone(cone, dir string) []string {
	if cone == "one-dir" {
		return []string{dir}
	}
	return nil
}

// historyDir is the first directory of a gittest.History, which a cone of
// one directory names.
const historyDir = "d000"

// BenchmarkBoxClone is WHAT THE TWO RECIPES THAT FETCH COST: a box given the
// repository by sandbox.CloneRecipe, timed from the step's start to its
// checked-out tree, on a warm node, a fresh box an operation.
//
//   - size=100MB|1GB is the history (boxSizes; 1GB only under
//     CREWLET_BENCH_LARGE), built once for every arm under it;
//   - git=pin|client-floor is the box's git: the node pin `make bench` puts
//     first on PATH, the image's, and the client floor's own build (2.40.4),
//     the oldest git a box may run — a template's git is the operator's —
//     at 100MB;
//   - recipe=coding|single-sha is sandbox.ClonePurpose, each recipe given the
//     repository its purpose takes (boxRepoFor): a blobless clone at the tip
//     on the run's branch, or a check's one commit at depth one. The third
//     purpose, sandbox.CloneBorrowed, is no arm: it fetches nothing — the box
//     reads a git node's own copy, mounted into it — so the node answers it
//     none of what an arm reports, and what a borrow saves a box is
//     BenchmarkBoxDissociate's question;
//   - cone=none|one-dir checks out the whole tree or the history's first
//     directory.
//
// Reported an operation: the wall time (s/op); sent-MB, what the node
// answered the box; fetches, the fetch commands the box sent, of which
// lazy-fetches came after the recipe's own clone or fetch — its checkout's
// fetch of the blobs it writes — bringing lazy-MB; objects, what the box's
// repository holds; and server-cpu-ms, the node's git. Those lazy fetches
// are all a build reading every file the checkout holds needs: the checkout
// fetched every blob it wrote, so such a build asks git for nothing more.
func BenchmarkBoxClone(b *testing.B) {
	codetest.Quiet(b)
	for _, size := range boxSizes {
		b.Run("size="+size.name, func(b *testing.B) {
			if size.large {
				gittest.Large(b, "a gigabyte's history pushed in 64 sessions")
			}
			f, p := benchFleet(b, memobj.New(), 1)
			rows, tip := boxHistory(b, f, p, size.bytes)
			gits := []string{"pin", "client-floor"}
			if size.large {
				gits = gits[:1]
			}
			for _, g := range gits {
				b.Run("git="+g, func(b *testing.B) {
					git := gittest.Require(b)
					if g == "client-floor" {
						git = gittest.ClientFloor(b)
					}
					box := newBenchBox(b, f, rows, git)
					for _, purpose := range sandbox.ClonePurposes() {
						if purpose == sandbox.CloneBorrowed {
							continue
						}
						for _, cone := range []string{"none", "one-dir"} {
							b.Run(fmt.Sprintf("recipe=%s/cone=%s", purpose, cone), func(b *testing.B) {
								repo := boxRepoFor(rows, tip, boxCone(cone, historyDir), purpose)
								boxClones(b, f, p, box, repo, purpose)
							})
						}
					}
				})
			}
		})
	}
}

// boxClones is one BenchmarkBoxClone arm: a fresh box's clone of repo for
// purpose an operation.
func boxClones(b *testing.B, f *codetest.SoloFleet, p *codetest.Person, box *benchBox, repo sandbox.RunRepo,
	purpose sandbox.ClonePurpose) {
	b.Helper()
	step, err := sandbox.CloneRecipe(repo, purpose)
	if err != nil {
		b.Fatal(err)
	}
	node := f.Nodes()[0]
	var (
		sent, lazySent, objects int64
		fetches, lazy           int
		cpu                     float64
	)
	for b.Loop() {
		b.StopTimer()
		// A fresh box an operation, removed as the operation ends, so an
		// arm holds one checkout at a time.
		dir := b.TempDir()
		box.proxy.take()
		before := serverCPU(node.Metrics())
		b.StartTimer()
		boxStep(b, dir, box.env, step)
		b.StopTimer()
		cpu += serverCPU(node.Metrics()) - before
		first := true
		for _, a := range box.proxy.take() {
			sent += a.bytes
			if !a.fetch {
				continue
			}
			fetches++
			if first {
				first = false
				continue
			}
			lazy++
			lazySent += a.bytes
		}
		objects += countObjects(b, p.Git(), filepath.Join(dir, repo.Repo))
		if err := os.RemoveAll(dir); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	n := float64(b.N)
	b.ReportMetric(float64(sent)/n/1e6, "sent-MB")
	b.ReportMetric(float64(fetches)/n, "fetches")
	b.ReportMetric(float64(lazy)/n, "lazy-fetches")
	b.ReportMetric(float64(lazySent)/n/1e6, "lazy-MB")
	b.ReportMetric(float64(objects)/n, "objects")
	b.ReportMetric(cpu/n, "server-cpu-ms")
}

// BenchmarkBoxWorktree is WHAT A BOX'S OWN GIT COSTS ONCE ITS CHECKOUT IS
// MADE, and so which of git's settings for a large work tree the engine's
// box git environment carries:
//
//   - files=10000|100000|1000000 is a gittest.Tree of that many files of 4
//     KiB in directories of 48, pushed a commit at a time (1000000 only
//     under CREWLET_BENCH_LARGE), the width a checkout's cost follows;
//   - index=default|v4+untracked is the box's index configuration: git's own
//     — the engine's two index entries taken back by entries after them, to
//     version 2 and the untracked cache kept as the index has it — or the
//     engine's (sandbox.BoxGit's entries 6 and 7: index.version=4 and
//     core.untrackedCache=true, what feature.manyFiles sets but
//     index.skipHash, which writes an index older libgit2 builds refuse);
//   - cone=none|one-dir checks out the whole tree or its first directory;
//   - op=checkout is the coding recipe's clone and checkout, a fresh box an
//     operation; op=status a warm `git status` and op=commit `git commit -qam`
//     of one changed file, each in one checkout the arm made beforehand and
//     read twice before the timer;
//   - fsmonitor=on, at cone=none, is op=status and op=commit with git's
//     monitor on in the clone's own configuration, its daemon started by the
//     first status, which first-status-ms reports. The coding recipe turns
//     it on where the box can keep it (sandbox.CloneRecipe), so every other
//     status and commit arm turns it off in its checkout first.
//
// Every op reports its median and its p10 and p90 in milliseconds: the rule
// a setting is adopted by is judged on medians, the gain larger than the
// spread (code-sandbox.md, "What a clone costs").
//
// session/gc=foreground|detached runs commands of an agent's shape in a
// blobless coding clone of a history (boxSession) until git's own automatic
// gc runs, and reports what it cost against the session's git
// (gc-cpu-share) and whether any command met its lock.
func BenchmarkBoxWorktree(b *testing.B) {
	codetest.Quiet(b)
	for _, files := range []int{10_000, 100_000, 1_000_000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			if files > gittest.TreeCommitFiles {
				gittest.Large(b, "a tree of a million 4 KiB files, 4.1 GB pushed in ten commits")
			}
			f, p := benchFleet(b, memobj.New(), 1)
			rows, tip := boxTree(b, f, p, files)
			for _, index := range []struct {
				name      string
				overrides []string
			}{
				// git's own: the engine's index entries taken back to what
				// git does unset — version 2 (3 where an entry needs it) and
				// the untracked cache kept as the index has it, none.
				{"default", []string{"index.version", "2", "core.untrackedCache", "keep"}},
				// The engine's entries 6 and 7 (sandbox.BoxGit).
				{"v4+untracked", nil},
			} {
				box := newBenchBox(b, f, rows, gittest.Require(b), index.overrides...)
				for _, cone := range []string{"none", "one-dir"} {
					repo := boxRepo(rows, tip, boxCone(cone, gittest.TreeDir(0)))
					arm := fmt.Sprintf("index=%s/cone=%s", index.name, cone)
					b.Run(arm+"/op=checkout", func(b *testing.B) {
						boxCheckouts(b, box, repo)
					})
					checkout := boxCheckout(b, box, repo)
					// The monitor the coding recipe turned on stays off until
					// the arm that measures it.
					boxRun(b, checkout, box.env, "git config core.fsmonitor false")
					b.Run(arm+"/op=status", func(b *testing.B) {
						boxStatuses(b, box, checkout, 0)
					})
					b.Run(arm+"/op=commit", func(b *testing.B) {
						boxCommits(b, box, checkout, files)
					})
					if cone != "none" {
						continue
					}
					// THE SAME CHECKOUT with the monitor on, once the arms
					// above have read it without.
					b.Run(arm+"/fsmonitor=on", func(b *testing.B) {
						first, watches := boxFsmonitor(b, box, checkout, files)
						b.Run("op=status", func(b *testing.B) {
							boxStatuses(b, box, checkout, first)
							b.ReportMetric(float64(watches), "inotify-watches")
						})
						b.Run("op=commit", func(b *testing.B) {
							boxCommits(b, box, checkout, files)
						})
					})
				}
			}
		})
	}
	b.Run("session", func(b *testing.B) {
		f, p := benchFleet(b, memobj.New(), 1)
		rows, tip := boxHistory(b, f, p, sessionBytes)
		for _, gc := range []string{"foreground", "detached"} {
			b.Run("gc="+gc, func(b *testing.B) {
				var overrides []string
				if gc == "foreground" {
					// So the gc's CPU is the CPU of the command that started
					// it, which waits for it: detached, no process waits for
					// it and its CPU is counted nowhere.
					overrides = []string{"gc.autoDetach", "false", "maintenance.autoDetach", "false"}
				}
				box := newBenchBox(b, f, rows, gittest.Require(b), overrides...)
				boxSession(b, box, boxRepo(rows, tip, nil), gc == "foreground")
			})
		}
	})
}

// boxCheckouts is an op=checkout arm: a fresh box's coding recipe of repo an
// operation.
func boxCheckouts(b *testing.B, box *benchBox, repo sandbox.RunRepo) {
	b.Helper()
	step, err := sandbox.CloneRecipe(repo, sandbox.CloneCoding)
	if err != nil {
		b.Fatal(err)
	}
	var took []time.Duration
	for b.Loop() {
		b.StopTimer()
		// A fresh box an operation, removed as the operation ends, so an
		// arm holds one checkout at a time.
		dir := b.TempDir()
		b.StartTimer()
		at := time.Now()
		boxStep(b, dir, box.env, step)
		took = append(took, time.Since(at))
		b.StopTimer()
		if err := os.RemoveAll(dir); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	reportSpread(b, took)
}

// boxCheckout is a coding checkout of repo in a box, made once with no timer
// for the arms that read it, and its directory.
func boxCheckout(b *testing.B, box *benchBox, repo sandbox.RunRepo) string {
	b.Helper()
	step, err := sandbox.CloneRecipe(repo, sandbox.CloneCoding)
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	boxStep(b, dir, box.env, step)
	return filepath.Join(dir, repo.Repo)
}

// pastRacy waits until the clock has left the second checkout's index was last
// written in. git compares a file whose mtime falls in its index's second by
// its CONTENT — racily clean, at the second's grain the pinned git is built
// with — so a status in that second hashes every such file rather than stat
// it, and each rewrites the index in the same second again: right after a
// checkout of thousands of files, that is the whole tree, a transient a box
// leaves once its agent's first status runs seconds after its setup. The
// status after this wait rewrites the index past it.
func pastRacy(b *testing.B, checkout string) {
	b.Helper()
	info, err := os.Stat(filepath.Join(checkout, ".git", "index"))
	if err != nil {
		b.Fatal(err)
	}
	time.Sleep(time.Until(info.ModTime().Truncate(time.Second).Add(time.Second + 10*time.Millisecond)))
}

// boxStatuses is an op=status arm: a warm `git status` of checkout an
// operation, after two before the timer once the index has left its racy
// second (pastRacy) — the first of which writes what the index caches — and
// first, the first status's own time where the arm measured one
// (fsmonitor's, which starts its daemon), reported beside.
func boxStatuses(b *testing.B, box *benchBox, checkout string, first time.Duration) {
	b.Helper()
	pastRacy(b, checkout)
	for range 2 {
		boxRun(b, checkout, box.env, "git status")
	}
	var took []time.Duration
	for b.Loop() {
		wall, _ := boxRun(b, checkout, box.env, "git status")
		took = append(took, wall)
	}
	reportSpread(b, took)
	if first > 0 {
		b.ReportMetric(float64(first)/float64(time.Millisecond), "first-status-ms")
	}
}

// boxCommits is an op=commit arm: one file of the tree changed, then `git
// commit -qam` timed, an operation — the commit-trailer hook the coding
// recipe wrote runs in each, as in every box.
func boxCommits(b *testing.B, box *benchBox, checkout string, files int) {
	b.Helper()
	pastRacy(b, checkout)
	boxRun(b, checkout, box.env, "git status")
	var took []time.Duration
	n := 0
	for b.Loop() {
		b.StopTimer()
		// A file of the checkout, a different one each time, its first byte
		// rewritten: one changed file in a tree of files (a cone holds the
		// first directory).
		path := filepath.Join(checkout, filepath.FromSlash(gittest.TreeFile(n%min(files, 48))))
		changeFile(b, path, n)
		n++
		b.StartTimer()
		wall, _ := boxRun(b, checkout, box.env, "git commit -qam 'one file changed'")
		took = append(took, wall)
	}
	reportSpread(b, took)
}

// changes counts every change changeFile has made in this process, so no two
// write the same bytes: an arm that changes a file an earlier arm changed the
// same way would leave nothing to commit.
var changes atomic.Int64

// changeFile rewrites the first bytes of the file at path with n and a count
// of its own, so each operation changes it whatever any before wrote.
func changeFile(b *testing.B, path string, n int) {
	b.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, "change %08d %016d\n", n, changes.Add(1)); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
}

// boxFsmonitor turns git's file-system monitor on in checkout's own
// configuration — the one place it could be adopted, the clone step, since
// it runs a daemon — and answers the first status's time, which starts the
// daemon, and the inotify watches the checkout needs (a directory each).
// SKIPPED, naming git's own words, where the box's git has no monitor on
// this platform; the daemon is stopped, and the setting dropped, when the
// arm ends.
func boxFsmonitor(b *testing.B, box *benchBox, checkout string, files int) (time.Duration, int) {
	b.Helper()
	if out, err := boxTry(b, checkout, box.env, "git fsmonitor--daemon status"); err != nil &&
		strings.Contains(out, "not supported") {
		b.Skipf("the box's git has no file-system monitor here: %s", strings.TrimSpace(out))
	}
	watches := files/48 + 2 // a directory each, the top level and .git
	if limit, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(limit))); err == nil && n < watches {
			b.Skipf("fs.inotify.max_user_watches is %d, under the %d directories the checkout holds", n, watches)
		}
	}
	boxRun(b, checkout, box.env, "git config core.fsmonitor true")
	stopMonitor(b, box, checkout)
	first, _ := boxRun(b, checkout, box.env, "git status")
	if out, err := boxTry(b, checkout, box.env, "git fsmonitor--daemon status"); err != nil {
		b.Fatalf("the first status with core.fsmonitor left no daemon watching the checkout: %v\n%s", err, out)
	}
	return first, watches
}

// stopMonitor stops the monitor's daemon of the checkout at dir, if any runs,
// when b ends — before b's directories go: the coding recipe turns the
// monitor on (sandbox.CloneRecipe), and the first git command that asks it
// starts a daemon that would outlive the benchmark. Not under b's context,
// which is cancelled before a cleanup runs.
func stopMonitor(b *testing.B, box *benchBox, dir string) {
	b.Helper()
	b.Cleanup(func() {
		stop := exec.Command("/bin/sh", "-c", "git fsmonitor--daemon stop")
		stop.Dir, stop.Env = dir, box.env
		_ = stop.Run()
	})
}

// sessionBytes is the session arm's history: twenty megabytes, 26 files of a
// directory over 8 960 commits, a few hundred versions each — history enough
// for every `log -p` a session reads.
const sessionBytes = 20_000_000

// sessionRounds bounds a session that never crosses git's automatic gc: each
// of the shape's rounds lazily fetches at least one pack, so gc.autoPackLimit's
// fifty is crossed within about fifty rounds, and two hundred is four times
// that.
const sessionRounds = 200

// sessionAfter is how many rounds a detached gc's session runs past the round
// its gc rewrote the packs in, so the commands of the gc's whole life ran.
const sessionAfter = 5

// sessionCommands are a session's round, in order; the empty one is three
// changes of the round's file with their patches (sessionCommand).
var sessionCommands = []string{
	"git status",
	"git grep -I -c the -- . || true",
	"",
	"git log --oneline -20",
	"git commit -qam 'a round of edits'",
}

// sessionCommand is round r's command s over files, in a clone made at base.
// The history step reads round r's file's three changes, in the history the
// clone was given, before those the rounds before it read of that file —
// each pass over the files three changes further back, as an agent tracing a
// bug reads back through a file's history — so every round asks for versions
// the clone has never held: read again and again at its tip, a file's last
// changes are soon the session's own commits, which it holds, and the clone
// would stop fetching once it had read each file once.
func sessionCommand(r, s int, files []string, base string) string {
	if sessionCommands[s] != "" {
		return sessionCommands[s]
	}
	return "git log -p -3 --skip=" + strconv.Itoa(3*(r/len(files))) + " " + sandbox.ShellQuote(base) + " -- " +
		sandbox.ShellQuote(files[r%len(files)])
}

// boxSession is a session arm: a blobless coding clone, then ROUNDS of an
// agent's commands in it (sessionCommands) — status, grep, three of a file's
// changes with their patches, each pass over the files further back
// (sessionCommand), which is what makes a blobless clone fetch (each diff's
// missing blobs: a lazy fetch, and so a pack), the recent log, and four files
// of the tree changed and committed — until
// git's own automatic gc has rewritten the clone's packs (fewer than before a
// command) and, detached, sessionAfter rounds more; or sessionRounds rounds.
// A session an operation.
//
// Every command's CPU — its own and its waited children's, the lazy fetches
// among them — is the session's git. FOREGROUND, the gc runs inside the
// command that started it, which waits for it: its CPU is that command's
// above the median of the same command in every other round, and its share
// is of the session's git through the gc's round. DETACHED, as a box's git
// runs it, no process waits for the gc and its CPU is counted nowhere: that
// arm reports the commands that ran while the gc held its gc.pid, and any
// command that failed or named a lock.
//
// Reported: rounds, the rounds before the gc; packs, the most the clone held;
// gc-cpu-ms, session-cpu-ms and gc-cpu-share (foreground); beside and
// lock-failures (detached).
func boxSession(b *testing.B, box *benchBox, repo sandbox.RunRepo, foreground bool) {
	b.Helper()
	step, err := sandbox.CloneRecipe(repo, sandbox.CloneCoding)
	if err != nil {
		b.Fatal(err)
	}
	var (
		rounds, packsMost, beside, locks, sessions int
		gcCPU, total                               time.Duration
	)
	for b.Loop() {
		dir := b.TempDir()
		boxStep(b, dir, box.env, step)
		clone := filepath.Join(dir, repo.Repo)
		stopMonitor(b, box, clone)
		files := trackedFiles(b, clone)
		pid := filepath.Join(clone, ".git", "gc.pid")
		var cpu [][]time.Duration
		gcAt, gcStep := -1, -1
		var session time.Duration
		for r := 0; r < sessionRounds; r++ {
			if gcAt >= 0 && (foreground || r > gcAt+sessionAfter) {
				break
			}
			row := make([]time.Duration, len(sessionCommands))
			for s := range sessionCommands {
				command := sessionCommand(r, s, files, repo.Base)
				if s == len(sessionCommands)-1 {
					for k := range 4 {
						changeFile(b, filepath.Join(clone, filepath.FromSlash(files[(r*4+k)%len(files)])), r)
					}
				}
				before, running := packCount(b, clone), fileExists(pid)
				_, used, out, err := boxTimed(b, clone, box.env, command)
				if running || fileExists(pid) {
					beside++
				}
				if err != nil || strings.Contains(strings.ToLower(out), "lock") {
					if foreground {
						b.Fatalf("round %d: %q: %v\n%s", r, command, err, out)
					}
					locks++
					b.Logf("round %d: %q beside a detached gc: %v\n%s", r, command, err, out)
				}
				now := packCount(b, clone)
				packsMost = max(packsMost, before, now)
				row[s] = used
				if gcAt < 0 && now < before {
					gcAt, gcStep = r, s
				}
			}
			cpu = append(cpu, row)
			if gcAt < 0 || gcAt == r {
				for _, c := range row {
					session += c
				}
			}
		}
		sessions++
		total += session
		if gcAt < 0 {
			b.Logf("the session's git never ran its automatic gc in %d rounds (%d packs at most)", sessionRounds,
				packsMost)
			rounds += sessionRounds
			continue
		}
		rounds += gcAt
		if foreground {
			var others []time.Duration
			for r, row := range cpu {
				if r != gcAt {
					others = append(others, row[gcStep])
				}
			}
			if len(others) == 0 {
				b.Fatalf("the gc ran in the session's first round: no round to read its command's own cost from")
			}
			slices.Sort(others)
			gcCPU += max(cpu[gcAt][gcStep]-others[len(others)/2], 0)
			b.Logf("the gc ran in round %d's %q", gcAt, sessionCommand(gcAt, gcStep, files, repo.Base))
		}
	}
	n := float64(sessions)
	b.ReportMetric(float64(rounds)/n, "rounds")
	b.ReportMetric(float64(packsMost), "packs")
	if foreground {
		b.ReportMetric(float64(gcCPU)/n/float64(time.Millisecond), "gc-cpu-ms")
		b.ReportMetric(float64(total)/n/float64(time.Millisecond), "session-cpu-ms")
		b.ReportMetric(float64(gcCPU)/float64(total), "gc-cpu-share")
		return
	}
	b.ReportMetric(float64(beside)/n, "beside")
	b.ReportMetric(float64(locks)/n, "lock-failures")
}

// trackedFiles is every file of the clone at dir's tip, by path.
func trackedFiles(b *testing.B, dir string) []string {
	b.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		b.Fatalf("the clone at %s holds no file to work on (%v)", dir, err)
	}
	slices.Sort(files)
	return files
}

// packCount is how many packs the repository at dir holds.
func packCount(b *testing.B, dir string) int {
	b.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	if err != nil {
		b.Fatal(err)
	}
	return len(packs)
}

// BenchmarkBoxDissociate is BORROWING VERSUS COPYING: a box's checkout of
// the history and a build that reads every file it holds, an operation, made
// two ways on the node that holds the repository warm —
//
//   - borrow=none is the coding recipe over /box/git/, blobless, its
//     checkout's lazy fetch bringing the tip's files;
//   - borrow=reference-dissociate is `git clone --reference <the node's copy>
//     --dissociate` and a checkout of the run's branch: every object borrowed
//     from the copy and then copied into the box by the full repack
//     --dissociate makes, nothing fetched but the refs.
//
// — at size=100MB and, under CREWLET_BENCH_LARGE, 1GB, reporting beside the
// wall time sent-MB, the node's server-cpu-ms and box-cpu-ms, the box's own
// git (the repack's included). Nothing here is adopted: a coding box holds no
// copy of the node's to borrow (sandbox.CloneRecipe), and a check's box on a
// git node, which can be lent one, reads it in place for its run and never
// copies it (sandbox.CloneBorrowed).
func BenchmarkBoxDissociate(b *testing.B) {
	codetest.Quiet(b)
	for _, size := range boxSizes {
		b.Run("size="+size.name, func(b *testing.B) {
			if size.large {
				gittest.Large(b, "a gigabyte's history pushed in 64 sessions")
			}
			f, p := benchFleet(b, memobj.New(), 1)
			rows, tip := boxHistory(b, f, p, size.bytes)
			box := newBenchBox(b, f, rows, gittest.Require(b))
			repo := boxRepo(rows, tip, nil)
			coding, err := sandbox.CloneRecipe(repo, sandbox.CloneCoding)
			if err != nil {
				b.Fatal(err)
			}
			copyOf := incarnation(b, f.Nodes()[0], rows.Repo.ID)
			borrow := sandbox.SetupStep{Name: "borrow " + repo.Repo, Commands: []string{
				"git clone --quiet --no-checkout --reference " + sandbox.ShellQuote(copyOf) + " --dissociate -- " +
					`"$` + sandbox.GitURLVar + `"/` + sandbox.ShellQuote(repo.Repo+".git") + " " +
					sandbox.ShellQuote(repo.Repo) + " &&\n" +
					"git -C " + sandbox.ShellQuote(repo.Repo) + " checkout --quiet -B " +
					sandbox.ShellQuote(repo.Branch) + " " + sandbox.ShellQuote(repo.Base),
			}}
			for _, arm := range []struct {
				name string
				step sandbox.SetupStep
			}{{"borrow=none", coding}, {"borrow=reference-dissociate", borrow}} {
				b.Run(arm.name, func(b *testing.B) {
					boxBorrows(b, f, box, repo, arm.step)
				})
			}
		})
	}
}

// boxBorrows is one BenchmarkBoxDissociate arm: step into a fresh box, then
// the build, an operation.
func boxBorrows(b *testing.B, f *codetest.SoloFleet, box *benchBox, repo sandbox.RunRepo, step sandbox.SetupStep) {
	b.Helper()
	node := f.Nodes()[0]
	var (
		sent     int64
		cpu, own float64
	)
	for b.Loop() {
		b.StopTimer()
		// A fresh box an operation, removed as the operation ends, so an
		// arm holds one checkout at a time.
		dir := b.TempDir()
		box.proxy.take()
		before := serverCPU(node.Metrics())
		b.StartTimer()
		for _, command := range step.Commands {
			_, c := boxRun(b, dir, box.env, command)
			own += float64(c) / float64(time.Millisecond)
		}
		readCheckout(b, filepath.Join(dir, repo.Repo))
		b.StopTimer()
		cpu += serverCPU(node.Metrics()) - before
		for _, a := range box.proxy.take() {
			sent += a.bytes
		}
		if err := os.RemoveAll(dir); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	n := float64(b.N)
	b.ReportMetric(float64(sent)/n/1e6, "sent-MB")
	b.ReportMetric(cpu/n, "server-cpu-ms")
	b.ReportMetric(own/n, "box-cpu-ms")
}

// benchBox is a coding box on a fleet's git node: its environment, as the
// engine's launch makes it, and the proxy that records what the node
// answered it.
type benchBox struct {
	env   []string
	proxy *boxProxy
}

// newBenchBox launches a run of boxSeat given the repository rows names to
// write, and answers its box: git, the box's git, first on PATH; a HOME of
// its own with no configuration, as a template's box has; the engine's box
// git environment (sandbox.BoxGit) for the run, its base the proxy's
// /box/git; the commit hook's values; and overrides, pairs of key and value
// handed to git through the environment AFTER the engine's own entries, so
// for a key both set the arm's wins — what measures a box with a setting of
// the engine's taken back to git's own.
func newBenchBox(b *testing.B, f *codetest.SoloFleet, rows codetest.Rows, git string, overrides ...string) *benchBox {
	b.Helper()
	if len(overrides)%2 != 0 {
		b.Fatalf("configuration %q is not pairs of a key and a value", overrides)
	}
	proxy := proxyBox(b, f.Nodes()[0])
	run := f.Runs().Launch(boxSeat, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID, Name: codetest.RepoName,
		Access: code.AccessWrite, Branch: "refs/heads/" + boxBranch})
	trailers, err := sandbox.TrailerEnv(boxSeat, run.LaunchID,
		&types.WorkItem{Backend: types.WorkNative, ID: "task-7", Key: "ENG-7"})
	if err != nil {
		b.Fatal(err)
	}
	home := b.TempDir()
	global := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		b.Fatal(err)
	}
	// The variables a box's template sets of its own: none of git's
	// configuration, the host's attributes kept out as a box image holds
	// none, no terminal to prompt at, and a C locale for git's words.
	base := []string{"HOME=" + home, "GIT_CONFIG_GLOBAL=" + global, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	env := appendConfig(b, boxEnv(b, base, git, sandbox.BoxGit{Base: proxy.srv.URL + "/box/git", User: host.BoxUser,
		Token: f.Runs().Token(run), Name: "Alice", Email: "alice@seats.invalid"}, trailers), overrides...)
	version, err := boxTry(b, b.TempDir(), env, "git version")
	if err != nil {
		b.Fatalf("the box's git: %v\n%s", err, version)
	}
	b.Logf("the box runs %s", strings.TrimSpace(version))
	return &benchBox{env: env, proxy: proxy}
}

// appendConfig is env with pairs, keys and values in turn, appended to git's
// configuration through the environment after the entries env holds.
func appendConfig(b *testing.B, env []string, pairs ...string) []string {
	b.Helper()
	if len(pairs) == 0 {
		return env
	}
	out := make([]string, 0, len(env)+len(pairs)+1)
	n := 0
	for _, kv := range env {
		if count, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			var err error
			if n, err = strconv.Atoi(count); err != nil {
				b.Fatalf("the box's GIT_CONFIG_COUNT is %q", count)
			}
			continue
		}
		out = append(out, kv)
	}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, "GIT_CONFIG_KEY_"+strconv.Itoa(n)+"="+pairs[i],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(n)+"="+pairs[i+1])
		n++
	}
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(n))
}

// boxRepo is the run's repository as its launch resolved it: rows' at tip,
// on the run's branch, to write, narrowed to cone.
func boxRepo(rows codetest.Rows, tip string, cone []string) sandbox.RunRepo {
	return sandbox.RunRepo{Repo: codetest.RepoName, RepoID: rows.Repo.ID, Base: tip, Branch: boxBranch,
		Access: sandbox.RepoWrite, Paths: cone}
}

// boxRepoFor is the repository a box is given for purpose, in the shape
// sandbox.RunRepo.ValidateFor holds that purpose's to: a coding run's
// (boxRepo), or a check's — its one commit at tip, on no branch, to read —
// since a value a purpose does not read is refused rather than ignored.
func boxRepoFor(rows codetest.Rows, tip string, cone []string, purpose sandbox.ClonePurpose) sandbox.RunRepo {
	repo := boxRepo(rows, tip, cone)
	if purpose == sandbox.CloneSingleSHA {
		repo.Branch, repo.Access = "", sandbox.RepoRead
	}
	return repo
}

// boxHistory is a gittest.History of size bytes on f's git node, pushed by p a
// session at a time as a team's pushes would have built it, the node warm
// (warmBox): the repository's rows and its tip.
func boxHistory(b *testing.B, f *codetest.SoloFleet, p *codetest.Person, size int64) (codetest.Rows, string) {
	b.Helper()
	src, commits := gittest.History(b, p.Git(), size)
	pushSessions(b, p, src, repoURL(f.Nodes()[0]), commits)
	if err := os.RemoveAll(src); err != nil {
		b.Fatal(err)
	}
	return warmRows(b, f), commits[len(commits)-1]
}

// boxTree is a gittest.Tree of files files on f's git node, pushed by p a
// commit at a time, the node warm: the repository's rows and its tip.
func boxTree(b *testing.B, f *codetest.SoloFleet, p *codetest.Person, files int) (codetest.Rows, string) {
	b.Helper()
	src, commits := gittest.Tree(b, p.Git(), files)
	for _, c := range commits {
		mustPush(b, p, src, repoURL(f.Nodes()[0]), c+":refs/heads/main")
	}
	if err := os.RemoveAll(src); err != nil {
		b.Fatal(err)
	}
	return warmRows(b, f), commits[len(commits)-1]
}

// warmRows is the fleet's repository's rows once its git node holds it warm.
func warmRows(b *testing.B, f *codetest.SoloFleet) codetest.Rows {
	b.Helper()
	rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	warmBox(b, f.Nodes()[0], rows.Repo.ID)
	return rows
}

// warmBoxWait bounds warmBox: the maintainer's index of a gigabyte's history
// — its multi-pack-index, bitmap and commit-graph — on two cores is minutes,
// past warmIndex's five.
const warmBoxWait = 30 * time.Minute

// warmBox waits until node's copy of the repository is as a warm node serves
// it: synced, every pack in its maintainer's multi-pack-index, with the
// bitmap, and a commit-graph written — so no figure is of the minutes after a
// push, while the node still walks a clone's objects without either.
func warmBox(b *testing.B, node codetest.Node, repoID string) {
	b.Helper()
	s, err := node.Cache().Sync(b.Context(), repoID, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	s.Release()
	deadline := time.Now().Add(warmBoxWait)
	for {
		indexed, held := indexedPacks(b, node, repoID)
		dir := incarnation(b, node, repoID)
		bitmaps, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "multi-pack-index-*.bitmap"))
		graph := fileExists(filepath.Join(dir, "objects", "info", "commit-graph")) ||
			fileExists(filepath.Join(dir, "objects", "info", "commit-graphs", "commit-graph-chain"))
		if held > 0 && indexed == held && len(bitmaps) > 0 && graph {
			return
		}
		if time.Now().After(deadline) {
			b.Fatalf("node %s indexed %d of %d packs (%d bitmaps, commit-graph %t) in %v", node.ID(), indexed,
				held, len(bitmaps), graph, warmBoxWait)
		}
		time.Sleep(time.Second)
	}
}

// fileExists reports whether a file is at path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// boxStep runs step's commands as a box does — each through a shell in the
// box's working directory dir, under env — failing b on the first that exits
// non-zero.
func boxStep(b *testing.B, dir string, env []string, step sandbox.SetupStep) {
	b.Helper()
	for _, command := range step.Commands {
		boxRun(b, dir, env, command)
	}
}

// boxRun runs command as a box does — through a shell in dir under env — and
// answers its wall time and the CPU it and every process it waited for spent
// (the box's git and its lazy fetches; never a process git detached), failing
// b where it exits non-zero.
func boxRun(b *testing.B, dir string, env []string, command string) (wall, cpu time.Duration) {
	b.Helper()
	wall, cpu, out, err := boxTimed(b, dir, env, command)
	if err != nil {
		b.Fatalf("%q in the box: %v\n%s", command, err, out)
	}
	return wall, cpu
}

// boxTry is boxRun answering the command's output and its failure rather
// than failing b.
func boxTry(b *testing.B, dir string, env []string, command string) (string, error) {
	b.Helper()
	_, _, out, err := boxTimed(b, dir, env, command)
	return out, err
}

// boxTimed runs command through a shell in dir under env, and answers its
// wall time, its CPU and its waited children's, its output and its failure.
func boxTimed(b *testing.B, dir string, env []string, command string) (wall, cpu time.Duration, out string,
	err error) {
	b.Helper()
	cmd := exec.CommandContext(b.Context(), "/bin/sh", "-c", command)
	cmd.Dir, cmd.Env = dir, env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	at := time.Now()
	err = cmd.Run()
	wall = time.Since(at)
	if cmd.ProcessState != nil {
		cpu = cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	}
	return wall, cpu, buf.String(), err
}

// readCheckout is a BUILD of the checkout at dir: every file it holds read
// whole, outside .git — what a build that needs the whole tree reads.
func readCheckout(b *testing.B, dir string) {
	b.Helper()
	read := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if _, err := os.ReadFile(path); err != nil {
			return err
		}
		read++
		return nil
	})
	if err != nil || read == 0 {
		b.Fatalf("a build read %d files of the checkout at %s (%v)", read, dir, err)
	}
}

// countObjects is how many objects the box's repository at dir holds —
// packed and loose — as `git count-objects -v` counts them, which fetches
// nothing.
func countObjects(b *testing.B, c *gittest.Client, dir string) int64 {
	b.Helper()
	var n int64
	for line := range strings.Lines(c.Run(b, dir, "count-objects", "-v")) {
		name, value, _ := strings.Cut(strings.TrimSpace(line), ": ")
		if name == "count" || name == "in-pack" {
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				b.Fatalf("count-objects wrote %q", line)
			}
			n += v
		}
	}
	return n
}

// reportSpread reports took's median, p10 and p90 in milliseconds.
func reportSpread(b *testing.B, took []time.Duration) {
	b.Helper()
	slices.Sort(took)
	b.ReportMetric(quantile(took, 0.5), "median-ms")
	b.ReportMetric(quantile(took, 0.1), "p10-ms")
	b.ReportMetric(quantile(took, 0.9), "p90-ms")
}

// boxProxy is a reverse proxy in front of a git node that records, in the
// order they end, the git requests the node answered 200: whether each was a
// FETCH — protocol v2's command=fetch, what a clone, a fetch by id and a lazy
// fetch each send for objects, never ls-refs or an advertisement — and the
// bytes of its answer.
type boxProxy struct {
	srv *httptest.Server

	mu  sync.Mutex
	log []boxAnswer
}

// boxAnswer is one answer a boxProxy recorded.
type boxAnswer struct {
	fetch bool
	bytes int64
}

// proxyBox starts a boxProxy in front of node, until b ends.
func proxyBox(b *testing.B, node codetest.Node) *boxProxy {
	b.Helper()
	target, err := url.Parse(node.URL())
	if err != nil {
		b.Fatal(err)
	}
	p := &boxProxy{}
	proxy := &httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(target) },
		Transport: node.Client().Transport,
	}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetch := false
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if fetch, err = isFetch(raw, r.Header.Get("Content-Encoding")); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		cw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
		proxy.ServeHTTP(cw, r)
		if cw.status == http.StatusOK {
			p.mu.Lock()
			p.log = append(p.log, boxAnswer{fetch: fetch, bytes: cw.n})
			p.mu.Unlock()
		}
	}))
	b.Cleanup(p.srv.Close)
	return p
}

// take answers what the proxy recorded since the last take, and forgets it.
func (p *boxProxy) take() []boxAnswer {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.log
	p.log = nil
	return out
}

// isFetch reports whether an upload-pack request body, encoded as encoding
// names (git compresses a large one), is protocol v2's fetch command: its
// first pkt-line is command=fetch.
func isFetch(raw []byte, encoding string) (bool, error) {
	body := io.Reader(bytes.NewReader(raw))
	if encoding == "gzip" {
		z, err := gzip.NewReader(body)
		if err != nil {
			return false, err
		}
		body = z
	}
	head := make([]byte, 4+len("command=fetch"))
	if _, err := io.ReadFull(body, head); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return bytes.HasPrefix(head[4:], []byte("command=fetch")), nil
}

// countingWriter counts what a handler writes and the status it writes.
type countingWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *countingWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// Unwrap is the writer underneath, which the reverse proxy flushes a
// streamed answer through.
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
