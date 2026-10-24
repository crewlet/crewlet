package lint_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// A KIT FILE COUNTS WHERE A BROWSER LOADS IT: from a stylesheet link or an
// @import, or from a script source, by path on the artifact's own origin, each
// recorded with how it is loaded and the line it is written on. Anything that
// merely names a kit file (a preload, an anchor, text, another host, a
// relative path, a path with no file) is no link. Only the stylesheet loaded
// as one and the bootstrap loaded as a script theme the page.
func TestThemeOfReadsTheKitLinksABrowserLoads(t *testing.T) {
	t.Parallel()
	kit := func(version, file string) string { return artifacts.KitPrefix + version + "/" + file }
	cases := []struct {
		name   string
		text   string
		isHTML bool
		want   lint.Theme
	}{{
		name:   "the guide's head",
		isHTML: true,
		text: "<head>\n<link rel=\"stylesheet\" href=\"" + kit("0.20.0", "kit.css") + "\">\n" +
			"<script src=\"" + kit("0.20.0", "theme.js") + "\"></script>\n" +
			"<script type=\"module\" src=\"" + kit("0.20.0", "kit.js") + "\"></script></head>",
		want: lint.Theme{Kit: []lint.KitLink{
			{Version: "0.20.0", File: artifacts.KitStylesheet, As: lint.KitAsStylesheet, Line: 2},
			{Version: "0.20.0", File: artifacts.KitTheme, As: lint.KitAsScript, Line: 3},
			{Version: "0.20.0", File: "kit.js", As: lint.KitAsScript, Line: 4},
		}},
	}, {
		name:   "a query, a fragment, an escape and a rel list",
		isHTML: true,
		text: "<link rel=\"preload stylesheet\" href=\" " + kit("0.18.0", "kit.css") + "?v=1#x\">" +
			"<script src=\"" + kit("0.18.0", "theme%2Ejs") + "\"></script>",
		want: lint.Theme{Kit: []lint.KitLink{
			{Version: "0.18.0", File: artifacts.KitStylesheet, As: lint.KitAsStylesheet, Line: 1},
			{Version: "0.18.0", File: artifacts.KitTheme, As: lint.KitAsScript, Line: 1},
		}},
	}, {
		name:   "an @import in a style element",
		isHTML: true,
		text:   "<p>x</p>\n<style>\nbody{}\n@import \"" + kit("0.20.0", "kit.css") + "\";\n</style>",
		want: lint.Theme{Kit: []lint.KitLink{
			{Version: "0.20.0", File: artifacts.KitStylesheet, As: lint.KitAsStylesheet, Line: 4},
		}},
	}, {
		name: "an @import in a stylesheet",
		text: "/* tokens */\n@import url(" + kit("0.20.0", "kit.css") + ");\n" +
			"@media (prefers-color-scheme: dark) { body { color: #eee } }",
		want: lint.Theme{Rules: true,
			Kit: []lint.KitLink{{Version: "0.20.0", File: artifacts.KitStylesheet, As: lint.KitAsStylesheet, Line: 2}}},
	}, {
		name:   "a page's own rule",
		isHTML: true,
		text:   "<style>@media (PREFERS-COLOR-SCHEME: dark) { body { color: #eee } }</style>",
		want:   lint.Theme{Rules: true},
	}, {
		name:   "the content stylesheet, linked and imported",
		isHTML: true,
		text: "<link rel=\"stylesheet\" href=\"" + kit("0.33.0", "content.css") + "\">\n" +
			"<style>@import \"" + kit("0.33.0", "content.css") + "\";</style>",
		want: lint.Theme{Kit: []lint.KitLink{
			{Version: "0.33.0", File: "content.css", As: lint.KitAsStylesheet, Line: 1},
			{Version: "0.33.0", File: "content.css", As: lint.KitAsStylesheet, Line: 2},
		}},
	}, {
		name:   "kit files loaded as something they are not",
		isHTML: true,
		text: "<link rel=\"stylesheet\" href=\"" + kit("0.20.0", "theme.js") + "\">" +
			"<script src=\"" + kit("0.20.0", "kit.css") + "\"></script>" +
			"<link rel=\"stylesheet\" href=\"" + kit("0.20.0", "fonts/kit.css") + "\">",
		want: lint.Theme{Kit: []lint.KitLink{
			{Version: "0.20.0", File: artifacts.KitTheme, As: lint.KitAsStylesheet, Line: 1},
			{Version: "0.20.0", File: artifacts.KitStylesheet, As: lint.KitAsScript, Line: 1},
			{Version: "0.20.0", File: "fonts/kit.css", As: lint.KitAsStylesheet, Line: 1},
		}},
	}, {
		name:   "names that load nothing",
		isHTML: true,
		text: "<link rel=\"preload\" href=\"" + kit("0.20.0", "kit.css") + "\">" +
			"<a href=\"" + kit("0.20.0", "kit.css") + "\">kit</a>" +
			"<link rel=\"stylesheet\" href=\"https://example.com" + kit("0.20.0", "kit.css") + "\">" +
			"<link rel=\"stylesheet\" href=\"//example.com" + kit("0.20.0", "kit.css") + "\">" +
			"<link rel=\"stylesheet\" href=\"_crewlet/kit/0.20.0/kit.css\">" +
			"<link rel=\"stylesheet\" href=\"" + artifacts.KitPrefix + "kit.css\">" +
			"<link rel=\"stylesheet\" href=\"" + kit("0.20.0", "fonts/") + "\">" +
			"<p>" + kit("0.20.0", "kit.css") + "</p>",
		want: lint.Theme{},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := lint.ThemeOf([]byte(tc.text), tc.isHTML)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ThemeOf = %+v, want %+v", got, tc.want)
			}
			for _, k := range got.Kit {
				sheet := k.File == artifacts.KitStylesheet && k.As == lint.KitAsStylesheet
				boot := k.File == artifacts.KitTheme && k.As == lint.KitAsScript
				if k.ThemesStylesheet() != sheet || k.ThemesBootstrap() != boot {
					t.Errorf("%+v themes the page as stylesheet %v and bootstrap %v", k,
						k.ThemesStylesheet(), k.ThemesBootstrap())
				}
			}
		})
	}
}
