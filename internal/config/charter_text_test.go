package config_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
)

// charterText is `testdata/charter_text.json`: the text the engine and the
// dashboard's charter editor must judge alike. The editor reads the same file
// (`dashboard/src/routes/settings/principles/edit/charterText.test.ts`).
type charterText struct {
	// Lengths are values at a field's bound and one character past it, in
	// text where the ways of counting part.
	Lengths []charterLength `json:"lengths"`
	// Breaks are every character the engine reads as ending a line.
	Breaks []struct {
		Name string `json:"name"`
		Char string `json:"char"`
	} `json:"breaks"`
}

// charterLength is one value, prefix + unit×repeat + suffix, with what it
// measures once the engine has trimmed it under every way a client might
// count text, and whether its field's bound admits it.
type charterLength struct {
	Name      string `json:"name"`
	Field     string `json:"field"`
	Prefix    string `json:"prefix"`
	Unit      string `json:"unit"`
	Repeat    int    `json:"repeat"`
	Suffix    string `json:"suffix"`
	Chars     int    `json:"chars"`
	UTF16     int    `json:"utf16"`
	Bytes     int    `json:"bytes"`
	Graphemes int    `json:"graphemes"`
	Admitted  bool   `json:"admitted"`
}

func (v charterLength) value() string {
	return v.Prefix + strings.Repeat(v.Unit, v.Repeat) + v.Suffix
}

func readCharterText(t *testing.T) charterText {
	t.Helper()
	raw, err := os.ReadFile("testdata/charter_text.json")
	if err != nil {
		t.Fatal(err)
	}
	var text charterText
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatal(err)
	}
	if len(text.Lengths) == 0 || len(text.Breaks) == 0 {
		t.Fatal("testdata/charter_text.json holds no vectors, so this gate certifies nothing")
	}
	return text
}

// charterBound is the engine's bound for a vector's field.
func charterBound(t *testing.T, field string) int {
	t.Helper()
	switch field {
	case "mission":
		return config.MaxMissionChars
	case "vision":
		return config.MaxVisionChars
	case "policy":
		return config.MaxPolicyChars
	}
	t.Fatalf("a vector names the field %q, which the charter does not have", field)
	return 0
}

// charterWith is a charter holding text in field, as `PUT /config/charter`
// stores it (trimmed), and the problem path a refusal of it names. Each is
// judged as a write of it onto a company with nothing written, which is what
// every rule judges.
func charterWith(field, text string) (config.Charter, string) {
	switch field {
	case "mission":
		return config.Charter{Mission: text}.Trimmed(), "mission"
	case "vision":
		return config.Charter{Vision: text}.Trimmed(), "vision"
	}
	return config.Charter{Policies: []string{text}}.Trimmed(), "policies[0]"
}

// TestTheCharterCountsCharactersAsTheDashboardDoes holds the engine's charter
// bounds to the shared vectors the dashboard's editor is held to as well.
//
// ONE COUNT ON BOTH SIDES, OR A DRAFT THE COUNTER CALLED FINE IS REFUSED. The
// editor marks a draft past a bound before Save and shows "n of 1024
// characters"; the engine refuses a write past the same bound. Both count code
// points of the TRIMMED value. A browser's `.length` counts UTF-16 units, so
// every emoji outside the basic plane counts twice there; a byte count makes
// every accent two and every CJK character three; a grapheme count makes a
// letter and its combining accent one. The vectors sit at each bound and one
// past it in text where those counts part, and this test fails a vector set
// that could not tell them apart, so a rule that drifted to any of them on
// either side fails one of the two tests. The trim is part of the count: the
// engine trims what `strings.TrimSpace` trims, which keeps a byte order mark
// a browser's `String.prototype.trim` drops and drops a next-line character
// it keeps, and the vectors hold both.
func TestTheCharterCountsCharactersAsTheDashboardDoes(t *testing.T) {
	t.Parallel()
	var partsFromBytes, partsFromUTF16 bool
	for _, v := range readCharterText(t).Lengths {
		bound := charterBound(t, v.Field)
		partsFromBytes = partsFromBytes || (v.Bytes <= bound) != v.Admitted
		partsFromUTF16 = partsFromUTF16 || (v.UTF16 <= bound) != v.Admitted
		t.Run(v.Name, func(t *testing.T) {
			t.Parallel()
			ch, path := charterWith(v.Field, v.value())
			stored := ch.Mission + ch.Vision + strings.Join(ch.Policies, "")
			if got := utf8.RuneCountInString(stored); got != v.Chars {
				t.Errorf("chars = %d, the vector says %d", got, v.Chars)
			}
			if got := len(utf16.Encode([]rune(stored))); got != v.UTF16 {
				t.Errorf("UTF-16 units = %d, the vector says %d", got, v.UTF16)
			}
			if got := len(stored); got != v.Bytes {
				t.Errorf("bytes = %d, the vector says %d", got, v.Bytes)
			}
			if v.Admitted != (v.Chars <= bound) {
				t.Fatalf("the vector says admitted=%v for %d characters against a bound of %d: "+
					"it no longer sits where it claims to", v.Admitted, v.Chars, bound)
			}
			refused := false
			for _, p := range config.Problems(charterCompany(t, ch).ValidateCharterChange(unwritten(t))) {
				if p.Path != path || p.Kind != "out_of_range" {
					t.Errorf("an unexpected problem at %s: %s %s", p.Path, p.Kind, p.Message)
					continue
				}
				refused = true
			}
			if refused == v.Admitted {
				t.Errorf("refused = %v at %s for %d characters, want admitted = %v",
					refused, path, v.Chars, v.Admitted)
			}
		})
	}
	if !partsFromBytes {
		t.Error("no vector decides differently counted in bytes, so a byte count would pass this gate")
	}
	if !partsFromUTF16 {
		t.Error("no vector decides differently counted in UTF-16 units, so a .length count would pass this gate")
	}
}

// TestTheCharterBreaksALineWhereTheDashboardDoes holds the one-line rule to
// the shared list of line-ending characters: inside a mission or a policy each
// is refused as `shape`, and the vision, written in paragraphs, takes it. The
// editor marks the same characters, so a draft it lets through is never
// refused for a break it did not show.
func TestTheCharterBreaksALineWhereTheDashboardDoes(t *testing.T) {
	t.Parallel()
	for _, b := range readCharterText(t).Breaks {
		t.Run(b.Name, func(t *testing.T) {
			t.Parallel()
			text := "Split costs" + b.Char + "fairly."
			for _, field := range []string{"mission", "policy", "vision"} {
				ch, path := charterWith(field, text)
				var got []string
				for _, p := range config.Problems(charterCompany(t, ch).ValidateCharterChange(unwritten(t))) {
					got = append(got, p.Path+" "+p.Kind)
				}
				want := []string{path + " shape"}
				if field == "vision" {
					want = nil
				}
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s holding %s: problems %v, want %v", field, b.Name, got, want)
				}
			}
		})
	}
}
