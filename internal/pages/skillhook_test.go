package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/pages"
)

// The applier's post-commit hook is the ONLY way the tool-skill registry hears
// about a native page: there is no webhook, and the change feed drops a skill
// page's changes on purpose. So every write that can change what the skills
// container holds must fire it, and a write that cannot must not, or every
// edit in the wiki would cost a walk of the skills container.

// admission is the registry's own admission test, the one the engine wires.
type admission struct{}

func (admission) IsSkill(body string) bool { return skills.IsSkill(body) }

// hooked is a round trip whose applier reports skill pages, counting reports.
func hooked(t *testing.T) (*roundTrip, *int) {
	t.Helper()
	r := newRoundTrip(t)
	reports := 0
	r.applier = pages.NewApplier("node-a", admission{}, func() { reports++ })
	return r, &reports
}

const skillBody = "---\nkey: deploy\ntitle: Deploying\nsummary: how this company deploys\n" +
	"trigger:\n  tool: deploy\n---\nTag the release before you announce it."

// A PURGED SKILL PAGE IS REPORTED, and a purged ordinary page is not.
//
// A purge writes no head, so it never reached the one place every other write
// reports a skill page from, and the registry went on serving a page that no
// longer existed on any node until something else made it read the container.
func TestAPurgedSkillPageIsReportedToTheRegistry(t *testing.T) {
	t.Parallel()
	r, reports := hooked(t)
	skill := r.write(author("jane"), pages.NewPage{Title: "Deploying", Body: skillBody})
	ordinary := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	if *reports != 1 {
		t.Fatalf("writing one skill page and one ordinary page reported %d "+
			"times, want once", *reports)
	}

	if _, err := r.store.Purge(t.Context(), author("jane"), ordinary.Page.ID,
		"written in the wrong space"); err != nil {
		t.Fatalf("purge the ordinary page: %v", err)
	}
	r.drain()
	if *reports != 1 {
		t.Fatalf("purging an ordinary page was reported as a skill change")
	}

	if _, err := r.store.Purge(t.Context(), author("jane"), skill.Page.ID,
		"retired procedure"); err != nil {
		t.Fatalf("purge the skill page: %v", err)
	}
	r.drain()
	if *reports != 2 {
		t.Fatalf("purging a skill page was not reported, so the registry keeps "+
			"serving it (%d reports)", *reports)
	}
}
