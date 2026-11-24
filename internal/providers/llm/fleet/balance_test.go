package fleet_test

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

// lcg is the simulation's random source: x = (x*1664525 + 1013904223) mod 2^32,
// read as a fraction of 2^32, so a seed replays the same company.
type lcg struct{ x uint64 }

func (r *lcg) next() float64 {
	r.x = (r.x*1664525 + 1013904223) % 4294967296
	return float64(r.x) / 4294967296
}

// balanceRun is one simulated company's spread, in points, over the turns
// after the warm-up.
type balanceRun struct {
	mean, p95, max float64
	moves          int
}

const (
	simSeats     = 6
	simTurns     = 2000
	simWarmUp    = 500
	simTurnMin   = 20_000
	simTurnMax   = 200_000
	simWindow    = 300 // turns a rolling window spans, about
	simFollowUps = 0.6
)

type simPlan struct {
	key string
	cap float64
}

// simPlans are four plans of different sizes, the smallest a fifth of the
// largest, in the order they are listed.
var simPlans = []simPlan{
	{"plan-large", 20_000_000},
	{"plan-small", 4_000_000},
	{"plan-medium", 8_000_000},
	{"plan-large-2", 12_000_000},
}

// simulate runs simTurns pieces of work through [fleet.Fleet.Pick]: each a
// random seat's, 60% of them follow-ups of its last turn, 20k to 200k tokens
// on whichever member Pick hands it, every member's use decaying over a
// rolling window of about 300 turns.
func simulate(seed uint64) balanceRun {
	r := &lcg{x: seed}
	used := make([]float64, len(simPlans))
	served := make([]time.Time, len(simPlans))
	home := make([]string, simSeats)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	shareOf := func(i int) float64 { return used[i] / simPlans[i].cap * 100 }
	var (
		gaps  []float64
		moves int
	)
	for turn := 1; turn <= simTurns; turn++ {
		seat := int(r.next() * simSeats)
		own := home[seat]
		if own != "" {
			// Whether this is a follow-up (simFollowUps of them), drawn so a seed
			// replays the same company; the rule treats both alike.
			r.next()
		}
		f := fleet.Fleet{Key: "claude-opus", Strategy: fleet.Balanced}
		for i, p := range simPlans {
			f.Members = append(f.Members, fleet.Member{Key: p.key, Plan: true, State: fleet.Available,
				Share: int(math.Round(shareOf(i))), Known: true, LastServed: served[i]})
		}
		choice, ok := f.Pick(own)
		if !ok {
			panic("an available fleet picked nobody")
		}
		chosen := slices.IndexFunc(simPlans, func(p simPlan) bool { return p.key == choice.Key })
		if own != "" && choice.Key != own {
			moves++
		}
		home[seat] = choice.Key
		for i := range used {
			used[i] *= 1 - 1.0/simWindow
		}
		used[chosen] += simTurnMin + r.next()*(simTurnMax-simTurnMin)
		served[chosen] = start.Add(time.Duration(turn) * time.Second)
		if turn > simWarmUp {
			shares := make([]float64, len(simPlans))
			for i := range shares {
				shares[i] = shareOf(i)
			}
			gaps = append(gaps, slices.Max(shares)-slices.Min(shares))
		}
	}
	slices.Sort(gaps)
	sum := 0.0
	for _, g := range gaps {
		sum += g
	}
	return balanceRun{mean: sum / float64(len(gaps)), p95: gaps[len(gaps)*95/100],
		max: gaps[len(gaps)-1], moves: moves}
}

// TestBalancedFleetKeepsMembersWithinTheThreshold holds the promise the
// Fleets page makes of a Balanced fleet: across plans of different sizes, the
// mean gap between the most and least used member stays within the threshold,
// and no gap exceeds the threshold plus one turn on the smallest plan (what a
// pick can add after the home's allowance) plus the point a comparison in
// whole points hides, each share rounding by up to half of one.
func TestBalancedFleetKeepsMembersWithinTheThreshold(t *testing.T) {
	smallest := simPlans[0].cap
	for _, p := range simPlans {
		smallest = min(smallest, p.cap)
	}
	oneTurn := simTurnMax / smallest * 100
	for seed := uint64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			run := simulate(seed)
			t.Logf("mean %.2f, p95 %.2f, max %.2f points; %d moves in %d turns",
				run.mean, run.p95, run.max, run.moves, simTurns)
			if run.mean > fleet.DefaultThreshold {
				t.Errorf("mean gap %.2f points, want within the threshold %d", run.mean, fleet.DefaultThreshold)
			}
			if limit := fleet.DefaultThreshold + oneTurn + 1; run.max > limit {
				t.Errorf("max gap %.2f points, want within the threshold, one turn on the smallest "+
					"plan and a point of rounding (%.0f)", run.max, limit)
			}
		})
	}
}
