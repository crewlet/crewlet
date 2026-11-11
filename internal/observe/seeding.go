package observe

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/org"
)

// BootSeed is one read that fills the live projection with what happened
// before this process started: the fleet's history ([Seed]), the paused seats,
// the executors in their boxes.
//
// A SEED IS READ UNTIL IT LANDS. A boot used to give every seed one attempt
// inside one budget, so a seed that missed it, a slow store or a peer that did
// not answer, left the screens it fills starting at the boot for the life of
// the process, and `/health` said `seeded_from.complete: false` until the next
// restart. [StartSeeding] reads each one again until it completes.
type BootSeed struct {
	// Name is what the seed fills, for a person reading the log.
	Name string

	// NotSeeded and Seeded are the log events: a WARN for an attempt that
	// did not complete, and an INFO once it lands after the node began to
	// serve without it.
	NotSeeded, Seeded string

	// Hint is what an operator reads beside NotSeeded: what the screens
	// show until the seed lands.
	Hint string

	// Attempt reads the seed once and lands what it read, returning an
	// error when what it landed is not complete. It may land part of the
	// seed and fail, and is called again: so landing must be idempotent,
	// which every projection seed is ([livestate.LiveState.Seed]).
	Attempt func(ctx context.Context) error
}

// HistorySeed is [Seed] as a boot seed: the fleet's history, complete once
// every read was answered by every node the roster names, which is what
// `/health` reports as `seeded_from`.
//
// The ORG IS ASKED AT EVERY ATTEMPT, so a company applied while a seed was
// still being read again has its seats' turns read.
func HistorySeed(history FleetHistory, organization func() *org.Organization, live Seeded) BootSeed {
	return BootSeed{
		Name:      "the live projection's history",
		NotSeeded: "live_projection_not_seeded", Seeded: "live_projection_seeded",
		Hint: "the activity feed, the spend rollup and each seat's last turn start at " +
			"this process's boot until the seed lands; older history is answered by " +
			"the events, turns and tokens queries meanwhile",
		Attempt: func(ctx context.Context) error {
			coverage, err := Seed(ctx, history, organization(), live)
			if err != nil {
				return err
			}
			if !coverage.Complete {
				return incomplete(coverage)
			}
			return nil
		},
	}
}

// incomplete says which nodes a seed that read without an error still missed.
func incomplete(c eventfan.Coverage) error {
	missing := c.Missing()
	if len(missing) == 0 {
		return fmt.Errorf("observe: the seed could not read the fleet's roster")
	}
	return fmt.Errorf("observe: the seed did not hear from %s", strings.Join(missing, ", "))
}

// Seeding is the boot seeds, read side by side, each until it lands.
//
// SIDE BY SIDE, for [Seed]'s own reason one level up: read one after another
// under one budget, the history seed on a large store spent the whole budget
// and handed the pauses and the executors an expired context, so two reads of
// milliseconds failed every boot behind one slow one.
type Seeding struct {
	cancel context.CancelFunc
	// landed holds one channel per seed, closed when it lands or stops.
	landed []chan struct{}
	seeds  []BootSeed
	// serving is set once the node serves without every seed landed,
	// which is when a seed's landing is worth a line of its own.
	serving atomic.Bool
	wg      sync.WaitGroup
}

// The retry of a seed that did not land: doubling from the fleet read budget,
// the longest a silent peer is waited for inside one attempt, so a peer is
// never asked again sooner than it was just given, up to the interval the
// projection re-reads its other durable record at
// ([livestate.ReconcileInterval]), so a seed that stays incomplete costs at
// most one attempt per interval. Spread by a fifth, as every interval a fleet
// shares is ([backoff.Jitter]), so nodes that booted together do not retry
// against one peer on the same second.
const (
	seedRetryBase    = eventfan.FleetReadBudget
	seedRetryCeiling = livestate.ReconcileInterval
	seedRetryJitter  = 0.2
)

// StartSeeding starts every seed, each on its own goroutine, read again after
// every attempt that did not complete until it lands or ctx ends. The seeds are
// [Seeding.Stop]'s to end.
func StartSeeding(ctx context.Context, seeds ...BootSeed) *Seeding {
	run, cancel := context.WithCancel(ctx)
	s := &Seeding{cancel: cancel, seeds: seeds}
	for _, seed := range seeds {
		landed := make(chan struct{})
		s.landed = append(s.landed, landed)
		s.wg.Go(func() {
			defer close(landed)
			s.read(run, seed)
		})
	}
	return s
}

// read attempts one seed until it lands or ctx ends.
func (s *Seeding) read(ctx context.Context, seed BootSeed) {
	for attempt := 1; ; attempt++ {
		err := seed.Attempt(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if attempt > 1 || s.serving.Load() {
				log.InfoContext(ctx, seed.Seeded, "attempts", attempt)
			}
			return
		}
		wait := backoff.Jitter(backoff.Doubling(attempt, seedRetryBase, seedRetryCeiling), seedRetryJitter)
		// THE FIRST FAILURE AND EVERY TENTH, as the custody retry logs: a
		// seed that stays incomplete is retried every interval, and a line
		// per attempt would bury what it says.
		if attempt == 1 || attempt%10 == 0 {
			log.WarnContext(ctx, seed.NotSeeded, "error", err.Error(), "attempt", attempt,
				"retry_in", wait.String(), "hint", seed.Hint)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Wait waits up to budget for every seed to land, and logs and returns the name
// of each still being read: what the node starts serving without. Those land as
// they complete, and land on every open screen too.
func (s *Seeding) Wait(ctx context.Context, budget time.Duration) []string {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	for _, landed := range s.landed {
		select {
		case <-landed:
		case <-timer.C:
			return s.pending(ctx, budget)
		case <-ctx.Done():
			return s.pending(ctx, budget)
		}
	}
	return nil
}

// pending is every seed not yet landed, said once and marked, so each logs its
// landing when it lands.
func (s *Seeding) pending(ctx context.Context, budget time.Duration) []string {
	var out []string
	for i, landed := range s.landed {
		select {
		case <-landed:
		default:
			out = append(out, s.seeds[i].Name)
		}
	}
	if len(out) > 0 {
		s.serving.Store(true)
		log.InfoContext(ctx, "boot_seeds_pending", "seeds", out, "budget", budget.String(),
			"hint", "the node serves without them; each is read until it lands, "+
				"and every open screen is sent it then")
	}
	return out
}

// Stop ends every seed still being read and waits for it. Nil-safe.
func (s *Seeding) Stop() {
	if s == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}
