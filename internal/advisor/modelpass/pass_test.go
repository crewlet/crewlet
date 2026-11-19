package modelpass

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// shape is a company whose runs take a round cap of 30, with QA on a daily
// budget of 1,000,000 tokens.
const shape = `name: Acme
providers:
  llm:
    p: {model: m}
  sandbox: {fake: true, default_max_turns: 30}
roles:
  - name: Owner
    handle: owner
    kind: human
    contact: {slack_user_id: U0OWNER}
  - name: Dev
    handle: dev
    llm: p
  - name: QA
    handle: qa
    llm: p
    token_budget: {day: 1000000}
`

var monday = time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)

// planted is a week whose one problem no rule names: one of QA's runs ended on
// its round cap at 900,000 tokens, nine tenths of its daily budget, once (the
// round-cap rule waits for two, or a million).
func planted(t *testing.T) Data {
	t.Helper()
	c, err := config.ParseCompanyDocument([]byte(shape))
	if err != nil {
		t.Fatal(err)
	}
	ev := advisor.Evidence{Until: monday, Runs: []advisor.EndedRun{
		{Seat: "qa", Kind: types.ExecutorErrorMaxTurns, Turn: "turn-9", Tokens: 900_000, Rounds: 30}}}
	answer := advisor.ReadWithEvidence(advisor.Inputs{Company: c, Revision: "r1",
		Integrations: []integration.State{}}, ev)
	for _, f := range answer.Findings {
		if f.Class == advisor.ClassRoundCap {
			t.Fatalf("a rule already lists the planted problem: %+v", f)
		}
	}
	return Data{Company: c, Evidence: ev, Rules: answer.Findings}
}

func submission(t *testing.T, findings ...map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"findings": findings})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func finding(records []map[string]any, facts []map[string]any, fix map[string]any) map[string]any {
	return map[string]any{"title": "One run can spend most of QA's daily budget.",
		"why":      "A run that ends on its cap spends nine tenths of the seat's day, so one more stops it.",
		"severity": "costs", "subject": map[string]any{"kind": "seat", "seat": "qa"},
		"item": "900K of 1M tokens", "records": records, "facts": facts, "fix": fix}
}

// THE ANALYST FINDS A PLANTED PROBLEM NO RULE NAMES, citing records and numbers from its own
// reads, with a change that checks; one citing an unread record, a number its reads lack, or
// nothing, is dropped and counted. Mutation: skip a check in advisor.Ground, and it is listed.
func TestTheModelPassListsOnlyWhatItsOwnReadsShow(t *testing.T) {
	t.Parallel()
	d := planted(t)
	turn := []map[string]any{{"kind": "turn", "id": "turn-9"}}
	facts := []map[string]any{
		{"key": "tokens", "value": 900_000, "from": "executor_runs.runs[0].tokens"},
		{"key": "daily_budget", "value": 1_000_000, "from": "company_outline.seats[2].token_budget.day"},
	}
	raise := map[string]any{"builder": "token_budget", "seat": "qa", "period": "day", "value": 2_000_000}
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "reading", Calls: []llmtest.Call{{Name: "executor_runs", Args: `{}`},
			{Name: "company_outline", Args: `{}`}, {Name: "rule_findings", Args: `{}`}}},
		{Text: "submitting", Calls: []llmtest.Call{{Name: SubmitTool, Args: submission(t,
			finding(turn, facts, raise),
			finding([]map[string]any{{"kind": "item", "id": "ENG-404"}}, facts, raise),
			finding(turn, []map[string]any{{"key": "tokens", "value": 5, "from": "executor_runs.runs[0].tokens"}}, raise),
			finding([]map[string]any{}, facts, raise),
		)}}},
	}}}}
	host, err := toolsession.NewHost()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	var checked []advisor.Change
	got := Run(t.Context(), Deps{Provider: prov, ProviderKey: "p", Host: host, Now: func() time.Time { return monday },
		DryRun: func(ch advisor.Change) error {
			checked = append(checked, ch)
			return advisor.CheckChange(d.Company, ch)
		}}, d, Ceiling)
	if got.Status != advisor.PassDone || len(got.Findings) != 1 {
		t.Fatalf("run = %+v, want one finding listed", got)
	}
	f := got.Findings[0]
	if f.Source != advisor.SourceModel || f.Subject.Seat != "qa" || f.Subject.Name != "QA" ||
		len(f.Records) != 1 || f.Records[0].ID != "turn-9" || len(f.Evidence) != 2 {
		t.Errorf("finding = %+v, want QA's, citing turn-9 and both numbers", f)
	}
	if f.Fix.Kind != advisor.FixConfigChange || len(f.Fix.Options) != 1 ||
		f.Fix.Options[0].Change.Merge["token_budget"].(map[string]any)["day"] != 2_000_000 || len(checked) == 0 {
		t.Errorf("fix = %+v, want QA's daily budget at 2M, checked", f.Fix)
	}
	want := map[advisor.Drop]int{advisor.DropUnread: 1, advisor.DropWrongFact: 1, advisor.DropNoEvidence: 1}
	for drop, n := range want {
		if got.Dropped[drop] != n {
			t.Errorf("dropped %v, want %v", got.Dropped, want)
		}
	}
	if got.InputTokens == 0 {
		t.Error("the run's spend is not recorded")
	}
}

// A RUN THAT SUBMITS NOTHING IT CAN READ IS SAID TO HAVE DECLINED, and a run
// past its ceiling says so; neither is read as a clean list.
func TestARunThatDeclinesOrRunsOutSaysSo(t *testing.T) {
	t.Parallel()
	d := planted(t)
	host, err := toolsession.NewHost()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	declines := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "I would rather not."}, {Text: "I would rather not."}, {Text: "I would rather not."},
		{Text: "I would rather not."}}}}}
	if got := Run(t.Context(), Deps{Provider: declines, ProviderKey: "p", Host: host}, d,
		Ceiling); got.Status != advisor.PassDeclined || len(got.Findings) != 0 || got.Reason == "" {
		t.Errorf("a declining model's run = %+v, want refused, saying so", got)
	}
	reads := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Text: "reading", Calls: []llmtest.Call{{Name: "open_work", Args: `{}`}}},
		{Text: "reading", Calls: []llmtest.Call{{Name: "seat_turns", Args: `{}`}}},
		{Text: "reading", Calls: []llmtest.Call{{Name: "decisions", Args: `{}`}}},
	}}}}
	if got := Run(t.Context(), Deps{Provider: reads, ProviderKey: "p", Host: host}, d,
		150); got.Status != advisor.PassCeiling || got.Reason == "" {
		t.Errorf("a run past its ceiling = %+v, want the ceiling said", got)
	}
}
