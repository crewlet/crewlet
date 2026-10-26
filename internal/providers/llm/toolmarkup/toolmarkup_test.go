package toolmarkup

import (
	"errors"
	"strings"
	"testing"
)

// The measured reply that reached a person as prose.
const live = `<invoke name="propose_api_change"><parameter name="route">PUT /config/units/{id}</parameter>` +
	`<parameter name="path">{"id": "core"}</parameter><parameter name="body">{"key": "core"}</parameter></invoke>`

// Every form the grammar knows is found, and nothing that merely looks like
// XML, or is shown as code, is.
func TestIndexFindsTheCallFormsAndNothingElse(t *testing.T) {
	t.Parallel()
	found := []string{
		live,
		"Sure.\n<function_calls>\n" + live + "\n</function_calls>",
		`<ns:invoke name="x"></ns:invoke>`,
		"<tool_call>{\"name\": \"x\"}</tool_call>",
		"<tool_use>{\"name\": \"x\"}</tool_use>",
		"<function=x><parameter=a>1</parameter></function>",
		"a dangling close </invoke>",
		`<parameter name="route">GET /agents</parameter>`,
	}
	for _, text := range found {
		if Index(text) < 0 {
			t.Errorf("Index(%q) = -1, want the markup found", text)
		}
	}
	clean := []string{
		"plain prose",
		"a < b and c > d",
		"<function>a Maven element</function> is not a call",
		"<parameter>a WSDL element</parameter>",
		"```xml\n" + live + "\n```",
		"inline `<invoke name=\"x\">` is code",
		"<b>bold</b> and <i>italic</i>",
		"```\nunterminated fence " + live,
	}
	for _, text := range clean {
		if i := Index(text); i >= 0 {
			t.Errorf("Index(%q) = %d, want -1", text, i)
		}
	}
}

// Read returns whole calls, their parameters verbatim, and the spans Prose
// removes.
func TestReadReturnsWholeCalls(t *testing.T) {
	t.Parallel()
	text := "I will do it.\n<function_calls>\n" + live +
		"\n<invoke name=\"describe_api\"></invoke>\n</function_calls>\nDone."
	blocks, err := Read(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || blocks[0].Name != "propose_api_change" || blocks[1].Name != "describe_api" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if got := blocks[0].Params; len(got) != 3 || got[1] != (Param{Name: "path", Value: `{"id": "core"}`}) {
		t.Errorf("params = %+v", got)
	}
	if got := Prose(text, blocks); got != "I will do it.\n\nDone." {
		t.Errorf("Prose = %q", got)
	}
}

// A parameter's value is verbatim, markup and all.
func TestAParameterValueKeepsItsOwnMarkup(t *testing.T) {
	t.Parallel()
	text := `<invoke name="write"><parameter name="body"><p>Hi <invoke name="x"> there</p></parameter></invoke>`
	blocks, err := Read(text)
	if err != nil || len(blocks) != 1 {
		t.Fatalf("blocks, err = %+v, %v", blocks, err)
	}
	if v := blocks[0].Params[0].Value; v != `<p>Hi <invoke name="x"> there</p>` {
		t.Errorf("value = %q", v)
	}
}

// Markup that is not whole calls is malformed, never a partial run.
func TestMalformedMarkupIsRefused(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		`<invoke name="x"><parameter name="a">1`,
		`<invoke name="x">`,
		`</invoke>`,
		`<invoke><parameter name="a">1</parameter></invoke>`,
		`<invoke name="x"><parameter name="a">1</parameter><parameter name="a">2</parameter></invoke>`,
		`<function_calls></function_calls>`,
		`<tool_call>{"name": "x"}`,
		`<parameter name="a">1</parameter>`,
		`<invoke name="x"><tool_call>{}</tool_call></invoke>`,
	} {
		if _, err := Read(text); !errors.Is(err, ErrMalformed) {
			t.Errorf("Read(%q) err = %v, want ErrMalformed", text, err)
		}
	}
}

// A live view never draws a tag or the start of one.
func TestVisibleHoldsBackEveryTagAndItsStart(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Adding it." + live:            "Adding it.",
		"Adding it.<":                  "Adding it.",
		"Adding it.<inv":               "Adding it.",
		"Adding it.<invoke name=\"pro": "Adding it.",
		"Adding it.<ns:invoke":         "Adding it.",
		"Adding it.</func":             "Adding it.",
		"Adding it.<function=call_":    "Adding it.",
		"a < b":                        "a < b",
		"a <b>bold</b>":                "a <b>bold</b>",
		"x <bold":                      "x <bold",
		"plain":                        "plain",
	}
	for in, want := range cases {
		if got := Visible(in); got != want {
			t.Errorf("Visible(%q) = %q, want %q", in, got, want)
		}
	}
	// Every prefix of a reply that writes a call is shown no further than
	// its prose.
	text := "Adding it. " + live
	for i := range len(text) + 1 {
		if got := Visible(text[:i]); strings.Contains(got, "<") {
			t.Fatalf("Visible(%q) = %q, drawing a tag's start", text[:i], got)
		}
	}
}

// FuzzRead holds the grammar to its invariants on any text: no panic, spans
// in bounds and in order, Prose free of markup when every block read, and
// Visible a prefix of the text with no markup.
func FuzzRead(f *testing.F) {
	for _, seed := range []string{live, "<function_calls>" + live, "x < y", "```" + live,
		"<tool_call>{}</tool_call>", "<function=a><parameter=b>c</parameter></function>"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		blocks, err := Read(text)
		at := 0
		for _, b := range blocks {
			if b.Start < at || b.End < b.Start || b.End > len(text) {
				t.Fatalf("block span [%d,%d) out of order or bounds (prev end %d, len %d)",
					b.Start, b.End, at, len(text))
			}
			at = b.End
		}
		if err == nil && len(blocks) > 0 && Index(Prose(text, blocks)) >= 0 {
			t.Fatalf("Prose(%q) still carries markup", text)
		}
		v := Visible(text)
		if !strings.HasPrefix(text, v) || Index(v) >= 0 {
			t.Fatalf("Visible(%q) = %q", text, v)
		}
	})
}

// A MARKER WITH AN INFO STRING ONLY OPENS, so one stray ``` ends where the
// next labelled fence begins instead of shifting every pair after it; a
// marker followed by prose on its line closes, as a model writes one.
func TestFencesPairAroundAStrayMarker(t *testing.T) {
	t.Parallel()
	body := func(text string, f Fence) string { return strings.TrimSpace(text[f.BodyStart:f.BodyEnd]) }
	stray := "<invoke name=\"x\"></invoke>\n```\n\nActually, the format.\n\n```json\n{\"a\": 1}\n```"
	fs := Fences(stray)
	if len(fs) != 2 || fs[0].Closed || !fs[1].Closed || body(stray, fs[1]) != `{"a": 1}` {
		t.Fatalf("Fences(stray) = %+v, want the stray marker open and the labelled fence holding the object", fs)
	}
	trailing := "```\ncode\n``` and then some prose"
	if fs := Fences(trailing); len(fs) != 1 || !fs[0].Closed || body(trailing, fs[0]) != "code" {
		t.Errorf("Fences(trailing) = %+v, want one closed fence holding the code", fs)
	}
	if got := Prose(stray[:strings.Index(stray, "\n```")+4], []Block{{Start: 0, End: 26}}); got != "" {
		t.Errorf("Prose kept %q of a call and a stray fence, want nothing", got)
	}
}
