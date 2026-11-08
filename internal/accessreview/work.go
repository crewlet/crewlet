package accessreview

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/toolname"
)

// SectionTitle is the heading a turn is told its open work under.
const SectionTitle = "Your open work raised under your earlier access"

// MaxListed is the most items of each kind a section names; the rest are
// counted. Ten: a seat reads each one with its own tools.
const MaxListed = 10

// Work is one seat's open work a review covers, named by key and run id and
// never by anybody's words, which a prompt hands a seat only fenced (ADR-0040).
type Work struct {
	// Asks are the work items carrying a question the seat asked that is
	// still open, by key, and MoreAsks how many more there are.
	Asks     []string
	MoreAsks int

	// Runs are the seat's executor runs waiting on a person: a question,
	// or a call held for approval. MoreRuns counts the rest.
	Runs     []Run
	MoreRuns int

	// Items are the work items assigned to the seat that are in progress
	// or blocked, and MoreItems how many more there are.
	Items     []Item
	MoreItems int
}

// Run is one executor run of the seat waiting on a person.
type Run struct {
	// Turn is the run's turn, which `withdraw_ask` names it by.
	Turn string
	// Tool and Server name a call held for approval, both empty for a
	// question.
	Tool, Server string
	// Task is the work item the run is on, by key, where it is on one.
	Task string
}

// Item is one work item assigned to the seat.
type Item struct {
	Key string
	// Status is its status as the tracker names it (`in_progress`).
	Status string
	// Blocked is whether an open dependency holds it.
	Blocked bool
}

// Count is how many distinct open items the work holds: a work item counts
// once whether it carries a question, is assigned or has a run waiting on it,
// and the counted rest is added.
func (w Work) Count() int {
	seen := map[string]bool{}
	n := 0
	add := func(key string) {
		if key == "" {
			n++
			return
		}
		if !seen[key] {
			seen[key] = true
			n++
		}
	}
	for _, key := range w.Asks {
		add(key)
	}
	for _, it := range w.Items {
		add(it.Key)
	}
	for _, r := range w.Runs {
		add(r.Task)
	}
	return n + w.MoreAsks + w.MoreItems + w.MoreRuns
}

// Empty reports work holding nothing to review.
func (w Work) Empty() bool { return w.Count() == 0 }

// Section is the open work under [SectionTitle], with what to do about each
// item, for every turn that tells a change; empty for no work.
func (w Work) Section() string {
	if w.Empty() {
		return ""
	}
	lines := []string{"### " + SectionTitle,
		"Each of these was raised or started before the change above, and may assume " +
			"access you no longer have, or work around access you now have."}
	if len(w.Asks) > 0 || w.MoreAsks > 0 {
		lines = append(lines, "- Work items carrying a question you asked that is still open: "+
			listed(w.Asks, w.MoreAsks)+".")
	}
	if len(w.Runs) > 0 || w.MoreRuns > 0 {
		lines = append(lines, "- What your executor waits on a person for:")
		for _, r := range w.Runs {
			lines = append(lines, "  - "+r.line())
		}
		if w.MoreRuns > 0 {
			lines = append(lines, fmt.Sprintf("  - and %d more", w.MoreRuns))
		}
	}
	if len(w.Items) > 0 || w.MoreItems > 0 {
		items := make([]string, 0, len(w.Items))
		for _, it := range w.Items {
			items = append(items, it.Key+" ("+it.state()+")")
		}
		lines = append(lines, "- Work items assigned to you: "+listed(items, w.MoreItems)+".")
	}
	lines = append(lines, Instructions)
	return strings.Join(lines, "\n")
}

// Instructions is what a seat does with each item of its open work.
var Instructions = "For each one the change affects, update it or withdraw it, and say why " +
	"where the person reads it, starting with \"" + UpdatedPrefix + "\". A question's " +
	"options never change once it is asked: to re-word a question or change its options, " +
	"withdraw it with `" + toolname.WithdrawAsk + "` and ask again. Withdraw a question or " +
	"a held call your executor waits on with `" + toolname.WithdrawAsk + "` and its `run`. " +
	"Leave everything the change does not affect as it is."

// UpdatedPrefix opens what a seat says about an item it updated or withdrew
// after a change to its access.
const UpdatedPrefix = "Updated after my access changed: "

// line names one waiting run.
func (r Run) line() string {
	what := "a question to a person"
	if r.Tool != "" {
		what = "a call to `" + r.Tool + "`"
		if r.Server != "" {
			what += " on `" + r.Server + "`"
		}
		what += ", held for a person's approval"
	}
	where := "run `" + r.Turn + "`"
	if r.Task != "" {
		where += ", on " + r.Task
	}
	return what + " (" + where + ")"
}

// state is an item's status as a person reads it.
func (it Item) state() string {
	status := strings.ReplaceAll(it.Status, "_", " ")
	switch {
	case it.Blocked && status != "":
		return status + ", blocked"
	case it.Blocked:
		return "blocked"
	case status == "":
		return "open"
	}
	return status
}

// listed is names joined, with the counted rest.
func listed(names []string, more int) string {
	out := strings.Join(names, ", ")
	if more > 0 {
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf("and %d more", more)
	}
	return out
}
