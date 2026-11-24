package httpx

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/textcut"
)

// RefusalBytes is how much of a refused response is worth reading.
//
// Two kilobytes: a refusal's useful content is a sentence; past that a body is
// a document that arrived instead of an explanation.
//
// It is the ONE bound on what a refusal says. A second, silent cut after it
// would drop a long validation list's later fields with nothing to say so, to
// the operator and to the model whose tool call the refusal answers.
const RefusalBytes = 2 << 10

// Refusal is what an endpoint SAID about refusing a request, as one line.
//
// A body that is not an explanation is never pasted into an error: an HTML
// error page would bury the one fact that mattered in a rendered document. A
// non-JSON body yields an HTML page's TITLE (where proxies and login walls put
// the reason) or plain text as itself; no title yields nothing, the status code
// being carried separately.
//
// Only the first [RefusalBytes] are shaped, and a body past them is MARKED so
// "off-screen" and "explained badly" differ; shaped text is never cut again.
// [ReadRefusal] is the reading half. Vendor SHAPES stay with their vendors;
// only this last fallback is shared.
func Refusal(contentType string, body []byte) string {
	cut := len(body) > RefusalBytes
	text := strings.TrimSpace(textcut.Bytes(string(body), RefusalBytes))
	if text == "" {
		return ""
	}
	var said string
	switch {
	case looksJSON(contentType, text):
		// A SHAPE THE CALLER DID NOT KNOW. Pretty-printed JSON in a log line
		// is noise, and a compact object is at least readable, so it is
		// re-encoded rather than passed through. A cut one will not parse
		// and is read as text, which the marker below explains.
		said = compactJSON(text)
	case looksHTML(contentType, text):
		said = titleOf(text)
	default:
		said = collapseSpace(text)
	}
	if cut && said != "" {
		said += fmt.Sprintf(" …(the response runs past the %d KiB this build reads)", RefusalBytes>>10)
	}
	return said
}

// ReadRefusal reads a refused response's body and answers [Refusal] over it:
// at most [RefusalBytes] read, marked where the body ran past them, and a
// body that could not be read at all NAMED rather than reported as silence —
// a body that died mid-read is a different fact from a short one.
//
// The one read every client's error arm takes, because each spelled it
// `io.ReadAll(io.LimitReader(resp.Body, 2048))` for itself, which stops at the
// cap with a clean end of file — so none of them could say that it had cut.
func ReadRefusal(resp *http.Response) string {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, RefusalBytes+1))
	text := Refusal(resp.Header.Get("Content-Type"), raw)
	if err != nil && text == "" {
		return "(the response body could not be read: " + err.Error() + ")"
	}
	return text
}

func looksJSON(contentType, text string) bool {
	if strings.Contains(strings.ToLower(contentType), "json") {
		return true
	}
	return strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[")
}

func looksHTML(contentType, text string) bool {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return true
	}
	lower := strings.ToLower(text)
	return strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html")
}

// titleRe finds an HTML document's title, which is where every gateway,
// proxy and login wall puts the reason it refused.
var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func titleOf(text string) string {
	match := titleRe.FindStringSubmatch(text)
	if len(match) < 2 {
		// NOTHING, rather than the markup. The status code says more than a
		// page of layout does, and a caller that pastes this into a message
		// is better served by an empty detail than by a document.
		return ""
	}
	return collapseSpace(html.UnescapeString(match[1]))
}

// compactJSON re-encodes a JSON body onto one line, or leaves it alone where
// it will not parse.
func compactJSON(text string) string {
	var any any
	if err := json.Unmarshal([]byte(text), &any); err != nil {
		return collapseSpace(text)
	}
	out, err := json.Marshal(any)
	if err != nil {
		return collapseSpace(text)
	}
	return string(out)
}

// spaceRe is any run of whitespace, including the newlines that turn one
// refusal into twelve log lines.
var spaceRe = regexp.MustCompile(`\s+`)

func collapseSpace(text string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))
}
