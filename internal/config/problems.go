package config

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/logging"
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
	// Its own entry, although a duplicate NAME already carries both
	// sentinels: where an id is what collided the error carries this one
	// alone, and with no entry that collision arrives as `invalid`.
	{"conflict", org.ErrDuplicateUnit},
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
// # What Kind says
//
// "dangling_reference" is a name that resolves to nothing, with Ref naming
// what carries it (lead, unit, manages, or gitlab_access_level). "admission"
// is a rule a stored revision breaks that a new write would be refused for.
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
// rule a stored revision breaks. WarningAdvisory is the kind of one about a
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

// Warnings is everything a person should know about a company the engine
// will run: every reference it resolves to nothing
// ([Company.ReferenceWarnings]), every admission rule it breaks
// ([Company.AdmissionWarnings]), then every setting that is valid and carries
// a consequence ([Company.AdvisoryWarnings]).
//
// A document that passed [Company.Validate] carries no admission warning, so
// on a write that was admitted this is its references and its advisories. The
// admission half is what a revision re-activated under the runnable rules
// still says: a reload or a revert of a company stored before a rule existed.
func (c *Company) Warnings() []Warning {
	out := append(c.ReferenceWarnings(), c.AdmissionWarnings()...)
	return append(out, c.AdvisoryWarnings()...)
}

// AdmissionWarnings is every admission rule c breaks, as warnings located
// exactly like the problems a write keeping them is refused with.
//
// Built from those problems rather than beside them, so the two cannot place
// one violation differently: one warning per entity it is about (a duplicated
// name is a warning beside each seat or unit holding it), each carrying that
// problem's path, segments, seat, unit and message. Ref, From and To are
// empty, because an admission rule names no reference.
func (c *Company) AdmissionWarnings() []Warning {
	problems := Problems(c.ValidateAdmission())
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

// AdvisoryWarnings is everything valid about this company that its author
// should still know.
//
// Located at the field they would ADD rather than at the entity in prose: a
// unit with no id is reported at units[i].id, which is the line an editor
// jumps to and the node a dashboard marks, and the unit is named in Unit for
// a reader who has no document in front of them.
func (c *Company) AdvisoryWarnings() []Warning {
	o, x := c.organization()
	var out []Warning

	// A UNIT WITH NO ID IS KEYED ON ITS NAME, and a name is prose: it gets
	// renamed for the reasons prose does. Two different things follow, and
	// the warning names both because fixing one does not fix the other.
	//
	// THE MESSAGE NAMES THE UNIT, as every unit-scoped line in this engine
	// does ([org.DanglingRef.Message] opens `unit "Platform" names lead`).
	// The path is an index now rather than the name it used to be, and a
	// prose reader gets the path and the message and nothing else: without
	// the name in the sentence they would have to count units in their own
	// file to find out which one this is about.
	for u := range o.AllUnits() {
		if strings.TrimSpace(u.ID) != "" {
			continue
		}
		w := advisory(at(x.units[u], "id"), fmt.Sprintf(
			"unit %q has no `id`, so everything durable is keyed on its NAME, "+
				"and renaming it moves what is filed under it. Giving it an id "+
				"fixes that, and does NOT stop a rename re-onboarding the seats "+
				"beneath it: onboarding turns on the name, because the name is "+
				"what an agent reads as its team", u.Name))
		w.Unit = u.Name
		out = append(out, w)
	}
	return out
}

// Warnings is everything valid about this bootstrap that its author should
// still know.
//
// SEPARATE FROM Validate, and it takes no error path: a document that does not
// validate has problems worth fixing first, and a warning printed beside a
// refusal is noise at exactly the moment somebody is reading carefully.
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
	// trim until somebody tells it, and the log grows until its ceiling
	// refuses writes. That is the configuration working as asked; it is
	// also a state nobody discovers until the refusal.
	if b.Stream.TrackerRetention.Floor() == BackupFloorOperator {
		out = append(out, advisory(field("stream.tracker_retention.backup_floor"),
			"`operator` means the trim advances only as far as somebody "+
				"has acknowledged a backup. Until the first acknowledgement the log "+
				"is never trimmed, and it grows until `stream.tracker_log_max_bytes` "+
				"starts refusing writes"))
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

	// AN EMBEDDED STREAM WITH NOWHERE TO PERSIST loses everything on a
	// restart. It is the right configuration for a test and for an
	// ingress-only node, and the wrong one for anything holding a company.
	if b.Stream.Type != StreamNATS && strings.TrimSpace(b.Stream.StoreDir) == "" {
		out = append(out, advisory(field("stream.store_dir"),
			"an embedded stream with no store directory keeps everything in "+
				"memory: a restart loses every mailbox, every coordination record and "+
				"the company's own history. Correct for a test; not for a node that "+
				"holds seats"))
	}

	// A BROKER TOLD TO BE VERBOSE INTO A SINK THAT TAKES NO DEBUG says
	// nothing at all. `stream.debug` unlocks nats-server's own Debugf
	// population, but those are still DEBUG records and every destination
	// filters by level, so the pair that reads as "I asked for broker
	// diagnostics" is the pair that produces none. `crewlet run`'s own
	// -debug / -log-level flags override the file and are invisible from
	// it, which is why this names them rather than being a refusal.
	if b.Stream.Debug && !b.logsAtDebug() {
		out = append(out, advisory(field("stream.debug"),
			"the embedded broker will produce its debug output and no "+
				"destination records it: `logging.level` is "+b.loggingLevelName()+
				" and no `logging.file.level` is `debug`. Set one of them, or pass "+
				"`-debug` to `crewlet run`"))
	}
	return out
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

// loggingLevelName is the level this document names, spelled as an operator
// wrote it, and as the DEFAULT when they wrote nothing, because "logging.level
// is " followed by an empty string reads as a bug in the warning.
func (b *Bootstrap) loggingLevelName() string {
	if b.Logging.Level == "" {
		return "`info` (unset)"
	}
	return "`" + string(b.Logging.Level) + "`"
}

// CheckTiers holds the rules that need BOTH documents, and it exists because
// neither tier can see the other.
//
// Tier A is the operator's and Tier B is the founder's; each validates alone,
// and a rule about the pair has nowhere else to live. There is exactly one
// today, and it is worth the seam: it turns a permanent, unrecoverable state
// into a refusal at the moment somebody could still choose otherwise.
func CheckTiers(boot *Bootstrap, company *Company) error {
	var p problems
	if boot == nil || company == nil {
		return nil
	}

	// A NATIVE TRACKER ON AN IN-MEMORY STREAM IS UNRECOVERABLE, and that is
	// why it is an error rather than the warning Tier A raises alone.
	//
	// The engine's own tracker keeps its write-ahead log on the stream. An
	// embedded server with no store directory keeps its streams in MEMORY,
	// so a restart recreates them empty, and a node whose durable tables
	// are ahead of a stream that has restarted from nothing cannot tell
	// "the log was trimmed" from "the log is a different log", refuses to
	// serve, and stays refused: every snapshot it could adopt is above the
	// recreated stream too.
	//
	// It is an error rather than a warning because there is no correct
	// deployment it describes. A company on a vendor tracker starts no log
	// at all and is unaffected, which is why the rule needs both documents.
	if company.TrackerBackendFor() == TrackerNative &&
		boot.Stream.Type != StreamNATS &&
		strings.TrimSpace(boot.Stream.StoreDir) == "" {
		p.add(field("stream.store_dir"), ErrMissing,
			"this company runs the engine's own tracker, whose log lives on the "+
				"stream, and an embedded stream with no store directory keeps its "+
				"streams in memory, so a restart recreates them empty and this node "+
				"refuses to serve the tracker permanently. Name a directory")
	}
	return p.err()
}
