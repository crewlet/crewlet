package catalog

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
)

// NAMES A PERSON SAYS, KEYS A ROUTE TAKES. A person asks about "the QA
// Engineer", "the Company space" or "the Product and Engineering unit"; the
// routes take a seat's handle, a space's key, a project's key and a unit's
// key. A tool declares which of its arguments name one of those records
// ([Tool.Names]), and every such argument is resolved by ONE function before
// the call's request or preview is built ([Resolver.Resolve]): a key as it
// is, or a name read against the directory of that record, replaced by its
// key. So the card, the digest the person approves and the request the
// approval sends all carry the key, and a model that guessed a key from a
// name ("COMPANY" for the space named Company, whose key is HOME) is told the
// choices before anything is proposed, rather than after the person approved
// a change the route refuses.
//
// # It never decides access
//
// Each directory is read through the turn's own grant, as the person, so a
// name resolves only to a record their own read lists. A directory the person
// may not read, or one that could not be read, resolves nothing: the value is
// sent as written and the route decides it, in its own words. Resolution is a
// convenience over what the route would answer, never a second authority.

// Directory is a kind of record a person names by its name as often as by its
// key.
type Directory string

const (
	// DirSeat is the org chart's seats, by handle and seat name.
	DirSeat Directory = "seat"
	// DirMember is the org chart's seats as a chat room's membership names
	// them, by handle and seat name. A handle the chart no longer lists may
	// still be a member (a seat removed from the chart stays in the rooms it
	// was in), so it is sent as written, as a read's is, and the write's
	// preview, which reads the room, decides it.
	DirMember Directory = "member"
	// DirUnit is the org chart's units, by key and name.
	DirUnit Directory = "unit"
	// DirProject is Worklet's projects, by key and name.
	DirProject Directory = "project"
	// DirSpace is Doclet's spaces, by key and name.
	DirSpace Directory = "space"
)

// Directories are the five, in declaration order.
var Directories = []Directory{DirSeat, DirMember, DirUnit, DirProject, DirSpace}

// Valid reports whether d is a directory this build knows.
func (d Directory) Valid() bool { return slices.Contains(Directories, d) }

// directoryRead is where each directory is read and how its entries are found
// in the answer: the read every person who could use the argument may make
// (the org chart is every reader's; projects are Worklet's, spaces Doclet's).
var directoryRead = map[Directory]struct {
	route string
	// plural names the records in a refusal.
	plural string
	walk   func(doc any) []Entry
}{
	DirSeat:    {"GET /org", "seats", seatEntries},
	DirMember:  {"GET /org", "seats", seatEntries},
	DirUnit:    {"GET /org", "units", unitEntries},
	DirProject: {"GET /worklet/projects", "projects", listEntries("projects", "key", "name")},
	DirSpace:   {"GET /containers", "spaces", listEntries("containers", "key", "name")},
}

// Entry is one record of a directory: the key a route takes and the name a
// person says.
type Entry struct{ Key, Name string }

// Shown is the entry as a refusal lists it and a card names it: "Company
// (HOME)", or the key alone where the record has no other name.
func (e Entry) Shown() string {
	if e.Name == "" || strings.EqualFold(e.Name, e.Key) {
		return e.Key
	}
	return e.Name + " (" + e.Key + ")"
}

// Resolver resolves the named arguments of a turn's calls, reading each
// directory at most once: a turn is seconds long, and a chart that moves
// within one is the case the route's own answer still decides. Safe for the
// concurrent calls of one round: a directory is read once, under its lock.
type Resolver struct {
	read  proposal.Read
	mu    sync.Mutex
	cache map[Directory][]Entry
	// unreadable remembers a directory that could not be read, so a turn
	// does not ask again for every call.
	unreadable map[Directory]bool
}

// NewResolver is a resolver reading through read, the turn's grant.
func NewResolver(read proposal.Read) *Resolver {
	return &Resolver{read: read, cache: map[Directory][]Entry{},
		unreadable: map[Directory]bool{}}
}

// Entries is directory d as the person may read it, and whether it could be
// read at all.
func (r *Resolver) Entries(ctx context.Context, d Directory) ([]Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entries, ok := r.cache[d]; ok {
		return entries, true
	}
	if r.unreadable[d] {
		return nil, false
	}
	how, known := directoryRead[d]
	if !known || r.read == nil {
		return nil, false
	}
	doc, err := readGet(ctx, r.read, how.route, nil, nil)
	if err != nil {
		r.unreadable[d] = true
		return nil, false
	}
	entries := how.walk(doc)
	r.cache[d] = entries
	return entries, true
}

// SeatName is how a card names the seat handle names: its seat's name, or
// the name with the handle beside it where another seat shares the name, so
// two rows never read as one seat. False where the directory does not hold
// the handle (or could not be read), and the handle is drawn as it is.
func (r *Resolver) SeatName(ctx context.Context, handle string) (string, bool) {
	entries, ok := r.Entries(ctx, DirSeat)
	if !ok {
		return "", false
	}
	i := slices.IndexFunc(entries, func(e Entry) bool { return e.Key == handle })
	if i < 0 || entries[i].Name == "" {
		return "", false
	}
	name := entries[i].Name
	for j, e := range entries {
		if j != i && spoken(DirSeat, e.Name) == spoken(DirSeat, name) {
			return entries[i].Shown(), true
		}
	}
	return name, true
}

// Resolve answers args with every argument t names ([Tool.Names]) resolved
// to its record's key: unchanged where it is a key already, replaced where it
// is the record's name. A value that names two records is refused as
// [ErrInvalidArguments] listing them, so the model picks one rather than
// guess. An empty value is left as it is (an assignee of "" unassigns), and so
// is every value of a directory that could not be read. args itself is never
// changed.
//
// A LIST IS RESOLVED ENTRY BY ENTRY, by the same rule: a list of names (the
// people a direct message goes to), or a list of members each naming its seat
// by `handle` (a room's membership).
//
// A VALUE THAT NAMES NOTHING THE DIRECTORY LISTS is refused, listing the
// choices, for a write, which changes a record that must be there now: a card
// the route would refuse once approved is never drawn. A read sends a value
// that could be a key as it is, because a read may name what the directory no
// longer lists (a seat removed from the chart still has its turns), and the
// route decides it; only a value no key is spelled like (it has a space in it)
// is refused with the choices, since it can only have been meant as a name. A
// membership ([DirMember]) is held to the read's rule for the read's reason: a
// seat removed from the chart is still in the rooms it was in.
//
// EVERY VALUE IT CANNOT PLACE IS NAMED IN ONE REFUSAL (ADR-0059): every
// argument's and every list entry's, each by its path (`to[1]`,
// `members[2].handle`), with each directory's choices once after them. A
// direct message to three people the chart does not hold is told all three at
// once, where stopping at the first cost the model an attempt per person.
func (r *Resolver) Resolve(ctx context.Context, t Tool, args map[string]any) (map[string]any, error) {
	out, faults := r.resolveAll(ctx, t, args, false)
	if len(faults) > 0 {
		return nil, refusedNames(nil, faults)
	}
	return out, nil
}

// Prepare is a call's arguments as the turn sends them: held to t's schema
// ([Tool.Check]), and every name in them resolved to its record's key
// ([Resolver.Resolve]). EVERY FAULT OF BOTH IS NAMED IN ONE REFUSAL
// (ADR-0059), the schema's first: an argument it refuses and a name that
// places nothing are answered together, where resolving first answered a
// misnamed seat alone and the schema's faults only on the next attempt.
//
// A VALUE THE SCHEMA FAULTS IS NOT RESOLVED: its fault is named already, and
// it is not the shape a name is read from. A name and the key it resolves to
// are held to one bound ([nameBytes]), so the key of a name the schema took is
// a value it takes too.
func (r *Resolver) Prepare(ctx context.Context, t Tool, args map[string]any) (map[string]any, error) {
	if t.schema == nil {
		return nil, fmt.Errorf("%w: %s declares no schema", ErrInvalidArguments, t.Name)
	}
	if args == nil {
		args = map[string]any{}
	}
	found := t.schema.problems(t.Name, args)
	out, faults := r.resolveAll(ctx, t, args, true)
	if len(found) > 0 || len(faults) > 0 {
		return nil, refusedNames(found, faults)
	}
	return out, nil
}

// resolveAll is args with every name in them resolved, and every value that
// could not be placed, in the order met. Where checked, a value t's schema
// faults is left as it is: naming its fault is the schema's.
func (r *Resolver) resolveAll(ctx context.Context, t Tool, args map[string]any,
	checked bool) (map[string]any, []nameFault) {

	if len(t.Names) == 0 || len(args) == 0 {
		return args, nil
	}
	out, cloned := args, false
	var faults []nameFault
	for _, arg := range slices.Sorted(maps.Keys(t.Names)) {
		var held *schema
		if checked {
			held = t.schema.properties[arg]
		}
		resolved, moved := r.resolveValue(ctx, t, arg, argPath(t, arg), held, args[arg],
			&faults)
		if moved {
			if !cloned {
				out, cloned = maps.Clone(args), true
			}
			out[arg] = resolved
		}
	}
	return out, faults
}

// argPath is where a refusal names t's argument arg: under the tool's name, as
// its schema's faults are named, so the faults of one call read as one list.
func argPath(t Tool, arg string) string {
	if t.Name == "" {
		return arg
	}
	return t.Name + "." + arg
}

// resolveValue is v, the value of t's argument arg at path, with every name in
// it resolved: a string, or a list of strings or of members, each naming its
// seat by `handle`; and whether any of it moved. A list that moved is a copy,
// so the call's own is never changed. Every value it cannot place is added to
// faults, and every other entry of a list is still resolved. held, where set,
// is the schema v is held to: a value it faults is left for it to name.
func (r *Resolver) resolveValue(ctx context.Context, t Tool, arg, path string, held *schema,
	v any, faults *[]nameFault) (any, bool) {

	switch value := v.(type) {
	case string:
		if held != nil && len(held.problems(path, value)) > 0 {
			return v, false
		}
		key, fault := r.resolve(ctx, t, arg, path, value)
		if fault != nil {
			*faults = append(*faults, *fault)
			return v, false
		}
		return key, key != value
	case []any:
		var entries *schema
		if held != nil {
			if held.kind != "array" {
				return v, false
			}
			entries = held.items
		}
		out, moved := value, false
		for i, entry := range value {
			resolved, entryMoved := r.resolveEntry(ctx, t, arg, fmt.Sprintf("%s[%d]", path, i),
				entries, entry, faults)
			if entryMoved {
				if !moved {
					out, moved = slices.Clone(value), true
				}
				out[i] = resolved
			}
		}
		return out, moved
	}
	return v, false
}

// resolveEntry is one entry of a list resolved: a name, or a member naming its
// seat by `handle`, as a room's membership is written ({"handle": ...,
// "follow_all": ...}), every other field of it kept.
func (r *Resolver) resolveEntry(ctx context.Context, t Tool, arg, path string, held *schema,
	entry any, faults *[]nameFault) (any, bool) {

	member, isMember := entry.(map[string]any)
	if !isMember {
		return r.resolveValue(ctx, t, arg, path, held, entry, faults)
	}
	handle, isText := member["handle"].(string)
	if !isText || (held != nil && held.kind != "object") {
		return entry, false
	}
	var heldHandle *schema
	if held != nil {
		heldHandle = held.properties["handle"]
	}
	key, moved := r.resolveValue(ctx, t, arg, path+".handle", heldHandle, handle, faults)
	if !moved {
		return entry, false
	}
	resolved := maps.Clone(member)
	resolved["handle"] = key
	return resolved, true
}

// resolve is one value of t's argument arg as the call should send it: the key
// of the record it names; or raw itself where it is empty, where its directory
// could not be read, or where it is sent as written (a read's key, a member's
// handle). A value that names no record, or several, is its fault at path.
func (r *Resolver) resolve(ctx context.Context, t Tool, arg, path, raw string) (string, *nameFault) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return raw, nil
	}
	dir := t.Names[arg]
	asWritten := t.Kind != KindWrite || dir == DirMember
	if asWritten && keyShaped(dir, value) {
		// A READ OF A KEY AS IT IS SPELLED sends it as written, the route
		// deciding it as it would without the resolver, and reads no
		// directory: a read is one request, and the directory could only
		// have told it the key it already has. A member's handle is sent
		// the same way, for the write's preview to decide against the room.
		return raw, nil
	}
	entries, readable := r.Entries(ctx, dir)
	if !readable {
		return raw, nil
	}
	key, named := match(dir, value, entries)
	switch {
	case len(named) > 1:
		return "", &nameFault{path: path, value: value, dir: dir, named: named}
	case key != "":
		return key, nil
	case asWritten && !strings.ContainsFunc(value, unicode.IsSpace):
		return raw, nil
	}
	return "", &nameFault{path: path, value: value, dir: dir, choices: entries}
}

// nameFault is one value a call names that its directory cannot place: one
// naming no record it lists, refused with the directory's choices, or one
// naming several, refused naming them for the model to pick one by its key.
type nameFault struct {
	path, value string
	dir         Directory
	// named are the records a name several share.
	named []Entry
	// choices are the directory as the person may read it, for a value that
	// names none of it.
	choices []Entry
}

// Error is the fault as its refusal names it.
func (f nameFault) Error() string {
	plural := directoryRead[f.dir].plural
	if len(f.named) > 1 {
		return fmt.Sprintf("%s: %q names %d %s; name one by its key: %s", f.path, f.value,
			len(f.named), plural, shownList(f.named))
	}
	return fmt.Sprintf("%s: %q is not one of the %s you can see", f.path, f.value, plural)
}

// refusedNames is one refusal naming the schema's faults found and then
// every name's in the order met, and after them, once each, the choices of
// every directory a value named nothing in.
func refusedNames(found []error, faults []nameFault) error {
	var notes []string
	for _, f := range faults {
		found = append(found, f)
		if len(f.named) > 1 {
			continue
		}
		if note := choices(directoryRead[f.dir].plural, f.choices); !slices.Contains(notes, note) {
			notes = append(notes, note)
		}
	}
	return refusedFor(found, notes...)
}

// keyShaped reports whether v is spelled as dir's keys are: a seat's handle and
// a unit's key in lower case (`qa`, `core-web`), a project's and a space's in
// upper case (`ENG`, `HOME`). A name a person says ("Company", "QA Engineer")
// is not, so it is read against the directory.
func keyShaped(dir Directory, v string) bool {
	lower := dir == DirSeat || dir == DirMember || dir == DirUnit
	for i, r := range v {
		switch {
		case r >= '0' && r <= '9':
		case lower && r >= 'a' && r <= 'z', !lower && r >= 'A' && r <= 'Z':
		case i > 0 && (r == '-' || r == '_'):
		default:
			return false
		}
	}
	return v != ""
}

// maxChoices is how many records a refusal lists by name: enough for a
// company's seats or spaces, short enough that a refusal stays one read.
const maxChoices = 30

// match is value's key in entries: the key itself, compared without case
// (a space's key is upper case, a handle lower, and a person types either);
// else the one record whose name it is, compared without case, spacing or a
// leading "the" and a trailing noun for the directory ("the Company space").
// The key is "" for a value that names no record at all, which is refused
// with the choices, and for a name several records share, which named holds
// and which is refused naming them.
func match(dir Directory, value string, entries []Entry) (key string, named []Entry) {
	for _, e := range entries {
		if strings.EqualFold(e.Key, value) {
			return e.Key, nil
		}
	}
	want := spoken(dir, value)
	for _, e := range entries {
		if e.Name != "" && spoken(dir, e.Name) == want {
			named = append(named, e)
		}
	}
	if len(named) == 1 {
		return named[0].Key, named
	}
	return "", named
}

// spoken is a name as a person says it, for comparing: lower case, single
// spaces, without a leading "the" or a trailing word naming the directory.
func spoken(dir Directory, name string) string {
	words := strings.Fields(strings.ToLower(name))
	if len(words) > 1 && words[0] == "the" {
		words = words[1:]
	}
	if len(words) > 1 {
		last := words[len(words)-1]
		if slices.Contains(directoryNouns[dir], last) {
			words = words[:len(words)-1]
		}
	}
	return strings.Join(words, " ")
}

// directoryNouns are the trailing words a person adds to a name that say
// which kind of record it is ("the Company space", "the Core team").
var directoryNouns = map[Directory][]string{
	DirSeat:    {"seat", "agent"},
	DirMember:  {"seat", "agent"},
	DirUnit:    {"unit", "team"},
	DirProject: {"project"},
	DirSpace:   {"space"},
}

// choices is the sentence a refusal lists the records in.
func choices(plural string, entries []Entry) string {
	if len(entries) == 0 {
		return "You can see no " + plural + "."
	}
	return "The " + plural + " are: " + shownList(entries) + "."
}

// shownList is entries as a refusal lists them, at most [maxChoices].
func shownList(entries []Entry) string {
	shown := make([]string, 0, min(len(entries), maxChoices))
	for _, e := range entries[:min(len(entries), maxChoices)] {
		shown = append(shown, e.Shown())
	}
	out := strings.Join(shown, ", ")
	if len(entries) > maxChoices {
		out += fmt.Sprintf(", and %d more", len(entries)-maxChoices)
	}
	return out
}

// seatEntries are the org chart's seats, every unit's roles at every depth.
func seatEntries(doc any) []Entry {
	var out []Entry
	walkUnits(doc, func(node any) {
		roles, _ := cards.Lookup(node, "roles").([]any)
		for _, r := range roles {
			if handle := value(r, "handle"); handle != "" {
				out = append(out, Entry{Key: handle, Name: value(r, "name")})
			}
		}
	})
	return out
}

// unitEntries are the org chart's units at every depth.
func unitEntries(doc any) []Entry {
	var out []Entry
	walkUnits(doc, func(node any) {
		if id := value(node, "id"); id != "" {
			out = append(out, Entry{Key: id, Name: value(node, "name")})
		}
	})
	return out
}

// walkUnits visits the chart's root and every unit beneath it.
func walkUnits(node any, visit func(any)) {
	visit(node)
	for _, key := range []string{"units", "children"} {
		units, _ := cards.Lookup(node, key).([]any)
		for _, u := range units {
			walkUnits(u, visit)
		}
	}
}

// listEntries reads a directory that is one list of records.
func listEntries(rows, key, name string) func(any) []Entry {
	return func(doc any) []Entry {
		list, _ := cards.Lookup(doc, rows).([]any)
		out := make([]Entry, 0, len(list))
		for _, row := range list {
			if k := value(row, key); k != "" {
				out = append(out, Entry{Key: k, Name: value(row, name)})
			}
		}
		return out
	}
}
