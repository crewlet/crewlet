package httpx

import (
	"encoding/json"
	"fmt"
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
// itself. A page with no title yields nothing HERE — the markup is layout and
// the status code already says more than it does — and [RefusalOf] is what
// turns that nothing into a line, because a dropped page reported as "" reads
// as an endpoint that was silent.
//
// # What is cut, and how it is marked
//
// The distilled line is bounded by [RefusalDetail] with [textcut.Within]
// rather than [textcut.Bytes]: that budget is a CEILING its callers assert,
// so the marker has to fit inside it rather than push past it, and an
// unmarked cut is a severed sentence read as a whole one. The whole body is
// not recoverable from this process — it is read once and dropped — which is
// why the marker is the only thing standing between a reader and a half
// sentence they will treat as the endpoint's complete answer; the rest of it
// is in the endpoint's own logs.
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
		return textcut.Within(compactJSON(text), RefusalDetail)
	case looksHTML(contentType, text):
		return textcut.Within(titleOf(text), RefusalDetail)
	default:
		return textcut.Within(collapseSpace(text), RefusalDetail)
	}
}

// RefusalOf is [Refusal] for a caller that read the body with [ReadBody], and
// it is the call every vendor client makes.
//
// # Three outcomes, because "" used to mean all three
//
// A client that reads a refusal body has three things to report and had one
// word for them. [Refusal] answers the middle one — the body arrived, here is
// what it said — and the two it cannot see are the two an operator most needs
// kept apart:
//
//   - THE BODY WAS NOT READ, because it ran past [RefusalBytes] or the
//     connection failed part way through it. [ReadBody] refuses rather than
//     cutting and its error carries the ceiling, so the line names that
//     ceiling instead of implying the first 2 048 bytes are what was said.
//   - THE BODY ARRIVED AND DISTILLED TO NOTHING, which is an HTML page with
//     no <title>. Dropping the markup is right — it is layout, not an
//     explanation — but reporting the drop as "" says the endpoint was
//     silent, when what actually happened is that a proxy answered a page.
//
// So an empty answer from here means exactly one thing: the endpoint sent an
// empty body. [github.com/crewlet/crewlet/internal/mattermost.Error]'s Message
// field documents that invariant, and it is enforced here because this is the
// function that decides it.
//
// # One copy, because a second one drifts silently
//
// A client that wrote these arms itself would be choosing its own sentinel
// wording, its own bound and its own marker for a value class every client
// reports the same way — the shape internal/textcut, internal/whsec and
// internal/jsprovision each exist to have ended. The drift would be invisible:
// every copy still compiles, and every copy still produces a plausible
// string, so nothing but a reader comparing two logs would notice.
//
// # Where the whole body is
//
// Nowhere in this process, deliberately: a refusal body is read once and
// dropped, so there is no store column and no event payload to recover it
// from. That is exactly why nothing here may quote a prefix of it — the
// endpoint's own logs hold the whole answer, and this line has to say the
// body went unread rather than imply it has been quoted.
func RefusalOf(contentType string, body []byte, err error) string {
	if err != nil {
		// MARKED, NAMED AND BOUNDED. The sentinel's own text carries the
		// ceiling that refused the body, and a transport failure's text is
		// the only unbounded part of this line — so it is cut INSIDE
		// [RefusalDetail] rather than at a literal, with the marker that
		// stops a severed sentence reading as a complete one.
		return textcut.Within("the refusal body could not be read: "+err.Error(), RefusalDetail)
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		// THE ONE THING "" IS ALLOWED TO MEAN.
		return ""
	}
	if line := Refusal(contentType, body); line != "" {
		return line
	}
	return fmt.Sprintf("the endpoint answered %d bytes with nothing quotable in them: %s",
		len(body), unquotable(contentType, text))
}

// unquotable names what a body that distilled to nothing actually was.
//
// [Refusal] yields nothing over a body that HAS content in exactly one case:
// an HTML document whose <title> is missing or empty. The JSON arm re-encodes
// to at least a pair of quotes and the plain-text arm keeps the body's own
// text, so neither can reach here. Naming the case is what makes the line
// worth reading — "a page with no title" sends an operator to the proxy in
// front of the endpoint, where a bare "unquotable" sends them nowhere.
//
// The second arm is not decoration and not dead-by-accident: this is a
// CLASSIFIER over its two inputs rather than a branch of the caller's
// control flow, and a line that asserted HTML over a body that was not HTML
// would be a worse answer than the "" it replaces. It is reached the moment
// [Refusal] learns to drop any other shape.
func unquotable(contentType, text string) string {
	if looksHTML(contentType, text) {
		return "an HTML page with no <title>, which is layout rather than an explanation"
	}
	return "a shape with no sentence, envelope or title in it"
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
