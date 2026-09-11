package httpx_test

import (
	"strings"
	"testing"

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

// AND EVERYTHING IS BOUNDED. A refusal that needs more than a few hundred
// bytes to be understood needs the endpoint's own logs, and the caller pastes
// this into a log line and an error message.
func TestARefusalIsBounded(t *testing.T) {
	t.Parallel()
	got := httpx.Refusal("text/plain", []byte(strings.Repeat("verbose ", 4096)))
	if len(got) > httpx.RefusalDetail {
		t.Errorf("Refusal is %d bytes, past the %d-byte bound", len(got), httpx.RefusalDetail)
	}
	if got == "" {
		t.Error("a long refusal was dropped entirely rather than cut")
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
