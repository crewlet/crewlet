package harness

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// recordEvents keeps the lines of a harness's record that a limit or sign-in
// event looked for there could be ([harnessprofile.EventsInRecord]): the last
// [maxFailureEvents] that match one, which say how the run ended. Fed by the
// record's follower, a goroutine of its own; read once that has stopped.
type recordEvents struct {
	p harnessprofile.Profile

	mu    sync.Mutex
	lines []string
}

// line keeps one line of the record where it is a report a record event
// matches. Every other line, the bulk of a session's record, is dropped.
func (r *recordEvents) line(s string) {
	s = strings.TrimSpace(s)
	doc, ok := decodeObject(s)
	if !ok || !r.reports(doc) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, s)
	if len(r.lines) > maxFailureEvents {
		r.lines = r.lines[1:]
	}
}

func (r *recordEvents) reports(doc map[string]any) bool {
	return slices.ContainsFunc(r.p.LimitEvents, func(e harnessprofile.LimitEvent) bool {
		return e.In == harnessprofile.EventsInRecord && matches(doc, e.Match)
	}) || slices.ContainsFunc(r.p.AuthEvents, func(e harnessprofile.AuthEvent) bool {
		return e.In == harnessprofile.EventsInRecord && matches(doc, e.Match)
	})
}

// text is the reports kept, one a line, in the order the record wrote them.
func (r *recordEvents) text() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// recordPattern is the glob a run's record file is found by: the profile's
// file, rooted in the run's home or working directory, with the run's session
// substituted.
func recordPattern(r *harnessprofile.Requests, checkout *Checkout, session string) string {
	root := checkout.Work
	if r.In == harnessprofile.SeedHome {
		root = checkout.Home
	}
	return filepath.Join(root, strings.ReplaceAll(r.File, "{session}", session))
}

// followRecordEvents follows a text function's record for the reports a
// profile reads there ([harnessprofile.EventsInRecord]); a run with tools
// follows its record already, and feeds the same reports from it ([toolRun]).
// end stops the follower, reads the record to its end and returns what it
// kept; a no-op where the profile reads no report from its record.
func followRecordEvents(p harnessprofile.Profile, checkout *Checkout, session string) (end func() string) {
	if !p.ReadsRecordEvents() {
		return func() string { return "" }
	}
	reports := &recordEvents{p: p}
	stop := make(chan struct{})
	wait := tailRecord(recordPattern(p.Requests, checkout, session),
		recordOwner(p.Requests.Owner, session, checkout.Work), reports.line, stop)
	return func() string {
		close(stop)
		wait()
		return reports.text()
	}
}
