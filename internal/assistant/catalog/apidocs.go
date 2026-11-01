package catalog

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE API REFERENCE AS THE MODEL READS IT: docs/reference/api-endpoints.md,
// embedded in the binary (the module root's APIReference), indexed by route.
//
// The reference names a route in the shapes a person writes it, and every one
// is read: a row of a route table (`| GET | /path | what it does |`), a table
// row or a list item that leads with it (`| POST /iam/people | admin |`,
// `- POST /artifacts/{id}/tickets (…)`, several methods sharing one path as
// `PUT`, `DELETE /path`), a heading that names it, whose section is the
// route's long form, and a mention in a sentence, which documents a route
// only where nothing above does. A walk holds the index to the route table
// this build mounts in both directions (internal/api's
// TestEveryAssistantRouteIsDocumented), so a route added without its
// documentation, or documentation left behind by a removed route, is a red
// build rather than a model reading a guess.

// routeDoc is one route's documentation.
type routeDoc struct {
	// Summary is the line that documents it: its table row or list item,
	// or the sentence that mentions it where nothing leads with it.
	Summary string
	// Heading and Section are the heading that names it and the text
	// beneath it, where one does.
	Heading, Section string
	// strong marks a summary from a line that leads with the route.
	strong bool
}

// The reference's shapes of a route.
var (
	tableRow = regexp.MustCompile("^\\|\\s*`(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS|WS)`\\s*\\|" +
		"\\s*`([^`]+)`\\s*\\|(.*)\\|\\s*$")
	headingRoute = regexp.MustCompile("^(#{2,4})\\s.*?`(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS|WS) " +
		"(/[^`]*)`")
	headingLine = regexp.MustCompile(`^(#{1,6})\s`)
	wildcard    = regexp.MustCompile(`\{[^}]*\}`)
	// mention is a route named in backticks anywhere in a line, with the
	// methods that share its path ahead of it (`PUT`, `DELETE /path`).
	mention = regexp.MustCompile("((?:`(?:GET|HEAD|POST|PUT|PATCH|DELETE)`,\\s*)*)" +
		"`(GET|HEAD|POST|PUT|PATCH|DELETE) (/[^`\\s]*)`")
	method = regexp.MustCompile("GET|HEAD|POST|PUT|PATCH|DELETE")
)

// docKey is a route's pattern with every wildcard's name erased and any query
// the reference spells beside it dropped, so the reference's `{event_id}` and
// the mux's `{id}` are one route: what the reference documents is the shape,
// and a wildcard's name is the mux's.
func docKey(pattern string) string {
	pattern, _, _ = strings.Cut(pattern, "?")
	return wildcard.ReplaceAllStringFunc(pattern, func(w string) string {
		if strings.HasSuffix(w, "...}") {
			return "{...}"
		}
		return "{}"
	})
}

// referenceIndex is the reference, indexed by [docKey] once.
var referenceIndex = sync.OnceValue(func() map[string]routeDoc {
	return indexReference(crewlet.APIReference)
})

// indexReference reads a reference's routes.
func indexReference(md string) map[string]routeDoc {
	out := map[string]routeDoc{}
	note := func(key, summary string, strong bool) {
		doc := out[key]
		if doc.Summary == "" || (strong && !doc.strong) {
			doc.Summary, doc.strong = summary, strong
		}
		out[key] = doc
	}
	lines := strings.Split(md, "\n")
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := tableRow.FindStringSubmatch(line); m != nil {
			note(docKey(m[1]+" "+m[2]), strings.TrimSpace(m[3]), true)
			continue
		}
		if m := headingRoute.FindStringSubmatch(line); m != nil {
			key := docKey(m[2] + " " + strings.TrimSpace(m[3]))
			doc := out[key]
			if doc.Heading == "" {
				doc.Heading = strings.TrimSpace(strings.TrimLeft(line, "# "))
				doc.Section = section(lines[i+1:], len(m[1]))
				out[key] = doc
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		for _, at := range mention.FindAllStringSubmatchIndex(line, -1) {
			m := mention.FindStringSubmatch(line[at[0]:at[1]])
			path := m[3]
			if cut := strings.IndexAny(path, "[?"); cut >= 0 {
				path = path[:cut]
			}
			// A LINE THAT LEADS WITH THE ROUTE documents it; one that
			// names it in passing documents it only where nothing else
			// does.
			lead := strings.TrimLeft(trimmed, "|-* ")
			strong := strings.HasPrefix(lead, strings.TrimSpace(line[at[0]:at[1]]))
			for _, verb := range append(method.FindAllString(m[1], -1), m[2]) {
				note(docKey(verb+" "+path), strings.Trim(trimmed, "|- "), strong)
			}
		}
	}
	return out
}

// section is the text beneath a heading of level, up to the next heading of
// that level or a higher one.
func section(lines []string, level int) string {
	var b strings.Builder
	fenced := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if m := headingLine.FindStringSubmatch(line); !fenced && m != nil && len(m[1]) <= level {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// docOf is the documentation of the route a mux pattern names: its own entry,
// or else the entry of a documented shape that covers it, a wildcard segment
// of the reference's standing for a literal one of the mux's (the reference's
// `/config/{kind}/{id}` documents `/config/roles/{id}`).
func docOf(pattern string) (routeDoc, bool) {
	key := docKey(pattern)
	index := referenceIndex()
	if doc, ok := index[key]; ok {
		return doc, true
	}
	for _, documented := range slices.Sorted(maps.Keys(index)) {
		if covers(documented, key) {
			return index[documented], true
		}
	}
	return routeDoc{}, false
}

// covers reports whether the documented shape doc covers the route key: the
// same method and segments, each a wildcard or the same literal.
func covers(doc, key string) bool {
	docMethod, docPath, _ := strings.Cut(doc, " ")
	method, path, _ := strings.Cut(key, " ")
	want, got := strings.Split(docPath, "/"), strings.Split(path, "/")
	if docMethod != method || len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != got[i] && want[i] != "{}" && want[i] != "{...}" {
			return false
		}
	}
	return true
}

// Covers is [covers] over two mux or reference patterns, for the walk that
// holds the reference to the route table.
func Covers(doc, pattern string) bool { return covers(docKey(doc), docKey(pattern)) }

// DocumentedRoutes are the routes the embedded reference documents as its own,
// by a row, a leading item or a heading, by their documented pattern's
// [docKey]: for the walk that holds the reference to the
// route table.
func DocumentedRoutes() []string {
	out := make([]string, 0, len(referenceIndex()))
	for key, doc := range referenceIndex() {
		// A ROUTE NAMED IN PASSING is not documented as one of this API's:
		// a sentence may name a vendor's route, or a question's own spelling.
		if doc.strong || doc.Heading != "" {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// RouteDocumented reports whether the embedded reference documents the route
// a mux pattern names, whatever its wildcards are called.
func RouteDocumented(pattern string) bool {
	_, ok := docOf(pattern)
	return ok
}

// The most a docs answer hands the model: what one result holds, as every
// read's answer is cut to.
const docsBytes = 15 << 10

// document is a [DescribeAPITool] call's answer: one route's documentation, or
// the list of routes the person may use.
func (g generic) document(args Args) (string, error) {
	if pattern := args.String("route"); pattern != "" {
		return g.one(pattern)
	}
	return g.list(args.String("search")), nil
}

// one is one route's documentation.
func (g generic) one(pattern string) (string, error) {
	route, ok := g.table.Lookup(pattern)
	if !ok {
		return "", fmt.Errorf("%w: %q is not a route you can use; call describe_api with "+
			"no route to list them", ErrInvalidArguments, pattern)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n", route.Pattern, g.facts(route))
	doc, _ := docOf(route.Pattern)
	if doc.Summary != "" {
		fmt.Fprintf(&b, "\n%s\n", doc.Summary)
	}
	if doc.Section != "" {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", doc.Heading, doc.Section)
	}
	return cut(b.String(), docsBytes, "The documentation goes on in "+
		"docs/reference/api-endpoints.md."), nil
}

// list is every route the person may use, matching search where it names
// words, one line each.
func (g generic) list(search string) string {
	words := strings.Fields(strings.ToLower(search))
	var b strings.Builder
	b.WriteString("Routes you can use. A read is sent with api_request; a change is " +
		"proposed with propose_api_change and sent only once the person approves it. " +
		"Call describe_api with a route for its documentation.\n\n")
	listed := 0
	for _, route := range g.table.routes {
		if !g.mayUse(route) {
			continue
		}
		doc, _ := docOf(route.Pattern)
		if len(words) > 0 {
			hay := strings.ToLower(route.Pattern + " " + doc.Summary + " " + doc.Heading)
			if !slices.ContainsFunc(words, func(w string) bool { return strings.Contains(hay, w) }) {
				continue
			}
		}
		line := route.Pattern + " (" + g.facts(route) + ")"
		if len(words) > 0 && doc.Summary != "" {
			line += ": " + firstSentence(doc.Summary)
		}
		if b.Len()+len(line) > docsBytes {
			b.WriteString("\nThe list goes on; narrow it with search.\n")
			return b.String()
		}
		b.WriteString(line)
		b.WriteByte('\n')
		listed++
	}
	if listed == 0 {
		b.WriteString("No route matches.\n")
	}
	return b.String()
}

// facts is how a route is decided, in a few words: whether it reads or
// changes, through what, and whether a change asks the person to confirm it is
// them.
func (g generic) facts(route Route) string {
	parts := []string{"reads"}
	if !route.reads() {
		parts = []string{"a change the person approves"}
	}
	if route.Action == "" {
		return strings.Join(parts, ", ")
	}
	parts = append(parts, "decided by "+string(route.Action))
	if recency, _ := authz.RecencyOf(route.Action); recency == iam.RecencyStepUp {
		parts = append(parts, "asks the person to confirm it is them")
	}
	// WHAT THE PREVIEW DOES FOR THE MODEL (ADR-0055), so it neither sends the
	// check itself nor writes a summary the route would read twice.
	if route.DryRun {
		parts = append(parts, "checked by the route before the person sees it (a "+
			"refusal comes back to you to correct; never send dry_run)")
	}
	if route.Summary {
		parts = append(parts, "its history line is your `summary` argument (never "+
			"send X-Summary or _summary)")
	}
	if route.Replaces {
		parts = append(parts, "replacing: the body is the whole object, so send every "+
			"field it holds now that should stay, and name each field the person asked to "+
			"remove in `removes`; a field left out and not named is refused")
	}
	return strings.Join(parts, ", ")
}

// mayUse reports whether the person could be admitted to route at all: a
// filter on what is LISTED, never authority, which the route's own guard is.
// A verb whose row needs an app the person does not hold, or admin they do not
// hold, is left out; every other route is listed, and decided when it is sent.
func (g generic) mayUse(route Route) bool {
	if route.Action == "" {
		return true
	}
	if app, _ := authz.AppOf(route.Action); app != "" && !g.person.Can(app) {
		return false
	}
	class, _ := authz.ClassOf(route.Action)
	return class != authz.ClassAdmin || g.person.Can(iam.GrantAdmin)
}

// firstSentence is a description's first sentence.
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

// cut is s within limit bytes, on a line, saying it was cut.
func cut(s string, limit int, more string) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit-len(more)-2]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	return strings.ToValidUTF8(s, "") + "\n\n" + more
}

// queryRow is a row of the reference's question table: the question, the
// parameters it takes, and the route it answers.
var queryRow = regexp.MustCompile("^\\|\\s*`[a-z_]+`\\s*\\|\\s*`\\{([^}]*)\\}`\\s*\\|\\s*`GET (/[^`\\s]*)`")

// queryIndex is every read route's query parameters as the reference's
// question table documents them, by [docKey].
var queryIndex = sync.OnceValue(func() map[string][]string {
	return indexQuery(crewlet.APIReference)
})

// indexQuery reads the question table's rows of a reference: each route's
// parameters, an optional one's `?` dropped, in the order documented.
func indexQuery(md string) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(md, "\n") {
		m := queryRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := docKey("GET " + m[2])
		for _, param := range strings.Split(m[1], ",") {
			if name := strings.TrimSuffix(strings.TrimSpace(param), "?"); name != "" &&
				!slices.Contains(out[key], name) {
				out[key] = append(out[key], name)
			}
		}
	}
	return out
}

// queryParams is the query parameters the reference documents for a read
// route, nil for a route its question table does not hold.
func queryParams(pattern string) []string { return queryIndex()[docKey(pattern)] }

// pagedByLimit reports whether the reference says a read route pages by
// `limit`.
func pagedByLimit(pattern string) bool { return slices.Contains(queryParams(pattern), "limit") }
