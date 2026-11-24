package queries_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
)

// SERVING NOW IS THE WORK IN FLIGHT ALONE: a member serves the subjects whose
// calls run on it (a node's running work) and the seats whose executor run
// is launching or running on it (the live executors), and nobody else: an
// idle seat's next pick, and a run paused on a question, serve nothing. A job
// in flight says it runs; an idle one says where its next piece would go.
func TestServingNowIsTheWorkInFlightAlone(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	since := pinned.Add(-3 * time.Minute)
	presentNode(t, backend, "node-a", &coord.NodeStatusRecord{Models: []coord.ModelStatus{
		{Key: "max", SignIn: "token", Resolves: true,
			Running: []coord.FleetRunning{{Fleet: "opus", Subject: "engineer", Job: "review", Since: since}}},
		{Key: "team2", SignIn: "token", Resolves: true,
			Running: []coord.FleetRunning{{Fleet: "opus", Subject: "assistant", Job: "assistant", Since: since}}},
	}})
	state := livestate.New()
	state.ReconcileExecutors([]livestate.ExecutorRecord{
		{LaunchID: "run-1", WrittenAt: pinned, Entry: livestate.ExecutorEntry{TurnID: "t-1", AgentHandle: "writer",
			AgentID: "a-writer", ProviderKey: "a", Status: livestate.ExecutorRunning,
			StartedAt: since.Format(time.RFC3339Nano)}},
		{LaunchID: "run-2", WrittenAt: pinned, Entry: livestate.ExecutorEntry{TurnID: "t-2", AgentHandle: "night",
			AgentID: "a-night", ProviderKey: "b", Status: livestate.ExecutorAwaiting,
			StartedAt: since.Format(time.RFC3339Nano)}},
	}, pinned)
	s := llmFleetSources(t, backend, nil)
	s.State = state
	got := llmFleetsOf(t, s)

	opus := llmFleetNamed(t, got, "opus")
	serving := map[string][]string{}
	for _, m := range opus.Members {
		serving[m.Entry] = m.Serving
	}
	if !slices.Equal(serving["max"], []string{"engineer"}) || !slices.Equal(serving["team2"], []string{"assistant"}) {
		t.Errorf("opus serving = %v, want engineer on max and Ask Crewlet on team2", serving)
	}
	for entry, who := range serving {
		if entry != "max" && entry != "team2" && len(who) != 0 {
			t.Errorf("%s serves %v with nothing in flight on it", entry, who)
		}
	}
	busy := llmFleetNamed(t, got, "busy")
	if a, b := busy.Members[0], busy.Members[1]; !slices.Equal(a.Serving, []string{"writer"}) || len(b.Serving) != 0 {
		t.Errorf("busy serving a %v b %v, want writer's running executor on a and nobody on b, "+
			"whose run waits on a question", a.Serving, b.Serving)
	}

	engineer := assignmentOf(t, got, "engineer")
	for _, j := range engineer.Jobs {
		switch j.Job {
		case "review":
			if deref(j.Now) != "max" || j.NowKind != queries.NowRunning {
				t.Errorf("engineer's review now %v (%s), want max running", deref(j.Now), j.NowKind)
			}
		default:
			if j.Now != nil && j.NowKind != queries.NowNext {
				t.Errorf("engineer's idle %s now %v (%s), want next", j.Job, deref(j.Now), j.NowKind)
			}
		}
	}
	if a := assignmentOf(t, got, "assistant"); deref(a.Now) != "team2" || a.NowKind != queries.NowRunning {
		t.Errorf("Ask Crewlet now %v (%s), want team2 running", deref(a.Now), a.NowKind)
	}
	if w := assignmentOf(t, got, "writer"); deref(w.Now) != "a" || w.NowKind != queries.NowRunning {
		t.Errorf("writer now %v (%s), want its executor running on a", deref(w.Now), w.NowKind)
	}
}

// IDLE JOBS' NEXT PICKS ACCOUNT FOR EACH OTHER: with every member's share
// unknown and nothing served, each prediction takes its pick as the engine
// would, so the next tie goes to another member rather than every subject
// reading the first.
func TestIdlePredictionsSpreadAcrossTheMembers(t *testing.T) {
	t.Parallel()
	var llm, members, roles strings.Builder
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&llm, "    p%d: {model: claude-opus-5-5}\n", i)
		fmt.Fprintf(&members, "p%d, ", i)
	}
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&roles, "  - {name: Seat %d, handle: seat%d, llm: \"fleet:default-fleet\"}\n", i, i)
	}
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
%s  llm_fleets:
    default-fleet: {name: default-fleet, members: [%s]}
assistant: {model: "fleet:default-fleet"}
advisor: {model: "fleet:default-fleet"}
roles:
%s`, llm.String(), strings.TrimSuffix(members.String(), ", "), roles.String())))
	if err != nil {
		t.Fatal(err)
	}
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatusRecord{})
	got := llmFleetsOf(t, queries.Sources{Coord: backend, NodeStatuses: recordsOf(backend),
		Company: companySource(t, cfg), NodeID: "node-a", Models: fakeModels{}})
	count := map[string]int{}
	for _, a := range got.Assignments {
		if a.Now == nil || a.NowKind != queries.NowNext {
			t.Fatalf("%s now %v (%s), want a next pick", a.Subject, deref(a.Now), a.NowKind)
		}
		count[*a.Now]++
	}
	if len(count) != 5 {
		t.Errorf("11 idle subjects' next picks land on %v, want all five members", count)
	}
	for member, n := range count {
		if n > 3 {
			t.Errorf("%s is the next pick of %d subjects, want at most 3 of 11 over five members", member, n)
		}
	}
	for _, m := range got.Fleets[0].Members {
		if len(m.Serving) != 0 || m.LastServedAt != nil {
			t.Errorf("%s serving %v last served %v: a prediction is neither", m.Entry, m.Serving, deref(m.LastServedAt))
		}
	}
}

// A MEMBER WHOSE SIGN-IN A NODE REPORTS BAD READS SIGNED OUT before any turn
// tries it, as the Models page reads it: nothing signs it in, or its last
// call was refused. One no node has reported yet keeps its chance.
func TestAMemberWhoseSignInIsBadReadsSignedOut(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatusRecord{Models: []coord.ModelStatus{
		{Key: "max", SignIn: "token", Resolves: true},
		{Key: "team2", SignIn: "none"},
		{Key: "team", SignIn: "token", Resolves: false},
	}})
	got := llmFleetsOf(t, llmFleetSources(t, backend, nil))
	want := map[string]string{"max": "available", "team2": "signedout", "team": "signedout", "spare": "available"}
	for _, m := range llmFleetNamed(t, got, "opus").Members {
		w, ok := want[m.Entry]
		if !ok {
			continue
		}
		if m.State != w {
			t.Errorf("%s = %s (%q), want %s", m.Entry, m.State, m.Why, w)
		}
		if w == "signedout" && m.Why != "It is not signed in" {
			t.Errorf("%s why %q, want it is not signed in", m.Entry, m.Why)
		}
	}
}
