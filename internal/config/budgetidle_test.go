package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// idleDocs are company files whose budgets the golden below was taken over.
var idleDocs = []string{
	"token_budget: {day: 1000}\nroles:\n  - name: Dev\n    token_budget: {day: 1000}\n",
	"token_budget: {month: 1000}\nroles:\n  - name: Dev\n    token_budget: {day: 5000}\n",
	"token_budget: {day: 900, week: 700, month: 600}\nroles:\n  - name: Dev\n    token_budget: {day: 5000, week: 5000, month: 5000}\n",
	"token_budget: {week: 1000, month: 800}\nroles:\n  - name: Dev\n    token_budget: {day: 900, week: 1200}\n",
	"token_budget: {day: 10, week: 70}\nunits:\n  - name: Eng\n    roles:\n      - name: Dev\n        token_budget: {day: 10, week: 70, month: 9000}\n      - name: Ana\n        token_budget: {month: 20}\n",
	"token_budget: {month: 1000}\nroles:\n  - name: Dev\n    token_budget: {week: 5000}\n",
	"token_budget: {day: 1000}\nroles:\n  - name: Dev\n    token_budget: {day: 999}\n",
}

// THE WARNINGS A FILE IS GIVEN DID NOT MOVE when the seat-against-company rule
// became [config.TokenBudget.IdleUnder], the one judgement the org chart's
// continuous report asks of a running company too.
//
// THE GOLDEN WAS TAKEN FROM THE CODE BEFORE THE REFACTOR, byte for byte, over
// the boundaries the rule is about: an equal ceiling, a day under the company's
// month, the smallest of several company ceilings, a week the company's month
// does not hold, a seat one token under, and a seat's own pairwise warnings
// beside the company's. A rule written twice drifts on exactly those, which is
// why there is one. Mutation: compare `o >= ceiling` in IdleUnder, or pick the
// largest company ceiling, and this fails.
func TestBudgetWarningsAreUnchangedByTheIdleJudgement(t *testing.T) {
	t.Parallel()
	type warning struct {
		doc                 int
		path, seat, message string
	}
	golden := []warning{
		{0, "roles[0].token_budget.day", "dev", "seat \"dev\"'s token_budget.day (1000) is at or above the company's (1000), so it can never refuse this seat a turn: everything a seat spends is the company's spend too, so the company's ceiling is reached first. Remove it, or lower it below 1000 to hold this seat to a share of the company's"},
		{1, "roles[0].token_budget.day", "dev", "seat \"dev\"'s token_budget.day (5000) is at or above the company's token_budget.month (1000), and a day lies inside one month, so it can never refuse this seat a turn: everything a seat spends is the company's spend too, so the company's ceiling is reached first. Remove it, or lower it below 1000 to hold this seat to a share of the company's"},
		{2, "token_budget.day", "", "token_budget.day (900) is at or above token_budget.week (700), so it can never refuse a turn: a day lies inside one week, whose ceiling is reached first. Remove it, or lower it below 700 to hold one day to a share of the week"},
		{2, "token_budget.day", "", "token_budget.day (900) is at or above token_budget.month (600), so it can never refuse a turn: a day lies inside one month, whose ceiling is reached first. Remove it, or lower it below 600 to hold one day to a share of the month"},
		{2, "token_budget.week", "", "token_budget.week (700) is at or above token_budget.month (600), so it can refuse a turn only in a week that straddles two months: inside one month, the month's ceiling is reached first. Lower it below 600, or check the two are not swapped"},
		{2, "roles[0].token_budget.day", "dev", "token_budget.day (5000) is at or above token_budget.week (5000), so it can never refuse a turn: a day lies inside one week, whose ceiling is reached first. Remove it, or lower it below 5000 to hold one day to a share of the week"},
		{2, "roles[0].token_budget.day", "dev", "token_budget.day (5000) is at or above token_budget.month (5000), so it can never refuse a turn: a day lies inside one month, whose ceiling is reached first. Remove it, or lower it below 5000 to hold one day to a share of the month"},
		{2, "roles[0].token_budget.week", "dev", "token_budget.week (5000) is at or above token_budget.month (5000), so it can refuse a turn only in a week that straddles two months: inside one month, the month's ceiling is reached first. Lower it below 5000, or check the two are not swapped"},
		{2, "roles[0].token_budget.day", "dev", "seat \"dev\"'s token_budget.day (5000) is at or above the company's token_budget.month (600), and a day lies inside one month, so it can never refuse this seat a turn: everything a seat spends is the company's spend too, so the company's ceiling is reached first. Remove it, or lower it below 600 to hold this seat to a share of the company's"},
		{2, "roles[0].token_budget.week", "dev", "seat \"dev\"'s token_budget.week (5000) is at or above the company's (700), so it can never refuse this seat a turn: everything a seat spends is the company's spend too, so the company's ceiling is reached first. Remove it, or lower it below 700 to hold this seat to a share of the company's"},
		{2, "roles[0].token_budget.month", "dev", "seat \"dev\"'s token_budget.month (5000) is at or above the company's (600), so it can never refuse this seat a turn: everything a seat spends is the company's spend too, so the company's ceiling is reached first. Remove it, or lower it below 600 to hold this seat to a share of the company's"},
		{3, "token_budget.week", "", "token_budget.week (1000) is at or above token_budget.month (800), so it can refuse a turn only in a week that straddles two months: inside one month, the month's ceiling is reached first. Lower it below 800, or check the two are not swapped"},
		{3, "roles[0].token_budget.day", "dev", "seat \"dev\"'s token_budget.day (900) is at or above the company's token_budget.month (800), and a day lies inside one month, so it can never refuse this seat a turn: everything a seat spends is the company's spend too, so the company's ceiling is reached first. Remove it, or lower it below 800 to hold this seat to a share of the company's"},
		{3, "roles[0].token_budget.week", "dev", "seat \"dev\"'s token_budget.week (1200) is at or above the company's (1000), so it can never refuse this seat a turn: everything a seat spends is the company's spend too, so the company's ceiling is reached first. Remove it, or lower it below 1000 to hold this seat to a share of the company's"},
		{4, "token_budget.week", "", "token_budget.week (70) can never refuse a turn: a week is 7 days, and 7 at token_budget.day (10) is 70, which it does not exceed. Remove it, or lower it below 70 to hold a week to less than 7 full days"},
		{4, "units[0].roles[0].token_budget.week", "dev", "token_budget.week (70) can never refuse a turn: a week is 7 days, and 7 at token_budget.day (10) is 70, which it does not exceed. Remove it, or lower it below 70 to hold a week to less than 7 full days"},
		{4, "units[0].roles[0].token_budget.month", "dev", "token_budget.month (9000) can never refuse a turn: the longest month is 31 days, and 31 at token_budget.day (10) is 310, which it does not exceed. Remove it, or lower it below 310 to hold a month to less than 31 full days"},
		{4, "units[0].roles[0].token_budget.month", "dev", "token_budget.month (9000) can never refuse a turn: a month overlaps at most 6 ISO weeks, and 6 at token_budget.week (70) is 420, which it does not exceed. Remove it, or lower it below 420 to hold a month to less than 6 full weeks"},
		{4, "units[0].roles[0].token_budget.day", "dev", "seat \"dev\"'s token_budget.day (10) is at or above the company's (10), so it can never refuse this seat a turn: everything a seat spends is the company's spend too, so the company's ceiling is reached first. Remove it, or lower it below 10 to hold this seat to a share of the company's"},
		{4, "units[0].roles[0].token_budget.week", "dev", "seat \"dev\"'s token_budget.week (70) is at or above the company's (70), so it can never refuse this seat a turn: everything a seat spends is the company's spend too, so the company's ceiling is reached first. Remove it, or lower it below 70 to hold this seat to a share of the company's"},
	}
	var got []warning
	for i, doc := range idleDocs {
		cfg, err := config.ParseCompany([]byte("name: Acme\n" + doc))
		if err != nil {
			t.Fatalf("doc %d does not parse: %v", i, err)
		}
		for _, w := range cfg.AdvisoryWarnings() {
			if strings.Contains(w.Path, "token_budget") {
				got = append(got, warning{i, w.Path, w.Seat, w.Message})
			}
		}
	}
	if len(got) != len(golden) {
		t.Fatalf("%d budget warnings, want %d:\n%+v", len(got), len(golden), got)
	}
	for i := range golden {
		if got[i] != golden[i] {
			t.Errorf("warning %d:\n got  %+v\n want %+v", i, got[i], golden[i])
		}
	}
}

// IDLEUNDER ANSWERS THE SAME PAIR WITHOUT A FILE: a seat's ceilings as the org
// chart's runtime half carries them, against the company's settings. Each
// idle ceiling is named once, at the smallest company ceiling that idles it,
// and a ceiling one token under is not idle.
func TestIdleUnderJudgesASeatAgainstTheCompany(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	company := config.TokenBudget{Day: n(900), Week: n(700), Month: n(600)}
	idle := company.IdleUnder(org.TokenCeilings{period.Day: 5000, period.Week: 699, period.Month: 600})
	// THE WEEK IS NOT IDLE: 699 is one token under the company's week, and
	// the company's smaller month does not hold a week, which can straddle
	// two. The day is idled by the month, the smallest of the three company
	// ceilings that hold it, and the month by the month itself.
	want := []config.IdleCeiling{
		{Window: period.Day, Ceiling: 5000, Over: period.Month, Limit: 600},
		{Window: period.Month, Ceiling: 600, Over: period.Month, Limit: 600},
	}
	if len(idle) != len(want) {
		t.Fatalf("IdleUnder = %+v, want %+v", idle, want)
	}
	for i := range want {
		if idle[i] != want[i] {
			t.Errorf("idle %d = %+v, want %+v", i, idle[i], want[i])
		}
	}
	if got := (config.TokenBudget{}).IdleUnder(org.TokenCeilings{period.Day: 1}); got != nil {
		t.Errorf("an uncapped company idles %+v, want nothing", got)
	}
	if got := company.IdleUnder(org.TokenCeilings{period.Day: 0}); got != nil {
		t.Errorf("a refused seat ceiling was judged: %+v", got)
	}
}
