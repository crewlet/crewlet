package config

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// Problem is one validation failure, located in the document and classified:
// the shape `crewlet validate -json` reports, and the one a configuration
// write reports to a caller that never sees a terminal.
//
// # One shape for every consumer
//
// A CLI fix loop, an API client and the dashboard all ask the same question
// of a refusal (where is it, what kind of mistake, what does it say), so they
// get the same answer. Two shapes had already drifted: the CLI named the kind
// `type` and dropped the path from the message, while the API had no
// structure at all.
type Problem struct {
	// Path is the authored path in the whole validated document
	// (units[0].roles[1].name). Empty only for a failure that belongs to no
	// place in it: a document that did not parse, or a check that failed
	// outside the document, such as building the company's runtime.
	Path string `json:"path"`
	// Segments is Path taken apart: strings for keys, ints for list indexes.
	// A map key can hold a dot, so Path cannot be split reliably, and a
	// consumer finding the field reads these. Null when Path is empty.
	Segments Path `json:"segments"`
	// Kind is what sort of mistake it is: missing, unknown_value,
	// out_of_range, conflict, unknown_field, shape, or invalid for anything
	// this build does not classify. A closed set with a fallback, so a
	// consumer branching on it never reads an empty string as a field
	// somebody forgot to fill in.
	Kind string `json:"kind"`
	// Message is the failure's whole rendered line, exactly as it appears in
	// the refusal's joined text. Duplicates are one line naming every entity
	// and one problem per entity, so problems can outnumber lines.
	Message string `json:"message"`
	// Seat is the engine-derived handle of the seat the problem is about,
	// when it is about one.
	Seat string `json:"seat,omitempty"`
	// Unit is the name of the unit the problem is about, when it is about
	// one.
	Unit string `json:"unit,omitempty"`
	// Line is the 1-based line in the submitted text, for a failure the
	// parser found.
	Line int `json:"line,omitempty"`
}

// Problems flattens a validation error into its located, classified
// problems, in the order they were reported.
//
// A JOIN is split into its parts and nothing else is: an error built with
// several %w verbs renders as one line and is one problem (see
// [joinedParts]). A wrap that only adds context around failures this package
// built, such as the file name in front of a company's problems, is seen
// through, so a file with several problems reports all of them.
//
// An error this package did not build (a file that could not be read, an
// epoch that could not be constructed) is one problem with no path, classified
// by the sentinel it wraps and `invalid` otherwise, so a consumer rendering
// machine-readable output never needs a second branch.
func Problems(err error) []Problem {
	var out []Problem
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if l, ok := e.(*located); ok { //nolint:errorlint // A wrap around one adds text; see leafFault.
			out = append(out, l.problems...)
			return
		}
		if f, ok := leafFault(e); ok {
			out = append(out, f.problem())
			return
		}
		if parts, ok := joinedParts(e); ok {
			for _, part := range parts {
				walk(part)
			}
			return
		}
		if wrapped := errors.Unwrap(e); wrapped != nil && holdsProblems(wrapped) {
			walk(wrapped)
			return
		}
		out = append(out, Problem{Kind: kindName(e), Message: e.Error()})
	}
	walk(err)
	return out
}

// holdsProblems reports whether err carries anything this package located.
func holdsProblems(err error) bool {
	var f *Fault
	var l *located
	return errors.As(err, &f) || errors.As(err, &l)
}

// problem is the fault as a [Problem], with no seat or unit: the fault knows
// its path, and only the whole company can say whose it is.
func (f *Fault) problem() Problem {
	return Problem{
		Path: f.Path.String(), Segments: segmentsOf(f.Path),
		Kind: kindName(f.Kind), Message: f.Error(), Line: f.Line,
	}
}

// segmentsOf is a path's segments for the wire: null rather than an empty
// array when there are none, as the contract states.
func segmentsOf(p Path) Path {
	if len(p) == 0 {
		return nil
	}
	return extend(nil, p...)
}

// kindName is the machine-readable name of the sentinel err wraps, and
// `invalid` for anything this build does not classify.
func kindName(err error) string {
	for _, k := range problemKinds {
		if errors.Is(err, k.sentinel) {
			return k.name
		}
	}
	return "invalid"
}

// problemKinds is every sentinel a problem is classified by, with its name:
// the config layer's own, and the org model's, whose rules are validated
// through [Company.Validate] too. One table, read by [kindName] and by the
// parser's carried faults (see position.go), so the two cannot disagree.
var problemKinds = []struct {
	name     string
	sentinel error
}{
	{"missing", ErrMissing},
	{"out_of_range", ErrOutOfRange},
	{"conflict", ErrConflict},
	{"shape", ErrShape},
	{"unknown_field", ErrUnknownField},
	{"unknown_value", ErrUnknownValue},

	// A seat or unit with no name, and a person nobody can reach, are
	// required values that are absent.
	{"missing", org.ErrMissingName},
	{"missing", org.ErrNoContact},
	// A kind outside agent and human, a handle outside its grammar, and a
	// contact value that is neither a literal id nor one whole reference are
	// values outside the set a field accepts.
	{"unknown_value", org.ErrUnknownKind},
	{"unknown_value", org.ErrInvalidHandle},
	{"unknown_value", org.ErrEmbeddedEnvRef},
	// Every other org rule is two things that cannot both hold: two entities
	// on one identity, a field on the wrong kind of seat, a schedule and a
	// unit that has nobody to run it.
	{"conflict", org.ErrDuplicateHandle},
	{"conflict", org.ErrDuplicateSeatName},
	{"conflict", org.ErrDuplicateUnitName},
	{"conflict", org.ErrHumanSeatField},
	{"conflict", org.ErrAgentSeatField},
	{"conflict", org.ErrUnrunnableSchedule},
	{"conflict", org.ErrMisplacedUnitRef},
	// A schedule that cannot be evaluated is malformed rather than
	// conflicting, and an llm value that is neither a key nor a list is the
	// wrong shape of YAML.
	{"shape", org.ErrInvalidSchedule},
	{"shape", org.ErrProviderKeysShape},
}

// located is an error placed in the document by the company it came from:
// the error itself, the line it renders as, and the problems it is.
//
// A rule the org model reports about ONE seat or unit renders led by the path
// it was placed at ("units[0].name: unit: name must not be empty"), the way
// every rule the config layer reports already does. The org model describes
// the entity in words because it has no document; the path is what a reader
// searches their file for, and what a renderer looks for at the head of a
// line. A duplicate is about several entities and names them all in its own
// words, so it renders unchanged.
//
// A type of its own rather than a [Fault], because one line can be several
// problems: a duplicate names every entity sharing the identity, and a
// problem goes beside each of them.
type located struct {
	err      error
	text     string
	problems []Problem
}

func (l *located) Error() string { return l.text }

// Unwrap exposes the error, so errors.Is and errors.As still answer for the
// sentinel and the typed org error inside.
func (l *located) Unwrap() error { return l.err }

// identityIndex is where each entity of a built organization was AUTHORED,
// keyed by the pointer the organization holds.
//
// Recorded before [org.Organization.Normalize] runs and read after it,
// because normalization moves a root seat into the unit its `unit:` names:
// the pointer survives the move and the authored path is still the one in
// the document. A name cannot stand in for the pointer, since the problems
// worth locating are the ones that make names ambiguous.
type identityIndex struct {
	seats map[*org.Role]Path
	units map[*org.Unit]Path
	// authored is the document's own seat behind each organization seat,
	// for what normalization rewrites (a manages list, once expanded, no
	// longer says at which index an entry was written).
	authored map[*org.Role]*Role
	// byPath is every entity's path, keyed by [pathKey], for placing a
	// problem the config layer found at a path inside one.
	byPath map[string]entityAt
}

// entityAt is one entity an index holds a path for.
type entityAt struct {
	seat *org.Role
	unit *org.Unit
}

func newIdentityIndex() *identityIndex {
	return &identityIndex{
		seats:    map[*org.Role]Path{},
		units:    map[*org.Unit]Path{},
		authored: map[*org.Role]*Role{},
		byPath:   map[string]entityAt{},
	}
}

func (x *identityIndex) addSeat(r *org.Role, authored *Role, path Path) {
	x.seats[r] = path
	x.authored[r] = authored
	x.byPath[pathKey(path)] = entityAt{seat: r}
}

// addUnit records a unit and everything inside it. The organization's unit
// mirrors the document's element for element here, because it has not been
// normalized yet.
func (x *identityIndex) addUnit(u *org.Unit, authored *Unit, path Path) {
	x.units[u] = path
	x.byPath[pathKey(path)] = entityAt{unit: u}
	for j, r := range u.Roles {
		x.addSeat(r, &authored.Roles[j], idx(at(path, "roles"), j))
	}
	for k, child := range u.Children {
		x.addUnit(child, &authored.Children[k], idx(at(path, "children"), k))
	}
}

// pathKey renders a path unambiguously, for a map key: a string segment
// quoted and an index bare, so a map key that happens to read "0" is never
// taken for a list index.
func pathKey(p Path) string {
	var b strings.Builder
	for _, segment := range p {
		b.WriteByte('/')
		switch s := segment.(type) {
		case int:
			b.WriteString(strconv.Itoa(s))
		case string:
			b.WriteString(strconv.Quote(s))
		}
	}
	return b.String()
}

// locate turns every failure in err into a [located] error carrying its
// problems: an org error by the entity it holds, and a config fault by its
// own path, given the seat or unit that path sits inside.
func (x *identityIndex) locate(err error) error {
	var out problems
	for _, leaf := range leafErrors(err) {
		if l := x.place(leaf); l != nil {
			out = append(out, l)
			continue
		}
		out = append(out, leaf)
	}
	return out.err()
}

// place is one leaf as a [located] error, or nil for a leaf this index has
// nothing to say about.
func (x *identityIndex) place(leaf error) *located {
	text, kind := leaf.Error(), kindName(leaf)
	if f, ok := leafFault(leaf); ok {
		p := f.problem()
		p.Seat, p.Unit = x.entityOf(f.Path)
		return &located{err: leaf, text: text, problems: []Problem{p}}
	}
	var seatErr *org.SeatError
	var unitErr *org.UnitError
	var dup *org.DuplicateError
	switch {
	case errors.As(leaf, &dup):
		l := &located{err: leaf, text: text}
		for _, r := range dup.Seats {
			field := "name"
			if dup.Kind == org.DuplicateHandle && r.DeclaredHandle != "" {
				// The handle is written, so that is the line to change; a
				// derived one is changed by renaming the seat.
				field = "handle"
			}
			l.problems = append(l.problems,
				x.problem(extend(x.seats[r], field), kind, text, r.Handle(), ""))
		}
		for _, u := range dup.Units {
			l.problems = append(l.problems,
				x.problem(at(x.units[u], "name"), kind, text, "", u.Name))
		}
		return l
	case errors.As(leaf, &seatErr):
		path := extend(x.seats[seatErr.Seat], seatErr.Field...)
		return x.single(leaf, x.problem(path, kind, led(path, text), seatErr.Seat.Handle(), ""))
	case errors.As(leaf, &unitErr):
		path := extend(x.units[unitErr.Unit], unitErr.Field...)
		return x.single(leaf, x.problem(path, kind, led(path, text), "", unitErr.Unit.Name))
	}
	return nil
}

// single is a leaf that is exactly one problem, rendering as its message.
func (x *identityIndex) single(leaf error, p Problem) *located {
	return &located{err: leaf, text: p.Message, problems: []Problem{p}}
}

func (x *identityIndex) problem(path Path, kind, message, seat, unit string) Problem {
	return Problem{
		Path: path.String(), Segments: segmentsOf(path), Kind: kind, Message: message,
		Seat: seat, Unit: unit,
	}
}

// led is text led by the path it was placed at, as every config fault's line
// already is.
func led(path Path, text string) string {
	if len(path) == 0 {
		return text
	}
	return path.String() + ": " + text
}

// entityOf is the seat or unit a path sits inside, the nearest one: a path
// inside a seat inside a unit is about the seat.
func (x *identityIndex) entityOf(path Path) (seat, unit string) {
	for n := len(path); n > 0; n-- {
		e, ok := x.byPath[pathKey(path[:n])]
		switch {
		case !ok:
			continue
		case e.seat != nil:
			return e.seat.Handle(), ""
		default:
			return "", e.unit.Name
		}
	}
	return "", ""
}

// Warning is something the engine will run but a person should know about,
// located like a [Problem].
//
// Kind says which: "dangling_reference", a name that resolves to nothing,
// with Ref naming what carries it (lead, unit, manages, or
// gitlab_access_level); or "admission", a rule a stored revision breaks that
// a new write would be refused for, with Ref empty. From and To are display
// text: what holds the reference and what it names. Seat and Unit are always
// present, empty when the warning is about neither.
type Warning struct {
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Path     string `json:"path"`
	Segments Path   `json:"segments"`
	Seat     string `json:"seat"`
	Unit     string `json:"unit"`
	From     string `json:"from"`
	To       string `json:"to"`
	Message  string `json:"message"`
}

// WarningDanglingReference is the kind of a [Warning] about a reference that
// resolves to nothing. WarningAdmission is the kind of one about an admission
// rule a stored revision breaks.
const (
	WarningDanglingReference = "dangling_reference"
	WarningAdmission         = "admission"
)

// ReferenceWarnings is every reference this revision resolves to nothing
// ([Company.DanglingRefs]), located in the document: a unit's lead at its
// `lead`, a root seat's unit at its `unit`, a manages entry at the index it
// was written at, and a GitLab access level at its key.
func (c *Company) ReferenceWarnings() []Warning {
	o, x := c.organization()
	refs := append(o.DanglingRefs(), c.danglingAccessLevels(o)...)
	out := make([]Warning, 0, len(refs))
	for _, ref := range refs {
		w := Warning{
			Kind: WarningDanglingReference, Ref: string(ref.Kind),
			From: ref.From, To: ref.To, Message: ref.Message(),
		}
		var path Path
		switch {
		case ref.Unit != nil:
			path = at(x.units[ref.Unit], "lead")
			w.Unit = ref.Unit.Name
		case ref.Seat != nil && ref.Kind == org.RefUnit:
			path = at(x.seats[ref.Seat], "unit")
			w.Seat = ref.Seat.Handle()
		case ref.Seat != nil && ref.Kind == org.RefManages:
			path = at(x.seats[ref.Seat], "manages")
			if i := slices.Index(x.authored[ref.Seat].Manages, ref.To); i >= 0 {
				path = idx(path, i)
			}
			w.Seat = ref.Seat.Handle()
		case ref.Kind == org.RefGitLabAccessLevel:
			path = entry(field(accessLevelsPath), ref.To)
		}
		w.Path, w.Segments = path.String(), segmentsOf(path)
		out = append(out, w)
	}
	return out
}
