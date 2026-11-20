package modelpass

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/advisor"
)

// THE DEEP RUN'S AGENDA (ADR-0166). Before its first round the run is handed
// what it must answer: the snapshot's numbers that moved the wrong way most,
// what every open rule finding says, and every source the rules could not
// read. It concludes only once each item has a verdict (confirmed or refuted
// with numbers it read, or unreadable with the read that failed) or once its
// bounds are spent. It may add items; it never drops one, and the run keeps
// the agenda with its verdicts.

// Verdict is what the run concluded about one agenda item.
type Verdict string

// The verdicts.
const (
	VerdictConfirmed  Verdict = "confirmed"
	VerdictRefuted    Verdict = "refuted"
	VerdictUnreadable Verdict = "unreadable"
	// VerdictOpen is an item the run's bounds ran out on.
	VerdictOpen Verdict = "open"
)

// Where an agenda item came from.
const (
	AgendaStat    = "stat"
	AgendaFinding = "finding"
	AgendaUnread  = "unread"
	AgendaModel   = "model"
)

// AgendaItem is one question the run must answer, and its answer.
type AgendaItem struct {
	ID       string  `json:"id"`
	Source   string  `json:"source"`
	Question string  `json:"question"`
	Verdict  Verdict `json:"verdict"`
	// Note is the verdict in one line.
	Note string `json:"note"`
	// Numbers are the numbers a confirmed or refuted verdict rests on, each
	// at a path into an answer the run read.
	Numbers []AgendaNumber `json:"numbers"`
	// Read is the read that failed, for an unreadable verdict.
	Read string `json:"read,omitempty"`
}

// AgendaNumber is one number a verdict rests on.
type AgendaNumber struct {
	Label string `json:"label"`
	Value int64  `json:"value"`
	From  string `json:"from"`
}

// The agenda's bounds: the stats and rule findings it starts from, the items
// a run may add, and a verdict's note.
const (
	maxAgendaStats    = 4
	maxAgendaFindings = 16
	maxAgendaAdded    = 8
	maxVerdictNote    = 200
	// movedShare is how far a stat must move the wrong way, in percent of
	// the range before, to be worth an item.
	movedShare = 10
)

// agendaOf is a deep run's agenda over d, before its first round.
func agendaOf(d DeepData) []AgendaItem {
	out := []AgendaItem{}
	add := func(source, question string) {
		out = append(out, AgendaItem{ID: fmt.Sprintf("A%d", len(out)+1), Source: source, Question: question,
			Numbers: []AgendaNumber{}})
	}
	type moved struct {
		at  int
		by  float64
		was float64
	}
	var worst []moved
	for i, s := range d.Snapshot.Stats {
		if s.Was == nil || s.Better == nil || *s.Was == 0 {
			continue
		}
		by := (s.Value - *s.Was) / math.Abs(*s.Was) * 100
		if *s.Better == "up" {
			by = -by
		}
		if by >= movedShare {
			worst = append(worst, moved{at: i, by: by, was: *s.Was})
		}
	}
	slices.SortStableFunc(worst, func(a, b moved) int { return cmp.Compare(b.by, a.by) })
	for _, m := range worst[:min(len(worst), maxAgendaStats)] {
		s := d.Snapshot.Stats[m.at]
		add(AgendaStat, fmt.Sprintf("%s went from %s to %s (snapshot.stats[%d]): what drove it, and on which seats?",
			s.Label, numberSaid(m.was), numberSaid(s.Value), m.at))
	}
	findings := 0
	for _, f := range d.Rules {
		if f.Actor != advisor.ActorOwner {
			continue
		}
		if findings++; findings > maxAgendaFindings {
			break
		}
		add(AgendaFinding, fmt.Sprintf("Is it so, and what is behind it (%s): %s", f.Class, advisor.HeadlineOf(f)))
	}
	unread := slices.Clone(d.Unread)
	for _, u := range d.Snapshot.Unread {
		if !slices.Contains(unread, u) {
			unread = append(unread, u)
		}
	}
	for _, u := range unread {
		add(AgendaUnread, fmt.Sprintf("The rules could not read %s: read what it holds another way, or name the "+
			"read that fails.", u))
	}
	return out
}

// numberSaid is a stat's number as a person reads it: whole where it is.
func numberSaid(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// agendaSaid is the agenda as the run is told it.
func agendaSaid(items []AgendaItem) string {
	if len(items) == 0 {
		return "Your agenda is empty: follow what the numbers show."
	}
	var b strings.Builder
	b.WriteString("Your agenda. Give every item a verdict with the verdict tool before submit_findings, in any order:\n")
	for _, it := range items {
		fmt.Fprintf(&b, "- %s [%s] %s", it.ID, it.Source, it.Question)
		if it.Verdict != "" && it.Verdict != VerdictOpen {
			fmt.Fprintf(&b, " (answered before: %s, %s)", it.Verdict, it.Note)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// open is every item of items still without a verdict.
func open(items []AgendaItem) []AgendaItem {
	var out []AgendaItem
	for _, it := range items {
		if it.Verdict == "" {
			out = append(out, it)
		}
	}
	return out
}
