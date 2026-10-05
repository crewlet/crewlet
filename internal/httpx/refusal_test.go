package httpx_test

import (
	"errors"
	"io"
	"net/http"
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

// BOUNDED WHERE IT IS READ, AND SAID. Only the first RefusalBytes are shaped,
// a body past them is marked, and what is shaped is never cut a second time —
// a 400-byte cut used to follow the read, silently, so a validation list lost
// its later fields with nothing to say it had.
func TestARefusalIsBoundedOnceAndSaysSo(t *testing.T) {
	t.Parallel()
	got := httpx.Refusal("text/plain", []byte(strings.Repeat("verbose ", 4096)))
	if len(got) > httpx.RefusalBytes+80 {
		t.Errorf("Refusal is %d bytes, past the %d-byte read", len(got), httpx.RefusalBytes)
	}
	if !strings.Contains(got, "runs past the 2 KiB this build reads") {
		t.Errorf("a body past the read is unmarked: …%q", got[max(0, len(got)-80):])
	}

	fields := `{"errors":[` + strings.Repeat(`"field is required",`, 40) + `"last field"]}`
	if len(fields) > httpx.RefusalBytes || len(fields) < 600 {
		t.Fatalf("the case needs a body between the old cut and the read: %d bytes", len(fields))
	}
	if got := httpx.Refusal("application/json", []byte(fields)); !strings.Contains(got, "last field") ||
		strings.Contains(got, "runs past") {
		t.Errorf("a refusal within the read was cut: %q", got)
	}
}

// THE READING HALF says when it cut and when it could not read at all.
func TestReadRefusalMarksACutAndNamesAFailedRead(t *testing.T) {
	t.Parallel()
	resp := &http.Response{Header: http.Header{"Content-Type": {"text/plain"}},
		Body: io.NopCloser(strings.NewReader(strings.Repeat("x ", httpx.RefusalBytes)))}
	if got := httpx.ReadRefusal(resp); !strings.HasSuffix(got, "…(the response runs past the 2 KiB this build reads)") {
		t.Errorf("a long body read unmarked: …%q", got[max(0, len(got)-80):])
	}
	resp = &http.Response{Header: http.Header{}, Body: io.NopCloser(failingReader{})}
	if got := httpx.ReadRefusal(resp); !strings.Contains(got, "could not be read: connection reset") {
		t.Errorf("a failed read = %q, want it named", got)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

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
