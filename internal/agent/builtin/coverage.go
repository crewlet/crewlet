package builtin

import (
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// WHAT A GATHERED ANSWER COULD NOT REACH, as a model reads it.
//
// A list read across partitions is answered partition by partition, and one
// that did not answer makes the list SHORTER — which reads exactly like a
// company with less in it. A seat told "these are your three open tasks" acts
// on three, and files the fourth again. So every tool that renders such a list
// puts the one sentence the coverage renders as ([statelog.Coverage.Notice])
// beside it, under one key, and never leaves a model to infer a missing
// partition from a shorter list.
//
// THE COVERAGE ITSELF IS NOT RENDERED: which partitions answered and the cut
// each was read at are a reader's bookkeeping a model can do nothing with —
// and under a partitioned estate they are a position per log of hundreds,
// spent on every call out of the turn's context.

// unansweredKey is the key a tool result carries the notice under.
const unansweredKey = "unanswered"

// noteUnanswered puts cov's notice in a result rendered as a map, where any
// partition is missing.
func noteUnanswered(result map[string]any, cov statelog.Coverage) {
	if notice := cov.Notice(); notice != "" {
		result[unansweredKey] = notice
	}
}

// The answers a tool renders WHOLE, as a model reads them: the answer's own
// fields, its coverage shadowed by a field that is always absent (encoding/json
// takes the shallower of two fields of one name), and the notice beside them.

type myWorkView struct {
	tracker.MyWork
	Coverage   *struct{} `json:"coverage,omitempty"`
	Unanswered string    `json:"unanswered,omitempty"`
}

type activityView struct {
	tracker.ActivityAnswer
	Coverage   *struct{} `json:"coverage,omitempty"`
	Unanswered string    `json:"unanswered,omitempty"`
}

type inboxView struct {
	tracker.InboxAnswer
	Coverage   *struct{} `json:"coverage,omitempty"`
	Unanswered string    `json:"unanswered,omitempty"`
}

type projectsView struct {
	tracker.ProjectListing
	Coverage   *struct{} `json:"coverage,omitempty"`
	Unanswered string    `json:"unanswered,omitempty"`
}
