package config

import (
	"errors"
	"strings"
	"testing"
)

// A CEILING BELOW THE EXECUTOR'S LAUNCH FLOOR IS REFUSED, on every scope a seat
// that runs an executor is judged by, where the ceiling is written.
//
// Every executor launch is refused while a capped window of either scope —
// the seat's own, or the company's — has less than
// turn_engine.executor_min_budget_tokens left, and the refusal parks the seat
// until the window turns over. A ceiling below the floor never has the floor
// left, so the seat parks as starved before its first run, in every window it
// will ever open. RUNNABLE for that reason, and refused at the ceiling with
// both remedies named: raise the ceiling, or lower the floor.
//
// Nothing is refused where no launch reads the ceiling — a company with no
// model to run an executor on, a floor that is off — nor at a ceiling the
// floor fits in exactly, which a window's first launch takes.
//
// Mutations: compare with <= rather than <, skip the company's ceilings, or
// judge a company whose seats run no executor, and this goes red.
func TestACeilingBelowTheExecutorFloorIsRefused(t *testing.T) {
	t.Parallel()
	const person = "  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n"
	for _, tc := range []struct {
		name, doc string
		path      string // empty: the document is valid
		says      []string
	}{
		{
			name: "a seat's own day under the default floor",
			doc:  executorCompany(ccEntry, "  - name: SWE\n    token_budget: {day: 20000}\n"),
			path: "roles[0].token_budget.day",
			says: []string{`seat "swe"`, "token_budget.day (20000)",
				"turn_engine.executor_min_budget_tokens (25000)", "at least 25000", "at most 20000"},
		},
		{
			name: "a unit's seat, at its own path",
			doc: executorCompany(ccEntry, person+
				"units:\n  - name: Platform\n    roles:\n      - name: SWE\n        token_budget: {month: 900}\n"),
			path: "units[0].roles[0].token_budget.month",
		},
		{
			name: "the company's week, once for every seat",
			doc: executorCompany(ccEntry, "  - name: CEO\n  - name: PM\n  - name: Ops\n") +
				"token_budget: {week: 1000}\n",
			path: "token_budget.week",
			says: []string{"token_budget.week (1000)", `seat "ceo" and 2 more seats`,
				"at least 25000", "at most 1000", "0 turns the floor off"},
		},
		{
			name: "under a floor the company set",
			doc: executorCompany(ccEntry, "  - name: SWE\n    token_budget: {day: 400}\n") +
				"turn_engine:\n  executor_min_budget_tokens: 500\n",
			path: "roles[0].token_budget.day",
			says: []string{"(500)", "at least 500", "at most 400"},
		},
		{
			name: "a ceiling the floor fits in exactly",
			doc: executorCompany(ccEntry, "  - name: SWE\n    token_budget: {day: 500}\n") +
				"turn_engine:\n  executor_min_budget_tokens: 500\n",
		},
		{
			name: "a floor that is off",
			doc: executorCompany(ccEntry, "  - name: SWE\n    token_budget: {day: 100}\n") +
				"token_budget: {day: 100}\nturn_engine:\n  executor_min_budget_tokens: 0\n",
		},
		{
			name: "no model to run an executor on yet",
			doc:  "name: Acme\nroles:\n  - name: SWE\n    token_budget: {day: 100}\ntoken_budget: {day: 100}\n",
		},
		{
			name: "a company of people runs no executor",
			doc:  "name: Acme\nproviders:\n  llm:\n" + ccEntry + "roles:\n" + person + "token_budget: {day: 100}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.path == "" {
				mustCompany(t, tc.doc)
				return
			}
			err := rejects(t, tc.doc, tc.path)
			if !errors.Is(err, ErrConflict) {
				t.Errorf("want ErrConflict, got %v", err)
			}
			var at []Problem
			for _, p := range Problems(err) {
				if p.Path == tc.path {
					at = append(at, p)
				}
			}
			if len(at) != 1 {
				t.Fatalf("want one problem at %s, got %+v", tc.path, Problems(err))
			}
			for _, said := range tc.says {
				if !strings.Contains(at[0].Message, said) {
					t.Errorf("the refusal does not say %q:\n%s", said, at[0].Message)
				}
			}
			cfg, perr := ParseCompanyDocument([]byte(tc.doc))
			if perr != nil {
				t.Fatal(perr)
			}
			if err := cfg.ValidateRunnable(); err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Errorf("ValidateRunnable() = %v, want a refusal at %s: the seat never launches a run",
					err, tc.path)
			}
		})
	}
}
