package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
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
func (l *applyLog) LiveProgress(context.Context, statelog.Freshness) (string, []advisordomain.ProgressLine, bool, error) {
	return "", nil, false, nil
}
func (*applyLog) RunProgress(context.Context, string, statelog.Freshness) ([]advisordomain.ProgressLine, error) {
	return nil, nil
}
func (l *applyLog) Checkpoint(context.Context, string, statelog.Freshness) (json.RawMessage, bool, error) {
	return nil, false, nil
}
func (l *applyLog) PendingRequests(context.Context, statelog.Freshness) ([]advisordomain.RunRow, error) {
	return nil, nil
}
func (*applyLog) Unowned(context.Context, statelog.Freshness) ([]advisordomain.UnownedRun, error) {
	return nil, nil
}
func (l *applyLog) LastRunOfClasses(context.Context, statelog.Freshness) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}
func (l *applyLog) AlarmEpisodes(context.Context, time.Time, statelog.Freshness) ([]advisordomain.AlarmEpisodeRow, error) {
	return nil, nil
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
func (*applyLog) Progress(context.Context, advisordomain.Call, advisordomain.RunProgress) (statelog.Result, error) {
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

// applyReader observes what it holds now, holding partial in part.
type applyReader struct {
	observed []advisor.Observation
	partial  []advisor.Cut
}

func (r applyReader) Observe(context.Context, time.Time) (duty.Observed, error) {
	return duty.Observed{Observations: r.observed, Partial: r.partial}, nil
}
func (applyReader) Signature(context.Context) (string, error)            { return "", nil }
func (applyReader) Holds(context.Context, advisor.Premise) (bool, error) { return true, nil }
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
	out, err := rc.recheck(t.Context(), "f-1", recheckAsk{}, person)
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
	out, err := rc.recheck(t.Context(), "f-1", recheckAsk{}, person)
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
// finding with nothing to send, and a report of a send, or a re-check, after
// it is refused as a finding no longer open, which is said in words.
func TestAResolvedFindingIsNeverApplied(t *testing.T) {
	t.Parallel()
	log := newApplyLog(t, capFinding(60))
	cfg := &applyConfig{entity: map[string]any{"handle": "dev", "sandbox": map[string]any{"max_turns": 30}}}
	rc := applyRig(log, nil, cfg)
	out, err := rc.recheck(t.Context(), "f-1", recheckAsk{}, person)
	if err != nil || out.Outcome != advisor.RecheckResolved || out.Send != nil || log.row.State != advisordomain.StateResolved {
		t.Fatalf("re-check = %+v (%v), state %s, want resolved with nothing to send", out, err, log.row.State)
	}
	if err := rc.applied(t.Context(), "f-1", advisor.ApplyDigest("f-1", 1,
		advisor.OptionDigest(capFinding(60).Fix.Options[0])), person); !errors.Is(err, errNotOpen) {
		t.Errorf("a report on a resolved finding answered %v, want it said as no longer open", err)
	}
	if _, err := rc.recheck(t.Context(), "f-1", recheckAsk{}, person); !errors.Is(err, errNotOpen) {
		t.Errorf("a re-check of a closed finding answered %v, want it said as no longer open", err)
	}
}

// A RE-CHECK OF A FINDING A CUT RECORD MAY HAVE DROPPED CONCLUDES NOTHING:
// GitHub's record dropped findings nobody owes and no longer carries the open
// one, so the re-check neither sends nor resolves it.
//
// Mutation: leave the reading's Partial out of the re-check, and the finding
// is resolved.
func TestARecheckOfAFindingACutRecordMayHaveDroppedConcludesNothing(t *testing.T) {
	t.Parallel()
	f := advisor.FromIntegrations([]integration.State{{Kind: integration.KindGitHub,
		Findings: []integration.Finding{{Kind: integration.FindingGrantPending, Subject: "pm"}}}})[0]
	log := newApplyLog(t, f)
	rc := applyRig(log, nil, &applyConfig{})
	rc.read = applyReader{partial: []advisor.Cut{{Path: advisor.IntegrationPath(integration.KindGitHub), Unowed: true}}}
	if out, err := rc.recheck(t.Context(), "f-1", recheckAsk{}, person); !errors.Is(err, advisor.ErrUnconcluded) ||
		log.row.State != advisordomain.StateOpen {
		t.Errorf("re-check = %+v (%v), state %s; want it unconcluded and the finding open", out, err, log.row.State)
	}
}

// A CHANGE THAT NO LONGER BUILDS RETIRES ITS FINDING, and one a managed
// configuration refuses leaves it open, saying who can make it.
func TestAFixThatNoLongerBuildsIsNotApplicable(t *testing.T) {
	t.Parallel()
	log := newApplyLog(t, capFinding(60))
	cfg := &applyConfig{entity: map[string]any{"handle": "dev"}, refuse: &configapi.ManagedError{}}
	rc := applyRig(log, []advisor.Observation{observedNow(capFinding(60))}, cfg)
	out, err := rc.recheck(t.Context(), "f-1", recheckAsk{}, person)
	if err != nil || out.Refusal == nil || out.Send != nil || log.row.State != advisordomain.StateOpen {
		t.Fatalf("a managed configuration answered %+v (%v), want a refusal and the finding open", out, err)
	}
	cfg.refuse = errors.New("the change breaks a rule")
	out, err = rc.recheck(t.Context(), "f-1", recheckAsk{}, person)
	if err != nil || out.Outcome != advisor.RecheckNotApplicable || log.row.State != advisordomain.StateRetired {
		t.Fatalf("a change that breaks a rule answered %+v (%v), state %s, want not applicable", out, err, log.row.State)
	}
}

// A FINDING APPLIED IN ANOTHER WINDOW IS SAID IN WORDS: a re-check, or a
// report that a change was sent, on a finding that is no longer open answers
// 409 with what became of it (applied, when, and until when its effect is
// checked), never the log's own sentence for a refused move. Mutation: pass
// the refusal through, and the person reads "cannot make that move".
func TestAFindingAppliedInAnotherWindowIsSaidInWords(t *testing.T) {
	t.Parallel()
	log := newApplyLog(t, capFinding(60))
	log.row.State = advisordomain.StateApplied
	log.row.AppliedAt, log.row.EffectDueAt = applyAt.Add(-time.Minute), applyAt.Add(29*time.Minute)
	rc := applyRig(log, []advisor.Observation{observedNow(capFinding(60))}, &applyConfig{})
	_, err := rc.recheck(t.Context(), "f-1", recheckAsk{}, person)
	rec := httptest.NewRecorder()
	refuseApply(rec, httptest.NewRequest(http.MethodPost, "/advisor/findings/f-1/recheck", nil), err)
	var body map[string]any
	if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil {
		t.Fatal(jerr)
	}
	detail, _ := body["detail"].(string)
	if rec.Code != http.StatusConflict || body["outcome"] != "applied" || body["state"] != "applied" ||
		body["applied_at"] == nil || body["effect_due_at"] == nil || strings.Contains(detail, "cannot make that move") ||
		!strings.Contains(detail, "applied already") {
		t.Errorf("the re-check of an applied finding answered %d %v", rec.Code, body)
	}
	if err := rc.applied(t.Context(), "f-1", "any", person); !errors.Is(err, errNotOpen) {
		t.Errorf("a report on an applied finding answered %v, want it said as no longer open", err)
	}
}

// A RE-CHECK'S PREVIEW SAYS THE CHANGE IN WORDS, by the rule every change a
// person approves is shown by (catalog.ChangeRows): labelled by their raw keys
// and valued as JSON, its rows read as a code diff ("health_tools: true").
// Mutation: label a row by its field again, or say a truth as JSON, and the
// case goes red.
func TestARechecksPreviewSaysTheChangeInWords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		before, after map[string]any
		want          PreviewRow
	}{
		{"a switch set where the seat had none", map[string]any{"handle": "dev"},
			map[string]any{"handle": "dev", "health_tools": true},
			PreviewRow{Field: "health_tools", Label: "Health tools", After: ptr("Yes")}},
		{"a nested number raised", map[string]any{"handle": "dev", "sandbox": map[string]any{"max_turns": 30}},
			map[string]any{"handle": "dev", "sandbox": map[string]any{"max_turns": 60}},
			PreviewRow{Field: "sandbox.max_turns", Label: "Sandbox › max turns", Before: ptr("30"), After: ptr("60")}},
	}
	for _, c := range cases {
		after, _ := json.Marshal(c.after)
		rows, err := previewRows(c.before, after)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: rows = %+v (%v), want one", c.name, rows, err)
		}
		if got := rows[0]; got.Field != c.want.Field || got.Label != c.want.Label ||
			deref(got.Before) != deref(c.want.Before) || deref(got.After) != deref(c.want.After) ||
			(got.Before == nil) != (c.want.Before == nil) {
			t.Errorf("%s: row = %s %q %q→%q, want %s %q %q→%q", c.name, got.Field, got.Label, deref(got.Before),
				deref(got.After), c.want.Field, c.want.Label, deref(c.want.Before), deref(c.want.After))
		}
	}
}

func ptr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return "<none>"
	}
	return *s
}

// seatsConfig answers each seat's entity by handle.
type seatsConfig struct{ entities map[string]map[string]any }

func (c *seatsConfig) EntityAndTag(_ context.Context, kind, id string) (any, string, error) {
	e, ok := c.entities[id]
	if kind != advisor.EntityRoles || !ok {
		return nil, "", configapi.ErrNoSuchEntity
	}
	raw, _ := json.Marshal(e)
	return e, `"roles-` + id + `-` + string(raw[:min(len(raw), 12)]) + `"`, nil
}
func (c *seatsConfig) CheckEntity(context.Context, string, string, []byte, bool, iam.Actor) (*config.Company, error) {
	return nil, nil
}

// ONE CAUSE ON MANY SEATS IS SENT AS ONE ACTION OF SEVERAL CHANGES: the
// re-check answers every seat's request in order, its preview naming whose
// field each row is, and the finding is applied only once every change holds.
// Mutation: send the first change alone, or mark it applied on the first
// seat's change, and the other seats are never changed.
func TestOneCauseOnManySeatsIsSentAsOneActionOfSeveralChanges(t *testing.T) {
	t.Parallel()
	capped := func(seat string) advisor.Change {
		return advisor.Change{Entity: advisor.EntityRoles, ID: seat, Set: map[string]any{},
			Merge: map[string]any{"sandbox": map[string]any{"max_turns": 60}}, Summary: "raise " + seat + "'s cap"}
	}
	f := capFinding(60)
	f.Fix.Options = []advisor.Option{{Label: "Raise the caps (all 2)", Change: capped("dev"),
		Also: []advisor.Change{capped("qa")}}}
	log := newApplyLog(t, f)
	cfg := &seatsConfig{entities: map[string]map[string]any{
		"dev": {"handle": "dev", "sandbox": map[string]any{"max_turns": 30}},
		"qa":  {"handle": "qa", "sandbox": map[string]any{"max_turns": 30}}}}
	rc := rechecker{log: log, read: applyReader{observed: []advisor.Observation{observedNow(f)}}, config: cfg,
		now: func() time.Time { return applyAt },
		finding: func(_ context.Context, id string) (queries.AdvisorFinding, error) {
			return queries.AdvisorFinding{ID: id, State: advisor.State(log.row.State)}, nil
		}}
	out, err := rc.recheck(t.Context(), "f-1", recheckAsk{}, person)
	if err != nil || len(out.Sends) != 2 || out.Send == nil || out.Send.Path != "/config/roles/dev" ||
		out.Sends[1].Path != "/config/roles/qa" {
		t.Fatalf("re-check = %+v (%v), want dev's request then qa's", out, err)
	}
	if len(out.Preview.Rows) != 2 || out.Preview.Rows[0].Field != "dev: sandbox.max_turns" ||
		out.Preview.Rows[1].Field != "qa: sandbox.max_turns" {
		t.Errorf("preview = %+v, want each seat's row named", out.Preview.Rows)
	}
	cfg.entities["dev"] = out.Sends[0].Body.(map[string]any)
	if err := rc.applied(t.Context(), "f-1", out.Digest, person); !errors.Is(err, errRecheckAgain) {
		t.Errorf("a report with qa's change unsent answered %v, want a re-check", err)
	}
	cfg.entities["qa"] = out.Sends[1].Body.(map[string]any)
	if err := rc.applied(t.Context(), "f-1", out.Digest, person); err != nil {
		t.Fatalf("a report with both sent: %v", err)
	}
}

// moveWork is the work items a reassignment's re-check reads, by key.
type moveWork map[string]worklet.Task

func (w moveWork) Task(_ context.Context, key string, _ worklet.DetailWants, _ statelog.Freshness) (worklet.TaskDetail, error) {
	t, ok := w[key]
	if !ok {
		return worklet.TaskDetail{}, worklet.ErrNoTask
	}
	return worklet.TaskDetail{Task: t}, nil
}

// A REASSIGNMENT IS RE-CHECKED AND MOVED ITEM BY ITEM: asked to move the
// work, the re-check answers each item the busy seat still holds and nobody
// has started, at its version, to the seat the finding suggests, with a row
// for each; an item moved or started since is left where it is. The finding
// is applied once the busy seat holds none of them, and not before.
// Mutation: answer a started item, or mark it applied with an item unmoved,
// and work is taken from under a turn or a fix reads as done.
func TestAReassignmentIsRecheckedAndMovedItemByItem(t *testing.T) {
	t.Parallel()
	f := advisor.Finding{Class: advisor.ClassWorkImbalance, Severity: advisor.SeverityCosts, Actor: advisor.ActorOwner,
		Source: advisor.SourceEvidence, Area: advisor.AreaWork,
		Subject:  advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev", Name: "Dev"},
		Seats:    []advisor.SeatShare{{Seat: "dev", Name: "Dev", Value: 27}, {Seat: "dev-2", Name: "Dev 2", Value: 2}},
		Evidence: []advisor.Fact{{Key: "open", Value: 27}},
		Fix: advisor.Fix{Kind: advisor.FixOpenPage, Page: advisor.PageSeat, Target: "dev", Options: []advisor.Option{},
			Reassign: &advisor.Reassign{Items: []string{"ENG-1", "ENG-2", "ENG-3", "ENG-4"}, To: "dev-2"}}}
	log := newApplyLog(t, f)
	work := moveWork{
		"ENG-1": {Key: "ENG-1", Title: "Fix the login", Assignee: "dev", StatusGroup: worklet.GroupNotStarted, Version: 4},
		"ENG-2": {Key: "ENG-2", Assignee: "dev", StatusGroup: worklet.GroupActive, Version: 9},
		"ENG-3": {Key: "ENG-3", Assignee: "qa", StatusGroup: worklet.GroupNotStarted, Version: 2},
		"ENG-4": {Key: "ENG-4", Title: "Add the export", Assignee: "dev", StatusGroup: worklet.GroupNotStarted, Version: 6},
	}
	rc := applyRig(log, []advisor.Observation{observedNow(f)}, &applyConfig{})
	rc.work = work
	out, err := rc.recheck(t.Context(), "f-1", recheckAsk{reassign: true}, person)
	if err != nil || out.Outcome != advisor.RecheckHolds || out.Send != nil || len(out.Moves) != 2 {
		t.Fatalf("re-check = %+v (%v), want ENG-1 and ENG-4 to move", out, err)
	}
	if m := out.Moves[0]; m.Item != "ENG-1" || m.From != "dev" || m.To != "dev-2" || m.Version != 4 || m.Reason == "" ||
		out.Moves[1].Item != "ENG-4" || out.Moves[1].Version != 6 {
		t.Errorf("moves = %+v, want ENG-1 at 4 and ENG-4 at 6, from dev to dev-2", out.Moves)
	}
	if r := out.Preview.Rows; len(r) != 2 || r[0].Label != "ENG-1 Fix the login" || *r[0].Before != "Dev" || *r[0].After != "Dev 2" {
		t.Errorf("preview = %+v, want a row per item, by the seats' names", out.Preview.Rows)
	}
	if _, err := rc.recheck(t.Context(), "f-1", recheckAsk{}, person); err != nil {
		t.Errorf("a re-check of the fix itself: %v", err)
	}
	work["ENG-1"] = worklet.Task{Key: "ENG-1", Assignee: "dev-2", StatusGroup: worklet.GroupNotStarted}
	if err := rc.applied(t.Context(), "f-1", out.Digest, person); !errors.Is(err, errRecheckAgain) {
		t.Errorf("a report with ENG-4 unmoved answered %v, want a re-check", err)
	}
	work["ENG-4"] = worklet.Task{Key: "ENG-4", Assignee: "dev-2", StatusGroup: worklet.GroupNotStarted}
	if err := rc.applied(t.Context(), "f-1", out.Digest, person); err != nil || len(log.moves) == 0 ||
		log.moves[len(log.moves)-1].State != advisordomain.StateApplied {
		t.Fatalf("a report with both moved: %v, moves %+v", err, log.moves)
	}
}
