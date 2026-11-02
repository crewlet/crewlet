package statelog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
)

// mcpAt is the instant every MCP reading here is evaluated at.
var mcpAt = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// The two spans, named once for the tables below.
const (
	outage = statelog.MCPOutageGrace
	wait   = statelog.MCPWaitingGrace
)

// servers is a reading of MCP servers' conditions at [mcpAt].
func servers(conditions ...statelog.MCPServerCondition) statelog.Reading {
	return statelog.Reading{MCPServers: conditions, MCPServersAt: mcpAt}
}

// mcpKinds is which kinds a reading raises, each with the servers it stands on.
func mcpKinds(r statelog.Reading) map[statelog.Kind][]string {
	out := map[statelog.Kind][]string{}
	for _, a := range statelog.Evaluate(r) {
		for _, e := range a.Entries {
			out[a.Kind] = append(out[a.Kind], e.Key)
		}
	}
	return out
}

// EACH KIND RAISES AFTER ITS SPAN AND CLEARS, judged per server: the outage at
// the restart schedule's ceiling plus a beat, failing where nothing serves and
// partial where something does; the wait at a provisioning pass plus a beat,
// beside either; a refused name at once, since a rule refuses it rather than a
// moment. Each driven just inside its span, at it, and with the condition
// gone. A failure or a wait nothing dated is no span anybody measured.
//
// Mutation: compare with > where the table says >=, raise failing on a server
// something still serves, judge a failure or a wait nothing dated, or never
// raise a refused name, and a row here fails.
func TestEachMCPKindRaisesAfterItsSpanAndClears(t *testing.T) {
	t.Parallel()
	down := func(started, failed int, ago time.Duration) statelog.MCPServerCondition {
		return statelog.MCPServerCondition{Server: "github", Started: started, Failed: failed,
			DownSince: mcpAt.Add(-ago)}
	}
	held := func(ago time.Duration) statelog.MCPServerCondition {
		return statelog.MCPServerCondition{Server: "github", Started: 1, Waiting: 1,
			WaitingSince: mcpAt.Add(-ago)}
	}
	for name, tc := range map[string]struct {
		condition statelog.MCPServerCondition
		want      []statelog.Kind
	}{
		"nothing launched":                        {statelog.MCPServerCondition{Server: "github"}, nil},
		"every instance serving":                  {statelog.MCPServerCondition{Server: "github", Started: 3}, nil},
		"nothing serving, a second inside":        {down(0, 2, outage-time.Second), nil},
		"nothing serving, exactly the span":       {down(0, 2, outage), []statelog.Kind{statelog.KindMCPServerFailing}},
		"nothing serving for an hour":             {down(0, 1, time.Hour), []statelog.Kind{statelog.KindMCPServerFailing}},
		"nothing serving, nothing dated":          {statelog.MCPServerCondition{Server: "github", Failed: 1}, nil},
		"some serving, one down a second inside":  {down(2, 1, outage-time.Second), nil},
		"some serving, one down exactly the span": {down(2, 1, outage), []statelog.Kind{statelog.KindMCPServerPartial}},
		"serving again, the outage's date kept":   {down(3, 0, time.Hour), nil},
		"a wait a second inside a pass":           {held(wait - time.Second), nil},
		"a wait exactly a pass":                   {held(wait), []statelog.Kind{statelog.KindMCPServerWaitingLong}},
		"a wait nothing dated":                    {statelog.MCPServerCondition{Server: "github", Waiting: 1}, nil},
		"a wait beside a server nothing serves": {statelog.MCPServerCondition{Server: "github", Failed: 1,
			DownSince: mcpAt.Add(-outage), Waiting: 1, WaitingSince: mcpAt.Add(-wait)},
			[]statelog.Kind{statelog.KindMCPServerFailing, statelog.KindMCPServerWaitingLong}},
		"a refused name, at once": {statelog.MCPServerCondition{Server: "github", Started: 1,
			Refused: []string{"gh_read.file"}, RefusedCount: 1}, []statelog.Kind{statelog.KindMCPToolsRefused}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := mcpKinds(servers(tc.condition))
			if len(got) != len(tc.want) {
				t.Fatalf("raised %v, want %v", got, tc.want)
			}
			for _, kind := range tc.want {
				if on := got[kind]; len(on) != 1 || on[0] != "github" {
					t.Errorf("raised %v, want %s on github alone", got, kind)
				}
			}
		})
	}
}

// THE SPANS ARE THE ONES THE ENGINE ALREADY KEEPS: the restart schedule's
// ceiling, past which every attempt waits the whole ceiling, and the longest an
// integration waits between two passes it still owes, each plus the heartbeat
// that carries the result. Spelled in statelog so the table needs neither
// package, and held to both here, so a retune of the schedule fails until the
// alarm follows. The reference says each.
func TestTheMCPSpansAreTheSchedulesTheyWaitOut(t *testing.T) {
	t.Parallel()
	if want := mcp.RestartCeiling + coord.ReconcileInterval; statelog.MCPOutageGrace != want {
		t.Errorf("MCPOutageGrace = %v, want the restart ceiling plus a heartbeat (%v)", statelog.MCPOutageGrace, want)
	}
	if want := integration.DefaultSchedule.WaitingMax + coord.ReconcileInterval; statelog.MCPWaitingGrace != want {
		t.Errorf("MCPWaitingGrace = %v, want the longest wait between passes plus a heartbeat (%v)",
			statelog.MCPWaitingGrace, want)
	}
	page := statelog.AlarmReference()
	for _, says := range []string{"An instance down for " + statelog.Spoken(statelog.MCPOutageGrace),
		"instance held back " + statelog.Spoken(statelog.MCPWaitingGrace)} {
		if !strings.Contains(strings.Join(strings.Fields(page), " "), says) {
			t.Errorf("the reference does not say %q", says)
		}
	}
}

// AN ENTRY SAYS WHICH INSTANCE AND WHY: the server, how long, the agent and
// node its instance is on, the failure's class and what it said; and names the
// agent as its seat, which a remedy aimed at one agent's instance acts on. A
// refused name lists what the heartbeat names and counts the rest.
//
// Mutation: drop the note, the seat or the count of the rest, and this fails.
func TestAnMCPEntrySaysWhichInstanceAndWhy(t *testing.T) {
	t.Parallel()
	note := statelog.MCPInstanceNote{Node: "node-b", Seat: "backend-dev", Class: "credential_rejected",
		Said: "401 Bad credentials"}
	got := statelog.Evaluate(servers(
		statelog.MCPServerCondition{Server: "github", Started: 2, Failed: 1,
			DownSince: mcpAt.Add(-time.Hour), Down: note},
		statelog.MCPServerCondition{Server: "linear", Started: 1,
			Refused: []string{"lin_a", "lin_b"}, RefusedCount: 5},
	))
	partial, ok := find(got, statelog.KindMCPServerPartial)
	if !ok || len(partial.Entries) != 1 {
		t.Fatalf("raised %v, want one mcp_server_partial", kindsOf(got))
	}
	entry := partial.Entries[0]
	for _, says := range []string{"1 of 3 instances of github is not serving", "1h",
		"(backend-dev's, on node-b, credential_rejected): 401 Bad credentials"} {
		if !strings.Contains(entry.Detail, says) {
			t.Errorf("detail %q does not say %q", entry.Detail, says)
		}
	}
	if entry.Seat != "backend-dev" || !entry.Since.Equal(mcpAt.Add(-time.Hour)) {
		t.Errorf("entry = %+v, want backend-dev's instance, since it went down", entry)
	}
	refused, ok := find(got, statelog.KindMCPToolsRefused)
	if !ok || len(refused.Entries) != 1 {
		t.Fatalf("raised %v, want one mcp_tools_refused", kindsOf(got))
	}
	if want := "5 of linear's tools cannot be offered under their names: `lin_a`, `lin_b` and 3 more"; refused.Entries[0].Detail != want {
		t.Errorf("detail = %q, want %q", refused.Entries[0].Detail, want)
	}
	if !refused.Entries[0].Since.IsZero() || refused.Entries[0].Seat != "" {
		t.Errorf("a refused name carries %+v, want no instant and no seat", refused.Entries[0])
	}
}

// ONE ALARM PER KIND, ITS SERVERS LISTED, each raised and cleared in the log on
// its own, carrying the server as its entry; and a server whose last serving
// instance goes down hands over from partial to failing on that evaluation,
// with no new wait, since its outage had already begun. A reading nobody
// stamped judges nothing.
//
// Mutation: key the tracker's instances on the kind alone, measure failing
// from the last instance's fall, or judge an unstamped reading, and this fails.
func TestAnMCPKindStandsPerServerAndHandsOverWhenTheLastInstanceFalls(t *testing.T) {
	t.Parallel()
	clock := mcpAt
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })
	var logs bytes.Buffer
	statelog.LogTrackerTo(tracker, slog.New(slog.NewJSONHandler(&logs, nil)))
	lines := func(event string) []map[string]any {
		var out []map[string]any
		for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var line map[string]any
			if json.Unmarshal([]byte(raw), &line) == nil && line["msg"] == event {
				out = append(out, line)
			}
		}
		return out
	}
	wentDown := mcpAt.Add(-outage)
	at := func(now time.Time, githubServing int) statelog.Reading {
		return statelog.Reading{MCPServersAt: now, MCPServers: []statelog.MCPServerCondition{
			{Server: "github", Started: githubServing, Failed: 1, DownSince: wentDown},
			{Server: "jira", Started: 1, Failed: 1, DownSince: wentDown.Add(-time.Minute)},
		}}
	}

	got := tracker.Observe(t.Context(), statelog.Evaluate(at(clock, 1)))
	if len(got) != 1 || got[0].Kind != statelog.KindMCPServerPartial || len(got[0].Entries) != 2 ||
		got[0].Entries[0].Key != "github" || got[0].Entries[1].Key != "jira" {
		t.Fatalf("raised %+v, want one mcp_server_partial on github and jira", got)
	}
	if !strings.Contains(got[0].Detail, "jira") {
		t.Errorf("detail = %q, want the longest-standing server's (jira)", got[0].Detail)
	}
	if raised := lines("alarm_raised"); len(raised) != 2 || raised[0]["entry"] == raised[1]["entry"] {
		t.Fatalf("raise lines = %v, want one per server", raised)
	}

	clock = clock.Add(time.Second)
	got = tracker.Observe(t.Context(), statelog.Evaluate(at(clock, 0)))
	if kinds := kindsOf(got); !slices.Equal(kinds, []statelog.Kind{statelog.KindMCPServerFailing,
		statelog.KindMCPServerPartial}) {
		t.Fatalf("raised %v, want github's failing and jira's partial on the same evaluation", kinds)
	}
	cleared := lines("alarm_cleared")
	if len(cleared) != 1 || cleared[0]["alarm"] != string(statelog.KindMCPServerPartial) || cleared[0]["entry"] != "github" {
		t.Errorf("clear lines = %v, want github's partial alone", cleared)
	}
	raised := lines("alarm_raised")
	if len(raised) != 3 || raised[2]["alarm"] != string(statelog.KindMCPServerFailing) || raised[2]["entry"] != "github" {
		t.Errorf("raise lines = %v, want github's failing after the first two", raised)
	}

	// A refused name needs no span, so only the stamp keeps it from being
	// judged here.
	unstamped := append(at(clock, 0).MCPServers, statelog.MCPServerCondition{Server: "linear",
		Started: 1, Refused: []string{"lin_a"}, RefusedCount: 1})
	if got := statelog.Evaluate(statelog.Reading{MCPServers: unstamped}); len(got) != 0 {
		t.Errorf("an unstamped reading raised %v", kindsOf(got))
	}
}
