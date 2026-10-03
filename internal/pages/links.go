package pages

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// # A link to a page is its ID, in one of two grammars
//
// A page is ADDRESSED by its id and by nothing else: a title changes on rename,
// so a link that named one broke the day somebody fixed a heading, and the
// dashboard's own address for a page (`#/knowledge/pages/{id}`) carries the id
// for exactly that reason. A backlink is therefore a statement that one body
// carries another page's id in an address, and [Links] recognises exactly two:
//
//   - `#/knowledge/pages/<uuid>` — the dashboard's address, pasted from its
//     address bar or composed by a seat from [AddressPrefix];
//   - `/pages/<uuid>` — the same page as a path, which is the REST route and
//     the tail of any deployment URL a person copies
//     (`https://crewlet.example.com/#/knowledge/pages/<uuid>` carries both,
//     and is one link).
//
// A bare uuid in prose is not a link — it is a value somebody quoted — and
// neither is `CONTAINER/Title`, which is how a seat NAMES a page to get_page
// and is exactly the address a rename moves. Recognising either would make the
// backlink list a guess about intent.
//
// # Code is not a link
//
// A fenced block or an inline code span is an EXAMPLE — a runbook showing the
// shape of a URL, a snippet of a config — and a page that documents the
// address grammar would otherwise be "linked from" every page it uses as an
// illustration. The fences are CommonMark's: three or more backticks or
// tildes, closed by a fence of the same character at least as long, and an
// unclosed fence runs to the end of the body, which is what a renderer does
// with it too.
//
// # Percent-encoding
//
// An address that crossed a redirect or a chat surface arrives encoded
// (`%23%2Fknowledge%2Fpages%2F…`), and the id inside it is the same link. Each
// run of non-space text is matched both as written and decoded, so an encoded
// address is found and a malformed escape costs nothing but that decoding.

// AddressPrefix is the dashboard's address of a page, before its id.
//
// ONE SPELLING, held against the dashboard's own `PAGE_ADDRESS_PREFIX`
// (`dashboard/src/contract/links.ts`) by
// TestTheDashboardAddressesAPageTheWayTheBacklinksReadIt: the screen that
// mints the address and the extractor that reads it back are the two halves
// of one grammar, and a screen that moved its route would otherwise leave
// every link it minted afterwards invisible to the backlinks.
const AddressPrefix = "#/knowledge/pages/"

// pathPrefix is the tail both grammars share. [AddressPrefix] ends in it, so
// matching it alone finds both — which is the point: `#/knowledge/pages/<id>`
// and `/pages/<id>` are one link to one page.
const pathPrefix = "/pages/"

// linkID is a page id after [pathPrefix], and the character after it must not
// continue an id — `/pages/<uuid>x` is some other path. Case-insensitive,
// because a uuid is, and normalised to lowercase on the way out, which is how
// the engine mints every page id.
var linkID = regexp.MustCompile(`(?i)/pages/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:[^0-9a-z-]|$)`)

// Links answers the ids of every page a body links to, lowercased, sorted and
// without repeats. It is pure over the body, which is what lets the lexical
// indexer derive the backlink rows from exactly the text it indexes — and what
// makes a change to the grammar a derivation bump rather than a migration of
// anybody's pages.
func Links(body string) []string {
	if !strings.Contains(strings.ToLower(body), "pages") {
		// THE COMMON CASE, answered without a pass: most bodies link to
		// no page at all, and the indexer calls this on every one.
		return nil
	}
	seen := map[string]bool{}
	for _, run := range strings.Fields(proseOf(body)) {
		collect(run, seen)
		if strings.Contains(run, "%") {
			if decoded, err := url.PathUnescape(run); err == nil && decoded != run {
				collect(decoded, seen)
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// collect adds every id [linkID] finds in one run of text.
func collect(run string, into map[string]bool) {
	if !strings.Contains(strings.ToLower(run), pathPrefix) {
		return
	}
	for _, m := range linkID.FindAllStringSubmatch(run, -1) {
		into[strings.ToLower(m[1])] = true
	}
}

// proseOf is the body with its code removed: every fenced block and every
// inline code span becomes a space, so a link on either side of one is still
// its own run of text and nothing inside one is matched.
func proseOf(body string) string {
	var out strings.Builder
	out.Grow(len(body))
	var fence string // the open fence's marker run, or "" outside one
	for line := range strings.Lines(body) {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if marker := fenceMarker(trimmed); marker != "" && indent < 4 {
			switch {
			case fence == "":
				fence = marker
				out.WriteByte('\n')
				continue
			case marker[0] == fence[0] && len(marker) >= len(fence) &&
				strings.TrimSpace(strings.TrimLeft(trimmed, marker[:1])) == "":
				// A CLOSING FENCE carries nothing after its marker.
				fence = ""
				out.WriteByte('\n')
				continue
			}
		}
		if fence != "" {
			out.WriteByte('\n')
			continue
		}
		out.WriteString(withoutCodeSpans(line))
	}
	return out.String()
}

// fenceMarker is the run of three or more backticks or tildes a line starts
// with, or "".
func fenceMarker(line string) string {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return ""
	}
	return line[:n]
}

// withoutCodeSpans blanks every inline code span in one line: a run of N
// backticks closed by the next run of exactly N. An unclosed run is literal
// text, which is CommonMark's rule and a renderer's.
func withoutCodeSpans(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	var out strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			out.WriteByte(line[i])
			i++
			continue
		}
		n := 0
		for i+n < len(line) && line[i+n] == '`' {
			n++
		}
		open := line[i : i+n]
		end := closingRun(line[i+n:], n)
		if end < 0 {
			out.WriteString(open)
			i += n
			continue
		}
		out.WriteByte(' ')
		i += n + end + n
	}
	return out.String()
}

// closingRun is where a run of exactly n backticks starts in s, or -1.
func closingRun(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		if j-i == n {
			return i
		}
		i = j
	}
	return -1
}
