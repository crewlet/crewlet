package cache_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY INTERLEAVING OF AN EXPUNGE KEEPS WHAT A REF REACHES: an expunge is the
// one rewrite that DROPS objects, so it is held, over interleavings of what a
// node's cache meets, to a reference model of reachability — every object a
// ref reaches stays where every copy and every fresh copy finds it, and every
// commit no ref reached when an expunge's rescue read the refs, pushed before
// its begin, leaves the repository at its commit.
//
// Each expunge is the PRODUCTION EXPUNGER's (internal/code/compact) — its own
// scan, build, begin, rescue and commit — over this node's cache, the world's
// store and a committer (expungeCommitter) that decides its records on the
// world's log to the code domain's rules, so what is interleaved is what a
// maintainer runs. Beside the operations a node meets every day — pushes,
// force-pushes, deletions, tags, refused pushes, adoptions, syncs at three
// levels and killed ones, restarts, evictions, rebuilds, the maintainer's
// pass, a merge compaction and the sweep of what it retired past its grace —
// each seed draws a person's PURGE request; an EXPUNGE cycle, which finishes
// one left open from the rows; a cycle with a REVIVE between its scan and its
// begin — a branch created at a commit the scan found unreachable, as an
// engine write creating a branch at an old commit does, which the rescue must
// keep; a cycle over a copy whose COMMIT-GRAPH LISTS what it drops — a branch
// the maintainer graphed, then deleted; and a cycle CRASHED at a point of
// codefault.ExpungePath, which a later cycle resumes from the rows.
//
// After every expunge and at every check, each repository's copy, synced
// linearizable, passes `git fsck --connectivity-only --no-dangling` with its
// dropping view named, as a serving process names it (I1: no ref reaches an
// object the copy lacks); every check holds each copy to a fresh cache's at
// the same position — its image live and dropping packs where their rows
// place them, and nothing else — as the cache's own interleavings do; and
// after every commit no commit the model says dropped is one the copy's
// commit-graph lists or `git rev-list -1` walks (I2: over a graph still
// listing it, git answers it as though it were there).
//
// Mutations: give a push's scratch the dropping view as an alternate, and a
// push building on a dropping commit lands and its ref reaches what the
// commit retires (I1); keep the commit-graph on EventPackExpunged, and the
// copy's graph still lists the commit the commit dropped (I2); skip the
// rescue, and the revived branch names a commit no pack holds (I1).
func TestEveryInterleavingOfAnExpungeKeepsWhatARefReaches(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	ran, reached := 0, map[string]bool{}
	t.Cleanup(func() {
		if ran != expungeSeeds || t.Failed() {
			return
		}
		for _, o := range expungeOutcomes {
			if !reached[o] {
				t.Errorf("no seed reached %q: %d seeds of %d steps do not exercise it — draw more of them rather "+
					"than drop the operation", o, expungeSeeds, expungeSteps)
			}
		}
	})
	for seed := uint64(1); seed <= expungeSeeds; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			got := interleaveExpunge(t, seed)
			mu.Lock()
			defer mu.Unlock()
			ran++
			for o := range got {
				reached[o] = true
			}
		})
	}
}

// SIXTEEN SEEDS OF FORTY-EIGHT STEPS, a check every eight: an expunge cycle
// runs three git builds and a rescue's, so a step costs more than the cache's
// own interleavings' and a seed is fewer of them; sixteen reach every outcome
// of expungeOutcomes between them.
const (
	expungeSeeds      = 16
	expungeSteps      = 48
	expungeCheckEvery = 8
)

// expungeOps are the operations an expunge's interleaving draws, by weight.
var expungeOps = []struct {
	name   string
	weight int
	run    func(x *expungeRun, repo string) string
}{
	{"push", 20, (*expungeRun).push},
	{"force-push", 6, (*expungeRun).forcePush},
	{"delete a branch", 6, (*expungeRun).deleteBranch},
	{"tag", 3, (*expungeRun).tag},
	{"adopt", 4, (*expungeRun).adopt},
	{"refused push", 2, func(x *expungeRun, repo string) string { return x.in.refusedPush(repo) }},
	{"push onto a dropping commit", 4, (*expungeRun).pushOntoDropping},
	{"sync", 8, func(x *expungeRun, repo string) string { return x.in.sync(repo) }},
	{"killed sync", 3, func(x *expungeRun, repo string) string { return x.in.killedSync(repo) }},
	{"restart", 3, (*expungeRun).restart},
	{"evict", 2, func(x *expungeRun, repo string) string { return x.in.evict(repo) }},
	{"rebuild", 3, func(x *expungeRun, repo string) string { return x.in.rebuild(repo) }},
	{"maintain", 3, func(x *expungeRun, repo string) string { return x.in.maintain(repo) }},
	{"compact", 5, func(x *expungeRun, repo string) string { return x.in.compact(repo) }},
	{"sweep retirements", 2, func(x *expungeRun, repo string) string { return x.in.sweepRetirements(repo) }},
	{"purge", 7, (*expungeRun).purge},
	{"expunge", 10, (*expungeRun).expunge},
	{"expunge with a revive", 5, (*expungeRun).expungeWithRevive},
	{"expunge what the graph lists", 4, (*expungeRun).expungeGraphed},
	{"expunge crashed", 7, (*expungeRun).expungeCrashed},
}

// expungeOutcomes is every outcome the seeds must reach between them: each
// expunge operation having done what it is drawn to do.
var expungeOutcomes = []string{
	"purge", "expunge applied", "expunge resumed from its rows", "a revive rescued", "a commit dropped history",
	"a commit dropped a commit its copy's graph listed",
	"expunge crashed at " + string(codefault.ExpungeAfterBuild),
	"expunge crashed at " + string(codefault.ExpungeAfterBegin),
	"expunge crashed at " + string(codefault.ExpungeAfterRescueUpload),
	"expunge crashed at " + string(codefault.ExpungeAfterCommit),
	"a push onto a dropping commit refused", "force-push", "delete a branch", "compact", "restart",
}

// errExpungeCrashed is what a crashed cycle's fault point answers.
var errExpungeCrashed = errors.New("the maintainer crashed here")

// expungeRun is one seed's run: the cache's interleaving it draws operations
// beside, and what the expunge's committer and the model keep of each
// repository.
type expungeRun struct {
	in *interleaving
	// mu guards repos, which the committer moves on the expunger's
	// goroutine, and seq.
	mu    sync.Mutex
	repos map[string]*expungeRows
	// seq orders what the model must order finer than a step: a commit
	// pushed, and a begin applied — a step may push a commit and begin an
	// expunge whose inputs hold it.
	seq int
	// branches counts the branches the run created, which names each.
	branches int
}

// expungeRows is what the expunge's committer and the model keep of one
// repository: the expunge open — its id, when its begin applied (seq), the
// position its scan read and every commit a ref reached at its scan — the
// purge requested and its record's position; every drop a ref's force-move
// or deletion left; every commit the run pushed, and when (seq); every commit
// a commit dropped; and the commit a revive is creating a branch at, which no
// ref reached at the scan whatever reaches it by the begin.
type expungeRows struct {
	open      string
	begunAt   int
	beginSeen statelog.Position
	scanReach map[string]bool
	revived   string
	request   string
	requested statelog.Position
	drops     []expungeDrop
	pushed    []expungeCommit
	purged    map[string]bool
	// committed is set by a commit the committer applied, until the step
	// that drove it reads it, with when the begin it ended applied, what
	// that begin's scan reached, and every commit the copy's commit-graph
	// listed as the commit was proposed.
	committed      bool
	committedBegun int
	committedScan  map[string]bool
	committedGraph map[string]bool
}

// expungeDrop is one ref drop: the instant its record landed and the object
// the ref left.
type expungeDrop struct {
	at  time.Time
	old string
}

// expungeCommit is one commit the run pushed, and when (seq).
type expungeCommit struct {
	oid string
	seq int
}

// interleaveExpunge runs one seed and answers the outcomes its operations
// reached.
func interleaveExpunge(t *testing.T, seed uint64) map[string]bool {
	in := startInterleaving(t, seed)
	x := &expungeRun{in: in, repos: map[string]*expungeRows{}}
	for _, repo := range in.repos {
		x.repos[repo] = &expungeRows{purged: map[string]bool{},
			pushed: []expungeCommit{{oid: in.w.repos[repo].head}}}
	}
	total := 0
	for _, o := range expungeOps {
		total += o.weight
	}
	for step := 1; step <= expungeSteps; step++ {
		draw := in.rng.IntN(total)
		i := 0
		for draw >= expungeOps[i].weight {
			draw -= expungeOps[i].weight
			i++
		}
		o := expungeOps[i]
		repo := in.repos[in.rng.IntN(len(in.repos))]
		in.trace = append(in.trace, fmt.Sprintf("%d: %s, repo-%d", step, o.name, slices.Index(in.repos, repo)+1))
		in.trace[len(in.trace)-1] += ": " + o.run(x, repo)
		in.audit()
		if step%expungeCheckEvery == 0 || step == expungeSteps {
			in.check(step)
			for _, r := range in.repos {
				x.fsck(r)
			}
		}
	}
	return in.exercised
}

// rows is repo's expunge rows.
func (x *expungeRun) rows(repo string) *expungeRows {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.repos[repo]
}

// recordPushed records the work tree's head as a commit the run pushed now.
func (x *expungeRun) recordPushed(repo string) {
	r := x.rows(repo)
	x.mu.Lock()
	defer x.mu.Unlock()
	x.seq++
	r.pushed = append(r.pushed, expungeCommit{oid: x.in.w.repos[repo].head, seq: x.seq})
}

// isOpen reports whether repo has an expunge open.
func (x *expungeRun) isOpen(repo string) bool {
	r := x.rows(repo)
	x.mu.Lock()
	defer x.mu.Unlock()
	return r.open != ""
}

// unreachable is every commit the run pushed to repo that no live ref reaches
// and no commit dropped — those the open expunge's begin left dropping alone,
// where dropping is set: pushed before it, and reached by no ref at its scan.
func (x *expungeRun) unreachable(repo string, dropping bool) []string {
	reach := x.reachable(repo)
	r := x.rows(repo)
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
	for _, c := range r.pushed {
		if !reach[c.oid] && !r.purged[c.oid] && (!dropping || (c.seq < r.begunAt && !r.scanReach[c.oid])) {
			out = append(out, c.oid)
		}
	}
	return out
}

// onHeld puts repo's work tree back on a commit the repository holds where an
// expunge took its head out — main's — so the next commit made there builds
// on a parent its pack may leave out: a client's push sends every object the
// remote's refs do not reach, and the world's packs leave out every object an
// earlier push brought (syncWorld's tips), which holds only while the
// repository still has them.
func (x *expungeRun) onHeld(repo string) {
	f := x.in.w.repos[repo]
	r := x.rows(repo)
	x.mu.Lock()
	gone := r.purged[f.head]
	x.mu.Unlock()
	if !gone {
		return
	}
	main := x.in.w.log.refTarget(repo, "refs/heads/main")
	x.in.w.client.Run(x.in.t, f.work, "reset", "-q", "--hard", main)
	f.head = main
}

// restart is the cache's interleaving's crash and boot, which the expunger
// meets as a copy read again from its state and its rows.
func (x *expungeRun) restart(repo string) string {
	out := x.in.restart(repo)
	x.in.saw("restart")
	return out
}

func (x *expungeRun) push(repo string) string {
	x.onHeld(repo)
	ref := x.in.push(repo)
	x.recordPushed(repo)
	return ref
}

func (x *expungeRun) adopt(repo string) string {
	x.onHeld(repo)
	out := x.in.adopt(repo)
	x.recordPushed(repo)
	return out
}

func (x *expungeRun) tag(repo string) string {
	x.onHeld(repo)
	return x.in.tag(repo)
}

// forcePush moves a branch to a new commit made on an older one main
// reaches, by force: what the branch named before is a drop.
func (x *expungeRun) forcePush(repo string) string {
	t := x.in.t
	ref := x.in.branch()
	old := x.in.w.log.refTarget(repo, ref)
	if old == "" {
		return x.push(repo)
	}
	reach := x.reachable(repo)
	var older []string
	for _, c := range x.rows(repo).pushed {
		if reach[c.oid] && c.oid != old {
			older = append(older, c.oid)
		}
	}
	if len(older) == 0 {
		return x.push(repo)
	}
	base := older[x.in.rng.IntN(len(older))]
	f := x.in.w.repos[repo]
	x.in.w.client.Run(t, f.work, "reset", "-q", "--hard", base)
	f.head = base
	oid, row := x.in.w.commit(t, repo, nil, "")
	row.Added = x.in.w.log.writePushes(repo, fakePush{packs: []code.PackRow{row}, refs: map[string]string{ref: oid},
		forced: map[string]bool{ref: true}, introduced: []string{oid}})
	x.in.last[repo] = row.Added
	x.recordPushed(repo)
	x.dropped(repo, old)
	x.in.saw("force-push")
	return fmt.Sprintf("%s from %.7s to %.7s on %.7s", ref, old, oid, base)
}

// deleteBranch deletes a branch but main, which the run keeps so a ref
// always reaches a commit the repository holds: what it named is a drop.
func (x *expungeRun) deleteBranch(repo string) string {
	var branches []string
	for _, ref := range x.in.w.log.liveRefs(repo) {
		if strings.HasPrefix(ref, "refs/heads/") && ref != "refs/heads/main" {
			branches = append(branches, ref)
		}
	}
	if len(branches) == 0 {
		return "no branch but main"
	}
	ref := branches[x.in.rng.IntN(len(branches))]
	old := x.in.w.log.refTarget(repo, ref)
	x.in.last[repo] = x.in.w.log.write(repo, nil, map[string]string{ref: ""})
	x.dropped(repo, old)
	x.in.saw("delete a branch")
	return ref
}

// dropped records a drop of old at the run's clock now.
func (x *expungeRun) dropped(repo, old string) {
	r := x.rows(repo)
	x.mu.Lock()
	defer x.mu.Unlock()
	r.drops = append(r.drops, expungeDrop{at: x.in.clk.Now(), old: old})
}

// pushOntoDropping is a push received while an expunge is open, building on a
// commit its begin left dropping and carrying none of that commit's objects:
// its receive's connectivity check, run in a push's scratch over the copy as
// a receiving node runs it, must find the parent missing — a push's scratch
// leans on the copy's objects/ alone, never its dropping view — and the push
// lands nothing. That is what makes a rescue valid at any position after the
// begin, so a receive that PASSED lands its push where nothing after it is
// rescued — between the rescue's sync and the commit, the cycle held there —
// and the commit then retires the parent its new branch reaches. An expunge
// is left open first where none is; a repository with no commit dropping is
// pushed to as ever.
func (x *expungeRun) pushOntoDropping(repo string) string {
	t := x.in.t
	var did []string
	if !x.isOpen(repo) {
		did = append(did, x.openAnExpunge(repo))
	}
	dropping := x.unreachable(repo, true)
	if len(dropping) == 0 {
		return strings.Join(append(did, "no commit dropping: "+x.push(repo)), "; ")
	}
	parent := dropping[x.in.rng.IntN(len(dropping))]
	f := x.in.w.repos[repo]
	tips, head := slices.Clone(f.tips), f.head
	x.in.w.client.Run(t, f.work, "reset", "-q", "--hard", parent)
	f.head = parent
	s := x.in.syncAt(repo, linearizable)
	defer s.Release()
	sc, err := x.in.c.NewScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		x.in.fatalf("NewScratch: %v", err)
	}
	defer sc.Remove()
	tip, cs := x.in.w.packInto(t, repo, sc.Dir, nil)
	// THE RECEIVE'S CONNECTIVITY CHECK, as receive-pack makes it: every
	// object the new tip reaches, short of what the scratch's alternate's
	// refs reach, must be in the scratch or its alternate. Run by the case's
	// own git, so a scratch whose alternates file a mutation widened is read
	// as git reads it.
	_, stderr, err := x.in.w.client.Try(t, "", nil, "--git-dir="+sc.Dir, "rev-list", "--objects", tip, "--not",
		"--alternate-refs")
	f.tips, f.head = tips, head
	x.in.w.client.Run(t, f.work, "reset", "-q", "--hard", head)
	if err != nil {
		x.in.saw("a push onto a dropping commit refused")
		return strings.Join(append(did, fmt.Sprintf("onto dropping %.7s: refused (%s)", parent, firstLine(stderr))),
			"; ")
	}
	// A RECEIVE THAT PASSED lands the push, as its committer would — a branch
	// of its own at the tip, its pack stored — after the rescue read the refs
	// and before the commit retires what the rescue did not keep.
	row := x.in.w.storeBundle(t, x.in.w.bundle(cs), "")
	x.branches++
	ref := fmt.Sprintf("refs/heads/onto-%d", x.branches)
	cand, _ := x.candidate(repo)
	script := &codefault.Script{}
	held, reached := make(chan struct{}), make(chan struct{})
	script.Arm(codefault.ExpungeAfterRescueUpload, codefault.Action{Once: true, Block: held, Reached: reached})
	land := func(ended <-chan struct{}) {
		select {
		case <-reached:
		case <-ended:
			return
		}
		row.Added = x.in.w.log.writePushes(repo, fakePush{packs: []code.PackRow{row},
			refs: map[string]string{ref: tip}, introduced: []string{tip}})
		close(held)
	}
	out, cerr := x.cycle(cand, script, land)
	if !row.Added.IsZero() {
		x.in.last[repo] = row.Added
	}
	return strings.Join(append(did, fmt.Sprintf("onto dropping %.7s: landed %s before the commit: %s", parent, ref,
		x.answered(repo, cand, out, cerr))), "; ")
}

// openAnExpunge leaves an expunge open on repo — a cycle serving its purge
// stopped at its begin — with a commit dropping: a branch pushed and deleted
// first where every commit the run pushed is reachable or dropped.
func (x *expungeRun) openAnExpunge(repo string) string {
	if len(x.unreachable(repo, false)) == 0 {
		x.onHeld(repo)
		x.branches++
		ref := fmt.Sprintf("refs/heads/gone-%d", x.branches)
		oid, _ := x.in.w.push(x.in.t, repo, ref, nil)
		x.recordPushed(repo)
		x.in.last[repo] = x.in.w.log.write(repo, nil, map[string]string{ref: ""})
		x.dropped(repo, oid)
	}
	x.owe(repo)
	cand, _ := x.candidate(repo)
	script := &codefault.Script{}
	script.Arm(codefault.ExpungeAfterBegin, codefault.Action{Once: true, Err: errExpungeCrashed})
	if out, err := x.cycle(cand, script, nil); !errors.Is(err, errExpungeCrashed) {
		x.in.fatalf("a cycle stopped at its begin: %s, %v", out, err)
	}
	return "an expunge left open at its begin"
}

// firstLine is s's first line.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// purge is a person's purge request of repo, recorded at a record of its own:
// a request asked while one is open widens it to this one.
func (x *expungeRun) purge(repo string) string {
	at := x.in.w.log.mark()
	r := x.rows(repo)
	x.mu.Lock()
	r.request, r.requested = statelog.NewOpID(time.Now(), "code_purge_unreachable"), at
	x.mu.Unlock()
	x.in.last[repo] = at
	x.in.saw("purge")
	return "requested at " + at.String()
}

// candidate is repo as the expunge listing names it, and whether it does.
func (x *expungeRun) candidate(repo string) (code.ExpungeCandidate, bool) {
	a, err := expungeCommitter{x}.Read(x.in.t.Context(), code.ReadQuery{Kind: code.ReadExpungeDue})
	if err != nil {
		x.in.fatalf("the expunge listing: %v", err)
	}
	for _, cand := range a.Expunge {
		if cand.ID == repo {
			return cand, true
		}
	}
	return code.ExpungeCandidate{}, false
}

// owe makes sure repo owes an expunge — an open one, or a purge requested.
func (x *expungeRun) owe(repo string) {
	if _, ok := x.candidate(repo); !ok {
		x.purge(repo)
	}
}

// expunge is one cycle of the expunge repo owes, if any: an open one
// finished from its rows, or the purge requested served.
func (x *expungeRun) expunge(repo string) string {
	cand, ok := x.candidate(repo)
	if !ok {
		return "nothing owed"
	}
	out, err := x.cycle(cand, nil, nil)
	return x.answered(repo, cand, out, err)
}

// expungeWithRevive is a cycle serving a purge with a branch created between
// its scan and its begin at a commit the scan found unreachable — one no ref
// reaches, pushed before and not yet dropped — which the rescue must keep. An
// expunge open is finished first, so the cycle has a scan to come between.
func (x *expungeRun) expungeWithRevive(repo string) string {
	var done []string
	if cand, ok := x.candidate(repo); ok && cand.Open != "" {
		out, err := x.cycle(cand, nil, nil)
		done = append(done, x.answered(repo, cand, out, err))
	}
	x.owe(repo)
	cand, ok := x.candidate(repo)
	if !ok || cand.Open != "" {
		return strings.Join(append(done, "no purge to revive under"), "; ")
	}
	victims := x.unreachable(repo, false)
	if len(victims) == 0 {
		out, err := x.cycle(cand, nil, nil)
		return strings.Join(append(done, "no commit to revive", x.answered(repo, cand, out, err)), "; ")
	}
	victim := victims[x.in.rng.IntN(len(victims))]
	x.branches++
	ref := fmt.Sprintf("refs/heads/revived-%d", x.branches)
	script := &codefault.Script{}
	held, reached := make(chan struct{}), make(chan struct{})
	script.Arm(codefault.ExpungeAfterBuild, codefault.Action{Once: true, Block: held, Reached: reached})
	revived := false
	revive := func(ended <-chan struct{}) {
		select {
		case <-reached:
		case <-ended:
			return
		}
		r := x.rows(repo)
		x.mu.Lock()
		r.revived = victim
		x.mu.Unlock()
		x.in.last[repo] = x.in.w.log.writePushes(repo, fakePush{refs: map[string]string{ref: victim}})
		revived = true
		close(held)
	}
	out, err := x.cycle(cand, script, revive)
	answer := x.answered(repo, cand, out, err)
	if !revived {
		return strings.Join(append(done, fmt.Sprintf("%s at %.7s: the cycle built nothing: %s", ref, victim,
			answer)), "; ")
	}
	if out == compact.OutcomeApplied {
		x.in.saw("a revive rescued")
	}
	return strings.Join(append(done, fmt.Sprintf("%s at %.7s: %s", ref, victim, answer)), "; ")
}

// expungeGraphed is a cycle over a copy whose commit-graph lists what it
// drops, as a maintained node's copy does: a branch pushed and the maintainer
// run until its graph lists the branch's commit — a graph-first run where the
// copy holds no graph, and where it holds one the index's run, due again once
// interleavingMidxPacks packs gathered beside the index, which writes the
// graph after it — then the branch deleted and the purge served. The commit
// takes the graph with what it drops (X04's one drop), or git walks the
// dropped commit from the graph alone (afterCommit). An expunge open is
// finished first, so the cycle's own scan reads the deletion.
func (x *expungeRun) expungeGraphed(repo string) string {
	var done []string
	if cand, ok := x.candidate(repo); ok && cand.Open != "" {
		out, err := x.cycle(cand, nil, nil)
		done = append(done, x.answered(repo, cand, out, err))
	}
	x.onHeld(repo)
	x.branches++
	ref := fmt.Sprintf("refs/heads/graphed-%d", x.branches)
	var oid string
	for range interleavingMidxPacks + 1 {
		var row code.PackRow
		oid, row = x.in.w.push(x.in.t, repo, ref, nil)
		x.in.last[repo] = row.Added
		x.recordPushed(repo)
		x.in.syncAt(repo, linearizable).Release()
		x.in.maintain(repo)
		if x.graphed(repo)[oid] {
			break
		}
	}
	if !x.graphed(repo)[oid] {
		x.in.fatalf("repo-%d's commit-graph lists no %s after %d pushes beside its index, each maintained",
			slices.Index(x.in.repos, repo)+1, oid, interleavingMidxPacks+1)
	}
	x.in.last[repo] = x.in.w.log.write(repo, nil, map[string]string{ref: ""})
	x.dropped(repo, oid)
	x.owe(repo)
	cand, _ := x.candidate(repo)
	out, err := x.cycle(cand, nil, nil)
	return strings.Join(append(done, fmt.Sprintf("%s at %.7s graphed, then deleted: %s", ref, oid,
		x.answered(repo, cand, out, err))), "; ")
}

// expungeCrashed is a cycle that stops at a point of codefault.ExpungePath —
// one a fresh cycle reaches, or a resumed one where an expunge is open — as
// a maintainer killed there stops; a later cycle resumes from the rows.
func (x *expungeRun) expungeCrashed(repo string) string {
	x.owe(repo)
	cand, _ := x.candidate(repo)
	points := codefault.ExpungePath()
	if cand.Open != "" {
		// A resumed cycle starts at the rescue: it reaches neither a build
		// nor a begin of its own.
		points = []codefault.Point{codefault.ExpungeAfterRescueUpload, codefault.ExpungeAfterCommit}
	}
	p := points[x.in.rng.IntN(len(points))]
	script := &codefault.Script{}
	script.Arm(p, codefault.Action{Once: true, Err: errExpungeCrashed})
	out, err := x.cycle(cand, script, nil)
	if errors.Is(err, errExpungeCrashed) {
		x.in.saw("expunge crashed at " + string(p))
		x.afterCommit(repo)
		return fmt.Sprintf("crashed at %s", p)
	}
	return fmt.Sprintf("at %s: %s", p, x.answered(repo, cand, out, err))
}

// cycle is one cycle of cand by the production expunger over this node's
// cache — a compactor of its own, never started — with faults armed, and
// during run beside it until the cycle has ended (the channel it is handed
// closes then); a cycle the node's maintenance lane holds busy is asked
// again. The drops before it are before its scan: the clock moves a second
// first.
func (x *expungeRun) cycle(cand code.ExpungeCandidate, faults *codefault.Script,
	during func(ended <-chan struct{})) (compact.Outcome, error) {

	x.in.clk.Add(time.Second)
	o := compact.Options{Cache: x.in.c, Objects: x.in.w.store, Estate: expungeCommitter{x}, Maintainers: maintainsAll{},
		NodeID: "git-1", Limits: code.DefaultLimits, Logger: x.in.w.logs.logger(), Now: x.in.clk.Now}
	if faults != nil {
		o.Faults = faults
	}
	cp, err := compact.New(o)
	if err != nil {
		x.in.fatalf("a compactor over the interleaving's cache: %v", err)
	}
	defer cp.Stop(context.WithoutCancel(x.in.t.Context()))
	done, ended := make(chan struct{}), make(chan struct{})
	if during != nil {
		go func() {
			defer close(done)
			during(ended)
		}()
	} else {
		close(done)
	}
	defer func() {
		close(ended)
		<-done
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := cp.Expunge(x.in.t.Context(), cand)
		if out != compact.OutcomeBusy || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(10 * time.Millisecond)
		if fresh, ok := x.candidate(cand.ID); ok {
			cand = fresh
		}
	}
}

// answered is what a cycle's outcome makes of the run: an applied cycle's
// commit judged by the model; and anything else but nothing owed a failure
// — in an interleaving no other build holds the repository, and the plan is
// made over the rows the committer decides it from.
func (x *expungeRun) answered(repo string, cand code.ExpungeCandidate, out compact.Outcome, err error) string {
	switch out {
	case compact.OutcomeApplied:
		x.in.saw("expunge applied")
		if cand.Open != "" {
			x.in.saw("expunge resumed from its rows")
		}
		x.afterCommit(repo)
		return "applied"
	case compact.OutcomeNotDue:
		return "nothing to rewrite"
	}
	x.in.fatalf("an expunge of repo-%d: %s: %v", slices.Index(x.in.repos, repo)+1, out, err)
	return ""
}

// afterCommit is the model's judgement of a commit the committer applied to
// repo during the step, if any: every commit pushed before its begin that no
// ref reached at its scan — what it left dropping — and no ref reaches now —
// what its rescue did not keep — left the repository: the copy's
// commit-graph, if it holds one, lists none of them, and `git rev-list -1`
// refuses each, which over a graph still listing it git answers as though
// the commit were there. And the copy, synced to the commit, is whole (fsck).
func (x *expungeRun) afterCommit(repo string) {
	r := x.rows(repo)
	x.mu.Lock()
	committed, begun, scan, graphed := r.committed, r.committedBegun, r.committedScan, r.committedGraph
	r.committed, r.committedGraph = false, nil
	x.mu.Unlock()
	if !committed {
		return
	}
	reach := x.reachable(repo)
	var gone []string
	x.mu.Lock()
	for _, c := range r.pushed {
		if c.seq < begun && !scan[c.oid] && !reach[c.oid] && !r.purged[c.oid] {
			r.purged[c.oid] = true
			gone = append(gone, c.oid)
		}
	}
	x.mu.Unlock()
	s := x.in.syncAt(repo, linearizable)
	defer s.Release()
	left := x.graphed(repo)
	for _, oid := range gone {
		if graphed[oid] {
			x.in.saw("a commit dropped a commit its copy's graph listed")
		}
		if left[oid] {
			x.in.fatalf("after the commit, repo-%d's commit-graph still lists %s, which no ref reached",
				slices.Index(x.in.repos, repo)+1, oid)
		}
		if out, _, err := x.in.w.client.Try(x.in.t, "", nil, "--git-dir="+s.Dir, "rev-list", "-1", oid); err == nil {
			x.in.fatalf("after the commit, repo-%d's copy walks %s, which no ref reached: %q",
				slices.Index(x.in.repos, repo)+1, oid, out)
		}
	}
	if len(gone) > 0 {
		x.in.saw("a commit dropped history")
	}
	x.fsck(repo)
}

// graphed is every commit repo's copy's commit-graph lists, read from its OIDL
// chunk as the copy stands: none where the cache holds no copy of it, or the
// copy no commit-graph. An expunge's interleaving publishes nothing, so a
// copy's one commit-graph is the maintainer's objects/info/commit-graph.
func (x *expungeRun) graphed(repo string) map[string]bool {
	if _, ok := cache.Repos(x.in.c)[repo]; !ok {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(x.in.w.dir(x.in.t, x.in.c, repo), "objects", "info", "commit-graph"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		x.in.fatalf("repo-%d's commit-graph: %v", slices.Index(x.in.repos, repo)+1, err)
	}
	start, end, err := gittest.ChunkOf(body, gittest.GraphHeader, "OIDL")
	if err != nil {
		x.in.fatalf("repo-%d's commit-graph: %v", slices.Index(x.in.repos, repo)+1, err)
	}
	out := map[string]bool{}
	for at := start; at+20 <= end; at += 20 {
		out[hex.EncodeToString(body[at:at+20])] = true
	}
	return out
}

// fsck holds repo's copy, synced linearizable, to `git fsck
// --connectivity-only --no-dangling` with its dropping view named, as a
// serving process names it: every object a ref reaches is in the copy.
func (x *expungeRun) fsck(repo string) {
	s := x.in.syncAt(repo, linearizable)
	defer s.Release()
	res, err := x.in.w.pool.Run(x.in.t.Context(), gitproc.Cmd{Dir: s.Dir, Dropping: s.Dropping,
		Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
		Args: []string{"fsck", "--connectivity-only", "--no-dangling"}})
	if err != nil {
		x.in.fatalf("repo-%d's copy fails git fsck: %v\n%s", slices.Index(x.in.repos, repo)+1, err, res.Stderr)
	}
}

// reachable is every commit repo's live refs reach, as git walks them in the
// repository's work tree, which holds every commit the run made.
func (x *expungeRun) reachable(repo string) map[string]bool {
	targets := x.in.w.log.liveTargets(repo)
	out := map[string]bool{}
	if len(targets) == 0 {
		return out
	}
	args := append([]string{"rev-list"}, targets...)
	for _, oid := range strings.Fields(x.in.w.client.Run(x.in.t, x.in.w.repos[repo].work, args...)) {
		out[oid] = true
	}
	return out
}

// expungeCommitter is a repository's committer as an interleaving's expunger
// reaches it: the listing of the expunges owed and the tips the drops left,
// read from the run's rows, and an expunge's begin and commit decided from
// the log's rows as they stand when proposed — the code domain's rules: a
// begin over no expunge open, at the next drop epoch, every input a live pack;
// a commit of the open expunge at its epoch, retiring exactly the dropping
// packs — and, decided, applied there as one record each
// (fakeLog.expungeBegin, fakeLog.expungeCommit), its outputs kept as bundles
// for a later merge of them.
type expungeCommitter struct{ x *expungeRun }

func (e expungeCommitter) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	x := e.x
	switch q.Kind {
	case code.ReadExpungeDue:
		var out code.ReadAnswer
		for _, repo := range x.in.repos {
			r := x.rows(repo)
			x.mu.Lock()
			open, request, requested := r.open, r.request, r.requested
			x.mu.Unlock()
			if open == "" && request == "" {
				continue
			}
			c := code.ExpungeCandidate{ID: repo, Name: x.in.w.log.nameOf(repo), Open: open, Request: request,
				Requested: requested, Version: x.in.w.log.head()}
			for _, p := range x.in.w.log.packRows(repo) {
				if p.State != code.PackDropping {
					c.LivePacks, c.LiveBytes, c.LiveObjects = c.LivePacks+1, c.LiveBytes+p.Size,
						c.LiveObjects+p.Objects
				}
			}
			out.Expunge = append(out.Expunge, c)
		}
		return out, nil
	case code.ReadExpungeTips:
		r := x.rows(q.ExpungeTips.RepoID)
		x.mu.Lock()
		defer x.mu.Unlock()
		var out code.ReadAnswer
		for _, d := range r.drops {
			if !d.at.Before(q.ExpungeTips.Since) && !slices.Contains(out.Tips, d.old) {
				out.Tips = append(out.Tips, d.old)
			}
		}
		return out, nil
	}
	return code.ReadAnswer{}, fmt.Errorf("%w: an expunge's committer answers its two reads alone, not %q",
		code.ErrInvalid, q.Kind)
}

// Compact refuses: an expunge's interleaving merges packs by its own
// operation (interleaving.compact), never through its expunger.
func (e expungeCommitter) Compact(context.Context, code.CompactionProposal) (code.CompactResult, error) {
	return code.CompactResult{}, fmt.Errorf("%w: an expunge's interleaving compacts through its own operation",
		code.ErrInvalid)
}

func (e expungeCommitter) Expunge(ctx context.Context, p code.ExpungeProposal) (code.ExpungeResult, error) {
	if err := p.Validate(); err != nil {
		return code.ExpungeResult{}, err
	}
	x := e.x
	switch {
	case p.Begin != nil:
		b := p.Begin
		r := x.rows(b.RepoID)
		x.mu.Lock()
		open := r.open
		x.mu.Unlock()
		live := map[string]bool{}
		for _, row := range x.in.w.log.packRows(b.RepoID) {
			live[row.PackID] = row.State == code.PackLive
		}
		switch {
		case open != "":
			return refusedStale(fmt.Sprintf("expunge %s is open", open)), nil
		case b.DropEpoch != x.in.w.log.dropEpoch(b.RepoID)+1:
			return refusedStale(fmt.Sprintf("the begin moves the epoch to %d", b.DropEpoch)), nil
		}
		for _, id := range b.Retire {
			if !live[id] {
				return refusedStale(fmt.Sprintf("pack %s is no longer live", id)), nil
			}
		}
		if err := x.in.w.keepOutputs(ctx, slices.Concat(b.Add, b.Dropping)); err != nil {
			return code.ExpungeResult{}, err
		}
		scan := x.reachable(b.RepoID)
		pos := x.in.w.log.expungeBegin(b.RepoID, expungeRowsOf(b.Add), expungeRowsOf(b.Dropping), b.Retire...)
		x.mu.Lock()
		x.seq++
		delete(scan, r.revived)
		r.open, r.begunAt, r.beginSeen, r.scanReach, r.revived = b.ExpungeID, x.seq, b.Seen, scan, ""
		x.mu.Unlock()
		x.in.last[b.RepoID] = pos
		return code.ExpungeResult{Outcome: statelog.OutcomeApplied, Position: pos}, nil
	case p.Commit != nil:
		c := p.Commit
		r := x.rows(c.RepoID)
		x.mu.Lock()
		open, begun, scan := r.open, r.begunAt, r.scanReach
		x.mu.Unlock()
		var dropping []string
		for _, row := range x.in.w.log.packRows(c.RepoID) {
			if row.State == code.PackDropping {
				dropping = append(dropping, row.PackID)
			}
		}
		slices.Sort(dropping)
		switch {
		case open != c.ExpungeID:
			return refusedStale(fmt.Sprintf("expunge %q is open, not %s", open, c.ExpungeID)), nil
		case c.DropEpoch != x.in.w.log.dropEpoch(c.RepoID):
			return refusedStale(fmt.Sprintf("the commit names epoch %d", c.DropEpoch)), nil
		case !slices.Equal(dropping, c.Retire):
			return refusedStale(fmt.Sprintf("the commit retires %v and %v are dropping", c.Retire, dropping)), nil
		}
		if err := x.in.w.keepOutputs(ctx, c.Add); err != nil {
			return code.ExpungeResult{}, err
		}
		graph := x.graphed(c.RepoID)
		pos := x.in.w.log.expungeCommit(c.RepoID, expungeRowsOf(c.Add)...)
		x.mu.Lock()
		if r.request != "" && r.beginSeen.Packed() >= r.requested.Packed() {
			r.request, r.requested = "", statelog.Position{}
		}
		r.open, r.committed, r.committedBegun, r.committedScan, r.committedGraph = "", true, begun, scan, graph
		x.mu.Unlock()
		x.in.last[c.RepoID] = pos
		return code.ExpungeResult{Outcome: statelog.OutcomeApplied, Position: pos}, nil
	}
	return code.ExpungeResult{}, fmt.Errorf("%w: an expunge's maintainer proposes begins and commits alone",
		code.ErrInvalid)
}

// refusedStale is the refusal the committer's decide answers a plan its rows
// no longer hold by.
func refusedStale(detail string) code.ExpungeResult {
	return code.ExpungeResult{Refusal: code.RefusalStale, Detail: detail}
}

// expungeRowsOf is an expunge's outputs as the rows its apply adds, each
// record setting its own kind and state.
func expungeRowsOf(refs []code.PackRef) []code.PackRow {
	rows := make([]code.PackRow, len(refs))
	for i, r := range refs {
		rows[i] = code.PackRow{PackID: r.PackID, Part: "pack", Object: r.Object, Hash: r.Hash, Size: r.Size,
			IndexBytes: r.IndexBytes, ReverseBytes: r.ReverseBytes, GitChecksum: r.GitChecksum, Objects: r.Objects}
	}
	return rows
}

// refTarget is the object repo's live ref names, "" for none.
func (f *fakeLog) refTarget(repo, ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repos[repo].refs[ref].oid
}

// liveTargets is every object repo's live refs name, sorted and each once.
func (f *fakeLog) liveTargets(repo string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, ref := range f.repos[repo].refs {
		if ref.oid != "" && !slices.Contains(out, ref.oid) {
			out = append(out, ref.oid)
		}
	}
	slices.Sort(out)
	return out
}

// packRows is repo's pack rows, live and dropping, as they stand.
func (f *fakeLog) packRows(repo string) []code.PackRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repos[repo].packs)
}

// dropEpoch is repo's drop epoch as its rows hold it.
func (f *fakeLog) dropEpoch(repo string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repos[repo].ref.DropEpoch
}

// mark is a record on the log that moves no row a cache reads — a purge
// request's, whose apply writes the repository's request columns alone — and
// its position.
func (f *fakeLog) mark() statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return f.at()
}
