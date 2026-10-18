package artifactupload_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/objstore"
)

const page = `<!doctype html><html><head><meta name="viewport" content="width=device-width">
<style>@media (prefers-color-scheme: dark) { body { background: #111 } }</style>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"></script>
</head><body><h1>Quarterly plan</h1><button onclick="go()">Go</button>
<script>function go() { document.title = "go"; }</script></body></html>`

// A CREATE RUNS THE WHOLE PIPELINE: the page is linted (a nonce placeholder on
// every script, the CDN file copied into the version and the page pointing at
// the copy), its policy recorded on its entry, its text indexed, and the newest
// runtime pinned.
func TestACreateLintsCopiesAndPins(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", h.opKey("create"))
	got := h.create(c, map[string]string{"index.html": page, "app.css": "body { margin: 0 }"})
	v := h.version(got.Artifact, 1)
	if v.Contract != "1.0.0" {
		t.Errorf("the version pins %q, want the newest runtime", v.Contract)
	}
	stored := h.file(v, "index.html")
	if strings.Count(stored, artifacts.NoncePlaceholder) != 2 {
		t.Errorf("the stored page carries %d placeholders, want one per script:\n%s",
			strings.Count(stored, artifacts.NoncePlaceholder), stored)
	}
	copyPath := "_vendor/cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"
	if !strings.Contains(stored, `src="`+copyPath+`"`) {
		t.Errorf("the page does not point at the version's copy:\n%s", stored)
	}
	if got := h.file(v, copyPath); got != "/* /npm/chart.js@4.4.1/dist/chart.umd.min.js */" {
		t.Errorf("the copy holds %q", got)
	}
	e, _ := manifest.EntryAt(v.Manifest, "index.html")
	if e.Script == nil || !e.Script.Nonced || len(e.Script.HandlerHashes) != 1 {
		t.Errorf("the entry's policy is %+v, want nonced with one handler", e.Script)
	}
	if _, ok := manifest.EntryAt(v.Manifest, "app.css"); !ok {
		t.Error("the stylesheet is not in the version")
	}
	if h.held() != 0 {
		t.Errorf("a create that landed leaves %d bytes held", h.held())
	}
	if n := h.unnamedBytes(); n != 0 {
		t.Errorf("a create that landed leaves %d bytes nothing names in the store", n)
	}
}

// AN EDIT APPLIES TO THE AUTHOR'S SOURCE OF THE BASE VERSION, the placeholders
// the linter wrote removed, and the result is linted again.
func TestAnEditAppliesToTheBaseVersionsSource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", h.opKey("create"))
	created := h.create(c, map[string]string{"index.html": page})
	next, report, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("publish")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Edits: []edits.Edit{{Old: `<script>function go()`, New: `<script>function go2()`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Number != 2 {
		t.Fatalf("the edit published version %d", next.Number)
	}
	v := h.version(created.Artifact, 2)
	stored := h.file(v, "index.html")
	if !strings.Contains(stored, "<script"+lint.NonceAttr+">function go2()") {
		t.Fatalf("the edited page reads\n%s", stored)
	}
	if _, ok := manifest.EntryAt(v.Manifest, "_vendor/cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"); !ok {
		t.Fatal("the page's copy of its CDN file was dropped by a patch")
	}
	_ = report
	if h.unnamedBytes() != 0 || h.held() != 0 {
		t.Fatalf("an edit leaves %d unnamed bytes and %d held", h.unnamedBytes(), h.held())
	}
}

// A RETRY OF A PUBLISH THAT LANDED IS ANSWERED WITH WHAT IT LANDED, and makes
// no second version.
func TestARetriedPublishIsAnsweredWithItsFirstOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": page})
	c := h.person("ada", h.opKey("publish"))
	up := h.upload(c, created.Artifact, map[string]string{"notes.txt": "first notes"})
	req := artifactupload.PublishRequest{Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Upload: up, Files: []artifactupload.FileSpec{{Path: "notes.txt", Upload: true}}}
	first, _, err := h.svc.Publish(t.Context(), c, req)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := h.svc.Publish(t.Context(), c, req)
	if err != nil {
		t.Fatalf("the retry was refused: %v", err)
	}
	if again.Number != first.Number || again.VersionID != first.VersionID {
		t.Fatalf("the retry answered version %d (%s), the first %d (%s)", again.Number,
			again.VersionID, first.Number, first.VersionID)
	}
	if _, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("other")), req); !errors.Is(err, artifactupload.ErrUploadConsumed) {
		t.Fatalf("another publish of a used upload answered %v", err)
	}
}

// A FILE THE ARTIFACT HOLDS IS CARRIED: the intent moves no byte for it, and
// the version names the object the artifact already held.
func TestAHeldFileIsCarriedNotUploaded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created := h.create(h.person("ada", h.opKey("create")),
		map[string]string{"index.html": page, "data.json": `{"a":1}`})
	c := h.person("ada", h.opKey("publish"))
	intent, err := h.svc.Intend(t.Context(), c, created.Artifact,
		declare(map[string]string{"data.json": `{"a":1}`}))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Upload != "" || !slices.Equal(intent.Carried, []string{"data.json"}) {
		t.Fatalf("the intent answered %+v, want data.json carried and nothing reserved", intent)
	}
	got, _, err := h.svc.Publish(t.Context(), c, artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModeWhole, Entry: "index.html",
		Files: []artifactupload.FileSpec{
			{Path: "index.html", From: 1},
			{Path: "data.json", Carry: objstore.HashOf([]byte(`{"a":1}`))},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	v1, v2 := h.version(created.Artifact, 1), h.version(created.Artifact, got.Number)
	a, _ := manifest.EntryAt(v1.Manifest, "data.json")
	b, _ := manifest.EntryAt(v2.Manifest, "data.json")
	if a.Object.Key != b.Object.Key || !b.Carried {
		t.Fatalf("data.json names %s (carried %v), want the held %s", b.Object.Key, b.Carried, a.Object.Key)
	}
	if _, ok := manifest.EntryAt(v2.Manifest, "_vendor/cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js"); !ok {
		t.Fatal("a carried page lost the copy of its CDN file in a whole publish")
	}
}

// A PUBLISH BASED ON A MOVED HEAD IS REFUSED WITH THE LIVE VERSION before any
// byte of its own is written.
func TestAPublishOnAMovedHeadIsRefusedWithTheLiveVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": page})
	if _, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("p1")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch, Label: "one",
		Files: []artifactupload.FileSpec{{Path: "a.txt", Inline: []byte("a")}},
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("p2")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Edits: []edits.Edit{{Old: "Quarterly", New: "Annual"}},
	})
	var moved *artifactsdomain.VersionMoved
	if !errors.As(err, &moved) || moved.Head.HeadNumber != 2 || moved.Live.Number != 2 {
		t.Fatalf("a publish on a moved head answered %v", err)
	}
}

// THE RUNTIME CONTRACT IS RESOLVED AT PUBLISH: latest to the newest served, an
// exact one this build does not serve refused, and a document kind pins none.
func TestTheRuntimeContractIsResolvedAtPublish(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", h.opKey("create"))
	_, _, err := h.svc.Create(t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindHTML, Title: "x", Owner: "ada", OwnerKind: "human", Entry: "index.html",
		Contract: "9.9.9", Files: []artifactupload.FileSpec{{Path: "index.html", Inline: []byte("<p>x</p>")}},
	})
	if !errors.Is(err, artifactupload.ErrContractUnknown) {
		t.Fatalf("an unknown contract answered %v", err)
	}
	_, _, err = h.svc.Create(t.Context(), h.person("ada", h.opKey("md")), artifactupload.CreateRequest{
		Kind: artifacts.KindMarkdown, Title: "x", Owner: "ada", OwnerKind: "human", Entry: "a.md",
		Contract: "latest", Files: []artifactupload.FileSpec{{Path: "a.md", Inline: []byte("# x")}},
	})
	if !errors.Is(err, artifactupload.ErrContractUnknown) {
		t.Fatalf("a markdown artifact pinning a runtime answered %v", err)
	}
	got, _, err := h.svc.Create(t.Context(), h.person("ada", h.opKey("md2")), artifactupload.CreateRequest{
		Kind: artifacts.KindMarkdown, Title: "x", Owner: "ada", OwnerKind: "human", Entry: "a.md",
		Files: []artifactupload.FileSpec{{Path: "a.md", Inline: []byte("# Notes on the plan")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := h.version(got.Artifact, 1); v.Contract != "" {
		t.Fatalf("a markdown version pins %q", v.Contract)
	}
}
