package datasetsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/worklet"
)

// sampleAnswers are each bound question's answer built from the domain's OWN
// types, so a projection is held to the JSON names those types really carry:
// a binding reading a field the answer does not hold answers nil for it, which
// a typed sample catches and a hand-written JSON string would not.
func sampleAnswers(t *testing.T) map[string]any {
	t.Helper()
	due := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	return map[string]any{
		"tracker.items": map[string]any{"items": []worklet.TaskRow{{
			Key: "ENG-1", Title: "Ship it", Status: "in_progress", StatusGroup: "active",
			Priority: "high", Assignee: "ana", Project: "ENG", Due: &due, Updated: due,
			Points: 3, Blocked: true, Overdue: true,
		}}},
		"tracker.workload": worklet.WorkloadAnswer{Rows: []worklet.WorkloadRow{{
			Handle: "ana", Open: 4, Points: 9, Blocked: 1, Overdue: 2, Unscheduled: 1}}},
		"spend.seats": tokens.Rollup{ByAgent: []tokens.AgentRow{{Role: "Ana", Handle: "ana",
			Person: true, Bucket: tokens.Bucket{TotalTokens: 30, InputTokens: 20, OutputTokens: 10, Calls: 2}}}},
		"spend.daily": tokens.Series{
			Points: []tokens.SeriesPoint{{Window: "2026-10-18",
				Groups: map[string]tokens.Bucket{"a1": {TotalTokens: 7}}, Residual: tokens.Bucket{TotalTokens: 2}}},
			ByGroup: []tokens.GroupRow{{Group: "a1", Label: "Ana"}, {Group: ""}},
		},
		"agents.status": []map[string]any{{"id": "ana", "handle": "ana", "role": "Ana", "activity": "working",
			"current_phase": "act", "paused": map[string]any{"by": "jo"},
			"last_turn": map[string]any{"ended_at": "2026-10-18T10:00:00Z", "outcome": "completed"}}},
	}
}

// EVERY BINDING ANSWERS EXACTLY ITS SOURCE'S COLUMNS, each with a value, from
// an answer of the domain's own types: the catalogue promises the columns a
// widget is validated against at publish, and a binding that read a renamed
// field would answer an empty column with nothing to say so.
func TestEveryBindingAnswersExactlyItsSourcesColumns(t *testing.T) {
	samples := sampleAnswers(t)
	for _, src := range dashspec.Catalogue() {
		raw, err := json.Marshal(samples[src.Name])
		if err != nil {
			t.Fatal(err)
		}
		table, err := project(src, raw)
		if err != nil {
			t.Fatalf("%s: %v", src.Name, err)
		}
		if len(table.Rows) == 0 {
			t.Fatalf("%s answered no row from its sample", src.Name)
		}
		var want []string
		for _, c := range src.Columns {
			want = append(want, c.Key)
		}
		for i, row := range table.Rows {
			got := slices.Sorted(maps.Keys(row))
			if !slices.Equal(got, slices.Sorted(slices.Values(want))) {
				t.Errorf("%s row %d holds %v, want exactly %v", src.Name, i, got, want)
			}
			for k, v := range row {
				if v == nil {
					t.Errorf("%s row %d: %q is empty: the binding reads a field the answer does not hold",
						src.Name, i, k)
				}
			}
		}
	}
}

func TestADailySeriesIsOneRowPerDayAndBandWithTheRestAsOther(t *testing.T) {
	raw, _ := json.Marshal(sampleAnswers(t)["spend.daily"])
	rows, _, err := dailyRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["band"] != "Ana" || rows[1]["band"] != "Other" {
		t.Errorf("rows = %v", rows)
	}
}

// fakeQuestions answers postures, and records the parameters each question
// was asked with.
type fakeQuestions struct {
	postures map[string]queries.Posture
	asked    map[string]map[string]any
}

func (f *fakeQuestions) AnswerWith(_ context.Context, what string, p queries.Params) (any, error) {
	f.asked[what] = p.Values()
	return map[string]any{"items": []any{}}, nil
}

func (f *fakeQuestions) PostureOf(what string) queries.Posture { return f.postures[what] }

// A BINDING TO A QUESTION DECIDED PER RECORD IS REFUSED AT WIRING: its answer
// depends on a relation the per-principal cache key does not carry.
func TestABindingToAPerRecordQuestionIsRefused(t *testing.T) {
	q := &fakeQuestions{postures: map[string]queries.Posture{
		"work_items": queries.DecidedBy(authz.ActionWorkRead)}}
	_, err := New(Options{Boards: nopBoards{}, Chart: authz.NoChart{}, Questions: q, Data: nopData{}})
	if err == nil || !strings.Contains(err.Error(), "work_items") {
		t.Fatalf("a per-record question was bound: %v", err)
	}
	q.postures["work_items"] = queries.Requires(iam.GrantWorklet)
	if _, err := New(Options{Boards: nopBoards{}, Chart: authz.NoChart{}, Questions: q, Data: nopData{}}); err != nil {
		t.Fatalf("a grant-decided question is bound: %v", err)
	}
}

// A PERSONAL SCOPE NAMES THE VIEWER'S RECORD, never a parameter: whoever reads
// `scope: mine` is the one whose assignments are listed.
func TestMineIsTheViewersOwnRecord(t *testing.T) {
	ana := iam.Principal{ID: uuid.New(), Login: "ana.lee", Kind: iam.KindPerson, Seat: "ana"}
	got := bindings["tracker.items"].params(map[string]string{"scope": "mine"}, ana)
	if got["assignee"] != "ana" || got["status_group"] != openStatusGroups {
		t.Errorf("mine for ana asks %v", got)
	}
	got = bindings["tracker.items"].params(map[string]string{"scope": "triage"}, ana)
	if got["preset"] != "triage" || got["assignee"] != nil {
		t.Errorf("a preset scope asks %v", got)
	}
}

// THE CACHE KEY CHANGES WITH EVERY FACT ABOUT THE PRINCIPAL A QUESTION READS,
// so a grant revoked, a seat rebound or a stage suspended is a new key at once.
func TestThePrincipalKeyChangesWithEveryAuthorityFact(t *testing.T) {
	base := iam.Principal{ID: uuid.New(), Login: "ana.lee", Kind: iam.KindPerson, Seat: "ana",
		Grants: []iam.Grant{iam.GrantWorklet}, Position: "eng", Stage: iam.StageActive}
	k := principalKey(base)
	for name, change := range map[string]func(p *iam.Principal){
		"id":       func(p *iam.Principal) { p.ID = uuid.New() },
		"login":    func(p *iam.Principal) { p.Login = "ana.b" },
		"seat":     func(p *iam.Principal) { p.Seat = "bo" },
		"grants":   func(p *iam.Principal) { p.Grants = nil },
		"position": func(p *iam.Principal) { p.Position = "ops" },
		"stage":    func(p *iam.Principal) { p.Stage = iam.StageSuspended },
	} {
		p := base
		p.Grants = slices.Clone(base.Grants)
		change(&p)
		if principalKey(p) == k {
			t.Errorf("a principal differing in %s shares the cache key", name)
		}
	}
	reordered := base
	reordered.Grants = []iam.Grant{iam.GrantWorklet, iam.GrantWorklet}
	if principalKey(reordered) != k {
		t.Error("the same effective grants made a second key")
	}
}

func TestACachedAnswerExpires(t *testing.T) {
	c := newAnswerCache()
	now := time.Unix(1_700_000_000, 0)
	c.put("k", result{asOf: now}, now)
	if _, ok := c.get("k", now.Add(artifacts.DatasetCacheTTL-time.Millisecond)); !ok {
		t.Error("an answer inside its life was forgotten")
	}
	if _, ok := c.get("k", now.Add(artifacts.DatasetCacheTTL)); ok {
		t.Error("an answer past its life was answered")
	}
}

// THE CATALOGUE'S SPEND WINDOW IS THE USAGE DOMAIN'S: a source validated to
// take a window the domain refuses would publish and never draw.
func TestTheCataloguesSpendBoundsAreTheUsageDomains(t *testing.T) {
	if dashspec.MaxSpendDays != tokens.MaxSpendRangeDays {
		t.Errorf("the catalogue reads %d spend days and the domain answers %d", dashspec.MaxSpendDays,
			tokens.MaxSpendRangeDays)
	}
	if dashspec.MaxSpendBands != tokens.DefaultSeriesGroups {
		t.Errorf("the catalogue bounds %d bands and the series names %d", dashspec.MaxSpendBands,
			tokens.DefaultSeriesGroups)
	}
	for _, src := range dashspec.Catalogue() {
		if src.Domain == dashspec.DomainTracker && src.MaxRows > worklet.PageMax {
			t.Errorf("%s answers %d rows and the tracker pages %d", src.Name, src.MaxRows, worklet.PageMax)
		}
	}
}

// A VIEWER'S LIVE READS ARE BUDGETED, and the refusal says when to come back.
func TestAViewersLiveReadsAreBudgeted(t *testing.T) {
	q := &fakeQuestions{postures: map[string]queries.Posture{}, asked: map[string]map[string]any{}}
	now := time.Unix(1_700_000_000, 0)
	s, err := New(Options{Boards: nopBoards{}, Chart: authz.NoChart{}, Questions: q, Data: nopData{},
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	rd := &read{a: admission(iam.Principal{ID: uuid.New(), Login: "ana.lee"})}
	board := rd.a.Head.ID.String()
	for range artifacts.DatasetReadsViewerBurst {
		if err := s.charge(board, rd.a.Principal); err != nil {
			t.Fatalf("a read inside the burst was refused: %v", err)
		}
	}
	var spent *overBudget
	if err := s.charge(board, rd.a.Principal); !errors.As(err, &spent) || spent.refusal.RetryAfter <= 0 {
		t.Errorf("the read past the burst = %v, want a budget refusal with a wait", err)
	}
	preview := previewScope(rd.a.Principal)
	for range artifacts.DatasetReadsViewerBurst {
		if err := s.charge(preview, rd.a.Principal); err != nil {
			t.Fatalf("a preview inside its own burst was refused: %v", err)
		}
	}
	if err := s.charge(preview, rd.a.Principal); !errors.As(err, &spent) {
		t.Errorf("the preview past the burst = %v, want a budget refusal: a principal's previews "+
			"are budgeted as their reads of one board are", err)
	}
	other := &read{a: admission(iam.Principal{ID: uuid.New(), Login: "bo.k"})}
	if err := s.charge(board, other.a.Principal); err != nil {
		t.Errorf("one viewer's spent budget refused another's read: %v", err)
	}
}

// A store document is a row of its declared columns, a value of another type
// answered as none rather than refusing the whole dataset.
func TestAStoreDocumentIsARowOfItsDeclaredColumns(t *testing.T) {
	shape := dashspec.Shape{Columns: []dashspec.Column{{Key: dashspec.StoreIDColumn, Type: dashspec.ColumnText},
		{Key: "amount", Type: dashspec.ColumnNumber}, {Key: "stage", Type: dashspec.ColumnText}}}
	row := storeRow(artifactdata.Doc{ID: "d1", Fields: json.RawMessage(`{"amount":"lots","stage":"won","x":1}`)}, shape)
	if row[dashspec.StoreIDColumn] != "d1" || row["amount"] != nil || row["stage"] != "won" || len(row) != 3 {
		t.Errorf("row = %v", row)
	}
}

// A DATASET IS CLOSED ONLY BY A REFUSAL THAT NAMES ITS DECISION.
//
// A question's refusal carries the rule that decided it, and the dataset is
// closed with that reason and those grants; a bare refusal sentinel, or a
// question asked by nobody, names no decision, so neither closes the dataset
// as a grant the viewer lacks.
//
// Mutation: close the dataset on a bare queries.ErrUnauthorized again and this
// goes red.
func TestADatasetIsClosedOnlyByARefusalNamingItsDecision(t *testing.T) {
	src := dashspec.EngineSource{Name: "tracker.items", Domain: "tracker", Access: "the tracker"}
	err := sourceFailure("items", src, fmt.Errorf("asking: %w", queries.NewRefusal("work_items",
		authz.Decision{Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantWorklet}})))
	var refused *closed
	if !errors.As(err, &refused) || refused.refusal.Reason != authz.ReasonNoGrant ||
		!slices.Equal(refused.refusal.Grants, []iam.Grant{iam.GrantWorklet}) {
		t.Fatalf("a question's refusal = %v, want the dataset closed with its reason and grants", err)
	}
	for name, bare := range map[string]error{
		"a bare refusal sentinel":    queries.ErrUnauthorized,
		"a question asked by nobody": queries.ErrUnauthenticated,
	} {
		if err := sourceFailure("items", src, fmt.Errorf("asking: %w", bare)); errors.As(err, &refused) {
			t.Errorf("%s closed the dataset as %s, which no decision said", name, refused.refusal.Reason)
		}
	}
}
