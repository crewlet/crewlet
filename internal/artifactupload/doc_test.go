package artifactupload_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// handbookFiles is a doc of two tabs and a picture one of them shows.
func handbookFiles() map[string]string {
	return map[string]string{
		"doc.json": `{"v":1,"tabs":[{"path":"tabs/start.md","title":"Start"},` +
			`{"path":"tabs/oncall.md","title":"On call"}]}`,
		"tabs/start.md":   "# Welcome\n\nRead this first.\n\n![Team](../images/team.png)\n",
		"tabs/oncall.md":  "# On call\n\nPage the escalation lead within ten minutes.\n",
		"images/team.png": "\x89PNG\r\n\x1a\nnot really",
	}
}

// createDoc makes a doc artifact from files, entry doc.json.
func (h *harness) createDoc(c artifactsdomain.Call, files map[string]string) (artifactsdomain.Published, error) {
	h.t.Helper()
	up := h.upload(c, artifacts.ID{}, files)
	got, _, err := h.svc.Create(h.t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindDoc, Title: "Handbook", Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind, Upload: up, Files: specs(files), Entry: "doc.json",
	})
	return got, err
}

// A TABBED DOC PUBLISHES AS ITS AUTHOR WROTE IT, and the gallery searches every
// tab's text, not the index the version opens on.
func TestATabbedDocPublishesAsWritten(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := handbookFiles()
	got, err := h.createDoc(h.agent("writer", "turn-1", h.opKey("create")), files)
	if err != nil {
		t.Fatal(err)
	}
	v := h.version(got.Artifact, 1)
	if v.Manifest.Entry != "doc.json" || len(v.Manifest.Files) != 4 {
		t.Fatalf("the doc's version holds %d files opening on %q", len(v.Manifest.Files), v.Manifest.Entry)
	}
	for _, q := range []string{"Welcome", "escalation lead"} {
		page, err := h.domain.reader.List(t.Context(), artifactsdomain.Viewer{Admin: true},
			artifactsdomain.ListQuery{Q: q})
		if err != nil || len(page.Items) != 1 {
			t.Errorf("a search for %q found %d (%v)", q, len(page.Items), err)
		}
	}
}

// AN INDEX THAT DOES NOT RESOLVE IS REFUSED BEFORE ANYTHING IS PUBLISHED, by the
// rule it broke, under the doc format's code.
func TestADocWhoseIndexDoesNotResolveIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := handbookFiles()
	files["doc.json"] = `{"v":1,"tabs":[{"path":"tabs/start.md","title":"Start"},` +
		`{"path":"tabs/gone.md","title":"Gone"},{"path":"images/team.png","title":"Team"}]}`
	_, err := h.createDoc(h.person("ada", h.opKey("create")), files)
	found := policyFindings(t, err)
	if len(found) != 2 || found[0].Code != lint.CodeDocFormat ||
		!strings.Contains(found[0].Message, "tabs/gone.md") ||
		!strings.Contains(found[1].Message, "a tab is Markdown") {
		t.Fatalf("findings %v", found)
	}
}

// A PATCH IS JUDGED AS THE VERSION IT MAKES: removing a tab's file while the
// index still names it is refused, though the patch itself touched no tab the
// index lists.
func TestAPatchThatOrphansATabIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created, err := h.createDoc(h.agent("writer", "turn-1", h.opKey("create")), handbookFiles())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = h.svc.Publish(t.Context(), h.person("ada", h.opKey("drop")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Files: []artifactupload.FileSpec{{Path: "tabs/oncall.md", Remove: true}},
	})
	if found := policyFindings(t, err); len(found) != 1 || !strings.Contains(found[0].Message, "tabs/oncall.md") {
		t.Fatalf("findings %v", found)
	}
	// And a typo fixed in one tab is one small edit.
	_, _, err = h.svc.Publish(t.Context(), h.person("ada", h.opKey("fix")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Edits: []edits.Edit{{Path: "tabs/oncall.md", Old: "ten minutes", New: "five minutes"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.file(h.version(created.Artifact, 2), "tabs/oncall.md"); !strings.Contains(got, "five minutes") {
		t.Errorf("the edited tab reads %q", got)
	}
}
