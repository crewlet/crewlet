package modelpass

import (
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/advisor"
)

// A RUN'S RECORD KEEPS A SUMMARY THAT FITS BY CONSTRUCTION (ADR-0166). The log
// keeps a run's model summary within [advisordomain.MaxModelBytes], and a
// deep run's own record (every progress line, its whole agenda with each
// verdict) grows with the run: kept whole, a long investigation's record was
// refused, the run never concluded, and its request ran again from the start.
// The record keeps the run's numbers, its last few lines, and its agenda as a
// tally, each part bounded; every line and every verdict stays on the run's
// progress, where they were written as it ran.

// The bounds of a kept summary: its last lines, and each word it keeps.
const (
	keptLines      = 3
	keptLineBytes  = 512
	keptReasonByte = 480
	keptWordBytes  = 64
	keptUnread     = 16
)

// AgendaTally is an agenda as a run's record keeps it: how many items, and how
// many came to each verdict.
type AgendaTally struct {
	Items      int `json:"items"`
	Confirmed  int `json:"confirmed"`
	Refuted    int `json:"refuted"`
	Unreadable int `json:"unreadable"`
	Open       int `json:"open"`
}

// tallyOf is items as a tally.
func tallyOf(items []AgendaItem) AgendaTally {
	out := AgendaTally{Items: len(items)}
	for _, it := range items {
		switch it.Verdict {
		case VerdictConfirmed:
			out.Confirmed++
		case VerdictRefuted:
			out.Refuted++
		case VerdictUnreadable:
			out.Unreadable++
		default:
			out.Open++
		}
	}
	return out
}

// Kept is r as a run's record keeps it: its status, numbers, bounds and stop,
// its reason and words cut to their bounds, its last [keptLines] lines and
// its agenda as a tally. Every part is bounded, so it fits the record
// whatever the run did.
func (r DeepRun) Kept() DeepRun {
	out := DeepRun{Pass: r.Pass, StoppedBy: cut(r.StoppedBy, keptWordBytes), Minutes: r.Minutes, Bounds: r.Bounds,
		Progress: []Progress{}, Resumed: r.Resumed}
	out.Findings, out.Explained = nil, nil
	out.Model, out.Reason = cut(r.Model, keptWordBytes), cut(r.Reason, keptReasonByte)
	out.Unread = []string{}
	for _, u := range r.Unread[:min(len(r.Unread), keptUnread)] {
		out.Unread = append(out.Unread, cut(u, keptWordBytes))
	}
	// A DROP IS ONE OF THE ENGINE'S OWN REASONS, so the map is as small as
	// their list; a key it does not know is not kept.
	out.Dropped = map[advisor.Drop]int{}
	for drop, n := range r.Dropped {
		if knownDrop(drop) {
			out.Dropped[drop] = n
		}
	}
	// A REPAIR IS ONE OF THE ENGINE'S OWN, so the map is as small as their list.
	out.Repaired = map[advisor.Repair]int{}
	for repair, n := range r.Repaired {
		if slices.Contains(advisor.Repairs(), repair) {
			out.Repaired[repair] = n
		}
	}
	for _, p := range r.Progress[max(0, len(r.Progress)-keptLines):] {
		out.Progress = append(out.Progress, Progress{At: p.At, Text: cut(p.Text, keptLineBytes)})
	}
	tally := tallyOf(r.Agenda)
	out.Tally = &tally
	return out
}

// knownDrop reports one of the engine's own reasons a finding is dropped for.
func knownDrop(d advisor.Drop) bool {
	return slices.Contains([]advisor.Drop{advisor.DropNoEvidence, advisor.DropUnread, advisor.DropWrongFact,
		advisor.DropRepeat, advisor.DropFix, advisor.DropWords, advisor.DropAnswered, advisor.DropTokensUnsplit,
		advisor.DropExplanation, advisor.DropTooMany}, d)
}

// cut is s at most n bytes, whole runes.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
