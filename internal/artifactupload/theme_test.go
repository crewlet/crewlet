package artifactupload_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// themePage is a page whose head is head.
func themePage(head string) string {
	return `<!doctype html><html><head><meta name="viewport" content="width=device-width">` +
		head + `</head><body><h1>Plan</h1></body></html>`
}

// kitLink and kitScript are the guide's two theming lines for one kit release.
func kitLink(version string) string {
	return `<link rel="stylesheet" href="` + artifacts.KitPrefix + version + `/kit.css">`
}

func kitScript(version string) string {
	return `<script src="` + artifacts.KitPrefix + version + `/theme.js"></script>`
}

// A VERSION FOLLOWS ITS READER WHEN IT CARRIES A DARK-MODE RULE OR LINKS BOTH
// OF THE KIT'S THEMING FILES at a release the engine serves, and is warned
// otherwise with what to add: the kit's stylesheet alone follows the device,
// its bootstrap alone styles nothing, and a release the origin does not hold
// loads nothing at all.
func TestTheThemeWarningReadsTheKitLinks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		files  map[string]string
		warned map[lint.Code]string
	}{{
		name:  "the kit's stylesheet and bootstrap",
		files: map[string]string{"index.html": themePage(kitLink(testKit) + kitScript(testKit))},
	}, {
		name:  "a rule of the page's own",
		files: map[string]string{"index.html": page},
	}, {
		name: "a rule in a stylesheet beside the page",
		files: map[string]string{
			"index.html": themePage(`<link rel="stylesheet" href="app.css">`),
			"app.css":    "@media (prefers-color-scheme: dark) { body { color: #eee } }",
		},
	}, {
		name: "the kit imported by the version's stylesheet, the bootstrap by the page",
		files: map[string]string{
			"index.html": themePage(`<link rel="stylesheet" href="app.css">` + kitScript(testKit)),
			"app.css":    `@import "` + artifacts.KitPrefix + testKit + `/kit.css";`,
		},
	}, {
		name:   "neither",
		files:  map[string]string{"index.html": themePage("")},
		warned: map[lint.Code]string{lint.CodeNoDarkMode: "ignores the reader's theme"},
	}, {
		name:   "the kit's stylesheet alone",
		files:  map[string]string{"index.html": themePage(kitLink(testKit))},
		warned: map[lint.Code]string{lint.CodeNoDarkMode: artifacts.KitPrefix + testKit + "/theme.js"},
	}, {
		name:   "the kit's bootstrap alone",
		files:  map[string]string{"index.html": themePage(kitScript(testKit))},
		warned: map[lint.Code]string{lint.CodeNoDarkMode: artifacts.KitPrefix + testKit + "/kit.css"},
	}, {
		name:  "a kit release the engine does not serve",
		files: map[string]string{"index.html": themePage(kitLink("9.9.9") + kitScript("9.9.9"))},
		warned: map[lint.Code]string{
			lint.CodeNoDarkMode:  "ignores the reader's theme",
			lint.CodeKitUnserved: "link " + artifacts.KitPrefix + testKit + "/",
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, options{})
			c := h.person("ada", h.opKey("create"))
			up := h.upload(c, artifacts.ID{}, tc.files)
			_, report, err := h.svc.Create(t.Context(), c, artifactupload.CreateRequest{
				Kind: artifacts.KindHTML, Title: "A page", Owner: "ada",
				OwnerKind: c.Authority.Actor.Kind, Upload: up, Files: specs(tc.files),
				Entry: "index.html",
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			got := map[lint.Code]string{}
			for _, w := range report.Warnings {
				if w.Code == lint.CodeNoDarkMode || w.Code == lint.CodeKitUnserved {
					got[w.Code] = w.Message
				}
			}
			for code, says := range tc.warned {
				if !strings.Contains(got[code], says) {
					t.Errorf("the %s warning reads %q, want it to say %q", code, got[code], says)
				}
			}
			for code, msg := range got {
				if _, want := tc.warned[code]; !want {
					t.Errorf("an unexpected %s warning: %s", code, msg)
				}
			}
		})
	}
}

// A CARRIED PAGE IS NOT WARNED AGAIN FOR ITS KIT LINKS, which were judged when
// it was published; the version it lands in is still judged whole.
func TestACarriedPagesKitLinksAreNotWarnedAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := map[string]string{"index.html": themePage(kitLink("9.9.9") + kitScript(testKit))}
	created := h.create(h.person("ada", h.opKey("create")), files)
	c := h.person("ada", h.opKey("publish"))
	extra := map[string]string{"notes.txt": "notes"}
	up := h.upload(c, created.Artifact, extra)
	_, report, err := h.svc.Publish(t.Context(), c, artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch, Upload: up, Files: specs(extra),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	unserved, dark := 0, 0
	for _, w := range report.Warnings {
		switch w.Code {
		case lint.CodeKitUnserved:
			unserved++
		case lint.CodeNoDarkMode:
			dark++
		}
	}
	if unserved != 0 || dark != 1 {
		t.Fatalf("a publish carrying the page warned %d unserved kit links and %d dark-mode "+
			"findings, want 0 and 1: %+v", unserved, dark, report.Warnings)
	}
}
