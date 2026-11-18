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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY INTERLEAVING OF AN EXPUNGE KEEPS WHAT A REF REACHES: the production
// expunger (internal/gitlet/compact), deciding on a committer with the code
// domain's rules (expungeCommitter), runs beside the cache's everyday operations,
// purges, a revive between scan and begin, a drop of what the commit-graph
// lists, and crashes at each gitletfault.ExpungePath point (resumed from rows).
// I1: after every expunge and check, each copy synced linearizable passes `git
// fsck --connectivity-only --no-dangling` with its dropping view named, and every
// check matches it to a fresh cache's. I2: after a commit, no commit the model
// drops is in the copy's commit-graph or walked by `git rev-list -1`.
// Mutations: give a push's scratch the dropping view as an alternate (I1); keep
// the commit-graph on EventPackExpunged (I2); skip the rescue, and the revived
// branch names a commit no pack holds (I1).
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

// SIXTEEN SEEDS OF FORTY-EIGHT STEPS, a check every eight: a cycle runs three
// git builds and a rescue's, so seeds are shorter than the cache's own
// interleavings'; sixteen reach every expungeOutcomes entry between them.
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

// expungeOutcomes is every outcome the seeds must reach between them.
var expungeOutcomes = []string{
	"purge", "expunge applied", "expunge resumed from its rows", "a revive rescued", "a commit dropped history",
	"a commit dropped a commit its copy's graph listed",
	"expunge crashed at " + string(gitletfault.ExpungeAfterBuild),
	"expunge crashed at " + string(gitletfault.ExpungeAfterBegin),
	"expunge crashed at " + string(gitletfault.ExpungeAfterRescueUpload),
	"expunge crashed at " + string(gitletfault.ExpungeAfterCommit),
	"a push onto a dropping commit refused", "force-push", "delete a branch", "compact", "restart",
}

// errExpungeCrashed is what a crashed cycle's fault point answers.
var errExpungeCrashed = errors.New("the maintainer crashed here")

// expungeRun is one seed's run: the cache's interleaving, and what the
// committer and the model keep of each repository.
type expungeRun struct {
	in *interleaving
	// mu guards repos, which the committer moves on the expunger's
	// goroutine, and seq.
	mu    sync.Mutex
	repos map[string]*expungeRows
	// seq orders a push against a begin within one step, which may do both.
	seq int
	// branches counts the branches the run created, naming each.
	branches int
}

// expungeRows is what the committer and the model keep of one repository: the
// open expunge (its begin's seq, seen position and scan's reach), the purge
// request, every ref drop, every push and its seq, and every commit a commit
// purged. revived is a revive's commit, unreached at the scan whatever reaches
// it by the begin.
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
	// committed is set by an applied commit until afterCommit reads it, with
	// its begin's seq and scan reach and the copy's graph as proposed.
	committed      bool
	committedBegun int
	committedScan  map[string]bool
	committedGraph map[string]bool
}

// expungeDrop is one ref drop: when its record landed and the object left.
type expungeDrop struct {
	at  time.Time
	old string
}

// expungeCommit is one commit the run pushed, and when (seq).
type expungeCommit struct {
	oid string
	seq int
}

// interleaveExpunge runs one seed and answers the outcomes it reached.
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

// unreachable is every commit pushed to repo that no live ref reaches and no
// commit purged; with dropping, only those the open begin left dropping
// (pushed before it, unreached at its scan).
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

// onHeld resets repo's work tree to main when an expunge purged its head: the
// world's packs omit what earlier pushes brought (syncWorld's tips), so the next
// commit must build on a parent the repository still holds.
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

// restart is the cache's interleaving's crash and boot.
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

// forcePush force-moves a branch to a new commit on an older reachable one;
// its old target is a drop.
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
	row.Added = x.in.w.log.writePushes(repo, fakePush{packs: []gitlet.PackRow{row}, refs: map[string]string{ref: oid},
		forced: map[string]bool{ref: true}, introduced: []string{oid}})
	x.in.last[repo] = row.Added
	x.recordPushed(repo)
	x.dropped(repo, old)
	x.in.saw("force-push")
	return fmt.Sprintf("%s from %.7s to %.7s on %.7s", ref, old, oid, base)
}

// deleteBranch deletes a branch other than main, which stays so a ref always
// reaches a held commit; its target is a drop.
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

// pushOntoDropping pushes, while an expunge is open, a commit on a dropping
// parent without that parent's objects. The receive's connectivity check in a
// push's scratch must find the parent missing: a scratch leans on the copy's
// objects/ alone, never its dropping view, which keeps a rescue valid at any
// position after the begin. A receive that passed lands between the rescue and
// the commit (the cycle held there), and the commit retires the parent it
// reaches. It opens an expunge first where none is; with nothing dropping it
// pushes as ever.
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
	// THE RECEIVE'S CONNECTIVITY CHECK, as receive-pack makes it, by the
	// case's own git, so an alternates file a mutation widened is read as
	// git reads it.
	_, stderr, err := x.in.w.client.Try(t, "", nil, "--git-dir="+sc.Dir, "rev-list", "--objects", tip, "--not",
		"--alternate-refs")
	f.tips, f.head = tips, head
	x.in.w.client.Run(t, f.work, "reset", "-q", "--hard", head)
	if err != nil {
		x.in.saw("a push onto a dropping commit refused")
		return strings.Join(append(did, fmt.Sprintf("onto dropping %.7s: refused (%s)", parent, firstLine(stderr))),
			"; ")
	}
	// A RECEIVE THAT PASSED lands as its committer would, after the rescue
	// read the refs and before the commit retires what it did not keep.
	row := x.in.w.storeBundle(t, x.in.w.bundle(cs), "")
	x.branches++
	ref := fmt.Sprintf("refs/heads/onto-%d", x.branches)
	cand, _ := x.candidate(repo)
	script := &gitletfault.Script{}
	held, reached := make(chan struct{}), make(chan struct{})
	script.Arm(gitletfault.ExpungeAfterRescueUpload, gitletfault.Action{Once: true, Block: held, Reached: reached})
	land := func(ended <-chan struct{}) {
		select {
		case <-reached:
		case <-ended:
			return
		}
		row.Added = x.in.w.log.writePushes(repo, fakePush{packs: []gitlet.PackRow{row},
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

// openAnExpunge leaves an expunge open on repo, stopped at its begin, with a
// commit dropping: it pushes and deletes a branch first where none would be.
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
	script := &gitletfault.Script{}
	script.Arm(gitletfault.ExpungeAfterBegin, gitletfault.Action{Once: true, Err: errExpungeCrashed})
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

// purge is a person's purge request of repo, at a record of its own; one asked
// while another is open widens it.
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
func (x *expungeRun) candidate(repo string) (gitlet.ExpungeCandidate, bool) {
	a, err := expungeCommitter{x}.Read(x.in.t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadExpungeDue})
	if err != nil {
		x.in.fatalf("the expunge listing: %v", err)
	}
	for _, cand := range a.Expunge {
		if cand.ID == repo {
			return cand, true
		}
	}
	return gitlet.ExpungeCandidate{}, false
}

// owe makes sure repo owes an expunge — an open one, or a purge requested.
func (x *expungeRun) owe(repo string) {
	if _, ok := x.candidate(repo); !ok {
		x.purge(repo)
	}
}

// expunge runs one cycle of the expunge repo owes, if any.
func (x *expungeRun) expunge(repo string) string {
	cand, ok := x.candidate(repo)
	if !ok {
		return "nothing owed"
	}
	out, err := x.cycle(cand, nil, nil)
	return x.answered(repo, cand, out, err)
}

// expungeWithRevive serves a purge with a branch created, between its scan and
// begin, at a commit the scan found unreachable, which the rescue must keep. An
// open expunge is finished first, so the cycle has a scan to come between.
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
	script := &gitletfault.Script{}
	held, reached := make(chan struct{}), make(chan struct{})
	script.Arm(gitletfault.ExpungeAfterBuild, gitletfault.Action{Once: true, Block: held, Reached: reached})
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

// expungeGraphed drops a commit the copy's commit-graph lists: a branch pushed
// and maintained until the graph lists it (once there is a graph, the index's
// run rewrites it, due every interleavingMidxPacks packs), then deleted and
// purged. The commit must take the graph with it (X04's one drop), or git walks
// the dropped commit from the graph (afterCommit). An open expunge is finished
// first, so the cycle's own scan reads the deletion.
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
		var row gitlet.PackRow
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

// expungeCrashed stops a cycle at a gitletfault.ExpungePath point it reaches,
// as a killed maintainer stops; a later cycle resumes from the rows.
func (x *expungeRun) expungeCrashed(repo string) string {
	x.owe(repo)
	cand, _ := x.candidate(repo)
	points := gitletfault.ExpungePath()
	if cand.Open != "" {
		// A resumed cycle starts at the rescue.
		points = []gitletfault.Point{gitletfault.ExpungeAfterRescueUpload, gitletfault.ExpungeAfterCommit}
	}
	p := points[x.in.rng.IntN(len(points))]
	script := &gitletfault.Script{}
	script.Arm(p, gitletfault.Action{Once: true, Err: errExpungeCrashed})
	out, err := x.cycle(cand, script, nil)
	if errors.Is(err, errExpungeCrashed) {
		x.in.saw("expunge crashed at " + string(p))
		x.afterCommit(repo)
		return fmt.Sprintf("crashed at %s", p)
	}
	return fmt.Sprintf("at %s: %s", p, x.answered(repo, cand, out, err))
}

// cycle runs one cycle of cand by the production expunger (a compactor never
// started) with faults armed and during beside it until the cycle ends (its
// channel closes then), retrying Busy. The clock moves a second first, so
// earlier drops precede the scan.
func (x *expungeRun) cycle(cand gitlet.ExpungeCandidate, faults *gitletfault.Script,
	during func(ended <-chan struct{})) (compact.Outcome, error) {

	x.in.clk.Add(time.Second)
	o := compact.Options{Cache: x.in.c, Objects: x.in.w.store, Estate: expungeCommitter{x}, Maintainers: maintainsAll{},
		NodeID: "git-1", Limits: gitlet.DefaultLimits, Logger: x.in.w.logs.logger(), Now: x.in.clk.Now}
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
		//fixedwait:measure the poll's own deadline: at it cycle hands back Busy, which every caller fails
		if out != compact.OutcomeBusy || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
		if fresh, ok := x.candidate(cand.ID); ok {
			cand = fresh
		}
	}
}

// answered judges a cycle's outcome: an applied commit by the model; anything
// but NotDue fails, since nothing else holds the repository and the plan comes
// from the rows the committer decides by.
func (x *expungeRun) answered(repo string, cand gitlet.ExpungeCandidate, out compact.Outcome, err error) string {
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

// afterCommit judges a commit applied to repo during the step, if any: every
// commit pushed before its begin, unreached at its scan and now, is gone — no
// commit-graph lists it and `git rev-list -1` refuses it (git walks a commit a
// stale graph lists) — and the synced copy passes fsck.
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

// graphed is every commit in the OIDL chunk of repo's copy's commit-graph, nil
// without a copy or graph. This interleaving publishes nothing, so the one graph
// is the maintainer's objects/info/commit-graph.
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
// serving process names it (I1).
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

// reachable is every commit repo's live refs reach, walked in its work tree
// (which holds every commit the run made); a git failure ends the case.
func (x *expungeRun) reachable(repo string) map[string]bool {
	out, err := x.reachableIn(x.in.t.Context(), repo)
	if err != nil {
		x.in.fatalf("%v", err)
	}
	return out
}

// reachableIn is [expungeRun.reachable] under ctx, returning git's failure: the
// committer calls it on the expunger's goroutine, which must not end the case.
func (x *expungeRun) reachableIn(ctx context.Context, repo string) (map[string]bool, error) {
	targets := x.in.w.log.liveTargets(repo)
	out := map[string]bool{}
	if len(targets) == 0 {
		return out, nil
	}
	args := append([]string{"rev-list"}, targets...)
	stdout, stderr, err := x.in.w.client.TryContext(ctx, x.in.w.repos[repo].work, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git %s in %s: %w\n%s", strings.Join(args, " "), repo, err, stderr)
	}
	for _, oid := range strings.Fields(stdout) {
		out[oid] = true
	}
	return out, nil
}

// expungeCommitter is the committer the expunger reaches: it lists expunges
// owed and drop tips from the run's rows, and decides begins and commits from
// the log's rows by the code domain's rules — a begin needs no expunge open, the
// next drop epoch and every input live; a commit needs the open expunge at its
// epoch, retiring exactly the dropping packs — applying each as one record
// (fakeLog.expungeBegin, fakeLog.expungeCommit) with outputs kept as bundles.
type expungeCommitter struct{ x *expungeRun }

func (e expungeCommitter) Read(_ context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	x := e.x
	switch q.Kind {
	case gitlet.ReadExpungeDue:
		var out gitlet.ReadAnswer
		for _, repo := range x.in.repos {
			r := x.rows(repo)
			x.mu.Lock()
			open, request, requested := r.open, r.request, r.requested
			x.mu.Unlock()
			if open == "" && request == "" {
				continue
			}
			c := gitlet.ExpungeCandidate{ID: repo, Name: x.in.w.log.nameOf(repo), Open: open, Request: request,
				Requested: requested, Version: x.in.w.log.head()}
			for _, p := range x.in.w.log.packRows(repo) {
				if p.State != gitlet.PackDropping {
					c.LivePacks, c.LiveBytes, c.LiveObjects = c.LivePacks+1, c.LiveBytes+p.Size,
						c.LiveObjects+p.Objects
				}
			}
			out.Expunge = append(out.Expunge, c)
		}
		return out, nil
	case gitlet.ReadExpungeTips:
		r := x.rows(q.ExpungeTips.RepoID)
		x.mu.Lock()
		defer x.mu.Unlock()
		var out gitlet.ReadAnswer
		for _, d := range r.drops {
			//fixedwait:stated a drop's at and the query's Since are both on the interleaving's clock (in.clk.Now)
			if !d.at.Before(q.ExpungeTips.Since) && !slices.Contains(out.Tips, d.old) {
				out.Tips = append(out.Tips, d.old)
			}
		}
		return out, nil
	}
	return gitlet.ReadAnswer{}, fmt.Errorf("%w: an expunge's committer answers its two reads alone, not %q",
		gitlet.ErrInvalid, q.Kind)
}

// Compact refuses: an expunge's interleaving merges packs by its own
// operation (interleaving.compact), never through its expunger.
func (e expungeCommitter) Compact(context.Context, gitlet.CompactionProposal) (gitlet.CompactResult, error) {
	return gitlet.CompactResult{}, fmt.Errorf("%w: an expunge's interleaving compacts through its own operation",
		gitlet.ErrInvalid)
}

func (e expungeCommitter) Expunge(ctx context.Context, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
	if err := p.Validate(); err != nil {
		return gitlet.ExpungeResult{}, err
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
			live[row.PackID] = row.State == gitlet.PackLive
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
			return gitlet.ExpungeResult{}, err
		}
		scan, err := x.reachableIn(ctx, b.RepoID)
		if err != nil {
			return gitlet.ExpungeResult{}, err
		}
		pos := x.in.w.log.expungeBegin(b.RepoID, expungeRowsOf(b.Add), expungeRowsOf(b.Dropping), b.Retire...)
		x.mu.Lock()
		x.seq++
		delete(scan, r.revived)
		r.open, r.begunAt, r.beginSeen, r.scanReach, r.revived = b.ExpungeID, x.seq, b.Seen, scan, ""
		x.mu.Unlock()
		x.in.last[b.RepoID] = pos
		return gitlet.ExpungeResult{Outcome: statelog.OutcomeApplied, Position: pos}, nil
	case p.Commit != nil:
		c := p.Commit
		r := x.rows(c.RepoID)
		x.mu.Lock()
		open, begun, scan := r.open, r.begunAt, r.scanReach
		x.mu.Unlock()
		var dropping []string
		for _, row := range x.in.w.log.packRows(c.RepoID) {
			if row.State == gitlet.PackDropping {
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
			return gitlet.ExpungeResult{}, err
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
		return gitlet.ExpungeResult{Outcome: statelog.OutcomeApplied, Position: pos}, nil
	}
	return gitlet.ExpungeResult{}, fmt.Errorf("%w: an expunge's maintainer proposes begins and commits alone",
		gitlet.ErrInvalid)
}

// refusedStale refuses a plan the rows no longer hold.
func refusedStale(detail string) gitlet.ExpungeResult {
	return gitlet.ExpungeResult{Refusal: gitlet.RefusalStale, Detail: detail}
}

// expungeRowsOf is an expunge's outputs as the rows its apply adds, each
// record setting its own kind and state.
func expungeRowsOf(refs []gitlet.PackRef) []gitlet.PackRow {
	rows := make([]gitlet.PackRow, len(refs))
	for i, r := range refs {
		rows[i] = gitlet.PackRow{PackID: r.PackID, Part: "pack", Object: r.Object, Hash: r.Hash, Size: r.Size,
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
func (f *fakeLog) packRows(repo string) []gitlet.PackRow {
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

// mark is a record moving no row a cache reads (a purge request's), and its
// position.
func (f *fakeLog) mark() statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return f.at()
}
