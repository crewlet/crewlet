package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

var applyAt = time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)

// capFinding is a round-cap finding about dev whose fix sets its cap to rounds.
func capFinding(rounds int) advisor.Finding {
	return advisor.Finding{Class: advisor.ClassRoundCap, Severity: advisor.SeverityCosts, Actor: advisor.ActorOwner,
		Source: advisor.SourceEvidence, Area: advisor.AreaSetup, Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev"},
		Evidence: []advisor.Fact{{Key: "runs", Value: 3}},
		Fix: advisor.Fix{Kind: advisor.FixConfigChange, Sentence: "Raise its cap.", Options: []advisor.Option{{
			Label: "Raise the cap", Change: advisor.Change{Entity: advisor.EntityRoles, ID: "dev", Set: map[string]any{},
				Merge: map[string]any{"sandbox": map[string]any{"max_turns": rounds}}, Summary: "raise the cap"}}}}}
}

// applyLog is an advisor log holding one finding, keeping every move made on it.
type applyLog struct {
	row   advisordomain.FindingRow
	moves []advisordomain.Move
}

func newApplyLog(t *testing.T, f advisor.Finding) *applyLog {
	t.Helper()
	body, err := json.Marshal(advisor.Observe(f, advisor.Span{From: applyAt.Add(-advisor.Window), To: applyAt}, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	return &applyLog{row: advisordomain.FindingRow{ID: "f-1", Key: advisor.Key(f), Class: string(f.Class),
		State: advisordomain.StateOpen, Revision: 1, Body: body, KeyVersion: 7, OpenedAt: applyAt.Add(-time.Hour)}}
}

func (l *applyLog) Finding(_ context.Context, id string, _ statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error) {
	if id != l.row.ID {
		return advisordomain.FindingRow{}, nil, advisordomain.ErrNotFound
	}
	return l.row, nil, nil
}
func (l *applyLog) Findings(context.Context, advisordomain.Filter, statelog.Freshness) ([]advisordomain.FindingRow, string, error) {
	return nil, "", nil
}
func (l *applyLog) NonClosed(context.Context, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return nil, nil
}
func (l *applyLog) Episodes(context.Context, string, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return nil, nil
}
func (l *applyLog) LatestRun(context.Context, statelog.Freshness) (advisordomain.RunRow, bool, error) {
	return advisordomain.RunRow{}, false, nil
}
func (l *applyLog) PendingRequests(context.Context, statelog.Freshness) ([]advisordomain.RunRow, error) {
	return nil, nil
}
func (l *applyLog) LastRunOfClasses(context.Context, statelog.Freshness) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}
func (l *applyLog) Reports(context.Context, string, int, statelog.Freshness) ([]advisordomain.ReportRow, string, error) {
	return nil, "", nil
}
func (l *applyLog) Report(context.Context, string, statelog.Freshness) (advisordomain.ReportRow, bool, error) {
	return advisordomain.ReportRow{}, false, nil
}
func (l *applyLog) Tally(context.Context, time.Time, time.Time, statelog.Freshness) (advisordomain.Tally, error) {
	return advisordomain.Tally{}, nil
}
func (l *applyLog) Explain(context.Context, advisordomain.Call, string, advisordomain.Explanation) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (l *applyLog) WriteReport(context.Context, advisordomain.Call, advisordomain.Report) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (l *applyLog) Key(context.Context, string, statelog.Freshness) (advisordomain.KeyRow, bool, error) {
	return advisordomain.KeyRow{}, false, nil
}
func (l *applyLog) Open(context.Context, advisordomain.Call, advisordomain.Opening, uint64) (statelog.Result, error) {
	return statelog.Result{}, errors.New("a re-check opens nothing")
}
func (l *applyLog) Revise(_ context.Context, _ advisordomain.Call, _ string, r advisordomain.Revision, ifMatch uint64) (statelog.Result, error) {
	if ifMatch != l.row.KeyVersion {
		return statelog.Result{}, advisordomain.ErrStaleVersion
	}
	l.row.Revision, l.row.Body, l.row.KeyVersion = r.Revision, r.Body, l.row.KeyVersion+1
	return statelog.Result{}, nil
}
func (l *applyLog) Refresh(_ context.Context, _ advisordomain.Call, _ string, r advisordomain.Refresh, _ uint64) (statelog.Result, error) {
	l.row.Body, l.row.KeyVersion = r.Body, l.row.KeyVersion+1
	return statelog.Result{}, nil
}
func (l *applyLog) Move(_ context.Context, _ advisordomain.Call, _ string, m advisordomain.Move, ifMatch uint64) (statelog.Result, error) {
	if ifMatch != l.row.KeyVersion {
		return statelog.Result{}, advisordomain.ErrStaleVersion
	}
	l.moves = append(l.moves, m)
	l.row.State, l.row.KeyVersion = m.State, l.row.KeyVersion+1
	return statelog.Result{}, nil
}
func (l *applyLog) RecordRun(context.Context, advisordomain.Call, advisordomain.RunRecord) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (l *applyLog) Expire(context.Context, advisordomain.Call, time.Time, advisordomain.Expiry) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (l *applyLog) RequestRun(context.Context, advisordomain.Call, advisordomain.RunRequest) (statelog.Result, error) {
	return statelog.Result{}, nil
}
func (l *applyLog) Release(context.Context, advisordomain.Call, string, uint64) (statelog.Result, error) {
	return statelog.Result{}, nil
}

// applyReader observes what it holds now.
type applyReader struct{ observed []advisor.Observation }

func (r applyReader) Observe(context.Context, time.Time) (duty.Observed, error) {
	return duty.Observed{Observations: r.observed}, nil
}
func (applyReader) Signature(context.Context) (string, error) { return "", nil }
func (applyReader) Week(context.Context, time.Time, time.Time) ([]advisor.Metric, []string, error) {
	return nil, nil, nil
}

// applyConfig is one seat entity at a tag, checked as refuse says.
type applyConfig struct {
	entity map[string]any
	refuse error
}

func (c *applyConfig) EntityAndTag(_ context.Context, kind, id string) (any, string, error) {
	if kind != advisor.EntityRoles || id != "dev" {
		return nil, "", configapi.ErrNoSuchEntity
	}
	raw, _ := json.Marshal(c.entity)
	return c.entity, `"roles-` + string(raw[:min(len(raw), 12)]) + `"`, nil
}
func (c *applyConfig) CheckEntity(context.Context, string, string, []byte, bool, iam.Actor) (*config.Company, error) {
	return nil, c.refuse
}

var person = iam.Actor{Name: "ana", Kind: iam.ActorHuman}

func applyRig(log *applyLog, observed []advisor.Observation, cfg *applyConfig) rechecker {
	return rechecker{log: log, read: applyReader{observed: observed}, config: cfg, now: func() time.Time { return applyAt },
		finding: func(_ context.Context, id string) (queries.AdvisorFinding, error) {
			return queries.AdvisorFinding{ID: id, State: advisor.State(log.row.State)}, nil
		}}
}

func observedNow(f advisor.Finding) advisor.Observation {
	return advisor.Observe(f, advisor.Span{From: applyAt.Add(-advisor.Window), To: applyAt}, "r2")
}

// THE SEND IS BOUND TO THE REVISION THE RE-CHECK READ: a finding that holds
// answers the entity with the change applied, under the If-Match of the entity
// it read, and the report that it was sent marks it applied only once the
// configuration holds the change, under the digest the re-check answered.
func TestTheSendIsBoundToTheRevisionTheRecheckRead(t *testing.T) {
	t.Parallel()
	log := newApplyLog(t, capFinding(60))
	cfg := &applyConfig{entity: map[string]any{"handle": "dev", "sandbox": map[string]any{"max_turns": 30}}}
	rc := applyRig(log, []advisor.Observation{observedNow(capFinding(60))}, cfg)
	out, err := rc.recheck(t.Context(), "f-1", 0, person)
	if err != nil || out.Outcome != advisor.RecheckHolds || out.Send == nil {
		t.Fatalf("re-check = %+v (%v), want holds with a request", out, err)
	}
	_, tag, _ := cfg.EntityAndTag(t.Context(), advisor.EntityRoles, "dev")
	if out.Send.Method != "PUT" || out.Send.Path != "/config/roles/dev" || out.Send.Headers["If-Match"] != tag ||
		out.Send.DryRunPath == nil {
		t.Errorf("the request is %+v, want a PUT of the seat under its tag %s", out.Send, tag)
	}
	if !out.ExpiresAt.Equal(applyAt.Add(RecheckFor)) || len(out.Preview.Rows) != 1 || out.Preview.Rows[0].Field != "sandbox.max_turns" {
		t.Errorf("expires %v, preview %+v", out.ExpiresAt, out.Preview)
	}
	if err := rc.applied(t.Context(), "f-1", out.Digest, person); !errors.Is(err, errRecheckAgain) {
		t.Errorf("a report before the configuration holds the change answered %v, want a re-check", err)
	}
	cfg.entity = out.Send.Body.(map[string]any)
	if err := rc.applied(t.Context(), "f-1", "another digest", person); !errors.Is(err, errRecheckAgain) {
		t.Errorf("a report under another digest answered %v, want a re-check", err)
	}
	if err := rc.applied(t.Context(), "f-1", out.Digest, person); err != nil {
		t.Fatal(err)
	}
	m := log.moves[len(log.moves)-1]
	if m.State != advisordomain.StateApplied || m.FixDigest != advisor.OptionDigest(capFinding(60).Fix.Options[0]) ||
		!m.EffectDueAt.Equal(applyAt.Add(advisor.Window)) {
		t.Errorf("the finding was marked %+v, want applied with its option and a week's horizon", m)
	}
}

// A CHANGED FIX IS SHOWN BEFORE IT CAN BE SENT: the re-check revises the
// finding, answers the fix it replaced and the new request, and a report under
// the old revision's digest is refused.
func TestAChangedFixIsShownBeforeItCanBeSent(t *testing.T) {
	t.Parallel()
	log := newApplyLog(t, capFinding(60))
	cfg := &applyConfig{entity: map[string]any{"handle": "dev", "sandbox": map[string]any{"max_turns": 30}}}
	rc := applyRig(log, []advisor.Observation{observedNow(capFinding(90))}, cfg)
	old := advisor.ApplyDigest("f-1", 1, advisor.OptionDigest(capFinding(60).Fix.Options[0]))
	out, err := rc.recheck(t.Context(), "f-1", 0, person)
	if err != nil || out.Outcome != advisor.RecheckChanged || out.PreviousFix == nil || log.row.Revision != 2 {
		t.Fatalf("re-check = %+v (%v), revision %d, want changed and revised", out, err, log.row.Revision)
	}
	if sandbox, _ := out.Send.Body.(map[string]any)["sandbox"].(map[string]any); sandbox["max_turns"] != float64(90) {
		t.Errorf("the request sends %v, want the cap the re-check built", out.Send.Body)
	}
	cfg.entity = out.Send.Body.(map[string]any)
	if err := rc.applied(t.Context(), "f-1", old, person); !errors.Is(err, errRecheckAgain) {
		t.Errorf("a report of the change shown before the revision answered %v, want a re-check", err)
	}
	if err := rc.applied(t.Context(), "f-1", out.Digest, person); err != nil {
		t.Errorf("a report under the new digest answered %v", err)
	}
}

// A RESOLVED FINDING IS NEVER APPLIED: a condition that ended closes the
// finding with nothing to send, and a report of a send is refused after it.
func TestAResolvedFindingIsNeverApplied(t *testing.T) {
	t.Parallel()
	log := newApplyLog(t, capFinding(60))
	cfg := &applyConfig{entity: map[string]any{"handle": "dev", "sandbox": map[string]any{"max_turns": 30}}}
	rc := applyRig(log, nil, cfg)
	out, err := rc.recheck(t.Context(), "f-1", 0, person)
	if err != nil || out.Outcome != advisor.RecheckResolved || out.Send != nil || log.row.State != advisordomain.StateResolved {
		t.Fatalf("re-check = %+v (%v), state %s, want resolved with nothing to send", out, err, log.row.State)
	}
	if err := rc.applied(t.Context(), "f-1", advisor.ApplyDigest("f-1", 1,
		advisor.OptionDigest(capFinding(60).Fix.Options[0])), person); !errors.Is(err, errRecheckAgain) {
		t.Errorf("a report on a resolved finding answered %v, want a re-check", err)
	}
	if _, err := rc.recheck(t.Context(), "f-1", 0, person); !errors.Is(err, advisordomain.ErrIllegalMove) {
		t.Errorf("a re-check of a closed finding answered %v", err)
	}
}

// A CHANGE THAT NO LONGER BUILDS RETIRES ITS FINDING, and one a managed
// configuration refuses leaves it open, saying who can make it.
func TestAFixThatNoLongerBuildsIsNotApplicable(t *testing.T) {
	t.Parallel()
	log := newApplyLog(t, capFinding(60))
	cfg := &applyConfig{entity: map[string]any{"handle": "dev"}, refuse: &configapi.ManagedError{}}
	rc := applyRig(log, []advisor.Observation{observedNow(capFinding(60))}, cfg)
	out, err := rc.recheck(t.Context(), "f-1", 0, person)
	if err != nil || out.Refusal == nil || out.Send != nil || log.row.State != advisordomain.StateOpen {
		t.Fatalf("a managed configuration answered %+v (%v), want a refusal and the finding open", out, err)
	}
	cfg.refuse = errors.New("the change breaks a rule")
	out, err = rc.recheck(t.Context(), "f-1", 0, person)
	if err != nil || out.Outcome != advisor.RecheckNotApplicable || log.row.State != advisordomain.StateRetired {
		t.Fatalf("a change that breaks a rule answered %+v (%v), state %s, want not applicable", out, err, log.row.State)
	}
}
