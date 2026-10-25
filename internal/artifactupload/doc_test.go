package artifactupload_test

import (
	"errors"
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
		if err != nil || len(page.Contents) != 1 {
			t.Errorf("a search for %q found %d (%v)", q, len(page.Contents), err)
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

// A DOC BEGUN FROM ITS STARTER is one titled tab the engine publishes itself,
// so the dashboard's New › Doc sends no upload.
func TestADocBeginsFromItsStarter(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", h.opKey("start"))
	got, _, err := h.svc.Create(t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindDoc, Title: "On-call handbook", Owner: "ada",
		OwnerKind: c.Authority.Actor.Kind, Starter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := h.version(got.Artifact, 1)
	if v.Manifest.Entry != "index.md" || len(v.Manifest.Files) != 1 ||
		!strings.HasPrefix(h.file(v, "index.md"), "# On\\-call handbook\n") {
		t.Fatalf("the starter published %d files opening on %q: %q", len(v.Manifest.Files),
			v.Manifest.Entry, h.file(v, "index.md"))
	}
}

// A DOC BEGUN WITH ITS TEXT is the starter as its writer wrote it: one tab,
// the title as its heading and the text under it, held to the format and to a
// call's inline bound. The text with no starter, or on a kind that is not a
// doc, is refused before a byte moves.
func TestADocBeginsWithItsWrittenText(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", h.opKey("write"))
	got, _, err := h.svc.Create(t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindDoc, Title: "On-call handbook", Owner: "ada",
		OwnerKind: c.Authority.Actor.Kind, Starter: true,
		Doc: &artifactupload.DocStarter{Body: "## Paging\n\nAnswer within five minutes.\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	v := h.version(got.Artifact, 1)
	want := "# On\\-call handbook\n\n## Paging\n\nAnswer within five minutes.\n"
	if v.Manifest.Entry != "index.md" || len(v.Manifest.Files) != 1 || h.file(v, "index.md") != want {
		t.Fatalf("the doc published %d files opening on %q: %q", len(v.Manifest.Files),
			v.Manifest.Entry, h.file(v, "index.md"))
	}
	for name, req := range map[string]artifactupload.CreateRequest{
		"no starter": {Kind: artifacts.KindDoc, Title: "Doc", Owner: "ada",
			Doc: &artifactupload.DocStarter{Body: "Text"}},
		"a deck": {Kind: artifacts.KindSlides, Title: "Deck", Owner: "ada", Starter: true,
			Doc: &artifactupload.DocStarter{Body: "Text"}},
		"too long": {Kind: artifacts.KindDoc, Title: "Doc", Owner: "ada", Starter: true,
			Doc: &artifactupload.DocStarter{Body: strings.Repeat("a", int(artifacts.MaxInlineBytes))}},
	} {
		req.OwnerKind = c.Authority.Actor.Kind
		if _, _, err := h.svc.Create(t.Context(), h.person("ada", h.opKey(name)), req); !errors.Is(err, artifactupload.ErrInvalid) {
			t.Errorf("%s answered %v", name, err)
		}
	}
}
