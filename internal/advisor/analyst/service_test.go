package analyst

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// gatedModels hands out deps once gate is closed, so a run stays running until the test lets it go.
type gatedModels struct {
	gate <-chan struct{}
	deps Deps
}

func (m gatedModels) Deps(ctx context.Context, _ *config.Company) (Deps, error) {
	select {
	case <-m.gate:
		return m.deps, nil
	case <-ctx.Done():
		return Deps{}, ctx.Err()
	}
}

const serviceShape = `name: Acme
providers:
  llm:
    assistant: {harness: claude-code, model: sonnet, overrides: {web_tools: denied}}
  sandbox: {fake: true, default_max_turns: 30}
advisor:
  analyst:
    llm: [assistant]
roles:
  - name: Owner
    handle: owner
    kind: human
    contact: {slack_user_id: U0OWNER}
  - name: QA
    handle: qa
    llm: assistant
    token_budget: {day: 1000000}
`

// ONE RUN AT A TIME, ACROSS THE FLEET, AND AN ANSWER SETS A FINDING ASIDE: a press mid-run is
// refused, findings are recorded when it ends, a wrong answer drops the finding from the next
// run, and a scheduled run is due once per slot.
func TestOneRunAtATimeAndAnswersSetFindingsAside(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompanyDocument([]byte(serviceShape))
	if err != nil {
		t.Fatal(err)
	}
	ev := advisor.Evidence{Until: monday, Runs: []advisor.EndedRun{
		{Seat: "qa", Kind: types.ExecutorErrorMaxTurns, Turn: "turn-9", Tokens: 900_000, Rounds: 30}}}
	found := map[string]any{"title": "One run can spend most of QA's daily budget.",
		"why": "One more run would stop it.", "severity": "costs", "subject": map[string]any{"kind": "seat", "seat": "qa"},
		"records": []map[string]any{{"kind": "turn", "id": "turn-9"}},
		"facts":   []map[string]any{{"key": "tokens", "value": 900_000, "from": "executor_runs.runs[0].tokens"}},
		"fix":     map[string]any{"builder": "open_seat", "target": "qa"}}
	submit := func() llmtest.Run {
		return llmtest.Run{Responses: []llmtest.Response{
			{Calls: []llmtest.Call{{Name: "executor_runs", Args: `{}`}}},
			{Calls: []llmtest.Call{{Name: SubmitTool, Args: submission(t, found)}}}}}
	}
	host, err := toolsession.NewHost()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	gate := make(chan struct{})
	now := monday.Add(time.Hour)
	svc := &Service{Records: memory.NewFleet(),
		Models: gatedModels{gate: gate, deps: Deps{Provider: &llmtest.Script{Runs: []llmtest.Run{submit(), submit()}},
			ProviderKey: "assistant", Host: host}},
		Read:    func(context.Context, time.Time) (Data, error) { return Data{Company: c, Evidence: ev}, nil },
		Company: func() *config.Company { return c }, Now: func() time.Time { return now },
		Zone: func() *time.Location { return time.UTC }}
	ctx := t.Context()

	if due, err := svc.due(ctx); err != nil || !due {
		t.Fatalf("before any run the schedule is due %v (%v), want due", due, err)
	}
	first, err := svc.Start(ctx, "person", "owner")
	if err != nil || first.Status != advisor.StatusRunning {
		t.Fatalf("Start = %+v (%v), want a claimed run", first, err)
	}
	if _, err := svc.Start(ctx, "schedule", ""); !errors.Is(err, ErrRunning) {
		t.Errorf("a second start while one runs answered %v, want ErrRunning", err)
	}
	close(gate)
	svc.Wait()
	last, ok, err := svc.Last(ctx)
	if err != nil || !ok || last.ID != first.ID || last.Status != advisor.StatusDone || len(last.Findings) != 1 {
		t.Fatalf("the recorded run = %+v (%v), want the claim, done, with its finding", last, err)
	}
	if due, _ := svc.due(ctx); due {
		t.Error("a run started after today's slot left the schedule due")
	}
	now = now.Add(24 * time.Hour)
	if due, _ := svc.due(ctx); !due {
		t.Error("the next day's slot is not due")
	}
	if err := svc.Answer(ctx, "not-listed", advisor.VerdictWrong, "owner"); !errors.Is(err, ErrNoFinding) {
		t.Errorf("an answer to an unlisted finding answered %v", err)
	}
	if err := svc.Answer(ctx, advisor.Fingerprint(last.Findings[0]), advisor.VerdictWrong, "owner"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if _, err := svc.Start(ctx, "person", "owner"); err != nil {
		t.Fatalf("a second run: %v", err)
	}
	svc.Wait()
	again, _, _ := svc.Last(ctx)
	if len(again.Findings) != 0 || again.Dropped[advisor.DropAnswered] != 1 {
		t.Errorf("the next run = %+v, want the finding answered wrong left out and counted", again)
	}
}

// A SLOT IS TODAY'S HOUR, or Monday's for a weekly cadence, never one ahead of now.
func TestASlotIsTheLatestScheduledStart(t *testing.T) {
	t.Parallel()
	wednesday := time.Date(2030, 1, 9, 5, 0, 0, 0, time.UTC)
	daily := config.AdvisorAnalyst{}
	weekly := config.AdvisorAnalyst{Cadence: config.AnalystWeekly}
	for name, c := range map[string]struct {
		a        config.AdvisorAnalyst
		at, want time.Time
	}{
		"before the hour, yesterday's": {daily, wednesday, time.Date(2030, 1, 8, 6, 0, 0, 0, time.UTC)},
		"after the hour, today's":      {daily, wednesday.Add(2 * time.Hour), time.Date(2030, 1, 9, 6, 0, 0, 0, time.UTC)},
		"weekly, this Monday's":        {weekly, wednesday, time.Date(2030, 1, 7, 6, 0, 0, 0, time.UTC)},
		"weekly, Monday before six":    {weekly, time.Date(2030, 1, 7, 5, 0, 0, 0, time.UTC), time.Date(2029, 12, 31, 6, 0, 0, 0, time.UTC)},
	} {
		if got := Slot(c.at, c.a); !got.Equal(c.want) {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// A NODE'S STOP CUTS ITS RUNS SHORT, a person's press too, and returns once each is recorded as
// ended. Mutation: leave a press's run on its own context, and Serve never returns.
func TestANodesStopCutsItsRunsShort(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompanyDocument([]byte(strings.Replace(serviceShape,
		"llm: [assistant]", "llm: [assistant]\n    cadence: \"off\"", 1)))
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Records: memory.NewFleet(), Models: gatedModels{gate: make(chan struct{})},
		Read:    func(context.Context, time.Time) (Data, error) { return Data{Company: c}, nil },
		Company: func() *config.Company { return c }, Now: func() time.Time { return monday },
		Zone: func() *time.Location { return time.UTC }}
	ctx, stop := context.WithCancel(t.Context())
	served := make(chan struct{})
	go func() {
		defer close(served)
		svc.Serve(ctx)
	}()
	press, err := svc.Start(t.Context(), "person", "owner")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stop()
	<-served
	last, _, err := svc.Last(t.Context())
	if err != nil || last.ID != press.ID || last.Status != advisor.StatusFailed {
		t.Fatalf("after the stop the run reads %+v (%v), want the press's, ended failed", last, err)
	}
	if _, err := svc.Start(t.Context(), "person", "owner"); err != nil {
		t.Fatalf("a press after the stop: %v", err)
	}
	if after, _, _ := svc.Last(t.Context()); after.Status != advisor.StatusFailed {
		t.Errorf("a press after the stop reads %s, want it cut short at once", after.Status)
	}
}
