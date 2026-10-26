package catalog

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A GENERIC CHANGE IS NAMED FOR WHAT IT DOES TO WHAT THE PERSON CALLS IT.
//
// The route table says how a change is sent and decided (its verb, its check,
// whether it replaces); it says nothing a person reads. A card titled by its
// method ("Change through the API") over the route and the body asked a
// founder to review JSON to learn that it adds a Technical Writer. So the card
// is described here, from the same diff of the resource as it is now against
// what the change makes of it that its rows are drawn from, by one rule for
// every route:
//
//   - The NOUN is what the person calls the resource: [resources] names the
//     ones whose path says it wrong (`/config/roles/{id}` is a seat), and every
//     other route takes the noun its path names ([RequestTitle]).
//   - A resource that holds a list of named members (a unit's seats) is
//     titled for the members a change adds or removes: "Add seat Technical
//     Writer", "Remove seat QA Engineer". Any other change is titled by its
//     verb and the record's name: "Change seat QA Engineer", "Create MCP
//     server linear", "Delete task ENG-4".
//   - A change that deletes, removes a member, or removes fields of what it
//     replaces is DESTRUCTIVE ([wire.RiskDestructive]): its summary says what
//     goes, and its note whether it can be reverted, so a removal is never one
//     struck-through row among sixty.
//
// Nothing here is the model's: the names are read from the representation and
// the body the digest binds, and the risk is computed, never declared.

// resource is what a person calls the records one route family serves.
type resource struct {
	// Path is the route's path pattern, as the table holds it.
	Path string
	// Noun is one record, in the lower case a title says it in ("seat").
	Noun string
	// Members are the record's lists of named members, by field, each with
	// the noun one member is called ("roles" holds seats).
	Members map[string]string
	// Title, where set, names a change the diff cannot: a create whose
	// body names what it creates by reference (a brand kit's version).
	Title func(body map[string]any) (title, verb string)
	// Summary and Note, where set, are said of every change on the route.
	Summary, Note string
	// Singleton is a resource there is one of, titled by its noun alone:
	// the company configuration is not "company configuration Acme".
	Singleton bool
}

// historyNote is what a change to a route that keeps a revision history can
// say about being undone: every revision stays, and any one of them can be
// reverted to (`POST /config/revisions/{id}/revert`).
const historyNote = "The previous version stays in the configuration history, so " +
	"this can be reverted."

// noUndoNote is what a destructive change to a route that keeps no history
// says: the assistant has no undo for it.
const noUndoNote = "This cannot be undone from the assistant."

// resources are the route families whose path does not say what a person
// calls them. A route not here is named by its path ([RequestTitle]), which
// is right for most: a project is a project.
var resources = []resource{
	{Path: "/config", Noun: "company configuration", Singleton: true,
		Members: map[string]string{"roles": "seat", "units": "unit"}},
	{Path: "/config/roles/{id}", Noun: "seat"},
	{Path: "/config/units/{id}", Noun: "unit", Members: map[string]string{"roles": "seat"}},
	{Path: "/config/llm-providers/{id}", Noun: "model provider"},
	{Path: "/config/mcp-servers/{id}", Noun: "MCP server"},
	{Path: "/config/charter", Noun: "company charter", Singleton: true},
	{Path: "/brand/adoptions", Noun: "brand", Singleton: true,
		Title: func(body map[string]any) (string, string) {
			if v := cards.Text(body["version"]); v != "" {
				return "Adopt version " + v + " of the brand kit", "adopt"
			}
			return "Adopt a brand kit version", "adopt"
		},
		Summary: "Everyone in the company, every page and every agent uses the adopted " +
			"version as the company's brand.",
		Note: "Adopting another version, or withdrawing the brand, changes it again."},
	{Path: "/brand/adoptions/current", Noun: "company brand", Singleton: true,
		Summary: "The company has no brand until another version is adopted."},
}

// resourceOf is the resource a route's path names, if [resources] holds it.
func resourceOf(route Route) (resource, bool) { return resourceAt(route.Path()) }

// resourceAt is the resource a path pattern names, if [resources] holds it.
func resourceAt(path string) (resource, bool) {
	i := slices.IndexFunc(resources, func(r resource) bool { return r.Path == path })
	if i < 0 {
		return resource{}, false
	}
	return resources[i], true
}

// nameFields are the fields a record names itself by, for a person, in the
// order they are asked for.
var nameFields = []string{"name", "title", "label"}

// recordName is the name a record goes by: its first name field, else its
// identity ([identityKeys]).
func recordName(v any) string {
	record, _ := v.(map[string]any)
	for _, k := range append(slices.Clone(nameFields), identityKeys...) {
		if s := strings.TrimSpace(cards.Text(record[k])); s != "" && isPlainText(record[k]) {
			return oneLineValue(s)
		}
	}
	return ""
}

func isPlainText(v any) bool { _, ok := v.(string); return ok }

// described is a generic change as the person reads it: its title and
// approve label, what it does where the title does not say it, whether it
// can be reverted, its risk, and the record it acts on ("item ENG-1").
type described struct {
	Title, ApproveLabel, Summary, Note, Object string
	Risk                                       wire.ProposalRisk
}

// describe names the change req makes on route to the resource read as before
// (nil where nothing was read, or nothing is there), leaving it as after (nil
// for a delete); removals are the fields a replace removes, address the path's
// values and l the labeller its rows were drawn with.
func describe(route Route, req string, creates bool, before, after any, body any,
	removals []string, address map[string]string, l labeller) described {

	res, known := resourceOf(route)
	noun := res.Noun
	if !known {
		noun = lowerFirst(cards.Singular(pathNoun(route.Path())))
	}
	name := ""
	if !res.Singleton {
		name = recordName(after)
		if name == "" {
			name = recordName(before)
		}
		if name == "" {
			name = addressValue(address)
		}
	}
	d := described{Object: strings.TrimSpace(noun + " " + name)}
	var verb string
	switch {
	case res.Title != nil:
		record, _ := body.(map[string]any)
		d.Title, verb = res.Title(record)
	case req == http.MethodDelete:
		d.Title, verb = titled("Delete", noun, name), "delete"
	case req == http.MethodPost || creates:
		d.Title, verb = titled("Create", noun, name), "create"
	default:
		d.Title, verb = titled("Change", noun, name), "save"
		if t, v, ok := memberTitle(res, noun, name, before, after, l); ok {
			d.Title, verb = t, v
		}
	}
	d.ApproveLabel = "Approve and " + verb
	d.Summary, d.Note = res.Summary, res.Note

	gone, gonePaths := removedMembers(res, before, after, l)
	destructive := req == http.MethodDelete || len(gone) > 0 || len(removals) > 0
	if !destructive {
		return d
	}
	d.Risk = wire.RiskDestructive
	// WHAT GOES LEADS THE CARD, above every row (ADR-0055): the members a
	// change removes by name, then every other field a replace removes,
	// which only the person's own request may (the call's `removes`).
	var said []string
	switch {
	case req == http.MethodDelete:
		said = append(said, "This deletes "+d.Object+".")
	case len(gone) > 0:
		said = append(said, "This removes "+inWords(gone)+fromName(noun, name)+".")
	}
	var fields []string
	for _, r := range removals {
		if !covered(r, gonePaths) {
			fields = append(fields, l.label(r))
		}
	}
	if len(fields) > 0 {
		also := "This also removes "
		if len(said) == 0 {
			also = "This removes "
		}
		said = append(said, also+inWords(fields)+". Approve only if you asked for that.")
	}
	if len(said) > 0 {
		d.Summary = strings.Join(append(said, d.Summary), " ")
		d.Summary = strings.TrimSpace(d.Summary)
	}
	if d.Note == "" {
		d.Note = noUndoNote
		if route.Summary {
			d.Note = historyNote
		}
	}
	return d
}

// titled is a title: the verb, the noun and the record's name ("Change seat QA
// Engineer"), or the verb and the noun where the record has no name.
func titled(verb, noun, name string) string {
	return strings.TrimSpace(verb + " " + noun + " " + name)
}

// fromName is " from <noun> <name>" for a sentence naming what a removal
// leaves, empty where the resource has no name.
func fromName(noun, name string) string {
	if name == "" {
		return ""
	}
	return " from " + noun + " " + name
}

// addressValue is the record a path names, where it names one: its single
// path value.
func addressValue(address map[string]string) string {
	if len(address) != 1 {
		return ""
	}
	for _, v := range address {
		return oneLineValue(v)
	}
	return ""
}

// pathNoun is the last literal segment of a path pattern, in words: what its
// records are called where [resources] says nothing.
func pathNoun(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if _, wild := wildcardName(segments[i]); !wild && segments[i] != "" {
			return cards.Humanise(segments[i])
		}
	}
	return "Record"
}

// memberTitle titles a change by the members it adds or removes, where the
// resource holds named members and the change adds or removes some of one
// kind and nothing else of the record's own: "Add seat Technical Writer",
// "Remove 2 seats from unit Product". False for any other change.
func memberTitle(res resource, noun, name string, before, after any, l labeller) (string, string, bool) {
	b, _ := before.(map[string]any)
	a, _ := after.(map[string]any)
	if len(res.Members) == 0 || b == nil || a == nil {
		return "", "", false
	}
	// ONLY A CHANGE TO THE MEMBERS: one that also changes the record's own
	// fields is a change to the record, titled as one.
	if !equalJSON(without(b, res.Members), without(a, res.Members)) {
		return "", "", false
	}
	var added, removed []string
	member := ""
	for _, field := range slicesSorted(res.Members) {
		was, _ := b[field].([]any)
		is, _ := a[field].([]any)
		plus, minus := memberMoves(field, was, is, l)
		if len(plus)+len(minus) == 0 {
			continue
		}
		if member != "" {
			return "", "", false
		}
		member, added, removed = res.Members[field], plus, minus
	}
	switch {
	case member == "" || (len(added) > 0 && len(removed) > 0):
		return "", "", false
	case len(added) == 1:
		return "Add " + member + " " + added[0], "add", true
	case len(removed) == 1:
		return "Remove " + member + " " + removed[0], "remove", true
	case len(added) > 1:
		return "Add " + strconv.Itoa(len(added)) + " " + member + "s to " +
			strings.TrimSpace(noun+" "+name), "add", true
	}
	return "Remove " + strconv.Itoa(len(removed)) + " " + member + "s from " +
		strings.TrimSpace(noun+" "+name), "remove", true
}

// memberMoves are the members a list at field gains and loses, by identity,
// each by the name a person knows it by.
func memberMoves(field string, before, after []any, l labeller) (added, removed []string) {
	was, wasKeyed := keyed(before)
	is, isKeyed := keyed(after)
	if (!wasKeyed && len(before) > 0) || (!isKeyed && len(after) > 0) {
		return nil, nil
	}
	for _, e := range is {
		if find(was, e.id) == nil {
			added = append(added, l.member(join(field, e.id), e.id))
		}
	}
	for _, e := range was {
		if find(is, e.id) == nil {
			removed = append(removed, l.member(join(field, e.id), e.id))
		}
	}
	return added, removed
}

// removedMembers are the named members a change removes from the resource,
// each as its noun and name ("seat QA Engineer"), and their paths, across
// every member list it holds.
func removedMembers(res resource, before, after any, l labeller) (names, paths []string) {
	b, _ := before.(map[string]any)
	a, _ := after.(map[string]any)
	if b == nil {
		return nil, nil
	}
	for _, field := range slicesSorted(res.Members) {
		was, wasKeyed := keyed(asList(b[field]))
		is, isKeyed := keyed(asList(a[field]))
		if !wasKeyed || (!isKeyed && len(asList(a[field])) > 0) {
			continue
		}
		for _, e := range was {
			if find(is, e.id) == nil {
				names = append(names, res.Members[field]+" "+l.member(join(field, e.id), e.id))
				paths = append(paths, join(field, e.id))
			}
		}
	}
	return names, paths
}

func asList(v any) []any { list, _ := v.([]any); return list }

// without is a record without the given fields, for comparing what else of
// it a change moves.
func without(record map[string]any, fields map[string]string) map[string]any {
	out := make(map[string]any, len(record))
	for k, v := range record {
		if _, member := fields[k]; !member {
			out[k] = v
		}
	}
	return out
}

func slicesSorted(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// labeller names a change's rows as a person reads them: a member of a list
// by the name it carries rather than the identity it is matched by
// ("Technical Writer › Goal", not "Roles › tech writer › goal"), and a field
// that names other members (a seat's `manages`, a unit's `lead`) by their
// names.
type labeller struct {
	// names are the display names of every named list member, by the
	// path a row's field starts with ([join] of the list and the id).
	names map[string]string
	// ids are the same members' names by their identity alone, for the
	// fields that refer to them.
	ids map[string]string
}

// refFields are the fields whose values are other members' identities: a
// seat's `manages` and a unit's `lead` hold handles a person reads as names.
var refFields = []string{"manages", "lead"}

// newLabeller reads the named members of every keyed list in docs.
func newLabeller(docs ...any) labeller {
	l := labeller{names: map[string]string{}, ids: map[string]string{}}
	for _, d := range docs {
		l.walk("", d)
	}
	return l
}

func (l labeller) walk(field string, v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			l.walk(join(field, k), e)
		}
	case []any:
		list, ok := keyed(t)
		if !ok {
			return
		}
		for _, e := range list {
			path := join(field, e.id)
			if name := recordName(e.value); name != "" && name != e.id {
				l.names[path] = name
				l.ids[e.id] = name
			}
			l.walk(path, e.value)
		}
	}
}

// member is the name of the list member at path, else its identity.
func (l labeller) member(path, id string) string {
	if name, ok := l.names[path]; ok {
		return name
	}
	return id
}

// label is a row's field as a person reads it: the named member it lies
// under by its name, and the rest in words ([cards.FieldLabel]).
func (l labeller) label(field string) string {
	best := ""
	for prefix := range l.names {
		if (field == prefix || strings.HasPrefix(field, prefix+".")) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return cards.FieldLabel(field)
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(field, best), ".")
	if rest == "" {
		return l.names[best]
	}
	return l.names[best] + " › " + cards.FieldLabel(rest)
}

// value is a row's value as a person reads it: a field that refers to other
// members by identity names them ([refFields]).
func (l labeller) value(field, v string) string {
	if !slices.Contains(refFields, lastKey(field)) {
		return v
	}
	if name, ok := l.ids[v]; ok {
		return name
	}
	return v
}

// items is [labeller.value] over a list's items.
func (l labeller) items(field string, items []string) []string {
	if !slices.Contains(refFields, lastKey(field)) || len(items) == 0 {
		return items
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = l.value(field, item)
	}
	return out
}

// lastKey is the last key of a dotted field path.
func lastKey(field string) string {
	if i := strings.LastIndexByte(field, '.'); i >= 0 {
		return field[i+1:]
	}
	return field
}

// moreRows is the row a bounded card counts the rest of its rows in.
func moreRows(n int, technical bool) string {
	line := fmt.Sprintf("%d more fields change.", n)
	if technical {
		line += " Technical details has the whole change."
	}
	return line
}

// capitalised is s with its first letter in capitals, for a value that opens
// a row.
func capitalised(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
