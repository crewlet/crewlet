package plane_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/plane"
)

// tree writes a directory of authored markdown, as an operator would.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

const skillFile = `---
key: gitlab-review
title: Reviewing a merge request
summary: How this company reviews.
phases: [execute]
trigger:
  mcp_server: gitlab
---

# Reviewing

- Read the diff.
- Say what you would change.
`

const docFile = `# Onboarding

Welcome to **Engineering**.

| Step | Owner |
|------|-------|
| Read | you   |
`

func walk(t *testing.T, root string, cfg *config.Plane) *plane.Plan {
	t.Helper()
	plan, err := plane.Walk(root, cfg, cfg.SkillsProjectKey())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return plan
}

// A FILE IS ROUTED BY WHAT IT DECLARES, not by where it sits: a skill filed
// under ENG/ is still a skill, and publishing it there as prose would put
// an instruction meant for one phase of one turn into a planner's context.
func TestASkillIsRoutedByItsTriggerNotItsDirectory(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"ENG/review.md":     skillFile,
		"ENG/onboarding.md": docFile,
	})
	plan := walk(t, root, enabledPlane())
	if len(plan.Items) != 2 {
		t.Fatalf("planned %d items", len(plan.Items))
	}
	var skill, doc plane.Item
	for _, item := range plan.Items {
		if item.Skill {
			skill = item
		} else {
			doc = item
		}
	}
	if skill.Container != config.DefaultSkillsProject {
		t.Errorf("the skill went to %q, not the skills project", skill.Container)
	}
	if skill.ExternalID != plane.SkillExternalID("gitlab-review") {
		t.Errorf("skill external id = %q", skill.ExternalID)
	}
	if doc.Container != "ENG" {
		t.Errorf("the doc went to %q, not its parent directory", doc.Container)
	}
	if doc.ExternalID != plane.DocExternalID("Onboarding") {
		t.Errorf("doc external id = %q", doc.ExternalID)
	}
}

// THE TITLE IS THE H1, because it is half the idempotency key and a
// filename is what an operator renames most casually — a rename would
// orphan the published page and leave a second one beside it.
func TestTheTitleComesFromTheHeadingAndIsRemovedFromTheBody(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{"ENG/anything.md": docFile})
	plan := walk(t, root, enabledPlane())
	item := plan.Items[0]
	if item.Title != "Onboarding" {
		t.Fatalf("title = %q", item.Title)
	}
	// The backend renders the page title itself; leaving the heading
	// prints the same words twice.
	if strings.Contains(item.HTML, "<h1>") {
		t.Errorf("the title heading was published into the body: %s", item.HTML)
	}
	if !strings.Contains(item.HTML, "<strong>Engineering</strong>") {
		t.Errorf("the body was not rendered: %s", item.HTML)
	}
	// GFM, because that is what the authored files are: a table renders
	// as literal pipes under plain CommonMark.
	if !strings.Contains(item.HTML, "<table>") {
		t.Errorf("a GFM table was not rendered: %s", item.HTML)
	}
}

// FRONTMATTER MAY NAME THE CONTAINER, for a doc the tree cannot express.
func TestFrontmatterOverridesTheTitleAndTheProject(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{"misc/thing.md": `---
title: The Charter
project: LEAD
---

# Something Else

Body.
`})
	plan := walk(t, root, enabledPlane())
	item := plan.Items[0]
	if item.Title != "The Charter" || item.Container != "LEAD" {
		t.Errorf("item = %+v", item)
	}
}

// A DOC WITH NO TITLE CANNOT BE FOUND AGAIN, so it is named as the fix
// rather than skipped quietly.
func TestADocWithNoTitleStopsTheWalk(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{"ENG/untitled.md": "Just prose, no heading.\n"})
	_, err := plane.Walk(root, enabledPlane(), config.DefaultSkillsProject)
	if err == nil {
		t.Fatal("a titleless doc was planned")
	}
	if !strings.Contains(err.Error(), "no title") {
		t.Errorf("error = %v", err)
	}
}

// TWO FILES CANNOT PUBLISH AS ONE PAGE: the second would overwrite the
// first on every run, and which one wins depends on walk order.
func TestTwoFilesPublishingAsOnePageStopTheWalk(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"ENG/a.md": "# Onboarding\n\nOne.\n",
		"ENG/b.md": "# Onboarding\n\nTwo.\n",
	})
	_, err := plane.Walk(root, enabledPlane(), config.DefaultSkillsProject)
	if err == nil {
		t.Fatal("two files were planned onto one page")
	}
	if !strings.Contains(err.Error(), "a.md") || !strings.Contains(err.Error(), "b.md") {
		t.Errorf("the error names neither file: %v", err)
	}
}

// THE SKILL PAGE CARRIES ITS FRONTMATTER BACK, which is what the engine
// parses out of it — a page whose block did not round-trip is a skill the
// sync silently declines to load.
func TestASkillPageRoundTripsThroughTheCodec(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{"skills/review.md": skillFile})
	plan := walk(t, root, enabledPlane())
	decoded := plane.DecodeSkillPage(plan.Items[0].HTML)
	if decoded == "" {
		t.Fatalf("the published page decodes to nothing: %s", plan.Items[0].HTML)
	}
	if !strings.Contains(decoded, "key: gitlab-review") {
		t.Errorf("decoded = %q", decoded)
	}
	if !strings.Contains(decoded, "Read the diff.") {
		t.Errorf("the body did not survive: %q", decoded)
	}
}

// ---- publishing --------------------------------------------------------- //

func publish(t *testing.T, f *instance, root string, prune bool) (*plane.PublishResult, error) {
	t.Helper()
	cfg := enabledPlane()
	plan, err := plane.Walk(root, cfg, cfg.SkillsProjectKey())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return plane.Publish(context.Background(), plane.PublishOptions{
		Client: workspaceClient(t, f), Config: cfg, Plan: plan, Prune: prune,
	})
}

func withSkillsProject(f *instance) *instance {
	f.projects = append(f.projects,
		plane.Project{ID: "p-ts", Identifier: config.DefaultSkillsProject, Name: "Tool Skills"})
	return f
}

func TestAnImportPublishesEveryPageOnce(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	root := tree(t, map[string]string{
		"ENG/onboarding.md": docFile,
		"skills/review.md":  skillFile,
	})
	res, err := publish(t, f, root, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(res.Created) != 2 || len(res.Updated) != 0 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(f.pages) != 2 {
		t.Errorf("the workspace holds %d pages", len(f.pages))
	}
}

// A RE-IMPORT UPDATES RATHER THAN DUPLICATING, which is what the external
// id is for.
func TestASecondImportUpdatesTheSamePages(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	root := tree(t, map[string]string{"ENG/onboarding.md": docFile})
	if _, err := publish(t, f, root, false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	res, err := publish(t, f, root, false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if len(res.Created) != 0 || len(res.Updated) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(f.pages) != 1 {
		t.Errorf("the workspace holds %d pages", len(f.pages))
	}
}

// RETITLING A PAGE IN PLANE NEVER ORPHANS IT: the match key is the
// external identity, not the name.
func TestRetitlingAPageInPlaneDoesNotOrphanIt(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	root := tree(t, map[string]string{"ENG/onboarding.md": docFile})
	if _, err := publish(t, f, root, false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	f.mu.Lock()
	for _, page := range f.pages {
		page.Name = "Renamed By A Human"
	}
	f.mu.Unlock()

	res, err := publish(t, f, root, false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if len(res.Updated) != 1 || len(res.Created) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(f.pages) != 1 {
		t.Errorf("the workspace holds %d pages", len(f.pages))
	}
}

// A PAGE SOMEBODY CREATED BY HAND under the same title is ADOPTED rather
// than duplicated, and stamped so the next run finds it by id.
func TestAnUnmarkedPageWithTheSameTitleIsAdopted(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	f.mu.Lock()
	f.pages["page-hand"] = &pageRow{
		ID: "page-hand", Project: "p-eng", Name: "Onboarding",
	}
	f.mu.Unlock()

	root := tree(t, map[string]string{"ENG/onboarding.md": docFile})
	res, err := publish(t, f, root, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(res.Updated) != 1 || len(res.Created) != 0 {
		t.Fatalf("result = %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pages) != 1 {
		t.Fatalf("the workspace holds %d pages", len(f.pages))
	}
	if f.pages["page-hand"].ExternalSource != plane.ExternalSource {
		t.Error("the adopted page was not stamped, so the next run would duplicate it")
	}
}

// A MISSING PROJECT STOPS THE RUN BEFORE A SINGLE PAGE IS WRITTEN: half an
// import looks like a complete knowledge base with holes in it.
func TestAMissingProjectStopsTheImportBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	root := tree(t, map[string]string{"PLTFRM/notes.md": docFile})
	_, err := publish(t, f, root, false)
	if err == nil {
		t.Fatal("an import into a missing project proceeded")
	}
	if !strings.Contains(err.Error(), "PLTFRM") || !strings.Contains(err.Error(), "ENG") {
		t.Errorf("error = %v", err)
	}
	if len(f.pages) != 0 {
		t.Errorf("%d pages were written before the refusal", len(f.pages))
	}
}

// THE IMPORTER NEVER CREATES A PROJECT — a container the whole company
// works in should not be named by a guess.
func TestTheImporterNeverCreatesAProject(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	root := tree(t, map[string]string{"PLTFRM/notes.md": docFile})
	if _, err := publish(t, f, root, false); err == nil {
		t.Fatal("the import proceeded")
	}
	if len(f.writes(http.MethodPost, "/projects/")) != 0 {
		t.Error("the importer created a project")
	}
}

// ONE PAGE'S FAILURE COSTS ONE PAGE. A locked page must not cost the other
// forty — and the run has to say which, so its exit code can be honest.
func TestOnePageFailureDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	f.failPageNamed = "Onboarding"
	root := tree(t, map[string]string{
		"ENG/onboarding.md": docFile,
		"skills/review.md":  skillFile,
	})
	res, err := publish(t, f, root, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(res.Created) != 1 {
		t.Errorf("created = %v", res.Created)
	}
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0], "onboarding.md") {
		t.Errorf("failed = %v", res.Failed)
	}
}

// ---- pruning ------------------------------------------------------------ //

// -prune DELETES SKILL PAGES WHOSE KEY IS GONE, and nothing else.
func TestPruneRemovesOrphanedSkillPagesOnly(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	root := tree(t, map[string]string{
		"skills/review.md":  skillFile,
		"ENG/onboarding.md": docFile,
	})
	if _, err := publish(t, f, root, false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// A skill that used to be published, a doc that used to be, and a
	// page somebody wrote themselves.
	f.mu.Lock()
	f.pages["page-gone"] = &pageRow{ID: "page-gone", Project: "p-ts",
		Name: "Old skill", ExternalID: plane.SkillExternalID("retired"),
		ExternalSource: plane.ExternalSource}
	f.pages["page-doc"] = &pageRow{ID: "page-doc", Project: "p-eng",
		Name: "Old doc", ExternalID: plane.DocExternalID("Old doc"),
		ExternalSource: plane.ExternalSource}
	f.pages["page-theirs"] = &pageRow{ID: "page-theirs", Project: "p-ts",
		Name: "Somebody's notes"}
	f.mu.Unlock()

	res, err := publish(t, f, root, true)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(res.Pruned) != 1 || res.Pruned[0] != "Old skill" {
		t.Fatalf("pruned = %v", res.Pruned)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, still := f.pages["page-doc"]; !still {
		t.Error("a knowledge doc was pruned — it is far more likely to have moved")
	}
	if _, still := f.pages["page-theirs"]; !still {
		t.Error("an unmarked page was pruned")
	}
}

// WITHOUT -prune NOTHING IS DELETED.
func TestWithoutPruneNothingIsDeleted(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	f.mu.Lock()
	f.pages["page-gone"] = &pageRow{ID: "page-gone", Project: "p-ts",
		Name: "Old skill", ExternalID: plane.SkillExternalID("retired"),
		ExternalSource: plane.ExternalSource}
	f.mu.Unlock()

	root := tree(t, map[string]string{"skills/review.md": skillFile})
	res, err := publish(t, f, root, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("pruned %v without -prune", res.Pruned)
	}
}

// A PRUNE THAT CANNOT DELETE PUTS THE PAGE BACK. An archive without its
// delete leaves the page invisible to every agent AND behind an external id
// that refuses every future import of the same skill.
func TestAPruneThatCannotDeleteRestoresTheArchive(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	f.refuseDelete = true
	f.mu.Lock()
	f.pages["page-gone"] = &pageRow{ID: "page-gone", Project: "p-ts",
		Name: "Old skill", ExternalID: plane.SkillExternalID("retired"),
		ExternalSource: plane.ExternalSource}
	f.mu.Unlock()

	root := tree(t, map[string]string{"skills/review.md": skillFile})
	res, err := publish(t, f, root, true)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(res.Pruned) != 0 || len(res.Failed) != 1 {
		t.Fatalf("pruned %v failed %v", res.Pruned, res.Failed)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	page := f.pages["page-gone"]
	if page == nil {
		t.Fatal("the page was deleted after the delete was refused")
	}
	if page.Archived {
		t.Error("a failed prune left the page archived, invisible and 409ing")
	}
}

var _ = json.Marshal

// A PAGE ANOTHER TOOL PUBLISHED under a skill-shaped id is never touched:
// the marker this run keys on is its OWN external_source, not the shape of
// somebody else's identifier.
func TestAPageFromAnotherToolIsNeitherAdoptedNorPruned(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	f.mu.Lock()
	f.pages["page-theirs"] = &pageRow{ID: "page-theirs", Project: "p-ts",
		Name: "Reviewing a merge request",
		// A skill-shaped id from a different tool, under the same title
		// this run publishes.
		ExternalID: plane.SkillExternalID("gitlab-review"), ExternalSource: "another-tool"}
	f.mu.Unlock()

	root := tree(t, map[string]string{"skills/review.md": skillFile})
	res, err := publish(t, f, root, true)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("another tool's page was pruned: %v", res.Pruned)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pages["page-theirs"].ExternalSource != "another-tool" {
		t.Error("another tool's page was adopted")
	}
	if len(f.pages) != 2 {
		t.Errorf("the workspace holds %d pages, want theirs plus ours", len(f.pages))
	}
}

// AN IDENTITY MATCH OUTRANKS A NAME MATCH. Somebody retitling an unrelated
// page to match must not steer this run's write away from its own page.
func TestAnIdentityMatchWinsOverATitleCollision(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	root := tree(t, map[string]string{"ENG/onboarding.md": docFile})
	if _, err := publish(t, f, root, false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	f.mu.Lock()
	var ours string
	for id := range f.pages {
		ours = id
	}
	// An unrelated page somebody titled the same thing.
	f.pages["page-decoy"] = &pageRow{ID: "page-decoy", Project: "p-eng", Name: "Onboarding"}
	f.mu.Unlock()

	if _, err := publish(t, f, root, false); err != nil {
		t.Fatalf("second import: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pages["page-decoy"].ExternalSource != "" {
		t.Error("the decoy was written to instead of this run's own page")
	}
	if f.pages[ours].ExternalID != plane.DocExternalID("Onboarding") {
		t.Error("this run's own page was not the one updated")
	}
}

// A PAGE THIS TOOL MANAGES UNDER A DIFFERENT IDENTITY is never adopted by
// title: two items would then fight over one page for ever.
func TestAManagedPageWithAnotherIdentityIsNotAdoptedByTitle(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	f.mu.Lock()
	f.pages["page-other"] = &pageRow{ID: "page-other", Project: "p-eng",
		Name: "Onboarding", ExternalID: plane.DocExternalID("Something Else"),
		ExternalSource: plane.ExternalSource}
	f.mu.Unlock()

	root := tree(t, map[string]string{"ENG/onboarding.md": docFile})
	res, err := publish(t, f, root, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(res.Created) != 1 || len(res.Updated) != 0 {
		t.Fatalf("result = %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pages["page-other"].ExternalID != plane.DocExternalID("Something Else") {
		t.Error("a page managed under another identity was taken over")
	}
}

// AN ORPHAN ALREADY ARCHIVED IS STILL DELETED. Left archived it keeps its
// external id, which 409s every future import of the same skill — the exact
// state a failed prune exists to avoid.
func TestPruneFinishesAnOrphanSomebodyAlreadyArchived(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	f.mu.Lock()
	f.pages["page-gone"] = &pageRow{ID: "page-gone", Project: "p-ts",
		Name: "Old skill", ExternalID: plane.SkillExternalID("retired"),
		ExternalSource: plane.ExternalSource, Archived: true}
	f.mu.Unlock()

	root := tree(t, map[string]string{"skills/review.md": skillFile})
	res, err := publish(t, f, root, true)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(res.Pruned) != 1 {
		t.Fatalf("pruned = %v, failed = %v", res.Pruned, res.Failed)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, still := f.pages["page-gone"]; still {
		t.Error("an already-archived orphan was left holding its external id")
	}
}

// TWO UNCLAIMED PAGES SHARING A TITLE resolve the same way on every run.
// Map iteration order would otherwise make the adopted page a coin flip,
// and the other one would be written to on the next import.
func TestATitleCollisionAmongUnclaimedPagesResolvesTheSameWayTwice(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{"ENG/onboarding.md": docFile})

	var adopted []string
	for range 2 {
		f := withSkillsProject(newInstance())
		f.mu.Lock()
		// SEVERAL, not two: the server returns them in no order, so a
		// two-page collision would be resolved correctly by chance
		// often enough to hide an index that does not sort.
		for _, id := range []string{
			"page-aaa", "page-bbb", "page-ccc", "page-ddd", "page-eee", "page-fff",
		} {
			f.pages[id] = &pageRow{ID: id, Project: "p-eng", Name: "Onboarding"}
		}
		f.mu.Unlock()

		if _, err := publish(t, f, root, false); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		f.mu.Lock()
		for id, page := range f.pages {
			if page.ExternalSource == plane.ExternalSource {
				adopted = append(adopted, id)
			}
		}
		f.mu.Unlock()
	}
	// THE LOWEST ID WINS, pinned rather than merely stable: "some page,
	// consistently" is still a rule nobody can predict from the outside,
	// and the page that is NOT adopted is the one a later import would
	// write to if the tie-break ever moved.
	if len(adopted) != 2 || adopted[0] != "page-aaa" || adopted[1] != "page-aaa" {
		t.Errorf("adopted = %v, want page-aaa both times", adopted)
	}
}

// resync READS THE SAME PAGES THE ENGINE WOULD, through the same walk and
// the same admission: a report that could disagree with what the engine
// loads would answer "why is this skill not applied" with a lie.
func TestSkillPagesReadsWhatTheEngineWouldLoad(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	root := tree(t, map[string]string{"skills/review.md": skillFile})
	if _, err := publish(t, f, root, false); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// An ordinary page in the same project — a project home page, an
	// operator's notes. Expected, and not a failure.
	f.mu.Lock()
	f.pages["page-notes"] = &pageRow{ID: "page-notes", Project: "p-ts",
		Name: "Notes", HTML: "<p>Just notes.</p>"}
	f.mu.Unlock()

	pages, err := plane.SkillPages(context.Background(),
		workspaceClient(t, f), config.DefaultSkillsProject)
	if err != nil {
		t.Fatalf("SkillPages: %v", err)
	}
	loaded, report := skills.Admit(pages)
	if len(loaded) != 1 || loaded[0].Key != "gitlab-review" {
		t.Fatalf("loaded %+v", loaded)
	}
	if report.Ordinary != 1 {
		t.Errorf("ordinary = %d", report.Ordinary)
	}
	if len(report.Undecodable) != 0 {
		t.Errorf("undecodable = %v", report.Undecodable)
	}
}

// A MISSING SKILLS PROJECT IS NAMED, because no tool skill could ever load
// from it and the symptom otherwise is agents ignoring conventions.
func TestSkillPagesNamesAMissingProject(t *testing.T) {
	t.Parallel()
	f := newInstance() // no skills project
	_, err := plane.SkillPages(context.Background(),
		workspaceClient(t, f), config.DefaultSkillsProject)
	if err == nil {
		t.Fatal("a missing skills project read as empty")
	}
	if !strings.Contains(err.Error(), "ENG") {
		t.Errorf("the error does not say what the workspace has: %v", err)
	}
}

// A PAGE THAT DECLARES A TRIGGER AND DOES NOT PARSE is reported: somebody
// wrote a trigger and got the rest wrong, and the only other symptom is
// guidance that never appears.
func TestAnUndecodableSkillPageIsNamed(t *testing.T) {
	t.Parallel()
	f := withSkillsProject(newInstance())
	f.mu.Lock()
	f.pages["page-broken"] = &pageRow{ID: "page-broken", Project: "p-ts",
		Name: "Broken skill",
		HTML: plane.EncodeSkillPage("key: broken\ntrigger:\n  tool: x\n  mcp_server: y\n",
			"<p>Body.</p>")}
	f.mu.Unlock()

	pages, err := plane.SkillPages(context.Background(),
		workspaceClient(t, f), config.DefaultSkillsProject)
	if err != nil {
		t.Fatalf("SkillPages: %v", err)
	}
	loaded, report := skills.Admit(pages)
	if len(loaded) != 0 {
		t.Fatalf("loaded %+v", loaded)
	}
	if len(report.Undecodable) != 1 || report.Undecodable[0] != "Broken skill" {
		t.Errorf("undecodable = %v", report.Undecodable)
	}
}

// TOOL SKILLS OFF MEANS A SKILL FILE HAS NOWHERE TO GO, and filing it under
// its parent directory's project instead would put an instruction written for
// one phase of one turn into every planner's knowledge search.
func TestASkillFileWithNoSkillsProjectStopsTheWalk(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{"skills/review.md": skillFile})
	_, err := plane.Walk(root, enabledPlane(), "")
	if err == nil {
		t.Fatal("a skill was planned with tool skills off")
	}
	if !strings.Contains(err.Error(), "skills_project") ||
		!strings.Contains(err.Error(), "-project") {
		t.Errorf("the error names neither the setting nor the flag: %v", err)
	}
}

// AN ORDINARY DOC IS UNAFFECTED. Turning tool skills off must not stop a
// company publishing its knowledge base.
func TestOrdinaryDocsPublishWithTheSkillsProjectOff(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{"ENG/onboarding.md": "# Onboarding\n\nProse.\n"})
	plan, err := plane.Walk(root, enabledPlane(), "")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Container != "ENG" {
		t.Errorf("plan = %+v", plan.Items)
	}
}

// THE CONTAINER THE CALLER NAMES IS THE ONE USED, which is what makes
// `-project` a per-run override rather than decoration.
func TestTheSkillsProjectTheCallerNamesIsWhereSkillsGo(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{"skills/review.md": skillFile})
	plan, err := plane.Walk(root, enabledPlane(), "OVERRIDE")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Container != "OVERRIDE" {
		t.Errorf("plan = %+v", plan.Items)
	}
}
