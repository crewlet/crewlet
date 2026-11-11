package prefetch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE SEAT'S OWN OPEN QUESTIONS TO THE PEOPLE IT IS TALKING WITH. A person who
// says "consider anything you have open with me" is talking to a seat that,
// without this block, cannot see what it asked them: its questions live on
// work items, and nothing in a chat turn or an item comment names them. So a
// turn woken by a person lists the seat's open asks to that person, with the
// call that withdraws each, and one that other work now covers is withdrawn
// rather than left waiting in their Inbox.

// MaxOpenAsksListed bounds the asks the block lists across every person in
// the turn, the rest counted per person: [worklet.MaxOwnAsks], since each line
// is one the seat acts on with its own tools, and the block stays a fraction
// of the thread block beside it.
const MaxOpenAsksListed = worklet.MaxOwnAsks

// Asks reads a seat's open asks to one person, newest first, at most limit.
type Asks interface {
	OwnAsks(ctx context.Context, asker, person string, limit int) (worklet.OwnAsks, error)
}

// OpenAsksLead opens the block: what the list is, and what to do with each.
var OpenAsksLead = "These questions you asked are still open and wait on the people in this turn. " +
	"Check each against what you are told now. When the work moved on, or other work now " +
	"covers one (a pull request that runs what you asked them to run, another work item), " +
	"withdraw it in the same step with `" + toolname.WithdrawAsk + "`: say why in `reason`, " +
	"which the person reads in their Inbox, and name what covers it in `replaced_by`. To change " +
	"a question's wording or options, withdraw it and ask again."

// openAsks renders the seat's open asks to each person who triggered the turn,
// or "" when it asked them nothing, no person triggered it, or the asks could
// not be read (logged).
func (f *Fetcher) openAsks(ctx context.Context, r Request) string {
	if f.src.Asks == nil || r.Seat == nil {
		return ""
	}
	asker := r.Seat.Handle()
	persons := personsOf(r)
	if asker == "" || len(persons) == 0 {
		return ""
	}
	now := f.now().UTC()
	left := MaxOpenAsksListed
	var sections []string
	for _, person := range persons {
		page, err := f.src.Asks.OwnAsks(ctx, asker, person.Handle, max(left, 1))
		if err != nil {
			log.WarnContext(ctx, "prefetch_open_asks_unread", "seat", asker, "person", person.Handle,
				"error", err.Error())
			continue
		}
		if page.Total.Total == 0 {
			continue
		}
		shown := page.Asks[:min(left, len(page.Asks))]
		left -= len(shown)
		sections = append(sections, renderOwnAsks(r.Boundary, asker, person, shown, page.Total, now))
	}
	if len(sections) == 0 {
		return ""
	}
	return provenance.Neutralise(r.Boundary, OpenAsksLead) + "\n\n" + strings.Join(sections, "\n\n")
}

// personsOf is the people among the turn's senders, each once, in the order
// they spoke: a sender the roster holds as a human seat.
func personsOf(r Request) []learning.Subject {
	if r.Org == nil {
		return nil
	}
	var out []learning.Subject
	seen := map[string]bool{}
	for _, s := range r.Senders {
		if s.Handle == "" || seen[s.Handle] {
			continue
		}
		if role := r.Org.Role(s.Handle); role == nil || !role.IsHuman() {
			continue
		}
		seen[s.Handle] = true
		out = append(out, s)
	}
	return out
}

// renderOwnAsks is one person's section: a heading line, a line per ask
// shown, and the count of the rest with the read that lists them.
//
// NEUTRALISED under fence, the turn's token (ADR-0040): an item's title and
// the person's name are somebody else's words, and the question is a stored
// field, so nothing here may open or close one of the turn's fences.
func renderOwnAsks(fence provenance.Boundary, asker string, person learning.Subject, asks []worklet.OwnAskRow,
	total worklet.ClaimTotal, now time.Time) string {

	who := firstNonEmpty(person.Label(), person.Handle)
	count := fmt.Sprintf("%d open", total.Total)
	if total.Capped {
		count = fmt.Sprintf("at least %d open", total.Total)
	}
	lines := []string{"To " + who + ", " + count + ":"}
	for _, a := range asks {
		lines = append(lines, ownAskLine(a, now))
	}
	if rest := total.Total - len(asks); rest > 0 {
		lines = append(lines, fmt.Sprintf("- and %d more: list them with %s(asked_by: %q, "+
			"asked_of: %q)", rest, worklet.ListWorkItemsTool, asker, person.Handle))
	}
	return provenance.Neutralise(fence, strings.Join(lines, "\n"))
}

// ownAskLine is one ask: the item, the question, its age, and the call that
// withdraws it. An ask with no decision is read with get_work_item rather
// than cut to fit.
func ownAskLine(a worklet.OwnAskRow, now time.Time) string {
	question := fmt.Sprintf("a question without a decision; read it with %s(item: %q, "+
		"comment: %q)", worklet.GetWorkItemTool, a.Address(), a.Comment)
	if q := strings.TrimSpace(a.Question); q != "" {
		question = fmt.Sprintf("%q", q)
	}
	return fmt.Sprintf("- %s %q: %s, asked %s (%s). Withdraw with %s", worklet.ItemNamed(a.ID, a.Key,
		a.KeyCollision), a.Title, question, a.AskedAt.UTC().Format(time.DateOnly), ageOf(a.AskedAt, now),
		a.Withdraw)
}

// ageOf is how long ago at was, in the largest whole unit a reader needs:
// minutes under an hour, hours under two days, days beyond.
func ageOf(at, now time.Time) string {
	d := now.Sub(at)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	}
	return plural(int(d/(24*time.Hour)), "day") + " ago"
}

// plural is n and its unit, with an s past one.
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
