package catalog

import (
	"strconv"
	"strings"
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

// lastDays is a window of days back from now, "last 7 days", "" for none.
func lastDays(a Args, arg string, fallback int) string {
	n, ok := a.Int(arg)
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

// quoted is a search's words as its scope says them.
func quoted(a Args, arg string) string {
	if q := strings.TrimSpace(a.String(arg)); q != "" {
		return "“" + q + "”"
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
	var statuses []string
	for _, s := range a.Strings("status") {
		statuses = append(statuses, strings.ReplaceAll(s, "_", " "))
	}
	closed := ""
	if a.Bool("show_closed") {
		closed = "including finished"
	}
	return scopeOf(a.String("priority"), strings.Join(statuses, " or "),
		dueScope[a.String("due")], labelled(a, "assignee", "assigned to"),
		labelled(a, "project", "in"), labelled(a, "type", "type"),
		labelled(a, "tag", "tagged"), quoted(a, "q"), closed)
}
