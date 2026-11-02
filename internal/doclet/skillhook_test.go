package doclet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/doclet"
)

// The applier's post-commit hook is the ONLY way the tool-skill registry hears
// about a Doclet page: there is no webhook, and the change feed drops a skill
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
	r.applier = doclet.NewApplier(admission{}, func() { reports++ })
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
	skill := r.write(author("jane"), doclet.NewPage{Title: "Deploying", Body: skillBody})
	ordinary := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
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

// A PAGE'S OWN ANSWER SAYS WHAT IT WAS DERIVED AS. The listing always carried
// the tool-skill mark and the detail never did, so the page a listing row
// opened could not say it was machinery rather than prose — and the screen's
// "loaded as a skill by" line keys on exactly this flag.
func TestAPagesDetailSaysWhetherItIsAToolSkill(t *testing.T) {
	t.Parallel()
	r, _ := hooked(t)
	skill := r.write(author("jane"), doclet.NewPage{Title: "Deploying", Body: skillBody})
	ordinary := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	if got := r.get(skill.Page.ID); !got.Skill {
		t.Errorf("the skill page's detail says skill=false")
	}
	if got := r.get(ordinary.Page.ID); got.Skill || got.Onboarding {
		t.Errorf("an ordinary page's detail says skill=%v onboarding=%v", got.Skill, got.Onboarding)
	}
}

// A RENAMED PAGE IS DERIVED AGAIN, because one of the marks the applier
// derives is a function of the title: a page is the onboarding page when it is
// titled [doclet.OnboardingTitle]. A rename wrote the head and left the derived
// row where the last save put it, so a page renamed to that title was not the
// page every seat reads first, and the onboarding page renamed away from it
// went on being one, until somebody saved its body. Each direction starts from
// a page whose mark its create derived, so each fails on its own.
//
// Mutation: skip the derivation in the rename's apply, and both directions go
// red.
func TestARenamedPageIsDerivedAgain(t *testing.T) {
	t.Parallel()
	r, _ := hooked(t)
	onboarding := func() []string {
		var out []string
		for _, p := range r.list(doclet.Filter{Onboarding: true}).Pages {
			out = append(out, p.ID)
		}
		return out
	}
	retired := r.write(author("jane"), doclet.NewPage{Container: "OPS",
		Title: doclet.OnboardingTitle, Body: "how we used to start"})
	fresh := r.write(author("jane"), doclet.NewPage{Title: "Welcome", Body: "start here"})
	if got := onboarding(); len(got) != 1 || got[0] != retired.Page.ID {
		t.Fatalf("before any rename the onboarding pages are %v, want the one "+
			"created under that title alone", got)
	}

	if _, err := r.store.Rename(t.Context(), author("jane"), retired.Page.ID,
		"Onboarding, retired", false); err != nil {
		t.Fatalf("rename away from the onboarding title: %v", err)
	}
	if _, err := r.store.Rename(t.Context(), author("jane"), fresh.Page.ID,
		doclet.OnboardingTitle, false); err != nil {
		t.Fatalf("rename to the onboarding title: %v", err)
	}
	r.drain()
	if got := onboarding(); len(got) != 1 || got[0] != fresh.Page.ID {
		t.Errorf("after one page was renamed away from %q and another to it, "+
			"the onboarding pages are %v, want %s alone", doclet.OnboardingTitle,
			got, fresh.Page.ID)
	}
	if r.get(retired.Page.ID).Onboarding {
		t.Errorf("the page renamed away from %q still says it is the "+
			"onboarding page", doclet.OnboardingTitle)
	}
	if !r.get(fresh.Page.ID).Onboarding {
		t.Errorf("a page renamed to %q says it is not the onboarding page",
			doclet.OnboardingTitle)
	}
}
