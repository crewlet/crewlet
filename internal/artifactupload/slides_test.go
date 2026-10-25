package artifactupload_test

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// deckFiles is the twelve-slide deck the slide format's own tests read.
func deckFiles(t *testing.T) map[string]string {
	t.Helper()
	root := os.DirFS("../artifacts/slideformat/testdata/deck")
	out := map[string]string{}
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
		out[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// createDeck makes a slides artifact from files, entry deck.json.
func (h *harness) createDeck(c artifactsdomain.Call, files map[string]string) (artifactsdomain.Published, error) {
	h.t.Helper()
	up := h.upload(c, artifacts.ID{}, files)
	got, _, err := h.svc.Create(h.t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindSlides, Title: "Quarterly plan", Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind, Upload: up, Files: specs(files), Entry: "deck.json",
	})
	return got, err
}

// policyFindings is the findings of a policy refusal.
func policyFindings(t *testing.T, err error) []lint.Finding {
	t.Helper()
	var refusal *lint.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the publish answered %v, not a policy refusal", err)
	}
	return refusal.Findings
}

// A DECK PUBLISHES AS ITS AUTHOR WROTE IT: every slide is held to the slide
// format and stored byte for byte, never linted as a page, and the gallery
// searches its slides' text and notes rather than its index.
func TestADeckPublishesAsWritten(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := deckFiles(t)
	got, err := h.createDeck(h.agent("writer", "turn-1", h.opKey("create")), files)
	if err != nil {
		t.Fatal(err)
	}
	v := h.version(got.Artifact, 1)
	if v.Manifest.Entry != "deck.json" || len(v.Manifest.Files) != 14 {
		t.Fatalf("the deck's version holds %d files opening on %q", len(v.Manifest.Files), v.Manifest.Entry)
	}
	for p, body := range files {
		if stored := h.file(v, p); p != "images/team.png" && stored != body {
			t.Errorf("%s was stored as\n%s", p, stored)
		}
		if e, _ := manifest.EntryAt(v.Manifest, p); e.Script != nil {
			t.Errorf("%s carries a page's script policy", p)
		}
	}
	page, err := h.domain.reader.List(t.Context(), artifactsdomain.Viewer{Admin: true},
		artifactsdomain.ListQuery{Q: "plcae"})
	if err != nil || len(page.Contents) != 1 {
		t.Errorf("a search for slide 4's text found %d (%v)", len(page.Contents), err)
	}
	if notes, _ := h.domain.reader.List(t.Context(), artifactsdomain.Viewer{Admin: true},
		artifactsdomain.ListQuery{Q: "ten minutes"}); len(notes.Contents) != 1 {
		t.Error("a search for a slide's notes found nothing")
	}
	if h.held() != 0 || h.unnamedBytes() != 0 {
		t.Errorf("the create leaves %d held and %d unnamed bytes", h.held(), h.unnamedBytes())
	}
}

// A DECK OUTSIDE THE FORMAT IS REFUSED WHOLE, every finding naming its file
// and line, and the store holds the caller's upload alone, open for the
// corrected try, and nothing the refused run wrote.
func TestADeckOutsideTheFormatIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := deckFiles(t)
	files["slides/agenda.html"] = `<section id="agenda" style="background:#fff">
<p style="z-index:3">Agenda</p>
<script>alert(1)</script>
</section>`
	_, err := h.createDeck(h.person("ada", h.opKey("create")), files)
	found := policyFindings(t, err)
	if len(found) != 2 || found[0].Line != 2 || found[1].Line != 3 ||
		found[0].Path != "slides/agenda.html" || found[0].Code != lint.CodeSlideFormat {
		t.Fatalf("findings %v", found)
	}
	var uploaded int64
	for _, body := range files {
		uploaded += int64(len(body))
	}
	if h.unnamedBytes() != uploaded {
		t.Errorf("a refused deck leaves %d bytes in the store, and its upload is %d", h.unnamedBytes(), uploaded)
	}
}

// THE EXIT TEST'S EDITING HALF: a person fixes a typo on slide 4 and swaps two
// slides in one version based on the head; an agent that published from the
// older version is refused with the slides that changed and, merging onto the
// new head, lands its own change beside the person's.
func TestAPersonsEditAndAConcurrentAgentPublishMerge(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created, err := h.createDeck(h.agent("writer", "turn-1", h.opKey("create")), deckFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Artifact
	person, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("fix")), artifactupload.PublishRequest{
		Artifact: id, Base: 1, Mode: manifest.ModePatch, Label: "Fix a typo, swap two slides",
		Edits: []edits.Edit{
			{Path: "slides/plan.html", Old: "one plcae.", New: "one place."},
			{Path: "deck.json", Old: "\"team\",\n    \"timeline\"", New: "\"timeline\",\n    \"team\""},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if person.Number != 2 {
		t.Fatalf("the person's save landed as version %d", person.Number)
	}
	agentEdit := []edits.Edit{{Path: "slides/chart.html", Old: "Requests by week", New: "Requests each week"}}
	_, _, err = h.svc.Publish(t.Context(), h.agent("writer", "turn-2", h.opKey("chart")), artifactupload.PublishRequest{
		Artifact: id, Base: 1, Mode: manifest.ModePatch, Edits: agentEdit,
	})
	var moved *artifactsdomain.VersionMoved
	if !errors.As(err, &moved) || moved.Head.HeadNumber != 2 {
		t.Fatalf("the stale agent publish answered %v", err)
	}
	if changed := changedPaths(h.version(id, 1).Manifest, moved.Live.Manifest); !slices.Equal(changed,
		[]string{"deck.json", "slides/plan.html"}) {
		t.Errorf("the refusal's live version changed %q, want the index and slide 4", changed)
	}
	merged, _, err := h.svc.Publish(t.Context(), h.agent("writer", "turn-2", h.opKey("chart-again")), artifactupload.PublishRequest{
		Artifact: id, Base: 2, Mode: manifest.ModePatch, Edits: agentEdit,
	})
	if err != nil {
		t.Fatalf("the merged publish was refused: %v", err)
	}
	v := h.version(id, merged.Number)
	if !strings.Contains(h.file(v, "slides/plan.html"), "one place.") ||
		!strings.Contains(h.file(v, "slides/chart.html"), "Requests each week") ||
		!strings.Contains(h.file(v, "deck.json"), "\"timeline\",\n    \"team\"") {
		t.Errorf("the merged version lost a change")
	}
}

// changedPaths is every path whose bytes differ between two manifests.
func changedPaths(a, b artifacts.Manifest) []string {
	var out []string
	for _, e := range b.Files {
		if old, ok := manifest.EntryAt(a, e.Path); !ok || old.Object.Hash != e.Object.Hash {
			out = append(out, e.Path)
		}
	}
	slices.Sort(out)
	return out
}

// THE WHOLE VERSION IS JUDGED, not the files a publish brings: removing an
// image a carried slide shows, or ordering a slide whose file is gone, is
// refused though no slide's bytes changed.
func TestAPatchIsJudgedAsTheVersionItMakes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created, err := h.createDeck(h.person("ada", h.opKey("create")), deckFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = h.svc.Publish(t.Context(), h.person("ada", h.opKey("drop")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Files: []artifactupload.FileSpec{{Path: "images/team.png", Remove: true}},
	})
	found := policyFindings(t, err)
	if !slices.ContainsFunc(found, func(f lint.Finding) bool {
		return f.Path == "slides/team.html" && strings.Contains(f.Message, "images/team.png")
	}) {
		t.Errorf("removing a shown image answered %v", found)
	}
	_, _, err = h.svc.Publish(t.Context(), h.person("ada", h.opKey("unorder")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Files: []artifactupload.FileSpec{{Path: "slides/quote.html", Remove: true}},
	})
	if found := policyFindings(t, err); !strings.Contains(found[0].Message, "slides/quote.html") {
		t.Errorf("removing an ordered slide answered %v", found)
	}
}

// A NATIVE KIND DECLARES NO CAPABILITY: a deck has no page to grant one to.
func TestADeckDeclaresNoCapability(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := deckFiles(t)
	c := h.person("ada", h.opKey("create"))
	up := h.upload(c, artifacts.ID{}, files)
	_, _, err := h.svc.Create(t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindSlides, Title: "Deck", Owner: "ada", OwnerKind: c.Authority.Actor.Kind,
		Upload: up, Files: specs(files), Entry: "deck.json",
		Capabilities: []artifacts.Capability{artifacts.CapabilityDB},
	})
	if !errors.Is(err, artifactupload.ErrInvalid) || !strings.Contains(err.Error(), "slides") {
		t.Errorf("a deck declaring db answered %v", err)
	}
}

// A DECK BEGUN FROM ITS STARTER is published by the engine from its own
// files, inline, held to the format like any version, composed from the plan
// it names (its theme and slides) or the default one; a kind with no starter,
// a starter that also names files, a plan the format does not hold, and a
// plan sent without a slides starter are refused before a byte moves.
func TestADeckBeginsFromItsStarter(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", h.opKey("start"))
	got, _, err := h.svc.Create(t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindSlides, Title: "Board review", Owner: "ada",
		OwnerKind: c.Authority.Actor.Kind, Starter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := h.version(got.Artifact, 1)
	if v.Manifest.Entry != "deck.json" || len(v.Manifest.Files) != 5 ||
		!strings.Contains(h.file(v, "slides/cover.html"), "Board review") {
		t.Fatalf("the starter published %d files opening on %q", len(v.Manifest.Files), v.Manifest.Entry)
	}
	plan := &slideformat.Plan{Theme: "signal", Slides: []slideformat.PlanSlide{
		{Layout: "cover"}, {Layout: "number", Title: "Uptime"}, {Layout: "closing"},
	}}
	got, _, err = h.svc.Create(t.Context(), h.person("ada", h.opKey("planned")), artifactupload.CreateRequest{
		Kind: artifacts.KindSlides, Title: "Ops review", Owner: "ada",
		OwnerKind: c.Authority.Actor.Kind, Starter: true, Deck: plan,
	})
	if err != nil {
		t.Fatal(err)
	}
	v = h.version(got.Artifact, 1)
	if len(v.Manifest.Files) != 4 || !strings.Contains(h.file(v, "slides/number.html"), "Uptime") ||
		!strings.Contains(h.file(v, "deck.json"), `"theme": "signal"`) {
		t.Fatalf("the planned starter published %d files: %s", len(v.Manifest.Files), h.file(v, "deck.json"))
	}
	for _, req := range []artifactupload.CreateRequest{
		{Kind: artifacts.KindSheet, Title: "Sheet", Owner: "ada", Starter: true},
		{Kind: artifacts.KindSlides, Title: "Deck", Owner: "ada", Starter: true, Entry: "deck.json"},
		{Kind: artifacts.KindSlides, Title: "Deck", Owner: "ada", Starter: true,
			Deck: &slideformat.Plan{Theme: "neon"}},
		{Kind: artifacts.KindSlides, Title: "Deck", Owner: "ada", Deck: plan},
		{Kind: artifacts.KindDoc, Title: "Doc", Owner: "ada", Starter: true, Deck: plan},
	} {
		req.OwnerKind = c.Authority.Actor.Kind
		if _, _, err := h.svc.Create(t.Context(), h.person("ada", h.opKey("again")), req); !errors.Is(err, artifactupload.ErrInvalid) {
			t.Errorf("a %s starter with entry %q answered %v", req.Kind, req.Entry, err)
		}
	}
}
