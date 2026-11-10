package advisor_test

import (
	"encoding/json"
	"go/build"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

func company(t *testing.T, doc string) *config.Company {
	t.Helper()
	c, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// read is the advisor's answer about c alone, with every integration read
// and none reporting anything.
func read(c *config.Company, revision string) advisor.Answer {
	return advisor.Read(advisor.Inputs{Company: c, Revision: revision, Integrations: []integration.State{}})
}

// liveShape is a company shaped like one that ran two weeks: six agent seats and
// one person, every phase on one model, no agent holding the health grant, and
// three schedules that poll every one or two hours.
func liveShape(t *testing.T) *config.Company {
	t.Helper()
	doc, err := os.ReadFile("testdata/live-shape.company.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return company(t, string(doc))
}

// owned is the findings of the classes a fix is built for, as class:subject.
func owned(a advisor.Answer) []string {
	var out []string
	for _, f := range a.Findings {
		if f.Fix.Kind != advisor.FixConfigChange {
			continue
		}
		subject := f.Subject.Seat + f.Subject.Unit
		if f.Subject.Schedule != "" {
			subject += "/" + f.Subject.Schedule
		}
		out = append(out, string(f.Class)+":"+subject)
	}
	slices.Sort(out)
	return out
}

func TestTheLiveShapeRaisesExactlyTheStaticClasses(t *testing.T) {
	t.Parallel()
	got := owned(read(liveShape(t), "r1"))
	want := []string{
		"grant.health_unheld:",
		"learning.auxiliary_model:app", "learning.auxiliary_model:backend",
		"learning.auxiliary_model:designer", "learning.auxiliary_model:pm",
		"learning.auxiliary_model:qa", "learning.auxiliary_model:sre",
		"schedule.frequent:sre/health-check", "schedule.frequent:sre/post-release",
		"schedule.frequent:sre/release-window",
	}
	if !slices.Equal(got, want) {
		t.Errorf("findings with a change =\n  %v\nwant\n  %v", got, want)
	}
}

func TestFindingsAreListedTheMostCostlyFirst(t *testing.T) {
	t.Parallel()
	a := read(liveShape(t), "r1")
	if len(a.Findings) == 0 || a.Findings[0].Class != advisor.ClassHealthUnheld {
		t.Fatalf("first finding = %+v, want the costly health finding ahead of every note", a.Findings)
	}
	if a.Revision != "r1" {
		t.Errorf("revision = %q, want the one the findings were read at", a.Revision)
	}
}

// apply sends a change the way the dashboard does, in memory: the entity as
// the config surface reads it, Set's fields replaced, decoded back in place.
// decoded is v as JSON decodes it, the shape the dashboard reads a change in.
func decoded(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// mergeInto merges from into into as the dashboard does: nested objects key
// by key, anything else replaced.
func mergeInto(into, from map[string]any) map[string]any {
	out := maps.Clone(into)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range from {
		had, hadObject := out[k].(map[string]any)
		if next, isObject := v.(map[string]any); isObject && hadObject {
			out[k] = mergeInto(had, next)
			continue
		}
		out[k] = v
	}
	return out
}

func apply(t *testing.T, c *config.Company, ch advisor.Change) {
	t.Helper()
	replace := func(entity any) {
		raw, err := json.Marshal(entity)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		fields = mergeInto(fields, decoded(t, ch.Merge))
		for k, v := range decoded(t, ch.Set) {
			fields[k] = v
		}
		merged, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(merged, entity); err != nil {
			t.Fatalf("the change does not decode as its entity: %v", err)
		}
	}
	switch ch.Entity {
	case advisor.EntityRoles:
		for role := range c.EachRole() {
			if role.Seat().Handle() == ch.ID {
				replace(role)
				return
			}
		}
	case advisor.EntityUnits:
		var walk func(units []config.Unit) bool
		walk = func(units []config.Unit) bool {
			for i := range units {
				if units[i].ID == ch.ID {
					replace(&units[i])
					return true
				}
				if walk(units[i].Children) {
					return true
				}
			}
			return false
		}
		if walk(c.Units) {
			return
		}
	}
	t.Fatalf("change %+v names no entity", ch)
}

func TestEveryOptionOfEveryChangeClearsItsFindingAndBreaksNoRule(t *testing.T) {
	t.Parallel()
	for _, f := range read(liveShape(t), "r1").Findings {
		if f.Fix.Kind != advisor.FixConfigChange {
			continue
		}
		if len(f.Fix.Options) == 0 {
			t.Errorf("%s about %+v offers no option", f.Class, f.Subject)
		}
		for _, o := range f.Fix.Options {
			t.Run(string(f.Class)+"/"+o.Label, func(t *testing.T) {
				c := liveShape(t)
				before := len(config.Problems(c.ValidateAdmission()))
				apply(t, c, o.Change)
				if after := len(config.Problems(c.ValidateAdmission())); after > before {
					t.Errorf("the change breaks an admission rule: %v", c.ValidateAdmission())
				}
				if o.Change.Summary == "" {
					t.Error("the change carries no revision summary")
				}
				for _, g := range read(c, "r2").Findings {
					if g.Class == f.Class && g.Subject == f.Subject {
						t.Errorf("after the change the finding is still raised: %+v", g)
					}
				}
			})
		}
	}
}

func TestEveryClassHasWordsAndAFix(t *testing.T) {
	t.Parallel()
	c := liveShape(t)
	c.Roles[0].Manages = []string{"ghost"}
	seen := map[advisor.Class]bool{}
	for _, f := range read(c, "r1").Findings {
		seen[f.Class] = true
		if f.Title == "" || f.Why == "" || f.Fix.Sentence == "" || f.Fix.Kind == "" {
			t.Errorf("%s: a finding without its words or fix: %+v", f.Class, f)
		}
		if f.Fix.Kind == advisor.FixOpenSetting && f.Fix.Setting == "" {
			t.Errorf("%s: opens no setting", f.Class)
		}
		for _, s := range []string{f.Title, f.Why, f.Fix.Sentence, f.Fix.Unchanged} {
			if strings.ContainsRune(s, '\u2014') {
				t.Errorf("%s: %q holds an em dash", f.Class, s)
			}
		}
	}
	for _, class := range config.AdvisoryClasses {
		if !seen[advisor.Class(class)] {
			t.Errorf("%s is never read into a finding", class)
		}
	}
	if !seen[advisor.ClassConfigReference] {
		t.Error("a dangling reference is not listed")
	}
}

func TestAHealthChangeIsOfferedForEveryAgentAndNoPerson(t *testing.T) {
	t.Parallel()
	for _, f := range read(liveShape(t), "r1").Findings {
		if f.Class != advisor.ClassHealthUnheld {
			continue
		}
		var ids []string
		for _, o := range f.Fix.Options {
			ids = append(ids, o.Change.ID)
		}
		if want := []string{"pm", "designer", "app", "backend", "qa", "sre"}; !slices.Equal(ids, want) {
			t.Errorf("options = %v, want every agent in the chart's order, %v", ids, want)
		}
		if f.Subject.Kind != advisor.SubjectCompany {
			t.Errorf("subject = %+v, want the company: no one seat is at fault", f.Subject)
		}
		return
	}
	t.Fatal("no health finding")
}

func TestALearningChangeIsNeverToTheModelItAlreadyRunsOn(t *testing.T) {
	t.Parallel()
	for _, f := range read(liveShape(t), "r1").Findings {
		if f.Class != advisor.ClassAuxiliaryModel {
			continue
		}
		if len(f.Fix.Options) != 1 || f.Fix.Options[0].Change.Set["llm_auxiliary"] == nil {
			t.Fatalf("options = %+v, want the one smaller model", f.Fix.Options)
		}
	}
	only := company(t, "name: Acme\nproviders:\n  llm:\n    default:\n      harness: claude-code\n"+
		"      model: opus\nroles:\n  - name: PM\n    health_tools: true\n")
	for _, f := range read(only, "r1").Findings {
		if f.Class == advisor.ClassAuxiliaryModel &&
			(f.Fix.Kind != advisor.FixOpenSetting || f.Fix.Setting != "providers.llm") {
			t.Errorf("with one model the fix = %+v, want the models opened to add a smaller one", f.Fix)
		}
	}
}

func TestAUnitsScheduleIsChangedOnItsUnit(t *testing.T) {
	t.Parallel()
	c := company(t, "name: Acme\nunits:\n  - name: Ops\n    id: ops\n    children:\n      - name: Night\n"+
		"        id: night\n        roles:\n          - name: SRE\n            health_tools: true\n"+
		"        schedules:\n          - name: poll\n            cron: \"0 * * * 1-5\"\n            task: Look.\n")
	for _, f := range read(c, "r1").Findings {
		if f.Class != advisor.ClassScheduleFrequent {
			continue
		}
		if f.Subject.Unit != "night" || f.Subject.Schedule != "poll" {
			t.Fatalf("subject = %+v, want the child unit's schedule", f.Subject)
		}
		daily := f.Fix.Options[0].Change
		if daily.Entity != advisor.EntityUnits || daily.ID != "night" {
			t.Errorf("change = %+v, want a write of the child unit", daily)
		}
		raw, _ := json.Marshal(daily.Set["schedules"])
		if !strings.Contains(string(raw), `"cron":"0 9 * * 1-5"`) {
			t.Errorf("daily change = %s, want the same weekdays at 09:00", raw)
		}
		return
	}
	t.Fatal("no schedule finding")
}

func TestTheChangeEntitiesAreTheConfigSurfacesOwn(t *testing.T) {
	t.Parallel()
	if advisor.EntityRoles != configapi.EntityRoles || advisor.EntityUnits != configapi.EntityUnits {
		t.Errorf("entities %q and %q, want the config surface's %q and %q",
			advisor.EntityRoles, advisor.EntityUnits, configapi.EntityRoles, configapi.EntityUnits)
	}
}

// THE ADVISOR WRITES NOTHING. A fix is data an administrator sends; a
// package that could reach a store, the coordination store, a state log or
// the config surface's writes could apply one itself.
func TestTheAdvisorImportsNoWritePath(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		for _, banned := range []string{"internal/api", "internal/store", "internal/statelog",
			"internal/coord", "internal/queue", "net/http"} {
			if strings.Contains(imp, banned) {
				t.Errorf("internal/advisor imports %s: a finding's fix is sent by a person, never by this package", imp)
			}
		}
	}
}

// A SCHEDULE WHOSE FIRES RARELY MOVE WORK IS THE OWNER'S COST, with
// schedule.frequent's fix; a frequent schedule says what its fires did and how
// many turns it starts in a plan window, and one that is also unproductive is
// said once, as the cost. An unread source adds nothing.
func TestAScheduleWhoseFiresRarelyMoveWorkIsTheOwnersCost(t *testing.T) {
	t.Parallel()
	c := liveShape(t)
	fires := func(name string, n, moved int) []advisor.ScheduleFire {
		var out []advisor.ScheduleFire
		for i := range n {
			out = append(out, advisor.ScheduleFire{Seat: "sre", Name: name, Moved: i < moved})
		}
		return out
	}
	ev := advisor.Evidence{Fires: append(fires("health-check", 12, 1), fires("post-release", 8, 2)...)}
	answer := advisor.ReadWithEvidence(advisor.Inputs{Company: c, Revision: "r1"}, ev)
	of := func(class advisor.Class, schedule string) *advisor.Finding {
		for i, f := range answer.Findings {
			if f.Class == class && f.Subject.Seat == "sre" && f.Subject.Schedule == schedule {
				return &answer.Findings[i]
			}
		}
		return nil
	}
	cost := of(advisor.ClassScheduleUnproductive, "health-check")
	if cost == nil || cost.Severity != advisor.SeverityCosts || cost.Actor != advisor.ActorOwner ||
		cost.Title != "health-check ran 12 times and changed something once." ||
		!strings.Contains(cost.Why, "11 of them moved no work") ||
		!strings.Contains(cost.Why, "about 0.4 turns in every five-hour plan window") ||
		cost.Fix.Kind != advisor.FixConfigChange || len(cost.Fix.Options) != 2 {
		t.Errorf("the health check's cost = %+v, want the owner's, its fires said, with the frequent fix", cost)
	}
	if of(advisor.ClassScheduleFrequent, "health-check") != nil {
		t.Error("an unproductive schedule was said twice, as frequent too")
	}
	if of(advisor.ClassScheduleUnproductive, "post-release") != nil {
		t.Error("a schedule a quarter of whose fires moved work was called unproductive")
	}
	frequent := of(advisor.ClassScheduleFrequent, "post-release")
	if frequent == nil || !strings.Contains(frequent.Why, "it fired 8 times and 2 of them (25%) moved any work") ||
		!strings.Contains(frequent.Why, "turns in every five-hour plan window") {
		t.Errorf("post-release's frequent note = %+v, want what its fires did and its turns per window", frequent)
	}
	ev.FiresUnread = true
	unread := advisor.ReadWithEvidence(advisor.Inputs{Company: c, Revision: "r1"}, ev)
	for _, f := range unread.Findings {
		if f.Class == advisor.ClassScheduleUnproductive || strings.Contains(f.Why, "In the last 7 days it fired") {
			t.Errorf("an unread source raised %+v", f)
		}
	}
	if !slices.Contains(unread.Unread, advisor.UnreadFires) {
		t.Errorf("unread = %v, want the fires named", unread.Unread)
	}
}
