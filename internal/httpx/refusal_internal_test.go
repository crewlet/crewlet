package httpx

import "testing"

// A BODY THAT DISTILLED TO NOTHING IS NAMED, AND THE NAME IS A CLASSIFICATION
// OF THE BODY — not a restatement of which branch the caller took.
//
// [RefusalOf] reaches [unquotable] down exactly one path today, so a test
// driven through RefusalOf could only ever exercise the first arm and would
// leave the second looking dead. It is not dead: it is what this function
// answers the moment [Refusal] learns to drop any other shape, and a line
// asserting "an HTML page" over a body that was not HTML would be a worse
// answer than the "" it replaced. So both arms are asserted here, in the
// package that owns the rule, over the two inputs the classifier actually
// reads.
//
// Internal rather than in the black-box suite for the only honest reason:
// the second arm has no caller to reach it through yet.
func TestAnUnquotableBodyIsClassifiedByWhatItIs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, contentType, text, want string
	}{
		{"a page announced as one", "text/html",
			"<body><div>nope</div></body>",
			"an HTML page with no <title>, which is layout rather than an explanation"},
		{"a page that announces itself", "",
			"<!doctype html><body><div>nope</div></body>",
			"an HTML page with no <title>, which is layout rather than an explanation"},
		{"anything else", "application/octet-stream", "\x00\x01\x02",
			"a shape with no sentence, envelope or title in it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := unquotable(c.contentType, c.text); got != c.want {
				t.Errorf("unquotable = %q, want %q", got, c.want)
			}
		})
	}
}

// AND THE HTML ARM IS THE ONLY ONE THAT CAN YIELD NOTHING OVER A BODY THAT
// HAD CONTENT, which is the reachability claim [unquotable]'s doc rests on.
//
// If it stopped holding, [RefusalOf] would start describing bodies it could
// have quoted — so it is checked rather than remembered.
func TestOnlyTheHTMLArmCanDistilToNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, contentType, body string }{
		{"a JSON null", "application/json", "null"},
		{"an empty JSON object", "application/json", "{}"},
		{"an empty JSON array", "", "[]"},
		{"JSON that will not parse", "application/json", "{not json"},
		{"a single character", "text/plain", "x"},
		{"bytes with no shape at all", "application/octet-stream", "\x00\x01\x02"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := Refusal(c.contentType, []byte(c.body)); got == "" {
				t.Errorf("Refusal(%q) = \"\", so a body that could be quoted "+
					"is described instead", c.body)
			}
		})
	}
}
