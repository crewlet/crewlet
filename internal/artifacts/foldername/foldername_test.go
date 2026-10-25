package foldername

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// A FOLDER NAME IS CANONICAL BEFORE IT IS STORED: NFC, trimmed, and keyed by
// its case fold, so two names a reader takes as one are one key.
func TestAFolderNameIsCanonicalAndKeyedByItsFold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw, name, key string
	}{
		{raw: "Reports", name: "Reports", key: "reports"},
		{raw: "  Q3 launch \t", name: "Q3 launch", key: "q3 launch"},
		// A decomposed é is composed, so it is the same name as the
		// composed one.
		{raw: "Cafe\u0301", name: "Café", key: "café"},
		// Full case folding: the sharp s folds to "ss".
		{raw: "Straße", name: "Straße", key: "strasse"},
		{raw: "STRASSE", name: "STRASSE", key: "strasse"},
		{raw: "Ünïcödé", name: "Ünïcödé", key: "ünïcödé"},
		{raw: "a.b", name: "a.b", key: "a.b"},
		{raw: "...", name: "...", key: "..."},
		{raw: "Marketing & Brand", name: "Marketing & Brand", key: "marketing & brand"},
	} {
		name, key, err := Check(tc.raw)
		if err != nil {
			t.Errorf("Check(%q) refused: %v", tc.raw, err)
			continue
		}
		if name != tc.name || key != tc.key {
			t.Errorf("Check(%q) = (%q, %q), want (%q, %q)", tc.raw, name, key,
				tc.name, tc.key)
		}
		if Key(name) != key {
			t.Errorf("Key(%q) = %q and Check keyed it %q", name,
				Key(name), key)
		}
	}
}

// A NAME OUTSIDE THE GRAMMAR IS REFUSED, never repaired: each refusal names
// the rule, and nothing is silently stripped from what a person typed.
func TestAFolderNameOutsideTheGrammarIsRefused(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"empty":                "",
		"only white space":     " \t ",
		"a slash":              "Reports/Weekly",
		"dot":                  ".",
		"dot dot":              "..",
		"a C0 control":         "Re\x07ports",
		"a C1 control":         "Re\u0085ports",
		"a tab inside":         "Re\tports",
		"a newline inside":     "Re\nports",
		"a bidi override":      "Reports\u202Egnp.exe",
		"a bidi isolate":       "Reports\u2066x\u2069",
		"a zero-width space":   "Re\u200Bports",
		"a zero-width joiner":  "Re\u200Dports",
		"a byte-order mark":    "\uFEFFReports",
		"a soft hyphen":        "Re\u00ADports",
		"a line separator":     "Re\u2028ports",
		"invalid UTF-8":        "Re\xffports",
		"one byte over":        strings.Repeat("a", artifacts.MaxFolderNameBytes+1),
		"multibyte over bound": strings.Repeat("é", artifacts.MaxFolderNameBytes/2+1),
	} {
		if got, key, err := Check(raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Check(%q) = (%q, %q, %v), want a refusal", name, raw,
				got, key, err)
		}
	}
	if _, _, err := Check(strings.Repeat("a", artifacts.MaxFolderNameBytes)); err != nil {
		t.Errorf("a name of exactly %d bytes was refused: %v", artifacts.MaxFolderNameBytes, err)
	}
}
