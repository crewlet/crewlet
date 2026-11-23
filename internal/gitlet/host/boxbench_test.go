package host_test

import (
	"bufio"
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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A coding box's git, measured (ADR-0093): the clone recipes
// (BenchmarkBoxClone), the box's own git and which settings the engine's box
// git environment carries (BenchmarkBoxWorktree), and borrowing a node's copy
// (BenchmarkBoxDissociate). Every box is one as the engine makes it: a launched
// run's token, requests through /box/git/, commands run by `sh -c` under
// EXACTLY sandbox.BoxGit's environment over an empty HOME, never the gittest
// client's, whose gc.auto=0 and maintenance.auto=false would hide a box's gc.
// The node is warm before any timer (warmBox). Figures are over memobj; the
// package doc's measured section and docs/concepts/code-sandbox.md's "What a
// clone costs" carry them and the decisions taken from them.

// boxSeat is the seat every benchmark box's run is launched for, and
// boxBranch the branch its run works on.
const (
	boxSeat   = gitlettest.FixtureAlice
	boxBranch = "seats/" + gitlettest.FixtureAlice + "/ENG-7"
)

// boxSize is a gittest.History of bytes, pushed a session at a time; large
// runs only under CREWLET_BENCH_LARGE.
type boxSize struct {
	name  string
	bytes int64
	large bool
}

// boxSizes are the histories a box clones: a hundred mebibytes — 138 files in
// three directories over 44 800 commits — and the gigabyte BenchmarkPushSmall
// pushes, 1 421 files in thirty directories over 455 680 commits.
var boxSizes = []boxSize{{"100MB", 100 << 20, false}, {"1GB", 1 << 30, true}}

// boxCone is the cone an arm's checkout narrows to: none, or the one directory
// dir.
func boxCone(cone, dir string) []string {
	if cone == "one-dir" {
		return []string{dir}
	}
	return nil
}

// historyDir is a gittest.History's first directory, the one-dir cone.
const historyDir = "d000"

// BenchmarkBoxClone is what the two recipes cost: sandbox.CloneRecipe's step,
// timed to the checked-out tree, a fresh box an operation, on a warm node,
// over size (boxSizes), git=pin|client-floor (the node pin, or at 100MB
// 2.40.4, the oldest a box may run), recipe (sandbox.ClonePurpose,
// boxRepoFor), cone, and seats (boxSeatArms, at 100MB: other seats' branches
// beside main, with clone=every-branch the control).
//
// Reported: s/op; sent-MB, of which listed-KB carried no objects (the
// advertisements and ref listing); fetches, of which lazy-fetches (lazy-MB)
// came after the recipe's own; objects; server-cpu-ms. Those lazy fetches are
// all a build reading the whole checkout needs: the checkout fetched every blob.
func BenchmarkBoxClone(b *testing.B) {
	gitlettest.Quiet(b)
	for _, size := range boxSizes {
		b.Run("size="+size.name, func(b *testing.B) {
			if size.large {
				gittest.Large(b, "a gigabyte's history pushed in 64 sessions")
			}
			f, p := benchFleet(b, memobj.New(), 1)
			rows, commits, src := boxSource(b, f, p, size.bytes)
			tip := commits[len(commits)-1]
			if size.large {
				// Only the 100MB seats arms need the source, and a gigabyte's
				// would hold its disk through every arm.
				if err := os.RemoveAll(src); err != nil {
					b.Fatal(err)
				}
			}
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
						for _, cone := range []string{"none", "one-dir"} {
							b.Run(fmt.Sprintf("recipe=%s/cone=%s", purpose, cone), func(b *testing.B) {
								repo := boxRepoFor(rows, tip, boxCone(cone, historyDir), purpose)
								boxClones(b, f, p, box, repo, cloneStep(b, repo, purpose))
							})
						}
					}
				})
			}
			if !size.large {
				boxSeatArms(b, f, p, src, commits)
			}
		})
	}
}

// boxSeats are the seats arms' counts of other seats' branches: none;
// gitlet.MaxCommandsPerPush, a hundred seats of ten work items; and ten times
// that, a tenth of gitlet.MaxRefsPerRepo.
var boxSeats = []int{0, gitlet.MaxCommandsPerPush, 10 * gitlet.MaxCommandsPerPush}

// boxSeatCommits is a seat branch's commits off main, a work item's few; each
// is a new root tree, directory tree and blob, as a seat's commit is.
const boxSeatCommits = 3

// seatIdent is every seat commit's committer in the seats arms.
const seatIdent = "Seat Work <seat@seats.invalid>"

// boxSeatArms are BenchmarkBoxClone's seats arms over src's history (commits,
// oldest first): for each of boxSeats, the branches grown to it and the node
// synced, then clone=recipe and, beside any branch, the control
// clone=every-branch (everyBranch).
func boxSeatArms(b *testing.B, f *gitlettest.SoloFleet, p *gitlettest.Person, src string, commits []string) {
	b.Helper()
	made := 0
	for _, n := range boxSeats {
		b.Run(fmt.Sprintf("seats=%d", n), func(b *testing.B) {
			pushSeatBranches(b, f, p, src, commits, made, n)
			made = n
			rows := syncedRows(b, f)
			box := newBenchBox(b, f, rows, gittest.Require(b))
			repo := boxRepo(rows, commits[len(commits)-1], nil)
			recipe := cloneStep(b, repo, sandbox.CloneCoding)
			b.Run("clone=recipe", func(b *testing.B) { boxClones(b, f, p, box, repo, recipe) })
			if n > 0 {
				b.Run("clone=every-branch", func(b *testing.B) {
					boxClones(b, f, p, box, repo, everyBranch(b, recipe))
				})
			}
		})
	}
}

// pushSeatBranches makes seat branches [from, to) in src, each boxSeatCommits
// commits off the tip, and pushes them gitlet.MaxCommandsPerPush a push, so the
// node holds a pack a push as seats' pushes leave it until compaction. Each
// refspec names its tip by id, which git takes as is rather than matching it
// against every ref src holds.
func pushSeatBranches(b *testing.B, f *gitlettest.SoloFleet, p *gitlettest.Person, src string, commits []string,
	from, to int) {
	b.Helper()
	tip := commits[len(commits)-1]
	url := repoURL(f.Nodes()[0])
	for lo := from; lo < to; lo += gitlet.MaxCommandsPerPush {
		hi := min(lo+gitlet.MaxCommandsPerPush, to)
		gittest.FastImport(b, p.Git(), src, func(w *bufio.Writer) error {
			for i := lo; i < hi; i++ {
				for c := range boxSeatCommits {
					// Dated after every earlier commit, so no git walk reads a
					// skewed clock (gittest.After).
					date := strings.TrimPrefix(gittest.After(commits, i*boxSeatCommits+c), "@")
					msg := fmt.Sprintf("change %d of ENG-%d\n", c, i)
					fmt.Fprintf(w, "commit %s\ncommitter %s %s\ndata %d\n%s", seatBranch(i), seatIdent, date,
						len(msg), msg)
					if c == 0 {
						fmt.Fprintf(w, "from %s\n", tip)
					}
					body := fmt.Sprintf("work item ENG-%d, change %d\n", i, c)
					fmt.Fprintf(w, "M 100644 inline %s/seat-%d-%d.txt\ndata %d\n%s", historyDir, i, c, len(body),
						body)
					if err := w.WriteByte('\n'); err != nil {
						return err
					}
				}
			}
			return nil
		})
		tips := map[string]string{}
		for line := range strings.Lines(p.Git().Run(b, src, "for-each-ref", "--format=%(refname) %(objectname)",
			"refs/heads/seats/")) {
			ref, oid, _ := strings.Cut(strings.TrimSpace(line), " ")
			tips[ref] = oid
		}
		refspecs := make([]string, 0, hi-lo)
		for i := lo; i < hi; i++ {
			refspecs = append(refspecs, tips[seatBranch(i)]+":"+seatBranch(i))
		}
		mustPush(b, p, src, url, refspecs...)
	}
}

// syncedRows is the repository's rows once the node holds every pack they
// name, and no more: the seats' packs stand outside the history's index, as
// newest pushes do until gitlet/cache's MidxPacks gather. Not warmBox: a few
// packs never make the maintainer run, so its wait would run out warmBoxWait.
func syncedRows(b *testing.B, f *gitlettest.SoloFleet) gitlettest.Rows {
	b.Helper()
	rows, err := f.Rows(b.Context(), gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	s, err := f.Nodes()[0].Cache().Sync(b.Context(), rows.Repo.ID, gitlettest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	s.Release()
	return rows
}

// seatBranch is seat i/10's branch for work item ENG-i.
func seatBranch(i int) string { return fmt.Sprintf("refs/heads/seats/seat%04d/ENG-%d", i/10, i) }

// everyBranch is step's clone without --single-branch, wanting every branch
// it is shown: the seats arms' control.
func everyBranch(b *testing.B, step sandbox.SetupStep) sandbox.SetupStep {
	b.Helper()
	command := strings.Replace(step.Commands[0], " --single-branch ", " ", 1)
	if command == step.Commands[0] {
		b.Fatalf("the coding recipe's clone names no --single-branch to take back:\n%s", command)
	}
	step.Commands = []string{command}
	return step
}

// cloneStep is sandbox.CloneRecipe's step giving a box repo for purpose.
func cloneStep(b *testing.B, repo sandbox.RunRepo, purpose sandbox.ClonePurpose) sandbox.SetupStep {
	b.Helper()
	step, err := sandbox.CloneRecipe(repo, purpose)
	if err != nil {
		b.Fatal(err)
	}
	return step
}

// boxClones is one BenchmarkBoxClone arm: step in a fresh box an operation.
func boxClones(b *testing.B, f *gitlettest.SoloFleet, p *gitlettest.Person, box *benchBox, repo sandbox.RunRepo,
	step sandbox.SetupStep) {
	b.Helper()
	node := f.Nodes()[0]
	var (
		sent, listed, lazySent, objects int64
		fetches, lazy                   int
		cpu                             float64
	)
	for b.Loop() {
		b.StopTimer()
		// Removed as the operation ends, so an arm holds one checkout at a time.
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
				listed += a.bytes
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
	b.ReportMetric(float64(listed)/n/1e3, "listed-KB")
	b.ReportMetric(float64(fetches)/n, "fetches")
	b.ReportMetric(float64(lazy)/n, "lazy-fetches")
	b.ReportMetric(float64(lazySent)/n/1e6, "lazy-MB")
	b.ReportMetric(float64(objects)/n, "objects")
	b.ReportMetric(cpu/n, "server-cpu-ms")
}

// BenchmarkBoxWorktree is what a box's own git costs once its checkout is
// made, and so which large-tree settings sandbox.BoxGit carries: adopted on
// medians, the gain past the p10-p90 spread (code-sandbox.md).
//
//   - files=10000|100000|1000000 (the last LARGE): 4 KiB files, 48 a directory;
//   - index=default|v4+untracked: git's own, or BoxGit's entries 6 and 7
//     (feature.manyFiles but index.skipHash, which older libgit2 refuses);
//   - cone=none|one-dir; op=checkout fresh, op=status|commit warm in one checkout;
//   - session/history/maintenance=git|gc|gc-small|incremental and
//     gc=foreground|detached: an agent's rounds in a blobless clone (boxSession).
func BenchmarkBoxWorktree(b *testing.B) {
	gitlettest.Quiet(b)
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
				// git's own, unset: version 2 (3 where an entry needs it), and
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
					b.Run(arm+"/op=status", func(b *testing.B) {
						boxStatuses(b, box, checkout)
					})
					b.Run(arm+"/op=commit", func(b *testing.B) {
						boxCommits(b, box, checkout, files)
					})
				}
			}
		})
	}
	b.Run("session", func(b *testing.B) {
		for _, size := range sessionSizes {
			b.Run("history="+size.name, func(b *testing.B) {
				f, p := benchFleet(b, memobj.New(), 1)
				rows, tip := boxHistory(b, f, p, size.bytes)
				for _, arm := range []struct {
					name     string
					settings []string
					detached []bool
				}{
					// git's own: from 2.55.0 no automatic task repacks a
					// blobless clone's lazy-fetch packs, so no gc runs.
					{"maintenance=git", nil, []bool{false}},
					// gc --auto at gc.autoPackLimit's fifty packs, all repacked
					// into one.
					{"maintenance=gc", []string{"maintenance.gc.enabled", "true"}, size.detached},
					// The same, keeping the clone's own pack (sessionKeepPack).
					{"maintenance=gc-small", []string{"maintenance.gc.enabled", "true", "gc.bigPackThreshold",
						sessionKeepPack}, size.detached},
					// A multi-pack-index and small packs repacked in batches:
					// work that follows the packs, not the history.
					{"maintenance=incremental", []string{"maintenance.incremental-repack.enabled", "true"},
						size.detached},
				} {
					for _, detached := range arm.detached {
						name := arm.name
						overrides := slices.Clone(arm.settings)
						if len(arm.detached) > 1 {
							name += "/gc=foreground"
							if detached {
								name = arm.name + "/gc=detached"
							}
						}
						if !detached {
							// So the command that started a gc waits for it and
							// counts its CPU; detached, nothing does.
							overrides = append(overrides, "gc.autoDetach", "false", "maintenance.autoDetach", "false")
						}
						b.Run(name, func(b *testing.B) {
							box := newBenchBox(b, f, rows, gittest.Require(b), overrides...)
							boxSession(b, box, boxRepo(rows, tip, nil), !detached)
						})
					}
				}
			})
		}
	})
}

// sessionSizes are the session arms' histories: 20 MB (26 files over 8 960
// commits, enough for every `log -p` a session reads), gcs attached and
// detached; and 100 MiB, five times the commits and trees a gc repacks, which
// a gc's cost follows and a lazy fetch's does not, attached alone.
var sessionSizes = []struct {
	name     string
	bytes    int64
	detached []bool
}{
	{"20MB", 20_000_000, []bool{false, true}},
	{"100MB", 100 << 20, []bool{false}},
}

// boxCheckouts is an op=checkout arm: the coding recipe in a fresh box an
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
		// Removed as the operation ends, so an arm holds one checkout at a time.
		dir := b.TempDir()
		b.StartTimer()
		at := time.Now()
		boxStep(b, dir, box.env, step)
		took = append(took, time.Since(at)) //fixedwait:measure a benchmark reports this; no gate reads it
		b.StopTimer()
		if err := os.RemoveAll(dir); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	reportSpread(b, took)
}

// boxCheckout makes a coding checkout of repo once, untimed, and answers its
// directory.
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

// pastRacy waits until the clock has left the second checkout's index was
// last written in. git hashes a file whose mtime falls in its index's second
// (racily clean, at the pinned git's one-second grain), so right after a
// checkout a status hashes the whole tree, a transient a box has left by its
// agent's first status. The status after this wait rewrites the index past it.
func pastRacy(b *testing.B, checkout string) {
	b.Helper()
	info, err := os.Stat(filepath.Join(checkout, ".git", "index"))
	if err != nil {
		b.Fatal(err)
	}
	//fixedwait:bench
	time.Sleep(time.Until(info.ModTime().Truncate(time.Second).Add(time.Second + 10*time.Millisecond))) //fixedwait:measure the remaining time only sizes the wait past the index's second
}

// boxStatuses is an op=status arm: a warm `git status` an operation, after
// two untimed ones past the racy second (pastRacy), the first filling the
// index's caches.
func boxStatuses(b *testing.B, box *benchBox, checkout string) {
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
}

// boxCommits is an op=commit arm: one file changed, then `git commit -qam`
// timed, an operation. No hook runs: the recipe writes none, and the
// attribution hook is the executor runner's (internal/sandbox/executor).
func boxCommits(b *testing.B, box *benchBox, checkout string, files int) {
	b.Helper()
	pastRacy(b, checkout)
	boxRun(b, checkout, box.env, "git status")
	var took []time.Duration
	n := 0
	for b.Loop() {
		b.StopTimer()
		// A different file each time, from the first directory, which a
		// cone holds.
		path := filepath.Join(checkout, filepath.FromSlash(gittest.TreeFile(n%min(files, 48))))
		changeFile(b, path, n)
		n++
		b.StartTimer()
		wall, _ := boxRun(b, checkout, box.env, "git commit -qam 'one file changed'")
		took = append(took, wall)
	}
	reportSpread(b, took)
}

// changes counts changeFile's writes in this process, so no two write the same
// bytes and leave nothing to commit.
var changes atomic.Int64

// changeFile rewrites the first bytes of the file at path with n and the next
// of changes.
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

// sessionRounds is a session's length: a round lazily fetches about three
// packs, so 200 rounds is a dozen gcs at gc.autoPackLimit's fifty, or, with
// none, six hundred packs to read the commands' cost against.
const sessionRounds = 200

// sessionTrend is the rounds at each end of a session whose medians are
// compared, so the round a gc ran in moves neither.
const sessionTrend = 10

// sessionKeepPack is the maintenance=gc-small arm's gc.bigPackThreshold: a
// pack of 1 MiB or more is kept, so a gc repacks only the lazy fetches'
// packs and an earlier gc's, never the clone's own (every commit and tree,
// 179 MB at a gigabyte), which it would otherwise repack whole every fifty
// lazy fetches.
const sessionKeepPack = "1m"

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
// The history step reads three changes of round r's file from base, each pass
// over the files three further back, as an agent tracing a bug does, so every
// round wants versions the clone never held: read at its tip, a file's last
// changes are soon the session's own commits and the clone stops fetching.
func sessionCommand(r, s int, files []string, base string) string {
	if sessionCommands[s] != "" {
		return sessionCommands[s]
	}
	return "git log -p -3 --skip=" + strconv.Itoa(3*(r/len(files))) + " " + shellword.Quote(base) + " -- " +
		shellword.Quote(files[r%len(files)])
}

// boxSession is a session arm: a blobless coding clone, then sessionRounds of
// sessionCommands in it, an operation; the log -p step's missing blobs make
// each round's lazy fetches, so packs.
//
// Every command's CPU, waited children included, is the session's git. A gc
// is a pack count that fell. FOREGROUND, its CPU is its command's above that
// command's median in gc-free rounds; DETACHED, as a box runs it, nothing
// counts it, so the arm reports commands run while gc.pid was held (beside)
// and any that failed or named a lock (lock-failures). Also reported: gcs,
// packs (the most) and packs-end, <command>-first-ms and -last-ms (medians over
// sessionTrend rounds), session-cpu-ms, and foreground gc-cpu-ms/-share.
func boxSession(b *testing.B, box *benchBox, repo sandbox.RunRepo, foreground bool) {
	b.Helper()
	step, err := sandbox.CloneRecipe(repo, sandbox.CloneCoding)
	if err != nil {
		b.Fatal(err)
	}
	trend := map[int]string{0: "status", 2: "log-p", 4: "commit"}
	var (
		gcs, packsMost, packsEnd, beside, locks, sessions int
		gcCPU, total                                      time.Duration
		first, last                                       = map[int]time.Duration{}, map[int]time.Duration{}
	)
	for b.Loop() {
		dir := b.TempDir()
		boxStep(b, dir, box.env, step)
		clone := filepath.Join(dir, repo.Repo)
		files := trackedFiles(b, clone)
		pid := filepath.Join(clone, ".git", "gc.pid")
		cpu := make([][]time.Duration, sessionRounds)
		gcRan := make([][]bool, sessionRounds)
		packs := packCount(b, clone)
		for r := range sessionRounds {
			cpu[r] = make([]time.Duration, len(sessionCommands))
			gcRan[r] = make([]bool, len(sessionCommands))
			for s := range sessionCommands {
				command := sessionCommand(r, s, files, repo.Base)
				if s == len(sessionCommands)-1 {
					for k := range 4 {
						changeFile(b, filepath.Join(clone, filepath.FromSlash(files[(r*4+k)%len(files)])), r)
					}
				}
				before, running := packCount(b, clone), fileExists(pid)
				if before < packs {
					gcs++ // a detached gc's, between two commands
				}
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
				packs = packCount(b, clone)
				packsMost = max(packsMost, before, packs)
				cpu[r][s] = used
				total += used
				if packs < before {
					gcs++
					gcRan[r][s] = true
				}
			}
		}
		// A repack must keep the lazy fetches' objects marked a
		// promisor's, or an unfetched blob reads as missing, not fetchable.
		if out, err := boxTry(b, clone, box.env,
			"git rev-list --objects --all --missing=allow-promisor >/dev/null"); err != nil {
			b.Fatalf("after %d rounds the clone cannot account for the objects it lacks: %v\n%s", sessionRounds,
				err, out)
		}
		sessions++
		packsEnd += packs
		for s := range sessionCommands {
			var quiet []time.Duration
			for r := range sessionRounds {
				if !gcRan[r][s] {
					quiet = append(quiet, cpu[r][s])
				}
			}
			slices.Sort(quiet)
			for r := range sessionRounds {
				if gcRan[r][s] && foreground && len(quiet) > 0 {
					gcCPU += max(cpu[r][s]-quiet[len(quiet)/2], 0)
				}
			}
			if _, ok := trend[s]; ok {
				first[s] += medianOf(cpu[:sessionTrend], s, gcRan[:sessionTrend])
				last[s] += medianOf(cpu[sessionRounds-sessionTrend:], s, gcRan[sessionRounds-sessionTrend:])
			}
		}
	}
	n := float64(sessions)
	ms := func(d time.Duration) float64 { return float64(d) / n / float64(time.Millisecond) }
	b.ReportMetric(float64(gcs)/n, "gcs")
	b.ReportMetric(float64(packsMost), "packs")
	b.ReportMetric(float64(packsEnd)/n, "packs-end")
	for s, name := range trend {
		b.ReportMetric(ms(first[s]), name+"-first-ms")
		b.ReportMetric(ms(last[s]), name+"-last-ms")
	}
	b.ReportMetric(ms(total), "session-cpu-ms")
	if foreground {
		b.ReportMetric(ms(gcCPU), "gc-cpu-ms")
		b.ReportMetric(float64(gcCPU)/float64(total), "gc-cpu-share")
		return
	}
	b.ReportMetric(float64(beside)/n, "beside")
	b.ReportMetric(float64(locks)/n, "lock-failures")
}

// medianOf is command s's median CPU over rows, leaving out those whose s ran
// a gc (ran).
func medianOf(rows [][]time.Duration, s int, ran [][]bool) time.Duration {
	var took []time.Duration
	for r, row := range rows {
		if !ran[r][s] {
			took = append(took, row[s])
		}
	}
	if len(took) == 0 {
		return 0
	}
	slices.Sort(took)
	return took[len(took)/2]
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

// BenchmarkBoxDissociate is borrowing versus copying: a checkout and a build
// reading every file, an operation, on a warm node, per boxSizes, reporting
// sent-MB, server-cpu-ms and box-cpu-ms (the repack's included).
//
//   - borrow=none: the coding recipe, blobless, its checkout lazily fetched;
//   - borrow=reference-dissociate: `git clone --reference <the node's copy>
//     --dissociate`, every object copied in by its repack, only refs fetched.
//
// Nothing here is adopted: no box holds the node's copy to borrow
// (sandbox.CloneRecipe), a check's box fetching its one commit
// (sandbox.CloneSingleSHA); a borrow read in place is crewlet/crewlet#496's.
func BenchmarkBoxDissociate(b *testing.B) {
	gitlettest.Quiet(b)
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
				"git clone --quiet --no-checkout --reference " + shellword.Quote(copyOf) + " --dissociate -- " +
					`"$` + sandbox.GitURLVar + `"/` + shellword.Quote(repo.Repo+".git") + " " +
					shellword.Quote(repo.Repo) + " &&\n" +
					"git -C " + shellword.Quote(repo.Repo) + " checkout --quiet -B " +
					shellword.Quote(repo.Branch) + " " + shellword.Quote(repo.Base),
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
func boxBorrows(b *testing.B, f *gitlettest.SoloFleet, box *benchBox, repo sandbox.RunRepo, step sandbox.SetupStep) {
	b.Helper()
	node := f.Nodes()[0]
	var (
		sent     int64
		cpu, own float64
	)
	for b.Loop() {
		b.StopTimer()
		// Removed as the operation ends, so an arm holds one checkout at a time.
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

// benchBox is a coding box: its environment as the engine's launch makes it,
// and the proxy recording what the node answered it.
type benchBox struct {
	env   []string
	proxy *boxProxy
}

// newBenchBox launches a run of boxSeat writing rows' repository and answers
// its box: git first on PATH, an empty HOME as a template's box has,
// sandbox.BoxGit through the proxy's /box/git, and overrides, key and value
// pairs set AFTER the engine's entries so the arm's win.
func newBenchBox(b *testing.B, f *gitlettest.SoloFleet, rows gitlettest.Rows, git string, overrides ...string) *benchBox {
	b.Helper()
	if len(overrides)%2 != 0 {
		b.Fatalf("configuration %q is not pairs of a key and a value", overrides)
	}
	proxy := proxyBox(b, f.Nodes()[0])
	run := f.Runs().Launch(boxSeat, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID, Name: gitlettest.RepoName,
		Access: gitlet.AccessWrite, Branch: "refs/heads/" + boxBranch})
	home := b.TempDir()
	global := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		b.Fatal(err)
	}
	// A template's own: no git configuration, no host attributes, no
	// prompt, and a C locale for git's words.
	base := []string{"HOME=" + home, "GIT_CONFIG_GLOBAL=" + global, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	env := appendConfig(b, boxEnv(b, base, git, sandbox.BoxGit{Base: proxy.srv.URL + "/box/git", User: host.BoxUser,
		Token: f.Runs().Token(b, run), Name: "Alice", Email: "alice@seats.invalid"}), overrides...)
	version, err := boxTry(b, b.TempDir(), env, "git version")
	if err != nil {
		b.Fatalf("the box's git: %v\n%s", err, version)
	}
	b.Logf("the box runs %s", strings.TrimSpace(version))
	return &benchBox{env: env, proxy: proxy}
}

// appendConfig is env with pairs (key, value, …) appended to git's
// environment configuration after env's entries.
func appendConfig(b testing.TB, env []string, pairs ...string) []string {
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

// boxRepo is the run's repository as its launch resolved it: at tip, on
// boxBranch, to write, narrowed to cone.
func boxRepo(rows gitlettest.Rows, tip string, cone []string) sandbox.RunRepo {
	return sandbox.RunRepo{Repo: gitlettest.RepoName, RepoID: rows.Repo.ID, Base: tip, Branch: boxBranch,
		Access: sandbox.RepoWrite, Paths: cone}
}

// boxRepoFor is boxRepo in the shape sandbox.RunRepo.ValidateFor holds
// purpose to: a check's has no branch and reads, since a value a purpose does
// not read is refused rather than ignored.
func boxRepoFor(rows gitlettest.Rows, tip string, cone []string, purpose sandbox.ClonePurpose) sandbox.RunRepo {
	repo := boxRepo(rows, tip, cone)
	if purpose == sandbox.CloneSingleSHA {
		repo.Branch, repo.Access = "", sandbox.RepoRead
	}
	return repo
}

// boxHistory pushes a gittest.History of size bytes a session at a time, as a
// team would, and answers the warm repository's rows and its tip.
func boxHistory(b *testing.B, f *gitlettest.SoloFleet, p *gitlettest.Person, size int64) (gitlettest.Rows, string) {
	b.Helper()
	rows, commits, src := boxSource(b, f, p, size)
	if err := os.RemoveAll(src); err != nil {
		b.Fatal(err)
	}
	return rows, commits[len(commits)-1]
}

// boxSource is boxHistory also answering the source repository (in b's
// temporary directory) and every commit, oldest first, for a fixture building
// on it (pushSeatBranches).
func boxSource(b *testing.B, f *gitlettest.SoloFleet, p *gitlettest.Person, size int64) (gitlettest.Rows, []string,
	string) {
	b.Helper()
	src, commits := gittest.History(b, p.Git(), size)
	pushSessions(b, p, src, repoURL(f.Nodes()[0]), commits)
	return warmRows(b, f), commits, src
}

// boxTree pushes a gittest.Tree of files files a commit at a time and
// answers the warm repository's rows and its tip.
func boxTree(b *testing.B, f *gitlettest.SoloFleet, p *gitlettest.Person, files int) (gitlettest.Rows, string) {
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

// warmRows is the fleet's repository's rows once its Gitlet node holds it warm.
func warmRows(b *testing.B, f *gitlettest.SoloFleet) gitlettest.Rows {
	b.Helper()
	rows, err := f.Rows(b.Context(), gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	warmBox(b, f.Nodes()[0], rows.Repo.ID)
	return rows
}

// warmBoxWait bounds warmBox: indexing a gigabyte's history (multi-pack-index,
// bitmap, commit-graph) on two cores takes minutes, past warmIndex's five.
const warmBoxWait = 30 * time.Minute

// warmBox waits until node's copy is synced, every pack in the maintainer's
// multi-pack-index with its bitmap, and a commit-graph written, so no figure
// is of the minutes after a push when the node walks without either.
func warmBox(b *testing.B, node gitlettest.Node, repoID string) {
	b.Helper()
	s, err := node.Cache().Sync(b.Context(), repoID, gitlettest.Linearizable)
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the benchmark
			b.Fatalf("node %s indexed %d of %d packs (%d bitmaps, commit-graph %t) in %v", node.ID(), indexed,
				held, len(bitmaps), graph, warmBoxWait)
		}
		time.Sleep(time.Second) //fixedwait:liveness
	}
}

// fileExists reports whether a file is at path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// boxStep runs step's commands as a box does (boxRun), failing b on the first
// that exits non-zero.
func boxStep(b *testing.B, dir string, env []string, step sandbox.SetupStep) {
	b.Helper()
	for _, command := range step.Commands {
		boxRun(b, dir, env, command)
	}
}

// boxRun runs command as a box does, through a shell in dir under env, and
// answers its wall time and the CPU of it and its waited children (never a
// process git detached), failing b where it exits non-zero.
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

// boxTimed is boxRun's run, answering its output and failure rather than
// failing b.
func boxTimed(b *testing.B, dir string, env []string, command string) (wall, cpu time.Duration, out string,
	err error) {
	b.Helper()
	cmd := exec.CommandContext(b.Context(), "/bin/sh", "-c", command)
	cmd.Dir, cmd.Env = dir, env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	at := time.Now()
	err = cmd.Run()
	wall = time.Since(at) //fixedwait:measure a benchmark reports this; no gate reads it
	if cmd.ProcessState != nil {
		cpu = cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	}
	return wall, cpu, buf.String(), err
}

// readCheckout is a build needing the whole tree: every file outside .git
// read whole.
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

// countObjects is the packed and loose objects at dir, by `git count-objects
// -v`, which fetches nothing.
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

// boxProxy is a reverse proxy in front of a Gitlet node recording, in the
// order they end, the requests answered 200: each answer's bytes, and whether
// it was protocol v2's command=fetch, which alone sends objects.
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
func proxyBox(b *testing.B, node gitlettest.Node) *boxProxy {
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

// isFetch reports whether an upload-pack request body (gzip where encoding
// says, as git sends a large one) opens with the pkt-line command=fetch.
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

// Unwrap lets the reverse proxy flush a streamed answer.
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
