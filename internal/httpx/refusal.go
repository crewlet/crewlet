package httpx

import (
	"encoding/json"
	"html"
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
// defect below.
const RefusalBytes = 2 << 10

// RefusalDetail bounds what reaches a log line or an error message.
//
// Shorter than what is READ, deliberately: the read cap stops a client
// buffering a document, and this stops a sentence-shaped answer from an
// endpoint having a paragraph's worth of stack trace after it. A refusal that
// needs more than this to be understood needs the endpoint's own logs.
const RefusalDetail = 400

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
// # Shared, because the fallback is the part that was wrong everywhere
//
// The vendor-specific SHAPES stay with their vendors: a Jira error envelope is
// Jira's business and a union of seven of them would be a grab-bag nobody
// owns. What is shared is the question every one of them asks last — "this is
// not a shape I know; what can I say?" — which is where each had its own
// mistake.
func Refusal(contentType string, body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	switch {
	case looksJSON(contentType, text):
		// A SHAPE THE CALLER DID NOT KNOW. Pretty-printed JSON in a log line
		// is noise, and a compact object is at least readable, so it is
		// re-encoded rather than passed through.
		return textcut.Bytes(compactJSON(text), RefusalDetail)
	case looksHTML(contentType, text):
		return textcut.Bytes(titleOf(text), RefusalDetail)
	default:
		return textcut.Bytes(collapseSpace(text), RefusalDetail)
	}
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
