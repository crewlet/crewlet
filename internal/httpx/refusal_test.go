package httpx_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/httpx"
)

// AN HTML ERROR PAGE IS NOT AN EXPLANATION, AND MUST NOT BE PASTED INTO ONE.
//
// Every vendor client decoded its own JSON shapes and then fell back to
// `string(body)`. Atlassian's admin API answers a 403 with a rendered HTML
// page, and Atlassian read up to a MEGABYTE of body — so the whole document,
// doctype, inline styles, script tags and all, reached an operator's log
// around a sentence nobody could find.
//
// The title is what a proxy, a gateway and a login wall all put the reason in,
// so that is what is kept.
func TestAnHTMLRefusalYieldsItsTitleAndNotItsMarkup(t *testing.T) {
	t.Parallel()
	page := `<!DOCTYPE html><html><head><title>403 Forbidden</title>
<style>body{font:14px sans-serif}</style></head>
<body><h1>Forbidden</h1><script>track()</script>
<p>You don't have permission to access this resource.</p></body></html>`

	got := httpx.Refusal("text/html; charset=utf-8", []byte(page))

	if got != "403 Forbidden" {
		t.Errorf("Refusal = %q, want the page's title alone", got)
	}
	for _, markup := range []string{"<", "script", "style", "DOCTYPE"} {
		if strings.Contains(got, markup) {
			t.Errorf("Refusal = %q, which still carries %q", got, markup)
		}
	}
}

// A PAGE WITH NO TITLE YIELDS NOTHING, which is the honest answer.
//
// The status code is carried separately and says more than a page of layout
// does. Returning the markup because "an empty string helps nobody" is what
// produced the defect above.
func TestAnUntitledPageYieldsNothingRatherThanMarkup(t *testing.T) {
	t.Parallel()
	got := httpx.Refusal("text/html", []byte(`<html><body><div class="err">nope</div></body></html>`))
	if got != "" {
		t.Errorf("Refusal = %q, want nothing: markup is not an explanation", got)
	}
}

// AN UNRECOGNISED JSON SHAPE IS STILL JSON, so it is compacted rather than
// dropped: a vendor envelope this build does not know is the caller's best
// clue about what happened.
func TestAnUnknownJSONShapeIsCompactedNotDropped(t *testing.T) {
	t.Parallel()
	got := httpx.Refusal("application/json", []byte("{\n  \"weird\": {\n    \"code\": 7\n  }\n}"))
	if got != `{"weird":{"code":7}}` {
		t.Errorf("Refusal = %q, want the body on one line", got)
	}
}

// PLAIN TEXT SURVIVES, collapsed onto one line.
//
// A refusal split over twelve lines becomes twelve log lines, and the one
// that matters is not reliably the first.
func TestPlainTextIsKeptOnOneLine(t *testing.T) {
	t.Parallel()
	got := httpx.Refusal("text/plain", []byte("your primary email address\n\n   is not confirmed\n"))
	if got != "your primary email address is not confirmed" {
		t.Errorf("Refusal = %q", got)
	}
}

// AND EVERYTHING IS BOUNDED, MARKED AND RUNE-SAFE — on every arm.
//
// [httpx.RefusalDetail] is a CEILING the vendor suites assert, so the marker
// has to fit INSIDE it: [textcut.Within], not Ellipsis and not Bytes. Without
// the marker a sentence severed mid-clause reads as the endpoint's complete
// answer, and nothing here can prove otherwise — the body is read once and
// dropped, so the rest of it exists only in the endpoint's own logs.
//
// The three arms are asserted together because they are one policy. A commit
// that changed one of them alone would ship two marking policies for one
// value class, and nothing else in the tree compares them.
func TestEveryRefusalArmIsBoundedMarkedAndRuneSafe(t *testing.T) {
	t.Parallel()
	// Each body is long enough to force the cut, and each carries a
	// multi-byte rune at the boundary: a plain byte slice there yields
	// invalid UTF-8 that a JSON encoder silently substitutes.
	for _, c := range []struct {
		arm, contentType, body string
	}{
		{"plain text", "text/plain", strings.Repeat("verboseé ", 4096)},
		{"a JSON shape this build does not know",
			"application/json", `{"é":"` + strings.Repeat("whyé ", 4096) + `"}`},
		{"a page whose title is longer than the line budget",
			"text/html", "<title>" + strings.Repeat("Forbiddené ", 4096) + "</title>"},
	} {
		t.Run(c.arm, func(t *testing.T) {
			t.Parallel()
			got := httpx.Refusal(c.contentType, []byte(c.body))
			if got == "" {
				t.Fatal("a long refusal was dropped entirely rather than cut")
			}
			if len(got) > httpx.RefusalDetail {
				t.Errorf("Refusal is %d bytes, past the %d-byte ceiling",
					len(got), httpx.RefusalDetail)
			}
			if !strings.HasSuffix(got, "…") {
				t.Errorf("Refusal = %q, which was cut with nothing saying so", got)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Refusal is not valid UTF-8: %q", got)
			}
		})
	}
}

// A BODY THAT WAS NOT READ IS NOT A BODY THAT SAID NOTHING, and the line says
// which by naming the ceiling that refused it.
//
// [httpx.ReadBody] refuses rather than cutting, so its error is the only
// thing that knows an overrun happened. A caller that drops it into `_`
// reports the overrun as an empty Detail, which reads as a silent endpoint
// and sends an operator to the wrong system.
func TestAnUnreadBodyIsReportedWithTheCeilingThatRefusedIt(t *testing.T) {
	t.Parallel()
	oversized := strings.Repeat("<div>sso</div>", httpx.RefusalBytes)
	body, err := httpx.ReadBody(strings.NewReader(oversized), httpx.RefusalBytes)
	if err == nil {
		t.Fatal("ReadBody accepted a body past its ceiling")
	}

	got := httpx.RefusalOf("text/html", body, err)
	if got == "" {
		t.Fatal("a body that was not read is reported exactly like an empty one")
	}
	if !strings.Contains(got, strconv.Itoa(httpx.RefusalBytes)) {
		t.Errorf("RefusalOf = %q, which names no ceiling, so a reader cannot "+
			"tell a refused body from a quoted one", got)
	}
	if strings.Contains(got, "<div") {
		t.Errorf("RefusalOf = %q, which quotes a prefix of a body that was "+
			"never read whole", got)
	}
}

// THE ERR ARM IS BOUNDED AND MARKED TOO, because a transport failure's text
// is the one part of that line this package does not choose.
//
// It is the arm most easily left unbounded — an err is "just a few words"
// until a proxy's error carries a URL, a chain of wrapped contexts and a
// certificate subject.
func TestAnUnreadableBodysReasonIsBoundedAndMarked(t *testing.T) {
	t.Parallel()
	got := httpx.RefusalOf("text/html", nil,
		errors.New(strings.Repeat("dial tcp: ", 400)))
	if len(got) > httpx.RefusalDetail {
		t.Errorf("RefusalOf is %d bytes, past the %d-byte ceiling",
			len(got), httpx.RefusalDetail)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("RefusalOf = %q, which was cut with nothing saying so", got)
	}
}

// AN EMPTY ANSWER MEANS AN EMPTY BODY, AND NOTHING ELSE.
//
// It is the invariant a caller's error field can rest on, and it is asserted
// here because this is the function that decides it. The three outcomes that
// would otherwise collapse into "" — a body that was not read, a page with no
// title, a shape with nothing quotable in it — each report themselves,
// because they call for opposite next steps.
func TestOnlyAnEmptyBodyYieldsAnEmptyAnswer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, contentType, body string
		err                     error
		wantEmpty               bool
	}{
		{name: "an empty body", contentType: "application/json", wantEmpty: true},
		{name: "a whitespace-only body", contentType: "text/plain", body: " \n\t ", wantEmpty: true},
		{name: "a page with no title", contentType: "text/html",
			body: `<html><body><div class="err">nope</div></body></html>`},
		{name: "a body that was not read", contentType: "text/html",
			body: "<html>", err: httpx.ErrResponseTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := httpx.RefusalOf(c.contentType, []byte(c.body), c.err)
			if c.wantEmpty && got != "" {
				t.Errorf("RefusalOf = %q, want \"\": an empty answer is "+
					"reserved for an empty body", got)
			}
			if !c.wantEmpty && got == "" {
				t.Error("this outcome is reported as though the endpoint " +
					"sent nothing, which is a different fact")
			}
		})
	}
}

// A PAGE WITH NO TITLE IS REPORTED AS THE PAGE IT IS — its size and its
// shape — with the markup still dropped.
//
// HAProxy's default error page and many WAF pages carry no title at all, so
// this is the common case rather than a corner: reporting one as "the
// endpoint said nothing" sends an operator to the endpoint instead of to the
// proxy in front of it. The line is bounded by construction — it quotes a
// length and a fixed phrase, never the body — and that is asserted here
// because it is the one arm that interpolates a value.
func TestAnUntitledPageIsNamedRatherThanDropped(t *testing.T) {
	t.Parallel()
	page := `<html><body>` + strings.Repeat("<div>x</div>", 40) + `</body></html>`

	got := httpx.RefusalOf("text/html", []byte(page), nil)

	if !strings.Contains(got, strconv.Itoa(len(page))) {
		t.Errorf("RefusalOf = %q, which does not say how much arrived", got)
	}
	if !strings.Contains(got, "<title>") {
		t.Errorf("RefusalOf = %q, which does not say what the body was, so a "+
			"reader cannot tell it from a shape this build failed to read", got)
	}
	if strings.Contains(got, "<div") {
		t.Errorf("RefusalOf = %q, which pasted the markup back in", got)
	}
	if len(got) > httpx.RefusalDetail {
		t.Errorf("RefusalOf is %d bytes, past the %d-byte ceiling",
			len(got), httpx.RefusalDetail)
	}
}

// AND WHAT DISTILS TO SOMETHING STILL WINS, so the arms above are the
// fallback rather than the answer.
func TestADistilledLineIsPreferredToADescriptionOfTheBody(t *testing.T) {
	t.Parallel()
	got := httpx.RefusalOf("text/html",
		[]byte(`<!doctype html><html><head><title>403 Forbidden</title></head>`+
			`<body><div>nope</div></body></html>`), nil)
	if got != "403 Forbidden" {
		t.Errorf("RefusalOf = %q, want the page's title alone", got)
	}
}

// A BODY WITH NO CONTENT TYPE IS STILL READ FOR WHAT IT IS.
//
// Not every endpoint sets one, and the two shapes that matter announce
// themselves: JSON opens with a brace, a page with a doctype.
func TestTheShapeIsRecognisedWithoutAContentType(t *testing.T) {
	t.Parallel()
	if got := httpx.Refusal("", []byte(`{"message":"nope"}`)); got != `{"message":"nope"}` {
		t.Errorf("untyped JSON = %q", got)
	}
	if got := httpx.Refusal("", []byte("<!doctype html><title>Sign in</title>")); got != "Sign in" {
		t.Errorf("untyped HTML = %q", got)
	}
}
