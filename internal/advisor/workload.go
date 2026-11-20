package advisor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/worklet"
)

// WORK THAT SITS WHERE NOTHING WILL MOVE IT (ADR-0166): a seat holding most of
// its job's open work while a seat doing the same job holds little, and work
// blocked on what nothing anyone holds will clear. Each is one finding per
// cause with the seats inside it, and a fix that moves the work or asks why
// it waits.

// The classes.
const (
	// ClassWorkImbalance is the owner's: a seat holding several times the
	// open work of a seat doing the same job.
	ClassWorkImbalance Class = "work.imbalance"
	// ClassBlockedNoCause is the owner's: open work blocked a day or more on
	// an item nobody holds, an item removed, or a change nothing moves.
	ClassBlockedNoCause Class = "work.blocked_no_cause"
)

// The bars each class clears.
const (
	// minImbalanceOpen, imbalanceTimes and minImbalanceGap: ten open items,
	// three times its twin's and eight more, so two seats a few items apart
	// are not one.
	minImbalanceOpen = 10
	imbalanceTimes   = 3
	minImbalanceGap  = 8
	// minUnheldAge: a day unchanged, so a wait set a moment ago is not one.
	minUnheldAge = 24 * time.Hour
)

// Reassign is what a reassign fix moves: items by key, to the seat To.
type Reassign struct {
	Items []string `json:"items"`
	To    string   `json:"to"`
}

// imbalance is one seat holding several times its twin's open work.
type imbalance struct {
	busy, idle worklet.HeldOpen
	twins      []string
}

// imbalances is every seat holding [imbalanceTimes] its least loaded twin's
// open work, each seat once, the most held first.
func imbalances(c *config.Company, ev Evidence) []imbalance {
	open := map[string]worklet.HeldOpen{}
	for _, h := range ev.Work.Open.Holders {
		open[h.Handle] = h
	}
	var out []imbalance
	for _, twins := range twinsOf(c) {
		busy, idle := twins[0], twins[0]
		for _, h := range twins {
			if open[h].Open > open[busy].Open {
				busy = h
			}
			if open[h].Open < open[idle].Open {
				idle = h
			}
		}
		b, l := open[busy], open[idle]
		b.Handle, l.Handle = busy, idle
		if b.Open < minImbalanceOpen || b.Open-l.Open < minImbalanceGap || b.Open < imbalanceTimes*l.Open ||
			slices.ContainsFunc(out, func(i imbalance) bool { return i.busy.Handle == busy }) {
			continue
		}
		out = append(out, imbalance{busy: b, idle: l, twins: twins})
	}
	slices.SortStableFunc(out, func(a, b imbalance) int { return cmp.Compare(b.busy.Open, a.busy.Open) })
	return out
}

// twinsOf is every set of two agent seats or more doing one job: in one
// place stating one goal and one set of responsibilities, or in one place
// named alike but for a number ("Engineer", "Engineer 2").
func twinsOf(c *config.Company) [][]string {
	var out [][]string
	seen := map[string]bool{}
	add := func(handles []string) {
		if len(handles) < 2 {
			return
		}
		if k := strings.Join(slices.Sorted(slices.Values(handles)), ","); !seen[k] {
			seen[k] = true
			out = append(out, handles)
		}
	}
	stems := map[string][]string{}
	var order []string
	for _, g := range groupsOf(c) {
		if r := g.roles[0]; r.Goal != "" || len(r.Responsibilities) > 0 {
			add(g.handles()) // seats that state no job are not one job
		}
		unit := ""
		if g.unit != nil {
			unit = g.unit.ID
		}
		for _, r := range g.roles {
			k := unit + "\x00" + nameStem(r.Name)
			if _, ok := stems[k]; !ok {
				order = append(order, k)
			}
			stems[k] = append(stems[k], r.Seat().Handle())
		}
	}
	for _, k := range order {
		add(stems[k])
	}
	return out
}

// nameStem is a seat's name without a trailing number, as a person reads it.
func nameStem(name string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(name), "0123456789 #-"))
}

// workImbalance is one finding per seat holding [imbalanceTimes] the open work
// of a seat doing the same job, its fix moving work not yet started to it.
func workImbalance(c *config.Company, ev Evidence) []Finding {
	var out []Finding
	for _, im := range imbalances(c, ev) {
		b, l := im.busy, im.idle
		busyName, idleName := seatName(c, b.Handle), seatName(c, l.Handle)
		keys := b.NotStartedKeys[:min((b.Open-l.Open)/2, len(b.NotStartedKeys))]
		fix := Fix{Kind: FixOpenPage, Page: PageSeat, Target: b.Handle, Options: []Option{},
			Sentence: fmt.Sprintf("Open the seat and hand some of its work to %s, who does the same job.", idleName)}
		if len(keys) > 0 {
			fix.Sentence = fmt.Sprintf("Move %s not yet started (%s) to %s, who does the same job.",
				countSaid(len(keys), "item", "items"), strings.Join(keys, ", "), idleName)
			fix.Unchanged = "Work already started stays where it is."
			fix.Reassign = &Reassign{Items: slices.Clone(keys), To: l.Handle}
		}
		by := map[string]int64{}
		for _, h := range im.twins {
			by[h] = 0
		}
		for _, h := range ev.Work.Open.Holders {
			if _, twin := by[h.Handle]; twin {
				by[h.Handle] = int64(h.Open)
			}
		}
		f := stallFinding(ClassWorkImbalance, AreaWork, Subject{Kind: SubjectSeat, Seat: b.Handle, Name: busyName},
			fmt.Sprintf("%d open items against %s's %d", b.Open, idleName, l.Open),
			[]Fact{{Key: "open", Value: int64(b.Open)}, {Key: "twin_open", Value: int64(l.Open)},
				{Key: "not_started", Value: int64(b.NotStarted)}, {Key: "seats", Value: int64(len(im.twins))}},
			recordsOf(RecordItem, b.Keys), fix)
		f.Headline = fmt.Sprintf("%s holds %d open items while %s, on the same job, holds %d.", busyName, b.Open,
			idleName, l.Open)
		f.Seats = seatShares(c, by, UnitItems)
		out = append(out, f)
	}
	return out
}

// blockedNoCause is one finding for every open item blocked [minUnheldAge] or
// more on what nothing anyone holds will clear, the seats holding them inside.
func blockedNoCause(c *config.Company, ev Evidence) []Finding {
	var items []worklet.UnheldWait
	for _, u := range ev.Work.Open.Unheld {
		if ev.Until.Sub(u.Since) >= minUnheldAge {
			items = append(items, u)
		}
	}
	if len(items) == 0 {
		return nil
	}
	by := map[string]int64{}
	keys := make([]string, 0, len(items))
	said := make([]string, 0, len(items))
	oldest := items[0].Since
	for _, u := range items {
		holder := "nobody"
		if u.Holder != "" {
			by[u.Holder]++
			holder = seatName(c, u.Holder)
		}
		if u.Since.Before(oldest) {
			oldest = u.Since
		}
		keys = append(keys, u.Key)
		said = append(said, fmt.Sprintf("%s (held by %s, waits on %s)", u.Key, holder, strings.Join(u.On, ", ")))
	}
	hours := int64(ev.Until.Sub(oldest) / time.Hour)
	f := stallFinding(ClassBlockedNoCause, AreaWork, Subject{Kind: SubjectCompany}, listed(keys, 3),
		[]Fact{{Key: "blocked", Value: int64(len(items))}, {Key: "unchanged_hours", Value: hours}},
		recordsOf(RecordItem, keys),
		Fix{Kind: FixAskCrewlet, Options: []Option{},
			Sentence:  "Find out what each one waits on, then remove the wait, give what it waits on to a seat, or ask its holder.",
			Unchanged: "Nothing changes until you approve what Ask Crewlet drafts.",
			Prompt: fmt.Sprintf("These open items have been blocked for a day or more on something nobody holds "+
				"(an item with no assignee, an item removed, or a change nothing is moving): %s. For each one, read "+
				"the item and what it waits on, tell me why it is blocked, and draft the change that unblocks it: "+
				"remove the wait, assign what it waits on, or ask its holder what it needs. Show me the changes "+
				"before making them.", strings.Join(said, "; "))})
	f.Headline = fmt.Sprintf("%s blocked on nothing anyone holds, the oldest unchanged for %s.",
		countSaid(len(items), "item is", "items are"), countSaid(int(hours/24), "day", "days"))
	f.Seats = seatShares(c, by, UnitItems)
	return []Finding{f}
}
