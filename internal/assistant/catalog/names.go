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
	// DirUnit is the org chart's units, by key and name.
	DirUnit Directory = "unit"
	// DirProject is Worklet's projects, by key and name.
	DirProject Directory = "project"
	// DirSpace is Doclet's spaces, by key and name.
	DirSpace Directory = "space"
)

// Directories are the four, in declaration order.
var Directories = []Directory{DirSeat, DirUnit, DirProject, DirSpace}

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
// A VALUE THAT NAMES NOTHING THE DIRECTORY LISTS is refused, listing the
// choices, for a write, which changes a record that must be there now: a card
// the route would refuse once approved is never drawn. A read sends a value
// that could be a key as it is, because a read may name what the directory no
// longer lists (a seat removed from the chart still has its turns), and the
// route decides it; only a value no key is spelled like (it has a space in it)
// is refused with the choices, since it can only have been meant as a name.
func (r *Resolver) Resolve(ctx context.Context, t Tool, args map[string]any) (map[string]any, error) {
	if len(t.Names) == 0 || len(args) == 0 {
		return args, nil
	}
	out, cloned := args, false
	for _, arg := range slices.Sorted(maps.Keys(t.Names)) {
		raw, isText := args[arg].(string)
		value := strings.TrimSpace(raw)
		if !isText || value == "" {
			continue
		}
		dir := t.Names[arg]
		if t.Kind != KindWrite && keyShaped(dir, value) {
			// A READ OF A KEY AS IT IS SPELLED sends it as written, the
			// route deciding it as it would without the resolver, and reads
			// no directory: a read is one request, and the directory could
			// only have told it the key it already has.
			continue
		}
		entries, readable := r.Entries(ctx, dir)
		if !readable {
			continue
		}
		key, found, err := match(dir, arg, value, entries)
		if !found && t.Kind != KindWrite && !strings.ContainsFunc(value, unicode.IsSpace) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if key != raw {
			if !cloned {
				out, cloned = maps.Clone(args), true
			}
			out[arg] = key
		}
	}
	return out, nil
}

// keyShaped reports whether v is spelled as dir's keys are: a seat's handle and
// a unit's key in lower case (`qa`, `core-web`), a project's and a space's in
// upper case (`ENG`, `HOME`). A name a person says ("Company", "QA Engineer")
// is not, so it is read against the directory.
func keyShaped(dir Directory, v string) bool {
	lower := dir == DirSeat || dir == DirUnit
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
// found is false for a value that names no record at all, which is refused
// with the choices; a name two records share is found and refused naming
// them.
func match(dir Directory, arg, value string, entries []Entry) (key string, found bool, err error) {
	for _, e := range entries {
		if strings.EqualFold(e.Key, value) {
			return e.Key, true, nil
		}
	}
	want := spoken(dir, value)
	var named []Entry
	for _, e := range entries {
		if e.Name != "" && spoken(dir, e.Name) == want {
			named = append(named, e)
		}
	}
	plural := directoryRead[dir].plural
	switch len(named) {
	case 1:
		return named[0].Key, true, nil
	case 0:
		return "", false, fmt.Errorf("%w: %s: %q is not one of the %s you can see. %s",
			ErrInvalidArguments, arg, value, plural, choices(plural, entries))
	}
	return "", true, fmt.Errorf("%w: %s: %q names %d %s; name one by its key: %s",
		ErrInvalidArguments, arg, value, len(named), plural, shownList(named))
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
