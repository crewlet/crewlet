package artifactrender_test

import (
	"bytes"
	"context"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifactrender"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/artifactserve/ticket"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/static"
)

const (
	dashboard = "https://crewlet.example.com"
	template  = "https://{artifact}.example-usercontent.net"
	author    = "launch-writer"
)

// The page under test: the kit linked at the newest release this build
// embeds, a sticky bar painted with a token the kit does not define, a file
// the version lacks, a link to a page it lacks, an external link, a console
// error, and a per-viewer write the render refuses.
func pageFiles(kit string) map[string]string {
	nonce := artifacts.NoncePlaceholder
	return map[string]string{
		"index.html": `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Launch review</title>
<link rel="stylesheet" href="/_crewlet/kit/` + kit + `/kit.css">
<script nonce="` + nonce + `" src="/_crewlet/kit/` + kit + `/theme.js"></script>
<link rel="stylesheet" href="styles.css">
</head>
<body>
<header class="bar"><a href="#pricing">Pricing</a> <button id="menu">Menu</button></header>
<main>
<h1>Launch review</h1>
<img src="missing.png" alt="">
<p><a href="notes.html">Notes</a> <a href="https://example.org/" target="_blank">Elsewhere</a></p>
<section id="pricing" style="margin-top: 1600px"><h2>Pricing</h2></section>
<footer style="height: 1200px"></footer>
</main>
<script>console.log("a script the publish linter never saw")</script>
<script nonce="` + nonce + `">
console.error("chart failed to draw");
crewlet.ready.then(async () => {
  try { await crewlet.state.set("seen", true); } catch (e) { console.warn("state refused: " + e.code); }
});
document.getElementById("menu").addEventListener("click", () => { document.body.dataset.menu = "open"; });
</script>
</body>
</html>
`,
		"styles.css": `body { margin: 0; background: var(--color-surface-background); }
.bar {
  position: sticky;
  top: 0;
  height: 56px;
  background: var(--color-surface-base);
  color: var(--color-text-muted);
}
`,
	}
}

// estate is the artifacts estate as the render's origin reads it.
type estate struct {
	head    artifactsdomain.Head
	version artifactsdomain.Version
}

func (e *estate) Artifact(_ context.Context, id artifacts.ID, _ statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	if id != e.head.ID {
		return artifactsdomain.Head{}, statelog.Position{}, artifactsdomain.ErrNotFound
	}
	return e.head, statelog.Position{}, nil
}

func (e *estate) Version(_ context.Context, id artifacts.ID, n int) (artifactsdomain.Version, error) {
	if id != e.head.ID || n != e.version.Number {
		return artifactsdomain.Version{}, artifactsdomain.ErrNotFound
	}
	return e.version, nil
}

func (e *estate) Assets(context.Context, artifacts.ID) ([]artifactsdomain.Asset, error) {
	return nil, nil
}

func (e *estate) BrandFile(context.Context, objstore.Hash) (artifactsdomain.BrandFile, error) {
	return artifactsdomain.BrandFile{}, artifactsdomain.ErrNotFound
}

// chart is a company with no units and no leads.
type chart struct{}

func (chart) Leads(context.Context, string, string) (bool, error)          { return false, nil }
func (chart) LeadsProject(context.Context, string, string) (bool, error)   { return false, nil }
func (chart) LeadsUnit(context.Context, string, string) (bool, error)      { return false, nil }
func (chart) LeadsContainer(context.Context, string, string) (bool, error) { return false, nil }
func (chart) LeadsAnyone(context.Context, string) (bool, error)            { return false, nil }
func (chart) MemberOf(context.Context, string, string) (bool, error)       { return false, nil }
func (chart) SeatGranted(context.Context, string, authz.SeatGrant) (bool, error) {
	return false, nil
}

type local struct{}

func (local) ArtifactsCopyServes(context.Context) bool { return true }

// reads answers the page's reads with nothing: the page under test reads no
// data.
type reads struct{}

func (reads) Read(context.Context, string, map[string]any) (any, *artifactrender.Refusal) {
	return nil, nil
}

// rig is the page published as version 1 of an artifact the author owns, a
// renderer over the real embedded runtime and kit, and the author's request.
type rig struct {
	renderer *artifactrender.Renderer
	request  artifactrender.Request
	admitted int
}

func newRig(t *testing.T, chrome string, mode artifacts.OriginMode, external string) *rig {
	t.Helper()
	tree, err := fs.Sub(static.FS(), "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	kit := newestKit(t, tree)
	store, err := objstore.NewStoreAt(memobj.NewAt(time.Now), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	files := pageFiles(kit)
	var entries []artifacts.Entry
	for _, path := range []string{"index.html", "styles.css"} {
		data := []byte(files[path])
		obj, err := store.Put(t.Context(), bytes.NewReader(data), int64(len(data))+1, objstore.PutMeta{})
		if err != nil {
			t.Fatal(err)
		}
		typ, _ := mediatype.For(path)
		e := artifacts.Entry{Path: path, ContentType: typ.Served, Object: obj}
		if path == "index.html" {
			e.Script = &artifacts.ScriptPolicy{Nonced: true}
		}
		entries = append(entries, e)
	}
	id := artifacts.IDFor("render-" + t.Name())
	caps := []artifacts.Capability{artifacts.CapabilityState}
	e := &estate{
		head: artifactsdomain.Head{ID: id, Label: id.Label(), Kind: artifacts.KindHTML,
			Title: "Launch review", Owner: author, OwnerKind: iam.ActorAgent, HeadNumber: 1,
			Capabilities: caps, Status: artifactsdomain.StatusLive, AccessEpoch: 1},
		version: artifactsdomain.Version{
			VersionSummary: artifactsdomain.VersionSummary{Artifact: id, Number: 1,
				Entry: "index.html", Contract: "1.0.0", Capabilities: caps},
			Manifest: artifacts.Manifest{Entry: "index.html", Files: entries},
		},
	}
	tickets, err := ticket.New(runtoken.OneKey("k1", "the-fleet-key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	origins, err := artifactserve.NewOrigins(artifactserve.OriginsConfig{External: external,
		Mode: mode, Dashboard: []string{dashboard}})
	if err != nil {
		t.Fatal(err)
	}
	ep := chromiumtest.LaunchPort(t, chrome)
	r, err := artifactrender.New(artifactrender.Config{
		BrowserURL: ep.HTTP, Client: httpx.Client(0), Tickets: tickets, Origins: origins,
		Dashboard: dashboard, KitVersion: kit, Contract: "1.0.0",
		Serve: artifactserve.Deps{Artifacts: e, Objects: store, Chart: chart{}, Local: local{},
			Embedded: tree},
	})
	if err != nil {
		t.Fatal(err)
	}
	g := &rig{renderer: r}
	g.request = artifactrender.Request{
		Principal: iam.Principal{ID: uuid.New(), Kind: iam.KindSeat, Seat: author, Login: author,
			Stage: iam.StageActive},
		Admit: func(context.Context) iam.Resolution {
			g.admitted++
			return iam.Resolved
		},
		Viewer: artifactrender.Viewer{Seat: author, DisplayName: "Launch Writer", Kind: "agent",
			Role: artifacts.RoleOwner},
		Head: e.head, Version: e.version,
		Widths:  []int{1440, 390},
		Themes:  []artifactrender.Theme{artifactrender.ThemeLight, artifactrender.ThemeDark},
		Scroll:  []artifactrender.Scroll{{}},
		Actions: []artifactrender.Action{{Kind: artifactrender.ActionClick, Selector: "#menu"}},
		Bridge:  reads{},
	}
	return g
}

// newestKit is the newest kit release the tree embeds.
func newestKit(t *testing.T, tree fs.FS) string {
	t.Helper()
	entries, err := fs.ReadDir(tree, "kit")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int { return compareVersions(a, b) })
	return names[len(names)-1]
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(pa), len(pb)) {
		if d := len(pa[i]) - len(pb[i]); d != 0 {
			return d
		}
		if c := strings.Compare(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	return len(pa) - len(pb)
}

// A page whose sticky bar is painted with a token the linked kit does not
// define is reported: the token, its rule, its line and the element it
// paints, the defined token nearest it, and the bar it leaves see-through in
// both themes, beside four screenshots of the published page.
func TestARenderReportsATokenTheKitDoesNotDefine(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		g := newRig(t, chrome, artifacts.OriginPerArtifact, template)
		rep, err := g.renderer.Render(t.Context(), g.request)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if rep.Stopped != "" {
			t.Fatalf("the render stopped early: %s", rep.Stopped)
		}
		var token *artifactrender.Unresolved
		for i, u := range rep.Unresolved {
			if u.Property == "--color-surface-base" {
				token = &rep.Unresolved[i]
			}
		}
		if token == nil {
			t.Fatalf("--color-surface-base was not reported; unresolved: %+v", rep.Unresolved)
		}
		use := token.Uses[0]
		if token.Fallback || use.Source != "styles.css" || use.Line != 6 || use.Selector != ".bar" ||
			use.Property != "background" || token.Elements != 1 || token.Example != "header.bar" ||
			!slices.Contains(token.Near, "--color-surface-background") {
			t.Fatalf("the finding: %+v", *token)
		}
		if len(rep.Unresolved) != 1 {
			t.Fatalf("only the page's own unresolved token is reported, never one the kit "+
				"defines nor one the kit itself references: %+v", rep.Unresolved)
		}
		themes := map[artifactrender.Theme]bool{}
		for _, b := range rep.SeeThrough {
			if b.Element == "header.bar" && b.Unresolved == "--color-surface-base" {
				themes[b.Theme] = true
			}
		}
		if !themes[artifactrender.ThemeLight] || !themes[artifactrender.ThemeDark] {
			t.Fatalf("the see-through bar, in both themes: %+v", rep.SeeThrough)
		}
		if len(rep.Shots) != 4 {
			t.Fatalf("%d shots, want 4: %+v", len(rep.Shots), rep.Shots)
		}
		for _, s := range rep.Shots {
			if !bytes.HasPrefix(s.PNG, []byte("\x89PNG")) || s.ScrollHeight < 1600 {
				t.Fatalf("shot %dpx %s: %d bytes, failed %q, scroll height %d", s.Width, s.Theme,
					len(s.PNG), s.Failed, s.ScrollHeight)
			}
		}
		if !rep.Runtime {
			t.Fatal("the page's runtime never said hello to the host")
		}
		if g.admitted == 0 {
			t.Fatal("no document was decided again through the request's admission")
		}
	})
}

// What a reviewer would trip over besides the token is reported too: the
// file the version lacks, the link to a page it lacks, the external link the
// frame cannot open, the console error, the refused write, the action played,
// and the in-page link that lands under the sticky bar.
func TestARenderReportsWhatAReviewerWouldTripOver(t *testing.T) {
	g := newRig(t, chromiumtest.Newest(t), artifacts.OriginShared, "https://artifacts.example-usercontent.net")
	g.request.Widths = []int{1440}
	g.request.Themes = []artifactrender.Theme{artifactrender.ThemeLight}
	rep, err := g.renderer.Render(t.Context(), g.request)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !slices.ContainsFunc(rep.Requests, func(f artifactrender.RequestFailure) bool {
		return f.URL == "missing.png" && f.Status == 404
	}) {
		t.Fatalf("missing.png: %+v", rep.Requests)
	}
	for _, f := range rep.Requests {
		if strings.Contains(f.URL, "/t/") {
			t.Fatalf("a report named the ticket: %q", f.URL)
		}
	}
	if !slices.ContainsFunc(rep.Links, func(l artifactrender.Link) bool {
		return l.Href == "notes.html" && strings.Contains(l.Problem, "holds no notes.html")
	}) || !slices.ContainsFunc(rep.Links, func(l artifactrender.Link) bool {
		return l.Href == "https://example.org/" && strings.Contains(l.Problem, "openLink")
	}) {
		t.Fatalf("links: %+v", rep.Links)
	}
	if !slices.ContainsFunc(rep.Console, func(l artifactrender.ConsoleLine) bool {
		return l.Level == "error" && l.Text == "chart failed to draw"
	}) || !slices.ContainsFunc(rep.Console, func(l artifactrender.ConsoleLine) bool {
		return l.Level == "warning" && l.Text == "state refused: forbidden"
	}) || !slices.ContainsFunc(rep.Console, func(l artifactrender.ConsoleLine) bool {
		return l.Level == "error" && l.Source == "index.html" &&
			strings.Contains(l.Text, "Content Security Policy")
	}) {
		t.Fatalf("console: %+v", rep.Console)
	}
	for _, l := range rep.Console {
		if strings.Contains(l.Text, "header") {
			t.Fatalf("a warning about the engine's own headers was reported: %+v", l)
		}
	}
	if !slices.ContainsFunc(rep.Bridge, func(n artifactrender.BridgeNote) bool {
		return n.Method == "state.set"
	}) {
		t.Fatalf("bridge: %+v", rep.Bridge)
	}
	if len(rep.Actions) != 1 || !rep.Actions[0].Done {
		t.Fatalf("actions: %+v", rep.Actions)
	}
	if !slices.ContainsFunc(rep.Covered, func(c artifactrender.Covered) bool {
		return c.Element == "#pricing" && c.By == "header.bar"
	}) {
		t.Fatalf("covered: %+v", rep.Covered)
	}
}
