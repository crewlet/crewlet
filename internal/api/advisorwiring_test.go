package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/statelog"
)

// memAdvisor is an advisor log holding at most one finding, which tells when a
// run is recorded and keeps what a person asked of it.
type memAdvisor struct {
	once     sync.Once
	recorded chan struct{}
	held     *advisordomain.FindingRow
	asked    []advisordomain.RunRequest
	moved    []advisordomain.Move
}

func (*memAdvisor) Findings(context.Context, advisordomain.Filter, statelog.Freshness) ([]advisordomain.FindingRow, string, error) {
	return nil, "", nil
}
func (*memAdvisor) NonClosed(context.Context, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return nil, nil
}
func (m *memAdvisor) Finding(_ context.Context, id string, _ statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error) {
	if m.held == nil || m.held.ID != id {
		return advisordomain.FindingRow{}, nil, advisordomain.ErrNotFound
	}
	return *m.held, nil, nil
}
func (*memAdvisor) Episodes(context.Context, string, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return nil, nil
}
func (*memAdvisor) LatestRun(context.Context, statelog.Freshness) (advisordomain.RunRow, bool, error) {
	return advisordomain.RunRow{}, false, nil
}
func (*memAdvisor) PendingRequests(context.Context, statelog.Freshness) ([]advisordomain.RunRow, error) {
	return nil, nil
}
func (*memAdvisor) LastRunOfClasses(context.Context, statelog.Freshness) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}
func (*memAdvisor) Reports(context.Context, string, int, statelog.Freshness) ([]advisordomain.ReportRow, string, error) {
	return nil, "", nil
}
func (*memAdvisor) Report(context.Context, string, statelog.Freshness) (advisordomain.ReportRow, bool, error) {
	return advisordomain.ReportRow{}, false, nil
}
func (*memAdvisor) Tally(context.Context, time.Time, time.Time, statelog.Freshness) (advisordomain.Tally, error) {
	return advisordomain.Tally{}, nil
}
func (*memAdvisor) WriteReport(context.Context, advisordomain.Call, advisordomain.Report) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (*memAdvisor) Key(context.Context, string, statelog.Freshness) (advisordomain.KeyRow, bool, error) {
	return advisordomain.KeyRow{}, false, nil
}
func (*memAdvisor) Open(context.Context, advisordomain.Call, advisordomain.Opening, uint64) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (*memAdvisor) Revise(context.Context, advisordomain.Call, string, advisordomain.Revision, uint64) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (*memAdvisor) Refresh(context.Context, advisordomain.Call, string, advisordomain.Refresh, uint64) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (m *memAdvisor) Move(_ context.Context, _ advisordomain.Call, _ string, mv advisordomain.Move, _ uint64) (statelog.Result, error) {
	m.moved = append(m.moved, mv)
	return statelog.Result{}, nil
}
func (m *memAdvisor) RecordRun(context.Context, advisordomain.Call, advisordomain.RunRecord) (statelog.Result, error) {
	m.once.Do(func() { close(m.recorded) })
	return statelog.Result{}, nil
}
func (*memAdvisor) Expire(context.Context, advisordomain.Call, time.Time, advisordomain.Expiry) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (m *memAdvisor) RequestRun(_ context.Context, _ advisordomain.Call, r advisordomain.RunRequest) (statelog.Result, error) {
	m.asked = append(m.asked, r)
	return statelog.Result{}, nil
}
func (*memAdvisor) Release(context.Context, advisordomain.Call, string, uint64) (statelog.Result, error) {
	return statelog.Result{}, nil
}

// advisorNode is a runtime that reaches the advisor log, with no fleet to claim
// the Advisor's lease on.
type advisorNode struct {
	*fakeRuntime
	log *memAdvisor
}

func (n advisorNode) AdvisorLog() (api.AdvisorReads, api.AdvisorWrites, bool) {
	return n.log, n.log, true
}
func (advisorNode) AdvisorClaim() func(context.Context) (bool, error) { return nil }

// A NODE WHOSE RUNTIME REACHES THE ADVISOR LOG SERVES THE ADVISOR AND RUNS ITS
// DUTY: its questions answer, and its duty records a run once the app starts.
// Mutation: drop the runtime's seam from the app's wiring, and the question is
// unknown and no run is recorded.
func TestANodeThatReachesTheAdvisorLogRunsTheAdvisor(t *testing.T) {
	t.Parallel()
	log := &memAdvisor{recorded: make(chan struct{})}
	a := newApp(t, api.Options{Runtime: advisorNode{
		fakeRuntime: &fakeRuntime{state: api.RuntimeState{Posture: "serve"}}, log: log}})
	if status, body := get(t, a, "/query/advisor_findings"); status != http.StatusOK {
		t.Fatalf("advisor_findings answered %d %v, want 200", status, body)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.Start(ctx)
	// A BOUND ON THE WAIT, never its verdict: the duty's first step runs as it starts.
	wait, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()
	select {
	case <-log.recorded:
	case <-wait.Done():
		t.Fatal("the Advisor's duty recorded no run")
	}
}

// writeAs sends a write to a with the fixture's credential and body.
func writeAs(t *testing.T, a *api.App, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fixtureToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var answer map[string]any
	if err := json.NewDecoder(rec.Result().Body).Decode(&answer); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return rec.Code, answer
}

// AN ADMINISTRATOR'S RUN NOW AND DECISIONS ARE RECORDED: the run is asked for
// on the log and answered 202 with its id, and each decision moves the finding
// it names. Mutation: name the write's operation with a verb the operation id
// grammar refuses (one with a dot), and every one is a 500.
func TestAnAdministratorsRunNowAndDecisionsAreRecorded(t *testing.T) {
	t.Parallel()
	f := advisor.Finding{Class: advisor.ClassNoopShare, Severity: advisor.SeverityNote, Actor: advisor.ActorEngine,
		Source: advisor.SourceEvidence, Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev"},
		Fix: advisor.Fix{Kind: advisor.FixNone, Options: []advisor.Option{}}}
	body, err := json.Marshal(advisor.Observe(f, advisor.Span{From: time.Now().Add(-time.Hour), To: time.Now()}, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	log := &memAdvisor{recorded: make(chan struct{}), held: &advisordomain.FindingRow{ID: id, Key: advisor.Key(f),
		Class: string(f.Class), Severity: advisordomain.SeverityNote, State: advisordomain.StateOpen, Body: body}}
	a := newApp(t, api.Options{Runtime: advisorNode{
		fakeRuntime: &fakeRuntime{state: api.RuntimeState{Posture: "serve"}}, log: log}})
	status, answer := writeAs(t, a, "/advisor/runs", "")
	if status != http.StatusAccepted || answer["run_id"] == nil || len(log.asked) != 1 {
		t.Fatalf("Run now answered %d %v, asked %d runs; want 202 and one run asked for", status, answer, len(log.asked))
	}
	for _, decision := range []string{`{"decision":"snooze"}`, `{"decision":"useful"}`,
		`{"decision":"dismiss","reason":"known"}`} {
		if status, answer := writeAs(t, a, "/advisor/findings/"+id+"/decision", decision); status != http.StatusOK {
			t.Errorf("%s answered %d %v, want 200", decision, status, answer)
		}
	}
	if len(log.moved) != 3 {
		t.Errorf("the decisions moved the finding %d times, want 3", len(log.moved))
	}
}
