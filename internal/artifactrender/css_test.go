package artifactrender

import (
	"reflect"
	"testing"
)

// A token the page uses and no sheet it loads defines is reported with the
// rule, the property and the line that use it; one some sheet defines, in any
// block, is not.
func TestAPropertyNoLoadedSheetDefinesIsUnresolved(t *testing.T) {
	kit := sheet{source: "/_crewlet/kit/1.0.0/kit.css", text: `:root {
  --color-surface-background: #fff;
}
[data-theme="dark"] { --color-surface-background: #111; }
@media (prefers-color-scheme: dark) { :root { --color-text-primary: #eee; } }
`}
	page := sheet{source: "styles.css", text: `/* the bar: var(--in-a-comment) */
.bar {
  position: sticky;
  background: var(--color-surface-base);
  color: var(--color-text-primary);
  content: "var(--in-a-string)";
}
.card { border-color: var(--color-border, #ccc); }
`}
	got := scanSheets([]sheet{kit, page}).unresolved()
	want := []unresolvedUse{
		{name: "--color-border", fallback: true, uses: []use{{name: "--color-border",
			source: "styles.css", line: 8, selector: ".card", property: "border-color",
			fallback: true}}},
		{name: "--color-surface-base", uses: []use{{name: "--color-surface-base",
			source: "styles.css", line: 4, selector: ".bar", property: "background"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unresolved:\n got %+v\nwant %+v", got, want)
	}
}

// A name is defined by a declaration anywhere, a style attribute's or a
// nested rule's included, and by an @property rule.
func TestEveryWayOfDefiningANameCounts(t *testing.T) {
	sheets := []sheet{
		{source: "a.css", text: `@property --angle { syntax: "<angle>"; inherits: false; initial-value: 0deg; }
.wrap { .inner { --pad: 4px; } padding: var(--pad); transform: rotate(var(--angle)); }
@supports (display: grid) { @layer base { .x { --gap: 1rem } } }
.y { gap: var(--gap); margin: var(--set-inline); }`},
		{source: `style=""`, text: `--set-inline: 2px; color: red`, inline: true},
	}
	if got := scanSheets(sheets).unresolved(); len(got) != 0 {
		t.Fatalf("every name is defined, yet unresolved: %+v", got)
	}
}

// A nested rule's uses are reported under the selector it resolves to.
func TestANestedRuleIsReportedUnderItsResolvedSelector(t *testing.T) {
	got := scanSheets([]sheet{{source: "n.css", text: `.nav {
  &:hover { color: var(--nope); }
  .item { color: var(--nope-too); }
}`}}).unresolved()
	if len(got) != 2 || got[0].uses[0].selector != ".nav:hover" ||
		got[1].uses[0].selector != ".nav .item" {
		t.Fatalf("selectors: %+v", got)
	}
}

// The suggestion for a mistyped token is the defined name that shares the
// most of its spelling.
func TestAnUnresolvedNameIsOfferedTheNearestDefinedOnes(t *testing.T) {
	s := scanSheets([]sheet{{source: "kit.css", text: `:root {
  --color-surface-background: #fff; --color-surface-elevated: #fff;
  --color-text-primary: #000; --spacing-4: 16px;
}`}})
	got := s.near("--color-surface-base")
	if len(got) == 0 || got[0] != "--color-surface-background" {
		t.Fatalf("near(--color-surface-base) = %v", got)
	}
	for _, n := range got {
		if n == "--spacing-4" {
			t.Fatalf("an unrelated token was offered: %v", got)
		}
	}
}

// Malformed text costs only what it cannot read: a stray brace ends no
// sheet, and an unterminated string or comment ends the scan quietly.
func TestMalformedTextIsPassedOver(t *testing.T) {
	got := scanSheets([]sheet{{source: "m.css", text: `} .a { color: var(--first) }
.b { content: "unterminated
; color: var(--second) }
.c { color: var(--third) } /* open`}}).unresolved()
	names := []string{}
	for _, u := range got {
		names = append(names, u.name)
	}
	if !reflect.DeepEqual(names, []string{"--first", "--second", "--third"}) {
		t.Fatalf("names = %v", names)
	}
}
