// Package advisor turns what the engine knows about a company's setup into
// findings an administrator can act on: what is wrong, why it matters, and the
// change that fixes it, which the administrator previews and sends themselves.
//
// # The engine never applies a fix
//
// A grant, a model, a schedule or a charter policy is the company's decision,
// so nothing here writes the company document. A finding carries its fix as
// data: for a configuration change, one entity write per option, which the
// dashboard sends to the entity route's dry run (ADR-0055) to draw the
// preview from the server's own answer, and then sends again, from the
// administrator's session and under the entity's own tag, only when they press
// Apply. ADR-0115 records the rule.
//
// # One detection, read where it is decided
//
// A finding about the document is a [config.Warning]: the same check answers
// `crewlet validate`, every `/config` write and every apply log, and this
// package only gives it an owner's words and a fix. A warning with no
// [config.AdvisoryClass] is still listed, in the words `crewlet validate`
// prints, with the setting it is about.
package advisor

import (
	"cmp"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// Class names what a finding is about. Each has one entry in [catalogue].
type Class string

// The classes a finding about the company document can have: one per
// [config.AdvisoryClass], and one per kind of warning that carries none.
const (
	ClassHealthUnheld           = Class(config.ClassHealthUnheld)
	ClassAuxiliaryModel         = Class(config.ClassAuxiliaryModel)
	ClassScheduleFrequent       = Class(config.ClassScheduleFrequent)
	ClassConfigAdvisory   Class = "config.advisory"
	ClassConfigReference  Class = "config.reference"
	ClassConfigAdmission  Class = "config.admission"
)

// Severity is what ignoring a finding costs.
type Severity string

const (
	// SeverityBlocks is a company that cannot work as configured.
	SeverityBlocks Severity = "blocks"
	// SeverityCosts is a company that works and pays for the setting in
	// spend, waiting or work nobody sees.
	SeverityCosts Severity = "costs"
	// SeverityNote is worth knowing, and may be exactly what was meant.
	SeverityNote Severity = "note"
)

// rank orders severities, the most costly first.
func (s Severity) rank() int {
	return slices.Index([]Severity{SeverityBlocks, SeverityCosts, SeverityNote}, s)
}

// Actor is who acts on a finding: the owner, through a fix, or the engine,
// whose own work it is and which never reaches the owner as a to-do.
type Actor string

// The two actors.
const (
	ActorOwner  Actor = "owner"
	ActorEngine Actor = "engine"
)

// Source is where a finding was read: the company document alone, or what
// the company has done.
type Source string

// The two sources.
const (
	SourceStatic   Source = "static"
	SourceEvidence Source = "evidence"
	// SourceReconcile is an integration's last reconcile report.
	SourceReconcile Source = "reconcile"
)

// Area groups findings the way an owner thinks of their setup.
type Area string

// The areas a finding is grouped under.
const (
	AreaAccess       Area = "access"
	AreaIntegrations Area = "integrations"
	AreaLearning     Area = "learning"
	AreaSchedules    Area = "schedules"
	AreaSetup        Area = "setup"
)

// SubjectKind is what a finding is about.
type SubjectKind string

// What a finding can be about.
const (
	SubjectCompany     SubjectKind = "company"
	SubjectIntegration SubjectKind = "integration"
	SubjectSeat        SubjectKind = "seat"
	SubjectSchedule    SubjectKind = "schedule"
)

// Subject is the one thing a finding is about.
type Subject struct {
	Kind SubjectKind `json:"kind"`
	// Seat is the seat's handle, for a seat or a seat's schedule.
	Seat string `json:"seat,omitempty"`
	// Unit is the unit's id, for a unit's schedule.
	Unit string `json:"unit,omitempty"`
	// Schedule is the schedule's name.
	Schedule string `json:"schedule,omitempty"`
	// Name is what a person calls it: the seat's or the schedule's name.
	Name string `json:"name,omitempty"`
}

// Fact is one number a finding's evidence states.
type Fact struct {
	Key   string `json:"key"`
	Value int64  `json:"value"`
}

// Finding is one thing about a company's setup an administrator should know,
// with the change that fixes it.
type Finding struct {
	Class    Class    `json:"class"`
	Severity Severity `json:"severity"`
	Actor    Actor    `json:"actor"`
	Source   Source   `json:"source"`
	Area     Area     `json:"area"`
	Subject  Subject  `json:"subject"`
	// Title says what is wrong, in one line.
	Title string `json:"title"`
	// Why says what it costs, in the owner's words.
	Why string `json:"why"`
	// Path is where in the company document the finding is located.
	Path     string `json:"path"`
	Evidence []Fact `json:"evidence"`
	Fix      Fix    `json:"fix"`
}

// Answer is every finding about the company at one revision, the most costly
// first. Revision is the one the findings were read at. Unread names what this
// answer could not read, so a list missing it never reads as all clear.
type Answer struct {
	Revision string    `json:"revision"`
	Findings []Finding `json:"findings"`
	Unread   []string  `json:"unread"`
}

// UnreadIntegrations is [Answer.Unread]'s word for integrations whose last
// reconcile this node could not read.
const UnreadIntegrations = "integrations"

// Inputs is what one answer is read from.
type Inputs struct {
	Company  *config.Company
	Revision string
	// Integrations is each surface's last reconcile, nil where this node
	// could not read them (an empty list is a real answer: none reported).
	Integrations []integration.State
}

// Read is every finding the company document and its integrations raise.
func Read(in Inputs) Answer {
	findings := []Finding{}
	for _, w := range in.Company.Warnings() {
		findings = append(findings, fromWarning(in.Company, w))
	}
	unread := []string{}
	if in.Integrations == nil {
		unread = append(unread, UnreadIntegrations)
	}
	findings = append(findings, FromIntegrations(in.Integrations)...)
	slices.SortStableFunc(findings, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.Severity.rank(), b.Severity.rank()),
			cmp.Compare(a.Area, b.Area),
			cmp.Compare(a.Class, b.Class),
			cmp.Compare(a.Path, b.Path),
		)
	})
	return Answer{Revision: in.Revision, Findings: findings, Unread: unread}
}

// fromWarning is w as a finding, in its class's words and with its fix.
func fromWarning(c *config.Company, w config.Warning) Finding {
	class := classOf(w)
	entry := catalogue[class]
	f := Finding{
		Class: class, Severity: entry.severity, Actor: ActorOwner, Source: SourceStatic,
		Area: entry.area, Path: w.Path, Evidence: []Fact{},
	}
	place := locate(c, w.Segments)
	f.Subject = place.subject()
	if entry.company {
		f.Subject = Subject{Kind: SubjectCompany}
	}
	f.Title, f.Why = entry.words(c, place, w)
	f.Fix = entry.fix(c, place, w)
	return f
}

// classOf is the class a warning is listed under.
func classOf(w config.Warning) Class {
	if w.Class.Valid() {
		return Class(w.Class)
	}
	switch w.Kind {
	case config.WarningDanglingReference:
		return ClassConfigReference
	case config.WarningAdmission:
		return ClassConfigAdmission
	default:
		return ClassConfigAdvisory
	}
}
