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

// RefusalDetail bounds how much of what an endpoint SAID reaches a log line
// or an error message.
//
// Shorter than what is READ, deliberately: the read cap stops a client
// buffering a document, and this stops a sentence-shaped answer from an
// endpoint having a paragraph's worth of stack trace after it.
//
// What a line cuts is not lost by the cutting: a [Quote] carries the whole
// of what was said beside the line, and whether the line holds less of it.
// Keeping that whole somewhere a reader will look is the caller's to do,
// because this package holds nothing once it returns.
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
// the status code already says more than it does — and [QuoteRefusal] is what
// turns that nothing into a line, because a dropped page reported as "" reads
// as an endpoint that was silent.
//
// # What is cut, and how it is marked
//
// The distilled line is bounded by [RefusalDetail] with [textcut.Within]
// rather than [textcut.Bytes]: that budget is a CEILING its callers assert,
// so the marker has to fit inside it rather than push past it, and an
// unmarked cut is a severed sentence read as a whole one.
//
// Refusal returns the line ALONE, so what it cut is gone once it returns:
// nothing in this package keeps a body, a line or the rest of one. A caller
// whose reader needs the rest takes [QuoteRefusal] instead, whose [Quote]
// hands back the whole of what was said beside the line.
//
// # Shared, because the fallback is the part that was wrong everywhere
//
// The vendor-specific SHAPES stay with their vendors: a Jira error envelope is
// Jira's business and a union of seven of them would be a grab-bag nobody
// owns. What is shared is the question every one of them asks last — "this is
// not a shape I know; what can I say?" — which is where each had its own
// mistake.
func Refusal(contentType string, body []byte) string {
	return QuoteSaid(distill(contentType, strings.TrimSpace(string(body)))).Line
}

// distill is the whole of what a trimmed body said, before any cut — or ""
// for a body with nothing quotable in it.
func distill(contentType, text string) string {
	switch {
	case text == "":
		return ""
	case looksJSON(contentType, text):
		// A SHAPE THE CALLER DID NOT KNOW. Pretty-printed JSON in a log line
		// is noise, and a compact object is at least readable, so it is
		// re-encoded rather than passed through.
		return compactJSON(text)
	case looksHTML(contentType, text):
		return titleOf(text)
	default:
		return collapseSpace(text)
	}
}

// A Quote is what an endpoint said about refusing a request: the line to
// report, and the whole of what that line was cut from.
//
// The whole travels BESIDE the line because this package holds nothing once
// it returns: a caller that keeps only the line has lost the rest for good.
// Shortened is what that caller reads to learn there is a rest to keep.
type Quote struct {
	// Line is the one line to report. Where the endpoint said something
	// quotable it is that, at most [RefusalDetail] bytes and ending in "…"
	// where it was cut. Otherwise it is a sentence naming why nothing could
	// be quoted: the read's own error, whole, or a body that distilled to
	// nothing. From [QuoteRefusal], empty means exactly one thing — the body
	// was empty.
	Line string

	// Said is the whole of what the endpoint said, distilled and never cut:
	// an unknown JSON shape compacted onto one line, a page's <title>, text
	// with its whitespace collapsed, or the sentence a vendor client pulled
	// out of its own envelope. Empty when nothing was quotable.
	Said string

	// Shortened reports that Line holds less than Said.
	Shortened bool
}

// QuoteSaid bounds one thing an endpoint said to a line, keeping the whole of
// it beside the line.
//
// It is the cut [Refusal] makes, exported for a vendor client that decoded
// its own envelope: the sentence it pulled out is bounded to the same
// ceiling and marked the same way as everything this package distils.
func QuoteSaid(said string) Quote {
	line := textcut.Within(said, RefusalDetail)
	return Quote{Line: line, Said: said, Shortened: line != said}
}

// RefusalOf is the line of [QuoteRefusal] alone, for a caller with nowhere to
// keep the rest: what that line cut is gone once this returns.
func RefusalOf(contentType string, body []byte, err error) string {
	return QuoteRefusal(contentType, body, err).Line
}

// QuoteRefusal is [Refusal] for a caller that read the body with [ReadBody],
// and it keeps what the line cut.
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
// So an empty Line from here means exactly one thing: the endpoint sent an
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
// # A body that was not read is named, never quoted
//
// [ReadBody] answers nil beside its error, so a caller that read with it
// holds none of a body it refused. There is no prefix to quote and no rest
// to keep, which is why that arm reports the read's failure and nothing
// else: Said stays empty, because the endpoint's words never arrived.
func QuoteRefusal(contentType string, body []byte, err error) Quote {
	if err != nil {
		// WHOLE, NOT CUT. This is the READ's error rather than anything the
		// body said — [ReadBody]'s sentinel naming the ceiling that refused
		// it, or whatever the transport reported for a body that stopped
		// arriving — and [RefusalDetail] bounds what an endpoint SAID. Cut
		// here, the rest would exist nowhere: the error goes no further than
		// this line.
		return Quote{Line: "the refusal body could not be read: " + err.Error()}
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		// THE ONE THING AN EMPTY LINE IS ALLOWED TO MEAN.
		return Quote{}
	}
	if quote := QuoteSaid(distill(contentType, text)); quote.Line != "" {
		return quote
	}
	return Quote{Line: fmt.Sprintf(
		"the endpoint answered %d bytes with nothing quotable in them: %s",
		len(body), unquotable(contentType, text))}
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
