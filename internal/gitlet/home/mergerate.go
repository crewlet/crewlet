package home

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE MERGE RATE (M5-D28) raises code_merge_rate_over_capacity: a queue asked
// to land more within [gitlet.MergeRateWindow] than it can land in it. Only the
// home builds a queue's trains, so its queue worker alone holds both figures
// and the rate is node-local; a new home judges from its first train (the quiet
// direction). ASKED is the target's enqueues in the window, live and ended
// (gitlet.QueueDue.Enqueued), so one past a page's 64 entries or dequeued
// between turns still counts. CAPACITY is config.LandingCapacity at the mean
// round (build record applied to land record applied) ended in the window; a
// train testing past that mean counts as one more round at its age, so a queue
// whose checks stopped finishing is not priced at its earlier rounds. The
// queue's own capacity is the threshold (ADR-0015): the alarm holds no rate.
type mergeRate struct {
	mu sync.Mutex
	// targets is each repository's targets by ref, keyed by repository so a
	// turn's [mergeRate.seen] walks only its own targets under mu, not every
	// target the node holds. An inner map is never left empty.
	targets map[string]map[string]*rateTarget
}

// rateTarget is one target's timing and its last measurement.
type rateTarget struct {
	// train is the round under way ("" for none): the train this node last
	// built onto the target, built when its record applied.
	train string
	built time.Time
	// rounds are the rounds this node timed, oldest first.
	rounds []timedRound
	// name, asked, capacity, round and at are the last measurement (zero at
	// for none): the target by branch and repository, its enqueues in the
	// window, what it can land in it at round, and when it was taken.
	name     string
	asked    int
	capacity int
	round    time.Duration
	at       time.Time
}

// timedRound is one round's length and when it ended.
type timedRound struct {
	ended time.Time
	took  time.Duration
}

// target is repoID's target's entry, made for a first use. Held under r.mu.
func (r *mergeRate) target(repoID, target string) *rateTarget {
	if r.targets == nil {
		r.targets = map[string]map[string]*rateTarget{}
	}
	repo, ok := r.targets[repoID]
	if !ok {
		repo = map[string]*rateTarget{}
		r.targets[repoID] = repo
	}
	t, ok := repo[target]
	if !ok {
		t = &rateTarget{}
		repo[target] = t
	}
	return t
}

// built records train built onto repoID's target at at, beginning its round.
// abandoned reports it replaces an earlier train of this node's that ended
// without landing.
func (r *mergeRate) built(repoID, target, train string, at time.Time) (abandoned bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.target(repoID, target)
	abandoned = t.train != "" && t.train != train
	t.train, t.built = train, at
	return abandoned
}

// landed records train's green prefix landed on repoID's target at at, ending
// its round: took and timed true for a train this node built, else nothing.
func (r *mergeRate) landed(repoID, target, train string, at time.Time) (took time.Duration, timed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.targets[repoID][target]
	if !ok || t.train != train || t.train == "" {
		return 0, false
	}
	took = at.Sub(t.built)
	t.rounds = append(t.rounds, timedRound{ended: at, took: took})
	t.train = ""
	return took, true
}

// ended records train ended on repoID's target WITHOUT LANDING, by an eject
// the worker itself appended: no round is timed (the queue landed nothing),
// and [mergeRate.measure] does not count it again as abandoned.
func (r *mergeRate) ended(repoID, target, train string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.targets[repoID][target]; ok && t.train == train {
		t.train = ""
	}
}

// measure takes the merge rate of repoID's queue work d at now: its enqueues
// in the window against its capacity at the mean round this node timed; none
// where no round is timed, nor where the gate requires no check (no queue to
// price). abandoned reports a train this node built that d no longer holds and
// that nothing this node appended ended ([mergeRate.ended]).
func (r *mergeRate) measure(repoID string, d gitlet.QueueDue, now time.Time) (abandoned bool) {
	if len(d.Required) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.target(repoID, d.TargetRef)
	from := now.Add(-gitlet.MergeRateWindow)
	t.rounds = slices.DeleteFunc(t.rounds, func(x timedRound) bool { return x.ended.Before(from) })
	if t.train != "" && (len(d.Train) == 0 || d.Train[0].Train != t.train) {
		// ABANDONED, never landed: a moved tip, a dequeue or a newer train
		// ended it, and the next one built begins the next round.
		t.train, abandoned = "", true
	}
	var sum time.Duration
	for _, x := range t.rounds {
		sum += x.took
	}
	n := len(t.rounds)
	if t.train != "" {
		if age := now.Sub(t.built); n == 0 || age > sum/time.Duration(n) {
			sum, n = sum+age, n+1
		}
	}
	if n == 0 {
		t.at = time.Time{}
		return abandoned
	}
	repo := d.Repo
	if repo == "" {
		repo = repoID
	}
	t.name = fmt.Sprintf("%s of %s", strings.TrimPrefix(d.TargetRef, "refs/heads/"), repo)
	t.round = sum / time.Duration(n)
	t.asked = d.Enqueued
	t.capacity = config.LandingCapacity(d.Depth, d.Batch, t.round, gitlet.MergeRateWindow)
	t.at = now
	return abandoned
}

// seen drops every target of repoID that a turn reading all of its queue work
// did not meet (its queue drained), and the repository once none is left.
func (r *mergeRate) seen(repoID string, met []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	repo, ok := r.targets[repoID]
	if !ok {
		return
	}
	for target := range repo {
		if !slices.Contains(met, target) {
			delete(repo, target)
		}
	}
	if len(repo) == 0 {
		delete(r.targets, repoID)
	}
}

// reading fills out's merge-rate half at now: how many targets their last
// measurement in the window found asked past capacity, and the one furthest
// past by ratio, so a small queue far behind outranks a large one barely past.
//
// It also FORGETS every target neither measured nor building a train in the
// window (its home moved, or the repository was deleted), which
// [mergeRate.seen] never reaches; otherwise a long-lived node would keep an
// entry for every target it was ever home to.
func (r *mergeRate) reading(now time.Time, out *statelog.Reading) {
	r.mu.Lock()
	defer r.mu.Unlock()
	from := now.Add(-gitlet.MergeRateWindow)
	var over int
	var worst *rateTarget
	for repoID, repo := range r.targets {
		for target, t := range repo {
			if (t.at.IsZero() || t.at.Before(from)) && (t.train == "" || t.built.Before(from)) {
				delete(repo, target)
				continue
			}
			if t.at.IsZero() || t.at.Before(from) || t.asked <= t.capacity {
				continue
			}
			over++
			if worst == nil || furtherPast(t, worst) {
				worst = t
			}
		}
		if len(repo) == 0 {
			delete(r.targets, repoID)
		}
	}
	out.CodeMergeRateOver = over
	if worst == nil {
		return
	}
	out.CodeMergeRateOverTarget = worst.name
	out.CodeMergeRateOverAsked = worst.asked
	out.CodeMergeRateOverCapacity = worst.capacity
	out.CodeMergeRateOverRound = worst.round
	out.CodeMergeRateWindow = gitlet.MergeRateWindow
}

// furtherPast reports a asked further past its capacity than b, comparing the
// ratios as integer products (each is asked past its capacity, so that is under
// [gitlet.MergeRateScan] and neither overflows), and on a tie the name first,
// so a reading names one target whatever order the map is walked in.
func furtherPast(a, b *rateTarget) bool {
	l, r := int64(a.asked)*int64(max(1, b.capacity)), int64(b.asked)*int64(max(1, a.capacity))
	if l != r {
		return l > r
	}
	return a.name < b.name
}
