package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/statelog"
)

// memAdvisor is an advisor log holding nothing, which tells when a run is recorded.
type memAdvisor struct {
	once     sync.Once
	recorded chan struct{}
}

func (*memAdvisor) Findings(context.Context, advisordomain.Filter, statelog.Freshness) ([]advisordomain.FindingRow, string, error) {
	return nil, "", nil
}
func (*memAdvisor) NonClosed(context.Context, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return nil, nil
}
func (*memAdvisor) Finding(context.Context, string, statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error) {
	return advisordomain.FindingRow{}, nil, advisordomain.ErrNotFound
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
func (*memAdvisor) Move(context.Context, advisordomain.Call, string, advisordomain.Move, uint64) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (m *memAdvisor) RecordRun(context.Context, advisordomain.Call, advisordomain.RunRecord) (statelog.Result, error) {
	m.once.Do(func() { close(m.recorded) })
	return statelog.Result{}, nil
}
func (*memAdvisor) Expire(context.Context, advisordomain.Call, time.Time, advisordomain.Expiry) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (*memAdvisor) RequestRun(context.Context, advisordomain.Call, advisordomain.RunRequest) (statelog.Result, error) {
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
