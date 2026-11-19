package queries

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// historyLog is an advisor log holding one finding a person moved twice.
type historyLog struct {
	noticeLog
	row     advisordomain.FindingRow
	history []advisordomain.HistoryRow
}

func (l historyLog) Finding(context.Context, string, statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error) {
	return l.row, l.history, nil
}

func (historyLog) LastRunOfClasses(context.Context, statelog.Freshness) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

// A FINDING'S HISTORY NAMES A PERSON AS THE COMPANY NAMES THEIR SEAT, beside
// the handle the log recorded, and a person the chart no longer holds by that
// handle: the timeline reads "Snoozed by Jane Founder", never the handle.
func TestAFindingsHistoryNamesThePersonByName(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte("name: Acme\nroles:\n" +
		"  - name: Jane Founder\n    handle: jane-founder\n    kind: human\n    contact:\n      slack_user_id: U0JANE\n" +
		"  - name: App\n    handle: app\n"))
	if err != nil {
		t.Fatal(err)
	}
	roster, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)
	f := advisor.Finding{Class: advisor.ClassRoundCap, Severity: advisor.SeverityCosts, Actor: advisor.ActorOwner,
		Source: advisor.SourceEvidence, Area: advisor.AreaSetup, Title: "A cap.", Why: "It costs.",
		Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "app"},
		Fix:     advisor.Fix{Kind: advisor.FixOpenSetting, Sentence: "Look.", Options: []advisor.Option{}}}
	body, err := json.Marshal(advisor.Observe(f, advisor.Span{From: now, To: now}, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	log := historyLog{row: advisordomain.FindingRow{ID: "f-1", Key: advisor.Key(f), Class: string(f.Class),
		State: advisordomain.StateOpen, Revision: 1, OpenedAt: now, Body: body},
		history: []advisordomain.HistoryRow{
			{At: now, Event: advisordomain.EventOpened, ByKind: iam.ActorSystem, ByName: "engine"},
			{At: now, Event: advisordomain.EventSnoozed, ByKind: iam.ActorHuman, ByName: "jane-founder"},
			{At: now, Event: advisordomain.EventUnsnoozed, ByKind: iam.ActorHuman, ByName: "left-the-company"}}}
	s := Sources{Advisor: log, Now: func() time.Time { return now },
		Company: func() (*config.Company, *org.Organization) { return cfg, roster }}
	ctx := iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Login: "token:test", Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin}})
	got, err := s.advisorFinding(ctx, FromMap(map[string]any{"id": "f-1"}))
	if err != nil {
		t.Fatal(err)
	}
	history := got.(AdvisorFindingDetail).History
	want := []AdvisorActor{{Kind: "engine"}, {Kind: "person", Handle: "jane-founder", Name: "Jane Founder"},
		{Kind: "person", Handle: "left-the-company", Name: "left-the-company"}}
	if len(history) != len(want) {
		t.Fatalf("the history has %d lines, want %d", len(history), len(want))
	}
	for i, h := range history {
		if h.By != want[i] {
			t.Errorf("line %d is by %+v, want %+v", i, h.By, want[i])
		}
	}
}
