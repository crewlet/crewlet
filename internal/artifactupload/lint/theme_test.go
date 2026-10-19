package lint_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// A KIT FILE COUNTS ONLY WHERE A BROWSER LOADS IT AS WHAT IT IS: the kit's
// stylesheet from a stylesheet link or an @import, its theme bootstrap from a
// script source, both by path on the artifact's own origin. Everything else
// that merely names a kit file (the wrong element, the wrong file, another
// host, a kit file that is neither) is no link, and each link carries the line
// it is written on.
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
			{Version: "0.20.0", File: artifacts.KitStylesheet, Line: 2},
			{Version: "0.20.0", File: artifacts.KitTheme, Line: 3},
		}},
	}, {
		name:   "a query, a fragment, an escape and a rel list",
		isHTML: true,
		text: "<link rel=\"preload stylesheet\" href=\" " + kit("0.18.0", "kit.css") + "?v=1#x\">" +
			"<script src=\"" + kit("0.18.0", "theme%2Ejs") + "\"></script>",
		want: lint.Theme{Kit: []lint.KitLink{
			{Version: "0.18.0", File: artifacts.KitStylesheet, Line: 1},
			{Version: "0.18.0", File: artifacts.KitTheme, Line: 1},
		}},
	}, {
		name:   "an @import in a style element",
		isHTML: true,
		text:   "<p>x</p>\n<style>\nbody{}\n@import \"" + kit("0.20.0", "kit.css") + "\";\n</style>",
		want:   lint.Theme{Kit: []lint.KitLink{{Version: "0.20.0", File: artifacts.KitStylesheet, Line: 4}}},
	}, {
		name: "an @import in a stylesheet",
		text: "/* tokens */\n@import url(" + kit("0.20.0", "kit.css") + ");\n" +
			"@media (prefers-color-scheme: dark) { body { color: #eee } }",
		want: lint.Theme{Rules: true,
			Kit: []lint.KitLink{{Version: "0.20.0", File: artifacts.KitStylesheet, Line: 2}}},
	}, {
		name:   "a page's own rule",
		isHTML: true,
		text:   "<style>@media (PREFERS-COLOR-SCHEME: dark) { body { color: #eee } }</style>",
		want:   lint.Theme{Rules: true},
	}, {
		name:   "names that load nothing that themes the page",
		isHTML: true,
		text: "<link rel=\"preload\" href=\"" + kit("0.20.0", "kit.css") + "\">" +
			"<link rel=\"stylesheet\" href=\"" + kit("0.20.0", "theme.js") + "\">" +
			"<script src=\"" + kit("0.20.0", "kit.css") + "\"></script>" +
			"<a href=\"" + kit("0.20.0", "kit.css") + "\">kit</a>" +
			"<link rel=\"stylesheet\" href=\"https://example.com" + kit("0.20.0", "kit.css") + "\">" +
			"<link rel=\"stylesheet\" href=\"//example.com" + kit("0.20.0", "kit.css") + "\">" +
			"<link rel=\"stylesheet\" href=\"_crewlet/kit/0.20.0/kit.css\">" +
			"<link rel=\"stylesheet\" href=\"" + artifacts.KitPrefix + "kit.css\">" +
			"<link rel=\"stylesheet\" href=\"" + kit("0.20.0", "fonts/kit.css") + "\">" +
			"<p>" + kit("0.20.0", "kit.css") + "</p>",
		want: lint.Theme{},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lint.ThemeOf([]byte(tc.text), tc.isHTML); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ThemeOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}
