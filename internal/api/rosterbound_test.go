package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/livestate"
)

// THE ROSTER READ IS BOUNDED BY ITS SEATS, NOT BY WHAT THEY ARE DOING.
//
// `GET /agents` carried each seat's call in flight (its prompt, its rounds and
// its tool executions), so its size grew with the company's busiest turn: on a
// live company one seat's call was 8.8 KB of a 9.7 KB row, and the assistant,
// which reads one step under 64 KiB, was refused the roster exactly while the
// company was working. The call has its own question (`live_call{agent_id}`),
// and the handshake snapshot, which a live view opens from, still carries it.
//
// Mutation: merge the call onto the roster's rows again (Roster through
// MergeAgents) and the `GET /agents` row carries the call and its prompt.
func TestTheRosterReadCarriesNoCallInFlight(t *testing.T) {
	t.Parallel()
	a := rosterApp(t, &fakeRuntime{state: api.RuntimeState{Seats: []string{"cto"}}})
	seat := rosterSeatID(t, "cto").String()
	prompt := strings.Repeat("Fix the flaky payments test. ", 20_000/29)
	call := map[string]any{"agent_id": seat, "turn_id": "tn-1", "phase": "execute", "iteration": 0}
	a.Stream().Ingest(livestate.Envelope{ID: "e1", Type: "agent_phase_started",
		Timestamp: "2026-11-01T09:00:00Z", Category: "system", Payload: call})
	progress := map[string]any{"round_num": 0, "response": prompt, "system_prompt": prompt}
	for k, v := range call {
		progress[k] = v
	}
	a.Stream().Ingest(livestate.Envelope{ID: "e2", Type: "agent_turn_progress",
		Timestamp: "2026-11-01T09:00:01Z", Payload: progress})

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/agents", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents = %d: %s", rec.Code, rec.Body)
	}
	var roster []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &roster); err != nil {
		t.Fatalf("decode the roster: %v", err)
	}
	var row map[string]any
	for _, r := range roster {
		if r["handle"] == "cto" {
			row = r
		}
	}
	if row == nil {
		t.Fatalf("the roster %v has no row for the working seat", roster)
	}
	for _, key := range []string{"live_call", "live_call_seq"} {
		if _, present := row[key]; present {
			t.Errorf("the roster row carries %s: a read of every seat grows with each "+
				"one's call in flight", key)
		}
	}
	if row["activity"] != "working" {
		t.Errorf("the working seat's row reads activity %v, want working: the status "+
			"left with the call", row["activity"])
	}
	if strings.Contains(rec.Body.String(), "Fix the flaky payments test.") {
		t.Errorf("the roster read carries the call's prompt (%d bytes)", rec.Body.Len())
	}

	snapshot := rows(t, a.Stream().Snapshot(everyRead)["agents"])
	for _, r := range snapshot {
		if r["handle"] == "cto" && r["live_call"] == nil {
			t.Error("the handshake snapshot lost the call a live view opens from")
		}
	}
}
