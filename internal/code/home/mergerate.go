package home

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE MERGE RATE (M5-D28): what raises code_merge_rate_over_capacity — a
// queue asked to land more changes within [code.MergeRateWindow] than it can
// land in it — measured by the queue worker of the repository's
// HOME, from the two figures only it holds together:
//
//   - WHAT IT WAS ASKED, from the rows: every page of its queue work counts
//     the target's enqueues within the window, live and ended alike
//     (code.QueueDue.Enqueued), so an asking it cannot see — past the 64
//     entries a page carries, or enqueued and dequeued between two turns —
//     is counted all the same;
//   - HOW FAST IT LANDS, from the rounds it timed: a round is one of its
//     trains, from the record that built it applying to the record that
//     landed its green prefix applying, and the mean of those that ended
//     within the window is the round its capacity is priced at
//     (config.LandingCapacity at the target's depth and batch). A train
//     still testing counts as one more round, at its age so far, once it
//     has outlasted that mean — a queue whose checks have stopped finishing
//     lands nothing, and must not be priced at the rounds it ran before.
//
// THE QUEUE'S OWN CAPACITY IS THE THRESHOLD (ADR-0015), so the alarm fires at
// any count and the framework holds no rate of its own: a deep queue's
// ordinary hour and a depth-one queue with hour-long checks are each judged by
// what they can land.
//
// NODE-LOCAL, as the compactor's last pass is: only the home builds a
// queue's trains, so only it can time them, and a home new to a queue — the
// last one gone, or the placement moved — judges it from the first train it
// builds, the quiet direction. A figure stands until a turn of its
// repository's queue work reads every target and no longer meets it — the
// queue drained — or for one window at most, after which it counts nothing
// the current window asked: what an old home holds once its repository moved
// on lapses with it, and the next reading forgets it ([mergeRate.reading]).
type mergeRate struct {
	mu      sync.Mutex
	targets map[rateKey]*rateTarget
}

// rateKey is one target of one repository.
type rateKey struct {
	repo, target string
}

// rateTarget is one target's timing and its last measurement.
type rateTarget struct {
	// train is the train this node last built onto the target and built
	// when its record applied: the round under way, "" for none.
	train string
	built time.Time
	// rounds are the rounds this node timed, oldest first: each a train's
	// build to its landing, and when it landed.
	rounds []timedRound
	// name, asked, capacity, round and at are the last measurement — the
	// target by its branch and repository, its enqueues within the window,
	// what it can land in it at round, and when it was taken — zero at for
	// none.
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

// target is key's entry, made for a first use. Held under r.mu.
func (r *mergeRate) target(key rateKey) *rateTarget {
	if r.targets == nil {
		r.targets = map[rateKey]*rateTarget{}
	}
	t, ok := r.targets[key]
	if !ok {
		t = &rateTarget{}
		r.targets[key] = t
	}
	return t
}

// built records train built onto repoID's target at at: the round it
// begins.
func (r *mergeRate) built(repoID, target, train string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.target(rateKey{repoID, target})
	t.train, t.built = train, at
}

// landed records train's green prefix landed on repoID's target at at: the
// round it ends, timed where this node built it.
func (r *mergeRate) landed(repoID, target, train string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.targets[rateKey{repoID, target}]
	if !ok || t.train != train || t.train == "" {
		return
	}
	t.rounds = append(t.rounds, timedRound{ended: at, took: at.Sub(t.built)})
	t.train = ""
}

// measure takes d's merge rate at now, from repoID's queue work d: its
// enqueues within the window against its capacity at the mean round this
// node timed — none where it has timed no round, and none for a target whose
// gate requires no check, which has no queue to price.
func (r *mergeRate) measure(repoID string, d code.QueueDue, now time.Time) {
	if len(d.Required) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.target(rateKey{repoID, d.TargetRef})
	from := now.Add(-code.MergeRateWindow)
	t.rounds = slices.DeleteFunc(t.rounds, func(x timedRound) bool { return x.ended.Before(from) })
	if t.train != "" && (len(d.Train) == 0 || d.Train[0].Train != t.train) {
		// ABANDONED, never landed: an ejection or a moved tip ended the
		// train, and the next one built begins the next round.
		t.train = ""
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
		return
	}
	repo := d.Repo
	if repo == "" {
		repo = repoID
	}
	t.name = fmt.Sprintf("%s of %s", strings.TrimPrefix(d.TargetRef, "refs/heads/"), repo)
	t.round = sum / time.Duration(n)
	t.asked = d.Enqueued
	t.capacity = config.LandingCapacity(d.Depth, d.Batch, t.round, code.MergeRateWindow)
	t.at = now
}

// seen drops every target of repoID a turn that read all of its queue work
// did not meet: a target with no live entry left, whose queue has drained.
func (r *mergeRate) seen(repoID string, met []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.targets {
		if key.repo == repoID && !slices.Contains(met, key.target) {
			delete(r.targets, key)
		}
	}
}

// reading fills out's merge-rate half at now: how many targets were asked past
// their capacity by their last measurement within the window, and the one
// asked furthest past it — by the ratio of the two, so a small queue far
// behind outranks a large one barely past.
//
// It also FORGETS every target neither measured nor building a train within
// the window: one whose repository's queue work this node no longer turns —
// its home moved to another node, or the repository was deleted — which
// [mergeRate.seen] never reaches, since only a turn of the repository calls
// it. Without that, a long-lived git node would keep an entry for every
// target it was ever home to.
func (r *mergeRate) reading(now time.Time, out *statelog.Reading) {
	r.mu.Lock()
	defer r.mu.Unlock()
	from := now.Add(-code.MergeRateWindow)
	var over int
	var worst *rateTarget
	for key, t := range r.targets {
		if (t.at.IsZero() || t.at.Before(from)) && (t.train == "" || t.built.Before(from)) {
			delete(r.targets, key)
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
	out.CodeMergeRateOver = over
	if worst == nil {
		return
	}
	out.CodeMergeRateOverTarget = worst.name
	out.CodeMergeRateOverAsked = worst.asked
	out.CodeMergeRateOverCapacity = worst.capacity
	out.CodeMergeRateOverRound = worst.round
	out.CodeMergeRateWindow = code.MergeRateWindow
}

// furtherPast reports a asked further past its capacity than b — the greater
// ratio of enqueues to capacity, compared as integer products (each asked
// past its capacity, so a capacity is under [code.MergeRateScan] and neither
// product overflows) — and on a tie the name first, so a reading names one
// target whatever order the map is walked in.
func furtherPast(a, b *rateTarget) bool {
	l, r := int64(a.asked)*int64(max(1, b.capacity)), int64(b.asked)*int64(max(1, a.capacity))
	if l != r {
		return l > r
	}
	return a.name < b.name
}
