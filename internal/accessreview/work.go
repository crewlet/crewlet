package accessreview

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/toolname"
)

// SectionTitle is the heading a turn is told its open work under.
const SectionTitle = "Your open work raised under your earlier access"

// MaxListed is the most items of each kind a section names; the rest are
// counted. Ten: a seat reads each one it is shown with its own tools, and a
// review past thirty reads is one the seat is better told to page through
// with them than handed whole.
const MaxListed = 10

// Work is one seat's open work a review covers: what may assume access the
// seat no longer has, or work around access it now has.
//
// KEYS AND IDS, NEVER TITLES. A title is words somebody else wrote, which a
// prompt may hand a seat only fenced (ADR-0040); the seat reads each item
// with its own tools, which fence it, so the section names what to read and
// nothing anybody else wrote.
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

// Count is how many open items the work holds, the counted rest included.
func (w Work) Count() int {
	return len(w.Asks) + w.MoreAsks + len(w.Runs) + w.MoreRuns + len(w.Items) + w.MoreItems
}

// Empty reports work holding nothing to review.
func (w Work) Empty() bool { return w.Count() == 0 }

// Section is the open work as a turn that is told a change reads it, under
// [SectionTitle], with what to do about each item. Empty for no work.
//
// ONE SECTION FOR EVERY TURN THAT TELLS A CHANGE, the review's and any other:
// whichever turn tells the change first is the one that asks for the review,
// since its told record keeps every later turn from being told it again.
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

// UpdatedPrefix opens what a seat says, in its own voice, about an item it
// updated or withdrew after a change to its access: the person who reads the
// item reads why it moved.
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
