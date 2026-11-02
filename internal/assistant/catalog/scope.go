package catalog

import (
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// THE SCOPES: what each read's arguments narrowed it to, as the person reads
// it on the step and the card ([Tool.Scope]). Two reads of one tool are told
// apart by these words and nothing else, so every argument that changes which
// rows come back is said here, and nothing the person did not narrow (a page
// size) is.

// scopeOf joins a call's scope parts, leaving out the empty ones.
func scopeOf(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ", ")
}

// lastDays is a call's `days` window back from now, "last 7 days", with
// fallback for a call that names none and "" for no window.
func lastDays(a Args, fallback int) string {
	n, ok := a.Int("days")
	if !ok || n <= 0 {
		n = fallback
	}
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "last day"
	}
	return "last " + strconv.Itoa(n) + " days"
}

// quoted is a search's words, its `q`, as its scope says them.
func quoted(a Args) string {
	if q := strings.TrimSpace(a.String("q")); q != "" {
		return "\u201c" + q + "\u201d"
	}
	return ""
}

// labelled is an argument with the word that says what it is, "" when unset.
func labelled(a Args, arg, word string) string {
	if v := strings.TrimSpace(a.String(arg)); v != "" {
		return word + " " + v
	}
	return ""
}

// dueScope is list_tasks' due filter as a person says it.
var dueScope = map[string]string{
	"overdue":   "overdue",
	"next7":     "due in the next 7 days",
	"thisweek":  "due this week",
	"thismonth": "due this month",
	"last7":     "due in the last 7 days",
	"earlier":   "due earlier",
}

// taskScope is list_tasks' filters, in the order a person says them.
func taskScope(a Args) string {
	var said []words.Said
	for _, s := range a.Strings("status") {
		said = append(said, words.Said(strings.ReplaceAll(s, "_", " ")))
	}
	// NO STATUS NAMED IS NO PART of the scope, never "none": scopeOf leaves
	// an empty part out.
	status := ""
	if len(said) > 0 {
		status = words.Or(said)
	}
	closed := ""
	if a.Bool("show_closed") {
		closed = "including finished"
	}
	return scopeOf(a.String("priority"), status,
		dueScope[a.String("due")], labelled(a, "assignee", "assigned to"),
		labelled(a, "project", "in"), labelled(a, "type", "type"),
		labelled(a, "tag", "tagged"), quoted(a), closed)
}
