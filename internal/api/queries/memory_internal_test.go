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

// runsEvents is an event log answering one listing, whole or not.
type runsEvents struct {
	FleetEvents
	rows     []store.EventRecord
	complete bool
	asked    *store.ListQuery
}

func (r runsEvents) List(_ context.Context, q store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	*r.asked = q
	return eventfan.Listing{Rows: r.rows}, eventfan.Coverage{Complete: r.complete}, nil
}

// THE ADVISOR READS THE RUNS STOPPED FOR MEMORY over its week, failed collected
// runs only, by the chart's handle: another failure is not one, a seat the
// chart no longer holds is left out, and a fleet that did not answer whole is
// unread rather than read as no runs.
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
	events := runsEvents{asked: &asked, complete: true, rows: []store.EventRecord{
		run(ceo.String(), types.ExecutorErrorOutOfMemory, 2<<30),
		run(ceo.String(), types.ExecutorErrorLaunchFailed, 0),
		run("a-gone", types.ExecutorErrorOutOfMemory, 2<<30),
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
	events.complete = false
	s.Events = events
	if _, err := s.memoryKills(t.Context(), now); err == nil {
		t.Error("a fleet that did not answer whole was read as its runs")
	}
}
