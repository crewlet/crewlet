package config

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/words"
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

	// A seat or unit with no name is a required value that is absent.
	{"missing", org.ErrMissingName},
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
	// Two seats on one external account, and two units on one key — the
	// other two "entities on one identity" this group's comment opens with.
	{"conflict", org.ErrDuplicateIdentity},
	{"conflict", org.ErrDuplicateUnit},
	{"conflict", org.ErrHumanSeatField},
	// A seat's token ceiling below one token is a number outside what the
	// key accepts, and a budget keyed by something no calendar window is
	// a value outside the set — the company's own block reports the first
	// as [ErrOutOfRange] through the same org rule.
	{"out_of_range", org.ErrTokenCeiling},
	{"unknown_value", org.ErrUnknownWindow},
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
			// A HANDLE IS PLACED AT THE HANDLE, which is its identity in
			// every document this reads — one is written in from the name
			// where none was declared ([MintIdentities]) — and the field the
			// message tells an operator to change, as a unit is placed at
			// its id below.
			field := "name"
			if dup.Kind == org.DuplicateHandle {
				field = "handle"
			}
			l.problems = append(l.problems,
				x.problem(extend(x.seats[r], field), kind, text, r.Handle(), ""))
		}
		// A UNIT IS PLACED AT ITS ID, which is its key in every document
		// this reads — one is minted from the name where none was written —
		// and the field the message tells an operator to change.
		for _, u := range dup.Units {
			l.problems = append(l.problems,
				x.problem(at(x.units[u], "id"), kind, text, "", u.Name))
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
// # Why warnings are their own channel and not errors
//
// A refusal says "this cannot run". Every one of these can, and some of them
// describe the correct configuration for somebody's deployment: a development
// topology, a company that has decided not to back up, an organization being
// assembled in pieces, a rename that will re-onboard a team on purpose.
// Turning any of them into an error would make the engine refuse a decision
// that was not its to make.
//
// # And why they are not just log lines
//
// A log line is read after the thing has already happened. These are read by
// `crewlet validate`, which is what a person runs BEFORE applying a config and
// what a CI step runs before a deploy, and by a configuration write, which
// answers them to a caller that never sees a terminal. Each is a moment where
// the consequence can still change the decision.
//
// They are ALSO log lines, because many deployments have neither moment: a
// document an image, a chart or an operator renders reaches a node nobody
// validated by hand, Tier A is never written through the API, and a company
// seeded from a file is answered to nobody. So `crewlet run` logs every Tier A
// advisory once per start, and every node logs a company's advisories once
// per epoch it applies — both as `config_advisory`, beside the lines the apply
// already writes for the other two kinds.
//
// # What Kind says
//
// "dangling_reference" is a name that resolves to nothing, with Ref naming
// what carries it (lead, unit, manages, or gitlab_access_level). "admission"
// is a rule an applied revision breaks that a new write would be refused for.
// "advisory" is a setting that is valid and carries a consequence, and it is
// the one kind either tier can raise. Ref is empty on the last two, because
// neither names a reference. From and To are display text: what holds the
// reference and what it names. Seat and Unit are always present, empty when
// the warning is about neither.
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
// rule an applied revision breaks. WarningAdvisory is the kind of one about a
// setting that is valid and carries a consequence its author should hear
// before production tells them.
const (
	WarningDanglingReference = "dangling_reference"
	WarningAdmission         = "admission"
	WarningAdvisory          = "advisory"
)

// advisory is one valid-but-worth-knowing setting, located at the field an
// authoring loop can jump to.
//
// The message says what will happen and what to do instead, in that order. A
// warning that only says something is unusual is one people learn to ignore.
func advisory(path Path, message string) Warning {
	return Warning{
		Kind: WarningAdvisory, Path: path.String(), Segments: segmentsOf(path),
		Message: message,
	}
}

// AdvisoryAt is [advisory] at a dotted field path, for an advisory whose
// judgement needs a package this one cannot import: the engine's, about how
// much broker a document's state logs reserve.
func AdvisoryAt(fields, message string) Warning {
	return advisory(field(fields), message)
}

// Warnings is everything a person should know about a company the engine
// will run: every reference it resolves to nothing
// ([Company.ReferenceWarnings]), every admission rule it breaks
// ([Company.AdmissionWarnings]), then every setting that is valid and carries
// a consequence ([Company.AdvisoryWarnings]).
//
// A document that passed [Company.Validate] carries no admission warning, so
// on a write that was admitted this is its references and its advisories. The
// admission half is what a revision applied under the runnable rules still
// says: one a newer peer admitted under rules this build does not share.
func (c *Company) Warnings() []Warning {
	out := append(c.ReferenceWarnings(), c.AdmissionWarnings()...)
	return append(out, c.AdvisoryWarnings()...)
}

// AdmissionWarnings is every admission rule c breaks, as warnings located
// exactly like the problems a write keeping them is refused with, and every
// charter rule it breaks ([Company.CharterWarnings]), which a write is refused
// for only where it changes the part at fault.
//
// Built from those problems rather than beside them, so the two cannot place
// one violation differently: one warning per entity it is about (a duplicated
// name is a warning beside each seat or unit holding it), each carrying that
// problem's path, segments, seat, unit and message. Ref, From and To are
// empty, because an admission rule names no reference.
func (c *Company) AdmissionWarnings() []Warning {
	return append(warningsOf(c.ValidateAdmission()), c.CharterWarnings()...)
}

// warningsOf is the problems err reports, as admission warnings.
func warningsOf(err error) []Warning {
	problems := Problems(err)
	out := make([]Warning, 0, len(problems))
	for _, p := range problems {
		out = append(out, Warning{
			Kind: WarningAdmission, Path: p.Path, Segments: p.Segments,
			Seat: p.Seat, Unit: p.Unit, Message: p.Message,
		})
	}
	return out
}

// ReferenceWarnings is every reference this revision resolves to nothing
// ([Company.DanglingRefs]), located in the document: a unit's lead at its
// `lead`, a root seat's unit at its `unit`, a manages entry at the index it
// was written at, and a GitLab access level at its key.
func (c *Company) ReferenceWarnings() []Warning {
	o, x := c.organization()
	refs := append(o.DanglingRefs(), c.DanglingSettingsRefs(o)...)
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
			path = entry(field(AccessLevelsSetting), ref.To)
		}
		w.Path, w.Segments = path.String(), segmentsOf(path)
		out = append(out, w)
	}
	return out
}

// AdvisoryWarnings is everything valid about this company that its author
// should still know: answered by `crewlet validate` and a `/config` write, and
// logged by every node that applies the company.
//
// A UNIT WITH NO ID USED TO BE ONE OF THESE, and it is now an ADMISSION rule
// instead: a unit is referenced by its key, so a document leaving a team
// keyed on prose is refused rather than advised. The warning survives where
// a warning is what an admission violation becomes — on a STORED revision
// ([Company.AdmissionWarnings]) — and it is located at the same
// `units[i].id`, so nothing about what an operator sees moved except the
// class. Reporting both would have put one mistake in front of them twice.
//
// A BARE `-e NAME` IN providers.sandbox.local.run_args THAT NOTHING SUPPLIES
// is one: the runtime's CLI copies NAME from its own environment, which is a
// fixed rule plus runtime_env and never the engine's, so the box gets nothing
// for it — valid, since an operator may mean exactly that, and silent until
// whatever read the variable inside the box finds it unset.
//
// A COMPANY WITH NO HUMAN SEAT is one ([Company.peoplelessWarnings]): nobody
// can be invited into it or created in it, which is the right shape for a
// company its agents run alone and a surprise for one somebody meant to sign
// in to.
//
// So is a token budget a seat's ceilings leave idle ([Company.budgetWarnings]),
// and what the unit channels say about each other ([Company.channelAdvisories]):
// one room two units claim on a vendor chat surface, and a Chatlet
// company that gives no team a room.
//
// EMPTY, NEVER NIL, for the reason [Company.ReferenceWarnings] gives.
func (c *Company) AdvisoryWarnings() []Warning {
	out := []Warning{}
	if local := c.Providers.Sandbox.localBlock(); local != nil {
		args := field("providers.sandbox.local.run_args")
		for _, unsupplied := range local.unsuppliedEnvArgs() {
			out = append(out, advisory(idx(args, unsupplied.at), fmt.Sprintf(
				"`-e %[1]s` copies %[1]s from the container runtime's own environment, "+
					"which is its fixed allowlist plus providers.sandbox.local.runtime_env "+
					"and never the engine's — and neither carries %[1]s, so the box gets "+
					"nothing for it. Put what a box needs in role.sandbox.env or a setup "+
					"step's env, which reach the box directly; or declare "+
					"`%[1]s: \"${%[1]s}\"` under runtime_env to have the runtime pass the "+
					"host's value on", unsupplied.name)))
		}
	}
	out = append(out, c.peoplelessWarnings()...)
	out = append(out, c.unreachableWarnings()...)
	out = append(out, c.budgetWarnings()...)
	return append(out, c.channelAdvisories()...)
}

// channelAdvisories is what this company's unit channels say about each
// other: a room two units both claim on a vendor surface, and a Chatlet
// company that has asked for no rooms at all.
//
// Read from DECLARED channels rather than effective ones, because a child
// unit inheriting its parent's channel is the feature rather than a
// collision: [org.Organization.Normalize] cascades one down a whole branch,
// so reading the effective value would warn about every team in a division
// that named a channel once.
func (c *Company) channelAdvisories() []Warning {
	o, x := c.organization()
	var declared []*org.Unit
	holders := map[string][]*org.Unit{}
	var first *org.Unit
	for u := range o.AllUnits() {
		if first == nil {
			first = u
		}
		if u.DeclaredChannel == "" {
			continue
		}
		declared = append(declared, u)
		holders[u.DeclaredChannel] = append(holders[u.DeclaredChannel], u)
	}

	var out []Warning

	// TWO UNITS IN ONE ROOM, warned about BESIDE EACH OF THEM, the same
	// placement a duplicated name gets: fixing it means renaming one of the
	// two, and an author who sees the warning on only one of them cannot
	// tell which.
	//
	// Not a refusal ON A VENDOR SURFACE, because there it is a shape a real
	// company writes on purpose: two teams that genuinely stand up together,
	// or a unit split in the chart before the room it will move to exists.
	// What it costs is worth saying once — every unit-scoped wake reaches
	// both teams, and each team's prompt tells it that room is its own.
	//
	// ON CHATLET IT IS AN ADMISSION RULE INSTEAD
	// ([Company.validateUnitRooms]), because there the engine builds the
	// room and a room belongs to one unit — and an admission problem is
	// already reported as a warning on an applied revision, so saying it
	// here too would print one finding twice.
	shared := declared
	if c.ChatBackendFor() == ChatChatlet {
		shared = nil
	}
	for _, u := range shared {
		// KEYED ON THE UNIT rather than on its name, because two units
		// sharing a name is itself a legal document to warn about (the
		// admission rule that refuses one is not run here), and comparing
		// names would have each of that pair hide the other.
		others := make([]string, 0, len(holders[u.DeclaredChannel])-1)
		for _, other := range holders[u.DeclaredChannel] {
			if other != u {
				others = append(others, other.Name)
			}
		}
		if len(others) == 0 {
			continue
		}
		w := advisory(at(x.units[u], "channel"), fmt.Sprintf(
			"unit %q shares its channel %q with %s. Everything scoped to one "+
				"of them lands in the other's room, and every seat in both is "+
				"told that channel is its team's own. Give one of them a "+
				"channel of its own, or leave the child's empty and let it "+
				"inherit", u.Name, u.DeclaredChannel, words.And(others)))
		w.Unit = u.Name
		out = append(out, w)
	}

	// A CHATLET COMPANY WITH NO UNIT ROOMS AT ALL. The engine opens a
	// channel for each one a unit declares, and that is the only kind of
	// room whose members the org chart keeps — every other room is one a
	// person opened from the dashboard and fills by hand. With none declared,
	// no team has a room of its own until somebody makes one and adds its
	// seats to it, and keeps adding them as the chart changes.
	//
	// IT DOES NOT SAY NOTHING ELSE OPENS A ROOM. It once did, and a person
	// can open one; an advisory that overstates is one an author learns to
	// skip.
	//
	// Only where there is a unit to write it on — a company with no units is
	// a flat one whose seats have no teams to give rooms to, and a warning
	// pointing at no line in the document is one nobody can act on. Located
	// at the FIRST unit's `channel`, which is the line an author adds it to;
	// the sentence says it is about all of them, because the path cannot.
	if c.ChatBackendFor() == ChatChatlet && first != nil && len(declared) == 0 {
		w := advisory(at(x.units[first], "channel"), fmt.Sprintf(
			"no unit in this company declares a `channel`. In Chatlet, "+
				"each channel a unit names gets a room whose members follow the "+
				"org chart. Without one, a team only has a room if someone opens "+
				"one by hand and adds its seats. Give the units that work in the "+
				"open a channel, starting with %q", first.Name))
		w.Unit = first.Name
		out = append(out, w)
	}
	return out
}

// peoplelessWarnings is the one warning a company declaring no human seat
// carries: nobody can be invited into it or created in it.
//
// EVERY PERSON HOLDS A HUMAN SEAT for as long as they are here
// (internal/iamdomain, ADR-0032) — an invitation and an administrator's create
// each name the seat the person will hold, and both are refused without one —
// so a company with none is a company nobody can join. Said HERE, before an
// operator meets the refusal at their first invitation, because `crewlet
// validate` and the builder's check are the moments the document can still
// change.
//
// A WARNING AND NOT A RULE, because the state is legitimate: a company its
// agents run alone, administered through the deployment's Tier A tokens, which
// act as themselves and need no seat, has nobody to invite. Refusing it would
// refuse that company. AT `roles`, which is where the remedy is written.
func (c *Company) peoplelessWarnings() []Warning {
	for role := range c.EachRole() {
		if role.Kind == org.KindHuman {
			return nil
		}
	}
	return []Warning{advisory(field("roles"), "this company has no "+
		"`kind: human` seats, so nobody can be invited. That's fine if agents "+
		"run it alone. Otherwise add a human seat for each person")}
}

// unreachableWarnings is every human seat with no account on the vendor chat
// surfaces this company talks on: nothing addressed to it there reaches
// anybody.
//
// ONLY ON A VENDOR SURFACE (`chat.backend: vendor`), because that is the only
// chat the sentence is true of. On the engine's own chat a person reads the
// rooms on the dashboard as their seat, and a mention of the seat reaches
// them with no identity to declare; with `chat.backend: none` there is no
// chat surface for it to be about at all. Said everywhere, it told every
// founder of a Chatlet company that the people they had just added could
// not be reached, which is false, and an advisory that is false on the
// default configuration is one an author learns to skip.
//
// JUDGED AGAINST THE SURFACES THIS COMPANY DECLARES ([chatVendors]), not
// against the contact block as a whole: a GitHub login or an Atlassian
// account id is how a code host or a tracker attributes the person, and says
// nothing about whether a Mattermost mention reaches them. A seat reachable
// on ANY of the declared surfaces is reachable — a company running both
// Mattermost and Slack has people who are in one workspace and not the
// other.
//
// A WARNING AND NOT A RULE, because the state is legitimate: a person who
// works only through the dashboard has no chat account to declare, and
// refusing the seat refused them. So this is the one signal an operator gets
// that nobody can be @-mentioned there — agents hand such a person work by
// assigning it in the tracker instead.
//
// AT THE SEAT'S `contact`, which is where the remedy is written.
func (c *Company) unreachableWarnings() []Warning {
	if c.ChatBackendFor() != ChatVendor {
		return nil
	}
	var surfaces []chatVendor
	var titles, keys []words.Said
	for _, vendor := range chatVendors {
		if vendor.declaredAt(c) != "" {
			surfaces = append(surfaces, vendor)
			titles = append(titles, words.Said(vendor.title))
			keys = append(keys, words.Said("`"+vendor.contactKey+"`"))
		}
	}
	var out []Warning
	for role, path := range c.EachRole() {
		if role.Kind != org.KindHuman || reachableOn(role.Contact, surfaces) {
			continue
		}
		handle := role.IdentityKey()
		w := advisory(at(path, "contact"), fmt.Sprintf("%s is a human seat with "+
			"no account on %s, so chat messages to it there reach nobody. Add a "+
			"%s to its `contact`, or leave it if they only use the dashboard",
			handle, words.Or(titles), words.Or(keys)))
		w.Seat = handle
		out = append(out, w)
	}
	return out
}

// reachableOn reports whether a human seat's contact names an account on any
// of surfaces. A ${VAR} reference counts, as [org.HumanContact.IsEmpty]
// counts one: it is an identity the operator has committed to.
func reachableOn(contact *org.HumanContact, surfaces []chatVendor) bool {
	for _, identity := range contact.Identities() {
		for _, surface := range surfaces {
			if identity.Transport == surface.transport {
				return true
			}
		}
	}
	return false
}

// Warnings is everything valid about this bootstrap that its author should
// still know: printed by `crewlet validate`, and logged by `crewlet run` on
// every start, judged on the document that run applies — the file with every
// run flag folded into it.
//
// SEPARATE FROM Validate, and it takes no error path: a document that does not
// validate has problems worth fixing first, and a warning printed beside a
// refusal is noise at exactly the moment somebody is reading carefully. Both
// callers ask it only of a document that validated — `crewlet run` of the one
// its flags left, which it holds to every Tier A rule again first, naming the
// flags in a refusal.
func (b *Bootstrap) Warnings() []Warning {
	var out []Warning

	// A DECLINED FSYNC IS A DECISION WITH A NUMBER ON IT. It is legitimate
	// (a replicated fleet across power domains genuinely trades a window
	// for throughput), and the window is what an operator should have said
	// out loud rather than inherited.
	if !b.Stream.SyncAlways() {
		out = append(out, advisory(field("stream.sync"), fmt.Sprintf(
			"an acknowledged write may be up to %s behind the disk. That is a "+
				"trade against a correlated power loss taking every replica at "+
				"once. Write `always` if this deployment cannot afford it",
			b.Stream.SyncInterval())))
	}

	// A FLEET THAT TRIMS ONLY WHAT IT HAS BEEN TOLD IS OFF-SITE does not
	// trim until somebody tells it, and each log grows until its ceiling
	// refuses writes. That is the configuration working as asked; it is
	// also a state nobody discovers until the refusal.
	//
	// AND IT IS TOLD PER LOG. An acknowledgement names one stream
	// (`crewlet retention ack -stream`), and the backup term blocks any log
	// the counted point does not cover, so an operator who acknowledged the
	// mutation log and read "until the first acknowledgement" as the whole
	// gate would expect the knowledge base's log to trim too, and it does
	// not.
	//
	// EVERY CEILING THE TRIM IS ALL THAT HOLDS BACK, rather than the
	// mutation log's alone: the backup term is evaluated per log, and the
	// ordered logs — the tracker's, the knowledge base's, the artifacts'
	// and their datastore's, the identity estate's, the Dashboard
	// Assistant's and chat's — carry no age bound, so nothing else ever
	// removes a record from them. Naming one field sent an operator to the
	// wrong ceiling whenever another log filled first: a full identity log
	// refuses every sign-in and every change to who may do what, a full
	// assistant log every question somebody asks the assistant, since it
	// cannot record the turn, and a full chat log a message somebody typed.
	//
	// NO COUNT IN THE SENTENCE: the fields are listed by name, and a test
	// reads them off [Stream] and holds the message to every one, so a log
	// added with its ceiling cannot be left out of it — while a number
	// beside the list is one more thing a new log has to remember to move.
	//
	// AND EVERY LOG, the two compacted ones included. They age their
	// records out on the broker whatever the trim decides, so they never
	// refuse a write — but the backup term and the `backup_age` alarm are
	// evaluated on every running log alike, so a log nobody acknowledged
	// raises that alarm for as long as nobody does. An advisory that named
	// the ordered logs alone sent an operator to acknowledge those and leave
	// two alarms firing for ever.
	//
	// ONLY WHERE THE FLOOR IS READ, which is a node holding the estate. Two
	// readers decide which backups count under it, and both are armed with
	// the state log, so only on a `data` node: the trim itself, a fleet
	// singleton that only a data node with `workers` may claim, and the
	// `backup_age` alarm, evaluated on EVERY data node whether or not it
	// ever holds the trim — so a [data, seats] node still reads the floor
	// for the alarm it raises. That is why the gate is
	// [placement.NodeProfile.HoldsData] and not
	// [placement.NodeProfile.RunsWorkers]: tightening it would drop the
	// warning on a node whose alarm still depends on the value. Every other
	// reader of `stream.tracker_retention` — the snapshot cadence, the
	// retention report, the API's backup surface — is a data node's too. On
	// a stateless node the block is read by nothing, so a warning about what
	// it does to the trim described the members' trim from a value they
	// never see. (`retention.backup_owner`, below, is not gated: who owns
	// the deployment's backups is a question every node's operator answers.)
	if b.Profile("").HoldsData() &&
		b.Stream.TrackerRetention.Floor() == BackupFloorOperator {
		out = append(out, advisory(field("stream.tracker_retention.backup_floor"),
			"`operator` means every state log is trimmed only as far as a "+
				"backup of that log has been acknowledged, once per log "+
				"(`crewlet retention ack -stream <log> -position <n>`; "+
				"`crewlet retention status` names every one). Until its own "+
				"acknowledgement a log is never trimmed. The ordered logs grow "+
				"until their own ceiling starts refusing writes: "+
				"`stream.tracker_log_max_bytes`, `stream.pages_log_max_bytes`, "+
				"`stream.artifacts_log_max_bytes`, "+
				"`stream.artifact_data_log_max_bytes`, "+
				"`stream.iam_log_max_bytes`, where a full log refuses every "+
				"sign-in, `stream.assistant_log_max_bytes`, where it refuses "+
				"every question somebody asks the Dashboard Assistant, and "+
				"`stream.chat_log_max_bytes`, where it refuses a message "+
				"somebody typed. The two compacted logs "+
				"(`stream.tracker_vectors_max_bytes`, "+
				"`stream.usage_log_max_bytes`) age their records out on the "+
				"broker regardless, but wait on an acknowledgement just the "+
				"same: each log's `backup_age` alarm fires until it has one, "+
				"and again once it is older than `backup_max_age`"))
	}

	// NOBODY OWNS THE BACKUP. A company that never backs up never trims,
	// since the log is the only copy of what no node has applied yet, so
	// "who is responsible for this" has a real answer on every deployment
	// that intends to keep working, and nowhere to write it is how it goes
	// unasked.
	if strings.TrimSpace(b.Retention.BackupOwner) == "" {
		out = append(out, advisory(field("retention.backup_owner"),
			"nobody is named as this deployment's backup owner. The trim "+
				"stops when the newest backup ages past "+
				"`stream.tracker_retention.backup_max_age`, and the alarm that says "+
				"so has nobody to name"))
	}

	// NOT HERE: an embedded stream with no `store_dir`. It was two
	// advisories — everything lost on a restart, and a `store_max_bytes`
	// that bounds nothing on a broker holding its streams in memory — and
	// it is a REFUSAL now ([Stream.Durable]) on every broker MEMBER, solo
	// included, because every member keeps its state logs on the stream from
	// boot, so no deployment is left standing for either to describe. A
	// LEAF runs no JetStream and is refused a store directory outright
	// ([StreamLeaf.validate]), and a CLIENT's streams are its external
	// cluster's, so neither has a store for either advisory to be about.

	// A PEER ENTRY THAT IS NOT ANOTHER MEMBER is not counted as one by any
	// rule that counts members ([StreamCluster.members]) — and that is a
	// decision the operator should see, because it is the difference between
	// the count they wrote and the count the quorum and replica rules used.
	// An alias of this host this file cannot recognise is the case the
	// warning cannot catch, so it says so.
	if b.Stream.Type != StreamNATS {
		for _, d := range b.Stream.Cluster.members().Discounted {
			where := idx(field("stream.cluster.peers"), d.Index)
			if d.Self {
				out = append(out, advisory(where, fmt.Sprintf(
					"%q is this node's own route (it matches %s), so it is not "+
						"counted as a member: stream.cluster.peers names the OTHER "+
						"members, and the quorum and replica rules count this node "+
						"once. Remove it. Another name for this host that this file "+
						"cannot recognise — a DNS alias, `localhost` — would be "+
						"counted as a member it is not", d.Raw, d.Matches)))
				continue
			}
			out = append(out, advisory(where, fmt.Sprintf(
				"%q repeats peers[%d], and one member listed twice is counted "+
					"once. Remove it", d.Raw, d.RepeatOf)))
		}
	}

	// TOO MANY VOTERS. No stream keeps more than [MaxStreamReplicas] copies,
	// so a member past the fifth holds no copy anybody can ask for and adds
	// only a voter to the metadata group — one more member every election
	// and every stream or consumer created waits on. The fleet's broker is a
	// fixed few members; a fleet that grows past them adds leaves.
	//
	// A WARNING RATHER THAN A REFUSAL: the fleet works, it only pays for
	// consensus nothing is bought with. And a node that holds data is always
	// a member, so a fleet of more than five data nodes has no other shape —
	// the warning says so rather than recommending one this build refuses.
	if kind := b.BrokerKind(); kind == placement.BrokerMember {
		if others := len(b.Stream.Cluster.members().Others); others > MaxStreamReplicas-1 {
			out = append(out, advisory(field("stream.cluster.peers"), fmt.Sprintf(
				"names %d other members, so this fleet's broker has %d, and no stream "+
					"keeps more than %d copies: every member past the %dth is a voter "+
					"in the metadata group — one more member every election and every "+
					"stream or consumer created waits on — holding no copy anybody "+
					"asked for. Beyond %d a fleet adds LEAVES, not members (a node that "+
					"holds data is always a member)",
				others, others+1, MaxStreamReplicas, MaxStreamReplicas,
				MaxStreamReplicas)))
		}
	}

	// UNDECLARED ROLES IN A FLEET. Omitting node.roles is every role, which
	// is the right default for the one process a company starts as and
	// rarely what each node of a fleet should do: it keeps a copy of the
	// company's state on this disk (`data`) and claims agents here (`seats`)
	// whether or not the operator meant this node to. A fleet is a leaf, or
	// a member with peers — a solo member is the single-process deployment
	// the default exists for.
	if b.Node.Roles == nil {
		if b.declaresFleet() {
			out = append(out, advisory(field("node.roles"), fmt.Sprintf(
				"is not declared, so this fleet node runs every role — %s — "+
					"including %q, which keeps a copy of the company's state on "+
					"this disk, and %q, which runs agents here. Name the roles "+
					"this node is for", strings.Join(nodeRoleNames, ", "),
				placement.RoleData, placement.RoleSeats)))
		}
	}

	// THE INGRESS ROLE WITH NO LISTENER. `api.port` is the one listener a
	// node binds, and its roles decide what it carries: the API, the
	// dashboard, the webhooks and the probes with `ingress`, the probes
	// alone without it. A node told to run ingress is the one integrations,
	// a browser and an orchestrator all look for, and a port of 0 binds
	// nothing for any of them.
	//
	// DECLARED ROLES ONLY. An omitted node.roles is every role, which is also
	// the single-process shape before anybody has chosen a port, and a fleet
	// node that left them out is warned about that already.
	//
	// A WARNING RATHER THAN A REFUSAL, because `crewlet run -api-port` sets
	// the port at run time and no file can show it: a file that leaves the
	// port to the flag is valid, and only this file's half of the pair is
	// visible here.
	if b.Node.Roles != nil && b.API.Port == 0 && b.Profile("").RunsIngress() {
		out = append(out, advisory(field("api.port"), fmt.Sprintf(
			"is 0, so this node binds no listener although node.roles names %q: "+
				"no webhook reaches it, it serves no dashboard and no REST API, "+
				"and no orchestrator can probe it. Set a port, or drop %q",
			placement.RoleIngress, placement.RoleIngress)))
	}

	out = append(out, b.API.Auth.writerWarnings(field("api.auth"))...)

	// A BROKER TOLD TO BE VERBOSE INTO A SINK THAT TAKES NO DEBUG says
	// nothing at all. `stream.debug` unlocks nats-server's own Debugf
	// population, but those are still DEBUG records and every destination
	// filters by level, so the pair that reads as "I asked for broker
	// diagnostics" is the pair that produces none. `crewlet run`'s own
	// -debug / -log-level flags override the file and are invisible from
	// it, which is why this names them rather than being a refusal. The run
	// folds them into the document before it asks, and the message offers
	// only what would change the answer for the document it was asked
	// about ([Bootstrap.unrecordedDebug]), so a node already at `debug` is
	// told why that is not enough rather than to pass `-debug`.
	if b.Stream.Debug && !b.logsAtDebug() {
		out = append(out, advisory(field("stream.debug"),
			"the embedded broker will produce its debug output and no "+
				"destination records it: "+b.unrecordedDebug()))
	}

	out = append(out, b.API.warnings(b.ServesAPI())...)
	out = append(out, b.API.Artifacts.warnings(b.API, b.ServesAPI())...)
	return out
}

// declaresFleet reports whether this document says its node is one of several:
// a leaf, or a member with peers. A solo member is the single-process
// deployment, and a client of an external cluster says nothing either way —
// the cluster's other clients are in no file this one can see.
func (b *Bootstrap) declaresFleet() bool {
	kind := b.BrokerKind()
	return kind == placement.BrokerLeaf ||
		(kind == placement.BrokerMember && len(b.Stream.Cluster.members().Others) > 0)
}

// logsAtDebug reports whether ANY destination this document INSTALLS would
// record a debug line.
//
// The most verbose destination decides, the same rule internal/logging's own
// fan-out follows: a `debug` file behind a `warn` console is a supported
// shape, and reading only `logging.level` would call that combination silent
// when it is not.
//
// INSTALLS, not "configures", and that is the half that is easy to get wrong
// in the other direction too: with `logging.stderr: false` the console is not
// a destination at all, so `logging.level: debug` beside a `warn` file
// records nothing, except where there is no file, because [logging.install]
// keeps stderr rather than leave a process logging nowhere. This mirrors that
// function; the two disagreeing would make the warning fire on a working
// deployment or stay quiet on a broken one.
func (b *Bootstrap) logsAtDebug() bool {
	file := strings.TrimSpace(b.Logging.File.Path) != ""
	if console := b.Logging.Stderr == nil || *b.Logging.Stderr || !file; console {
		if b.Logging.Level == logging.LevelDebug {
			return true
		}
	}
	if !file {
		return false
	}
	// An empty file level follows `logging.level`, which is what makes
	// `-debug` reach both destinations.
	level := b.Logging.File.Level
	if level == "" {
		level = b.Logging.Level
	}
	return level == logging.LevelDebug
}

// unrecordedDebug says why no destination this document installs records a
// debug line, and what would — one sentence for each shape of
// [Bootstrap.logsAtDebug]'s false answer, read off the destinations the same
// way, because the two disagreeing would name a cause the node does not have.
//
// # Only the remedies that work
//
// The obvious remedy, raising the level, is wrong in exactly the shape that
// is easiest to reach: `logging.stderr: false` makes the log file the only
// destination, and a file given a level of its own does not follow
// `logging.level` — so telling that node to pass `-debug`, or to set
// `logging.level`, names a change that leaves it exactly as silent, and tells
// a node already started with `-debug` to do what it did. Each case names the
// cause it found and the changes that would make a destination record the
// broker, and no others.
//
// `logging.level` is named with the flags that override it, because the
// level this reads is the one the document holds once `crewlet run` has
// folded `-log-level` and `-debug` into it — not necessarily the file's.
func (b *Bootstrap) unrecordedDebug() string {
	level := "`logging.level` — which `-log-level` and `-debug` override on " +
		"`crewlet run` — is " + b.loggingLevelName()
	raise := "Pass `-debug` to `crewlet run`, or set `logging.level`"
	switch {
	case strings.TrimSpace(b.Logging.File.Path) == "":
		// The console is the only destination: `logging.stderr: false`
		// with no file is refused by name, so it is on.
		return level + ", and no log file is configured. " + raise + " to `debug`"
	case b.Logging.File.Level == "":
		// The file follows the level, so raising it reaches the file
		// whether or not the console is a destination.
		where := "the log file follows it"
		if !b.Logging.StderrEnabled() {
			where = "`logging.stderr` is false, so the log file, which follows it, " +
				"is the only destination"
		}
		return level + ", and " + where + ". " + raise + " or `logging.file.level` to `debug`"
	case b.Logging.StderrEnabled():
		return level + ", and `logging.file.level` is `" + string(b.Logging.File.Level) +
			"`. " + raise + " or `logging.file.level` to `debug`"
	default:
		// The file keeps a level of its own and the console is not a
		// destination, so the process level reaches nothing: neither
		// `-debug` nor `logging.level` is a remedy here.
		out := "`logging.stderr` is false, so the log file is the only destination, " +
			"and `logging.file.level` is `" + string(b.Logging.File.Level) +
			"`. Set that to `debug`"
		if b.Logging.Level == logging.LevelDebug {
			out += ", or set `logging.stderr` back to true: `logging.level` is `debug` already"
		}
		return out
	}
}

// loggingLevelName is the level this document names, spelled as an operator
// wrote it, and as the DEFAULT when they wrote nothing, because "logging.level
// is " followed by an empty string reads as a bug in the warning.
func (b *Bootstrap) loggingLevelName() string {
	if b.Logging.Level == "" {
		return "`info` (unset)"
	}
	return "`" + string(b.Logging.Level) + "`"
}

// writerWarnings are the company_writers settings that are valid and almost
// certainly not what their author meant.
//
// WARNINGS RATHER THAN REFUSALS, because each is a posture somebody can
// choose: a list naming no configured token is a document nobody may change
// through the API — frozen, until Tier A changes — and a token may be listed
// here before it is issued, on a node whose file is rolled out ahead of the
// credential. What makes them worth saying is that the commonest way to reach
// either is a typo, and the symptom is a 403 on the one system that was
// supposed to be able to write.
func (a *APIAuth) writerWarnings(path Path) []Warning {
	if !a.CompanyManaged() {
		return nil
	}
	var out []Warning
	writers := at(path, "company_writers")
	configured := make(map[string]struct{}, len(a.Tokens))
	for _, t := range a.Tokens {
		configured[t.ID] = struct{}{}
	}
	named := 0
	for i, id := range a.CompanyWriters {
		if _, ok := configured[id]; ok {
			named++
			continue
		}
		out = append(out, advisory(idx(writers, i), fmt.Sprintf(
			"%q isn't in `api.auth.tokens`, so it can't write anything. Add "+
				"the token, or fix the id", id)))
	}
	if named == 0 {
		out = append(out, advisory(writers,
			"none of these writers is in `api.auth.tokens`, so nobody can "+
				"change the company config through the API"))
	}
	return out
}
