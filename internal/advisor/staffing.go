package advisor

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/worklet"
)

// STAFFING (ADR-0164, built on ADR-0162's flow record): how long the work a
// seat holds stood in each flow state over the week says whether the seat, or
// the model it runs on, is what the work waits for.

// ClassModelHeld is the owner's: an agent seat whose work was held on its
// model's limit for a large share of the week.
const ClassModelHeld Class = "capacity.model_held"

// The thresholds, judgment calls to be tuned on the first weeks of data: two
// item-hours a day on average held on the model's limit, a quarter or more of
// the time the seat's work stood wanting it or held.
const (
	minLimitHeld       = 14 * time.Hour
	limitHeldPercent   = 25
	maxFallbackOptions = 3
)

// seatFlow is one seat's item-hours over the window: held on its model's
// limit, and all the time its work stood wanting the seat or held.
type seatFlow struct{ limit, standing time.Duration }

// flowBySeat sums ev's flow hours by seat.
func flowBySeat(ev Evidence) map[string]seatFlow {
	out := map[string]seatFlow{}
	for _, h := range ev.Flow {
		if h.Seat == "" {
			continue
		}
		s := out[h.Seat]
		switch {
		case h.State == worklet.FlowHeld && h.Cause == worklet.FlowCauseLimit:
			s.limit += h.Stood
			s.standing += h.Stood
		case h.State == worklet.FlowHeld || h.State.Loads():
			s.standing += h.Stood
		}
		out[h.Seat] = s
	}
	return out
}

// modelHeld is one finding per agent seat whose work stood held on its model's
// limit [minLimitHeld] or more over the week, and [limitHeldPercent] or more of
// the time it stood wanting the seat or held.
func modelHeld(c *config.Company, ev Evidence) []Finding {
	by := flowBySeat(ev)
	var out []Finding
	for _, role := range agentSeats(c) {
		handle := role.Seat().Handle()
		s := by[handle]
		if s.limit < minLimitHeld || s.limit*100 < limitHeldPercent*s.standing {
			continue
		}
		entry := executorEntry(c, role)
		limitMinutes, standingMinutes := int64(s.limit/time.Minute), int64(s.standing/time.Minute)
		_, path := roleOf(c, handle)
		f := stallFinding(ClassModelHeld, AreaSetup, Subject{Kind: SubjectSeat, Seat: handle},
			fmt.Sprintf("%s of %s item-hours held on %s's limit", hoursSaid(s.limit), hoursSaid(s.standing), entry),
			[]Fact{{Key: "limit_minutes", Value: limitMinutes}, {Key: "standing_minutes", Value: standingMinutes}},
			nil, fallbackFix(c, role, entry, by))
		f.Path = at(path, "llm")
		out = append(out, f)
	}
	return out
}

// hoursSaid is a duration as whole hours, or minutes under one.
func hoursSaid(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	}
	return fmt.Sprintf("%dh", int64(d/time.Hour))
}

// fallbackFix offers role a fallback after its executor's chain: a model entry
// on another account (state_dir) or harness that the company's rules admit for
// the seat, the ones whose seats were held least on their own limit first; with
// none, the providers' setting, where one is added.
func fallbackFix(c *config.Company, role *config.Role, entry string, by map[string]seatFlow) Fix {
	handle := role.Seat().Handle()
	chain := c.ExecutorChain(role)
	current := c.Providers.LLM[entry]
	held := map[string]time.Duration{}
	for _, r := range agentSeats(c) {
		held[executorEntry(c, r)] += by[r.Seat().Handle()].limit
	}
	var candidates []string
	for _, key := range c.Providers.ProviderOrder() {
		other := c.Providers.LLM[key]
		if slices.Contains(chain, key) || (other.Harness == current.Harness && other.StateDir == current.StateDir) {
			continue
		}
		candidates = append(candidates, key)
	}
	slices.SortStableFunc(candidates, func(a, b string) int { return cmp.Compare(held[a], held[b]) })
	fix := Fix{Kind: FixConfigChange, Options: []Option{},
		Sentence:  fmt.Sprintf("Give %s a fallback model on another account, used when %s is at its limit.", role.Name, entry),
		Unchanged: fmt.Sprintf("%s still runs on %s first.", role.Name, entry)}
	for _, key := range candidates {
		ch := Change{Entity: EntityRoles, ID: handle, Set: map[string]any{},
			Merge:   map[string]any{"llm": map[string]any{"default": append(slices.Clone(chain), key)}},
			Summary: fmt.Sprintf("Fall back from %s to %s for %s", entry, key, handle)}
		if CheckChange(c, ch) != nil {
			continue
		}
		fix.Options = append(fix.Options, Option{Label: fmt.Sprintf("Fall back to %s", key), Change: ch})
		if len(fix.Options) == maxFallbackOptions {
			break
		}
	}
	if len(fix.Options) == 0 {
		return Fix{Kind: FixOpenSetting, Options: []Option{}, Setting: "providers.llm",
			Sentence: fmt.Sprintf("Add a model on another account to the providers, then give it to %s as a "+
				"fallback after %s.", role.Name, entry),
			Unchanged: fmt.Sprintf("%s's work waits on %s's limit meanwhile.", role.Name, entry)}
	}
	return fix
}
