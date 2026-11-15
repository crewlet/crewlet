package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/caveat/caveattest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// classedWarnings is every advisory c raises of class.
func classedWarnings(c *config.Company, class config.AdvisoryClass) []config.Warning {
	var out []config.Warning
	for _, w := range c.AdvisoryWarnings() {
		if w.Class == class {
			out = append(out, w)
		}
	}
	return out
}

// Two models on one harness, the shape every case below varies.
const setupModels = "providers:\n  llm:\n" +
	"    big:\n      harness: claude-code\n      model: opus\n" +
	"    big-again:\n      harness: claude-code\n      model: opus\n" +
	"    small:\n      harness: claude-code\n      model: haiku\n"

func TestACompanyWhoseAgentsHoldNoHealthGrantIsAdvised(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  string
		want int
	}{
		{"no agent holds it", "name: Acme\nroles:\n  - name: PM\n  - name: SRE\n", 1},
		{"one agent holds it", "name: Acme\nroles:\n  - name: PM\n  - name: SRE\n    health_tools: true\n", 0},
		{"no agent seats at all", "name: Acme\nroles:\n  - name: Founder\n    kind: human\n" +
			"    contact:\n      slack_user_id: U0FOUNDER\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classedWarnings(parsed(t, tc.doc), config.ClassHealthUnheld)
			if len(got) != tc.want {
				t.Fatalf("health advisories = %+v, want %d", got, tc.want)
			}
			if tc.want == 1 && got[0].Path != "roles[0].health_tools" {
				t.Errorf("path = %q, want the first agent seat's health_tools, where the fix goes", got[0].Path)
			}
		})
	}
}

func TestLearningOnTheExecutorsModelIsAdvised(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		role string
		want bool
	}{
		{"one entry for both", "    llm: big\n    llm_auxiliary: big\n", true},
		{"auxiliary falls back to the executor's chain", "    llm: big\n", true},
		{"two entries, one model", "    llm: big\n    llm_auxiliary: big-again\n", true},
		{"a smaller model", "    llm: big\n    llm_auxiliary: small\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := parsed(t, "name: Acme\n"+setupModels+"roles:\n  - name: PM\n    health_tools: true\n"+tc.role)
			got := classedWarnings(c, config.ClassAuxiliaryModel)
			if (len(got) == 1) != tc.want || len(got) > 1 {
				t.Fatalf("auxiliary advisories = %+v, want advised %v", got, tc.want)
			}
			if tc.want && (got[0].Seat != "pm" || got[0].Path != "roles[0].llm_auxiliary") {
				t.Errorf("advisory = %+v, want it on pm's llm_auxiliary", got[0])
			}
		})
	}
}

func TestAHumanSeatIsNeverAdvisedAboutLearning(t *testing.T) {
	t.Parallel()
	c := parsed(t, "name: Acme\n"+setupModels+"roles:\n  - name: PM\n    llm: big\n    llm_auxiliary: small\n"+
		"  - name: Founder\n    kind: human\n    contact:\n      slack_user_id: U0FOUNDER\n")
	if got := classedWarnings(c, config.ClassAuxiliaryModel); len(got) != 0 {
		t.Errorf("auxiliary advisories = %+v, want none: a person runs no phase", got)
	}
}

func TestAScheduleThatRunsTwelveTimesADayIsAdvised(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cron    string
		enabled string
		want    bool
	}{
		{"0 * * * *", "", true},
		{"0 */2 * * *", "", true},
		{"*/30 * * * *", "", true},
		{"0 9-17 * * 1-5", "", false},
		{"0 9,10 * * 1-5", "", false},
		{"0 */3 * * *", "", false},
		{"30 9 * * 1-5", "", false},
		{"0 9 1 * *", "", false},
		{"0 * * * *", "    enabled: false\n", false},
	} {
		t.Run(tc.cron+tc.enabled, func(t *testing.T) {
			t.Parallel()
			c := parsed(t, "name: Acme\nroles:\n  - name: SRE\n    health_tools: true\n    schedules:\n"+
				"      - name: check\n        cron: \""+tc.cron+"\"\n        task: Look.\n"+
				strings.ReplaceAll(tc.enabled, "    enabled", "        enabled"))
			got := classedWarnings(c, config.ClassScheduleFrequent)
			if (len(got) == 1) != tc.want || len(got) > 1 {
				t.Fatalf("schedule advisories = %+v, want advised %v", got, tc.want)
			}
			if tc.want && (got[0].Path != "roles[0].schedules[0].cron" || got[0].Seat != "sre") {
				t.Errorf("advisory = %+v, want it on the schedule's cron, about sre", got[0])
			}
		})
	}
}

func TestAUnitsScheduleIsAdvisedAboutItsUnit(t *testing.T) {
	t.Parallel()
	c := parsed(t, "name: Acme\nunits:\n  - name: Ops\n    id: ops\n    roles:\n      - name: SRE\n"+
		"        health_tools: true\n    schedules:\n      - name: poll\n        cron: \"0 * * * *\"\n"+
		"        task: Look.\n")
	got := classedWarnings(c, config.ClassScheduleFrequent)
	if len(got) != 1 || got[0].Unit != "Ops" || got[0].Path != "units[0].schedules[0].cron" {
		t.Fatalf("schedule advisories = %+v, want one on Ops's schedule", got)
	}
}

func TestAScheduleIsCountedOnItsBusiestDay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cron string
		want int
		ok   bool
	}{
		{"0 * * * *", 24, true},
		{"0 0 * * *", 1, true},
		{"0 9-17 * * 1-5", 9, true},
		{"0 9 * * 1", 1, true},
		{"0 9 1 * *", 0, true},
		{"not a cron", 0, false},
	} {
		got, ok := config.ScheduleFiresPerDay(org.Schedule{Name: "s", Cron: tc.cron})
		if got != tc.want || ok != tc.ok {
			t.Errorf("ScheduleFiresPerDay(%q) = %v, %v, want %v, %v", tc.cron, got, ok, tc.want, tc.ok)
		}
	}
}

// everyClass is a company that raises every advisory class: two agents
// learning on their own model, neither holding the health grant, one
// schedule firing hourly, and the company hosting code with its boxes in the
// `direct` cell by default.
func everyClass(t *testing.T) *config.Company {
	t.Helper()
	return parsed(t, "name: Acme\n"+setupModels+"  sandbox:\n    local: {}\n    default_run_in: direct\n"+
		"roles:\n  - name: SRE\n    llm: big\n    schedules:\n"+
		"      - name: poll\n        cron: \"0 * * * *\"\n        task: Look.\n"+
		"  - name: Dev\n    llm: big\n"+
		"code:\n  backend: gitlet\n")
}

func TestEveryAdvisoryClassIsValidAndRaisedSomewhere(t *testing.T) {
	t.Parallel()
	c := everyClass(t)
	var raised []config.AdvisoryClass
	for _, w := range c.AdvisoryWarnings() {
		if w.Class != "" {
			raised = append(raised, w.Class)
		}
	}
	for _, class := range config.AdvisoryClasses {
		if !class.Valid() {
			t.Errorf("%q is listed and not valid", class)
		}
		if !slices.Contains(raised, class) {
			t.Errorf("%q is never raised by a company that should raise every class: %v", class, raised)
		}
	}
	if config.AdvisoryClass("something.else").Valid() {
		t.Error("an unlisted class is valid")
	}
}

// EVERY CLASSED ADVISORY SPEAKS TO THE PERSON WHO OWNS THE COMPANY, beside the
// operator's sentence, and no other warning claims to.
//
// The owner reads a warning on the org chart and in a save's review, where the
// operator's sentence put `providers.llm.default`, `llm_auxiliary` and a Tier A
// keyring in front of a founder. So a classed advisory carries Person, worded
// as every sentence a person is shown is (caveattest.Problems: no field, no
// tool, no backtick, no em dash, a full stop), and Message stays the
// operator's, naming the key `crewlet validate` sends them to.
//
// Mutation: drop the person words from [config.classed], or put one back in
// operator vocabulary, and this goes red.
func TestEveryClassedAdvisorySpeaksToThePersonWhoOwnsTheCompany(t *testing.T) {
	t.Parallel()
	seen := map[config.AdvisoryClass]bool{}
	for _, w := range everyClass(t).Warnings() {
		if w.Class == "" {
			if w.Person != "" {
				t.Errorf("an unclassed warning claims a person's words: %+v", w)
			}
			continue
		}
		seen[w.Class] = true
		if w.Person == "" || w.Person == w.Message {
			t.Errorf("%s: no words for a person beside the operator's: %+v", w.Class, w)
			continue
		}
		for _, problem := range caveattest.Problems(w.Person) {
			t.Errorf("%s: its person words are not worded for a person: %s: %q", w.Class, problem, w.Person)
		}
	}
	for _, class := range config.AdvisoryClasses {
		if !seen[class] {
			t.Errorf("%s is never raised here, so its words are not checked", class)
		}
	}
}

// SEVEN AGENTS LEARNING ON THEIR OWN MODEL ARE ONE THING TO KNOW. The person's
// words of a class whose subject is a seat named apart (Warning.Seat) are the
// same for every seat, so a screen groups them and says them once, with how
// many agents; the operator's sentence still names each seat's entry.
//
// Mutation: name the seat in the person words, and the two differ.
func TestOneClassSaysTheSameToAPersonAboutEverySeat(t *testing.T) {
	t.Parallel()
	got := classedWarnings(everyClass(t), config.ClassAuxiliaryModel)
	if len(got) != 2 {
		t.Fatalf("auxiliary advisories = %+v, want one for each of the two agents", got)
	}
	if got[0].Seat == got[1].Seat || got[0].Message == got[1].Message {
		t.Errorf("the two advisories do not name their own seats apart: %+v", got)
	}
	if got[0].Person != got[1].Person {
		t.Errorf("one class says two things to a person: %q and %q", got[0].Person, got[1].Person)
	}
}
