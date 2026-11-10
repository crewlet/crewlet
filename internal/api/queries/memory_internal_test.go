package queries

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// runsEvents is an event log answering its pages in turn, the first to a read
// with no cursor, each saying more follow but the last; whole or not.
type runsEvents struct {
	FleetEvents
	pages    [][]store.EventRecord
	complete bool
	asked    *store.ListQuery
	reads    *int
}

func (r runsEvents) List(_ context.Context, q store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	if q.Before == nil {
		*r.asked = q
		*r.reads = 0
	}
	i := min(*r.reads, len(r.pages)-1)
	*r.reads++
	more := i < len(r.pages)-1
	return eventfan.Listing{Rows: r.pages[i], More: more}, eventfan.Coverage{Complete: r.complete}, nil
}

// THE ADVISOR READS THE RUNS STOPPED FOR MEMORY over its whole week, a page at a
// time, failed collected runs only, by the chart's handle: another failure is
// not one, a seat the chart no longer holds is left out, and a week past the
// bound or a fleet that did not answer whole is unread rather than read as whole.
func TestTheAdvisorReadsTheRunsStoppedForMemory(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte("name: Acme\nproviders:\n  llm:\n    p: {model: m}\n  sandbox: {fake: true}\n" +
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: p\n"))
	if err != nil {
		t.Fatal(err)
	}
	roster, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	ceo, _ := roster.AgentIDFor(roster.AgentSeatByHandle("ceo"))
	run := func(agent, kind string, bytes int64) store.EventRecord {
		payload, _ := json.Marshal(types.ExecutorCollected{Failed: true, ErrorKind: kind, MemoryBytes: bytes})
		return store.EventRecord{AgentID: agent, Payload: payload, Failed: true}
	}
	var asked store.ListQuery
	var reads int
	// THE OUT-OF-MEMORY RUN ON THE SECOND PAGE, past a first page of other failures.
	events := runsEvents{asked: &asked, reads: &reads, complete: true, pages: [][]store.EventRecord{
		{run(ceo.String(), types.ExecutorErrorLaunchFailed, 0), run("a-gone", types.ExecutorErrorOutOfMemory, 2<<30)},
		{run(ceo.String(), types.ExecutorErrorOutOfMemory, 2<<30)},
	}}
	s := Sources{Company: func() (*config.Company, *org.Organization) { return cfg, roster }, Events: events}
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	got, err := s.memoryKills(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []advisor.MemoryKill{{Seat: "ceo", MemoryBytes: 2 << 30}}; !reflect.DeepEqual(got, want) {
		t.Errorf("runs stopped for memory = %+v, want %+v", got, want)
	}
	if asked.Type != "executor_collected" || asked.Failed == nil || !*asked.Failed ||
		!asked.Since.Equal(now.Add(-advisor.Window)) || !asked.Until.Equal(now) {
		t.Errorf("asked %+v, want the week's failed collected runs", asked)
	}
	if reads != 2 {
		t.Errorf("read %d pages, want both pages of the week", reads)
	}
	endless := events
	endless.pages = make([][]store.EventRecord, maxFailedRunPages+1)
	for i := range endless.pages {
		endless.pages[i] = []store.EventRecord{run(ceo.String(), types.ExecutorErrorLaunchFailed, 0)}
	}
	s.Events = endless
	if _, err := s.memoryKills(t.Context(), now); err == nil {
		t.Error("a week past the bound was read as whole")
	}
	events.complete = false
	s.Events = events
	if _, err := s.memoryKills(t.Context(), now); err == nil {
		t.Error("a fleet that did not answer whole was read as its runs")
	}
}
