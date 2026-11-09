package config_test

import (
	"slices"
	"strings"
	"testing"
	"time"

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

func TestAScheduleEveryTwoHoursOrMoreOftenIsAdvised(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cron    string
		enabled string
		want    bool
	}{
		{"0 * * * *", "", true},
		{"0 */2 * * *", "", true},
		{"0 9-17 * * 1-5", "", true},
		{"*/30 * * * *", "", true},
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

func TestAScheduleGapIsTheShortestBetweenTwoFires(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cron string
		want time.Duration
		ok   bool
	}{
		{"0 9-17 * * 1-5", time.Hour, true},
		{"30 9 * * 1-5", 24 * time.Hour, true},
		{"0 9 * * 1", 0, false},
		{"not a cron", 0, false},
	} {
		got, ok := config.ScheduleGap(org.Schedule{Name: "s", Cron: tc.cron})
		if got != tc.want || ok != tc.ok {
			t.Errorf("ScheduleGap(%q) = %v, %v, want %v, %v", tc.cron, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEveryAdvisoryClassIsValidAndRaisedSomewhere(t *testing.T) {
	t.Parallel()
	c := parsed(t, "name: Acme\n"+setupModels+"roles:\n  - name: SRE\n    llm: big\n    schedules:\n"+
		"      - name: poll\n        cron: \"0 * * * *\"\n        task: Look.\n")
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
