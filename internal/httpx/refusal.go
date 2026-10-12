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
// TWO KILOBYTES, which is what four of the seven clients already used before
// this was shared. A refusal's useful content is a sentence; past a couple of
// kilobytes a body is not explaining the refusal, it is a document that
// happens to have arrived instead of one.
//
// It replaced six different answers to one question — 2048 twice, 4096, two
// named constants, and 1 MiB — and the outlier is the one that produced the
// defect below. It is the ONE bound on what a refusal says: there used to be a
// second, 400 bytes, applied silently after the first, so a validation list
// past four hundred bytes lost its later fields with nothing to say it had —
// to the operator, and to the model whose tool call the refusal answers.
const RefusalBytes = 2 << 10

// Refusal is what an endpoint SAID about refusing a request, as one line.
//
// # A body that is not an explanation must not be pasted into an error
//
// Every vendor client decodes the JSON shapes its own vendor uses and then
// needs an answer for everything else. The answer was `string(body)`, and for
// an HTML error page that is the whole page: Atlassian's 403 reached an
// operator's log as a rendered document — doctype, head, inline styles, script
// tags — around a sentence nobody could find, with the one fact that mattered
// (an unconfirmed address, a missing scope) buried in it if it was there at
// all. Atlassian read up to a MEGABYTE of it.
//
// So a non-JSON body is read for what it can honestly yield and nothing more:
// an HTML page's TITLE, which is where a proxy, a gateway and a login wall all
// put the reason ("403 Forbidden", "Sign in to continue"), and plain text as
// itself. A page with no title yields nothing, which is the honest answer — the
// status code is carried separately and already says more than the markup does.
//
// # Bounded where it is read, and said
//
// Only the first [RefusalBytes] of body are shaped, and a body that ran past
// them is MARKED, because "the explanation is off-screen" and "the endpoint
// explained itself badly" must not be the same string. What is shaped is never
// cut again. [ReadRefusal] is the reading half, for the callers that have the
// response rather than a body some other read already bounded.
//
// # Shared, because the fallback is the part that was wrong everywhere
//
// The vendor-specific SHAPES stay with their vendors: a Jira error envelope is
// Jira's business and a union of seven of them would be a grab-bag nobody
// owns. What is shared is the question every one of them asks last — "this is
// not a shape I know; what can I say?" — which is where each had its own
// mistake.
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
	return ReadRefusalFrom(resp.Header.Get("Content-Type"), resp.Body)
}

// ReadRefusalFrom is [ReadRefusal] for a body read through something of the
// caller's own — an idle timer, a stream frame reader — rather than straight
// off the response.
func ReadRefusalFrom(contentType string, body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, RefusalBytes+1))
	text := Refusal(contentType, raw)
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
