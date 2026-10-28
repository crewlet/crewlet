package queries_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/period"
)

// assistantBudgetsWire is the budgets answer's `assistant` block as a client
// decodes it.
type assistantBudgetsWire struct {
	Assistant struct {
		Counted bool                 `json:"counted"`
		Windows []types.BudgetWindow `json:"windows"`
	} `json:"assistant"`
}

// THE BUDGETS ANSWER STATES THE CEILING A PERSON'S ASSISTANT IS HELD TO, and
// the viewer's own spend against it only where it is counted.
//
// The ceiling is what refuses a person's question (`assistant.token_budget`),
// and the Budgets screen is where a refusal's remedy sends the person to raise
// it, so the screen must draw the ceiling the gate reads. The spend is a
// person's own only while the company caps it, and a caller who is not a person
// spends nothing through the assistant: there `counted` is false, and the
// zeroes beside it are not a figure.
//
// Mutation: read the ceilings from the company's own `token_budget`, or count
// a machine caller's spend, and a case fails.
func TestBudgetsStateTheAssistantCeilingAndTheViewersOwnSpend(t *testing.T) {
	t.Parallel()
	capped := parsed(t, `
name: Acme
providers:
  llm:
    p: {model: m}
roles:
  - name: CEO
    handle: ceo
    llm: p
token_budget: {month: 40000}
assistant:
  token_budget: {day: 1000, week: 5000}
`)
	uncapped := parsed(t, `
name: Acme
providers:
  llm:
    p: {model: m}
roles:
  - name: CEO
    handle: ceo
    llm: p
token_budget: {month: 40000}
`)
	person := iam.Principal{ID: uuid.New(), Login: "grace.hopper", Kind: iam.KindPerson,
		Seat: "grace", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}}
	machine := iam.Principal{ID: uuid.New(), Login: "ci:release", Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: iam.AllGrants}

	counter := coordmemory.NewFleet()
	if _, err := counter.Charge(t.Context(), coord.ChargeRequest{
		Seat: coord.PersonScope(person.ID), Tokens: 300,
		Windows:  coord.WindowsAt(budgetsAt, time.UTC),
		SeatCaps: coord.Caps{period.Day: 1000, period.Week: 5000},
	}); err != nil {
		t.Fatalf("charge: %v", err)
	}

	for _, c := range []struct {
		name     string
		company  string
		caller   iam.Principal
		counted  bool
		dayLimit *int
		dayUsed  int
	}{
		{"a person under a capped company", "capped", person, true, ceilingOf(1000), 300},
		{"a machine under a capped company", "capped", machine, false, ceilingOf(1000), 0},
		{"a person under an uncapped company", "uncapped", person, false, nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := capped
			if c.company == "uncapped" {
				cfg = uncapped
			}
			r := registryOver(t, queries.Sources{
				State:   livestate.New(),
				Company: companySource(t, cfg),
				Budget:  counter,
				Now:     func() time.Time { return budgetsAt },
			})
			got, err := r.Answer(iam.WithPrincipal(t.Context(), c.caller), "budgets", nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var answer assistantBudgetsWire
			if err := json.Unmarshal(raw, &answer); err != nil {
				t.Fatal(err)
			}
			if answer.Assistant.Counted != c.counted {
				t.Errorf("counted = %v, want %v: %s", answer.Assistant.Counted, c.counted, raw)
			}
			if len(answer.Assistant.Windows) != len(period.Periods) {
				t.Fatalf("assistant windows = %+v, want the day, the week and the month",
					answer.Assistant.Windows)
			}
			day := window(t, answer.Assistant.Windows, period.Day)
			if (day.Limit == nil) != (c.dayLimit == nil) ||
				(day.Limit != nil && *day.Limit != *c.dayLimit) || day.Used != c.dayUsed {
				t.Errorf("assistant day = %+v, want %d used under %v", day, c.dayUsed, c.dayLimit)
			}
			if month := window(t, answer.Assistant.Windows, period.Month); month.Limit != nil {
				t.Errorf("assistant month = %+v carries the company's own ceiling", month)
			}
		})
	}
}

func ceilingOf(v int) *int { return &v }
