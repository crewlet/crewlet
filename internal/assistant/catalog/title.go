package catalog

import (
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/cards"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A GENERIC REQUEST IS NAMED FOR THE RESOURCE IT REACHES, never for its route
// pattern: "Seat · engineer", "Agents", "Company configuration", where the pattern
// "GET /config/roles/{id}" is how the mux spells it and nothing a person
// calls it. The card a read draws, the step it runs as and the step a change
// is prepared in are all titled by [RequestTitle], so the three never name one
// request two ways.
//
// FROM THE PATH, NOT FROM THE REFERENCE'S PROSE. The API reference documents
// every route in a sentence ("Active revision, redacted (full JSON; ...)",
// "One entity, redacted, with an ETag"), which is what the model reads, and a
// sentence is not a name: cutting one down to a noun is a guess about English
// that the next row of the table breaks. The path IS the API's own naming of
// what it serves, a collection and the item of it a wildcard picks, so the
// title is read off it by one rule:
//
//   - the noun is the last literal segment, in the singular when a wildcard
//     follows it (`/config/roles/{id}` reads one role);
//   - a route that reads a fixed resource two or more literals deep names the
//     one above it too (`/tokens/breakdown` is "Tokens breakdown", where
//     "Breakdown" alone says nothing);
//   - the values the call filled in follow, so two reads of one route are
//     told apart (`Seat · engineer`, `File · ENG › docs/plan.md`);
//   - a route family whose path says it wrong is named as a person calls it
//     ([resources]): a `/config/roles/{id}` is a seat;
//   - a call on the act route is named for the tool it calls ("React to
//     message"), which is what it reaches: the route only carries it.

// titleValueBytes bounds one path value in a title: a handle, a key or a short
// path reads whole, and a value the model wrote longer than a line is cut
// rather than widening the card's heading past the drawer.
const titleValueBytes = 80

// RequestTitle is the title of the request a generic call names on pattern
// ("GET /config/roles/{id}", a HEAD read as its GET) with the wildcard values
// path holds, as a person reads it. Every value passes redact.Secrets: a value
// is the model's to write, and a credential it pasted into a path is a
// credential in a heading.
func RequestTitle(pattern string, path map[string]string) string {
	if tool := oneLineValue(path["tool"]); strings.TrimSpace(pattern) == ActRoute && tool != "" {
		return cards.Humanise(tool)
	}
	_, template, found := strings.Cut(strings.TrimSpace(pattern), " ")
	if !found {
		template = pattern
	}
	segments := strings.Split(strings.Trim(template, "/"), "/")
	var literals []int
	var values []string
	for i, segment := range segments {
		name, wild := wildcardName(segment)
		switch {
		case segment == "" || name == "$":
		case wild:
			if v := oneLineValue(path[name]); v != "" {
				values = append(values, v)
			}
		default:
			literals = append(literals, i)
		}
	}
	if len(literals) == 0 {
		return "API"
	}
	last := literals[len(literals)-1]
	noun := cards.Humanise(segments[last])
	followed := false
	for _, segment := range segments[last+1:] {
		if name, wild := wildcardName(segment); wild && name != "$" {
			followed = true
		}
	}
	switch {
	case followed:
		noun = cards.Singular(noun)
	case len(literals) > 1 && len(values) == 0 && literals[len(literals)-2] == last-1:
		// A FIXED RESOURCE BENEATH ANOTHER: the one above names it.
		noun = cards.Humanise(segments[last-1]) + " " + lowerFirst(noun)
	}
	// WHAT A PERSON CALLS IT, where the path says it wrong: a
	// `/config/roles/{id}` is a seat ([resources]).
	if r, ok := resourceAt(template); ok {
		noun = capitalised(r.Noun)
	}
	if len(values) == 0 {
		return noun
	}
	return noun + " · " + strings.Join(values, " › ")
}

// wildcardName is a segment's wildcard name ("id" for `{id}`, "path" for
// `{path...}`), and whether the segment is one.
func wildcardName(segment string) (string, bool) {
	if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
		return "", false
	}
	return strings.TrimSuffix(strings.Trim(segment, "{}"), "..."), true
}

// oneLineValue is one path value as a title carries it: redacted, on one line,
// within [titleValueBytes].
func oneLineValue(v string) string {
	v = strings.Join(strings.Fields(redact.Secrets(v)), " ")
	if len(v) > titleValueBytes {
		v = textcut.Within(v, titleValueBytes)
	}
	return v
}

// lowerFirst is a noun inside a phrase: its first letter lowered, unless it
// opens an initialism ("MCP servers" stays).
func lowerFirst(s string) string {
	if len(s) > 1 && s[1] >= 'A' && s[1] <= 'Z' {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// callTitle is the title of the request a generic call names: [RequestTitle]
// of its route and the path values it filled in. The method plays no part, so
// a HEAD is titled as its GET.
func callTitle(args Args) string {
	path, _ := pathValues(args["path"])
	return RequestTitle(args.String("route"), path)
}
