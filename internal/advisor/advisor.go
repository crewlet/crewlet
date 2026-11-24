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
// Apply. ADR-0115 records the rule. The package imports no store, coordination,
// state log or API package, so nothing here can apply a fix
// ([TestTheAdvisorImportsNoWritePath]).
//
// # Stated with its numbers, and every class says what it concluded (ADR-0166)
//
// A finding leads with a headline carrying its number, its stats, a chart, the
// seats it is about and one primary action; a run records, per class, found,
// clear, unread, or silent with why ([Outcomes]); the page reads the company's
// numbers from [ComputeSnapshot].
//
// # Kept, re-derived, and re-checked before a fix is sent (ADR-0164)
//
// A background duty runs every detector on its class's cadence and keeps what
// it finds with the evidence, verdict and fix it rests on; each run re-derives
// its class's findings, and Apply runs the detector for the finding's subject
// again before the person sends the fix under the revision that re-check
// read. The model pass on `advisor.model` explains and adds findings only as
// ADR-0157 grounded them, which ADR-0164 supersedes and carries.
//
// # One detection, read where it is decided
//
// A finding about the document is a [config.Warning]: the same check answers
// `crewlet validate`, every `/config` write and every apply log, and this
// package only gives it an owner's words and a fix. A warning with no
// [config.AdvisoryClass] is still listed, in the words `crewlet validate`
// prints, with the setting it is about.
//
// # An agent's access request is the same kind of fix
//
// An agent asks for more access by naming one key of the closed catalogue in
// access.go, and the change is built here for that agent alone; a person
// previews and sends it before the question is answered (ADR-0118). The
// catalogue is derived from what a seat can hold (every level of every GitHub
// permission the seat catalogue offers), never listed by hand, so it cannot
// fall behind the Agents tab.
//
// # Work that stalls where nothing else says so
//
// stalls.go reads six more classes off the week's records ([ClassRoundCap] and
// the rest). [Detect] turns the week's records into findings, each a fixed
// reading with its threshold beside it and no model call
// ([TestNoDetectorCallsAModel]). Every class says one thing for every subject,
// so it reads as one row ([TestEveryClassSaysOneThingAboutEverySubject]).
//
// # What the atlas shows needs a person
//
// atlas.go reads five more classes off the company's atlas as its asker reads
// it: an owner that left, a service open to the internet nothing watches, a
// source that stopped reading, a service nobody owns, and a disagreement a
// week old. A finding names its entries only to a holder of the atlas app,
// and its fix is an atlas tool call the reader sends (ADR-0141).
package advisor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Class names what a finding is about. Each has one entry in [catalogue].
type Class string

// The classes a finding about the company document can have: one per
// [config.AdvisoryClass], and one per kind of warning that carries none.
const (
	ClassHealthUnheld           = Class(config.ClassHealthUnheld)
	ClassAuxiliaryModel         = Class(config.ClassAuxiliaryModel)
	ClassScheduleFrequent       = Class(config.ClassScheduleFrequent)
	ClassDirectBox              = Class(config.ClassDirectBox)
	ClassConfigAdvisory   Class = "config.advisory"
	// ClassIntegrationRecordCut is no finding: the silence a reading says
	// where a surface's findings record dropped findings ([Answer.Partial]).
	ClassIntegrationRecordCut Class = "integration.record_cut"
	ClassConfigReference      Class = "config.reference"
	ClassConfigAdmission      Class = "config.admission"
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
	// SubjectRepository is a repository, named as its code host names it.
	SubjectRepository SubjectKind = "repository"
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

// Fact is one number a finding's evidence states. A rule's facts are said by
// their key ([FactLabel]); a model finding's carry the words the model labelled
// them with, since its keys are its own and a key read as a label ("blocks 1").
type Fact struct {
	Key   string `json:"key"`
	Value int64  `json:"value"`
	Label string `json:"label,omitempty"`
	// Unit is what the number counts, where the finding says it rather than
	// its key's words ([FactUnit]); an investigation's numbers say theirs.
	Unit Unit `json:"unit,omitempty"`
	// Was is the same number over the range before, where it was read.
	Was *int64 `json:"was,omitempty"`
}

// Counts is what the fact's number counts: its own unit, else its key's.
func (f Fact) Counts() Unit {
	if f.Unit != "" {
		return f.Unit
	}
	return FactUnit(f.Key)
}

// Said is how the fact is labelled: its own words where it carries them, else
// its key's ([FactLabel]).
func (f Fact) Said() string {
	if f.Label != "" {
		return f.Label
	}
	return FactLabel(f.Key)
}

// RecordKind is what a record names, so a reader knows how to open it.
type RecordKind string

// The records a finding names.
const (
	// RecordItem is a work item, by its key.
	RecordItem RecordKind = "item"
	// RecordTurn is one turn of an agent, by its id.
	RecordTurn RecordKind = "turn"
	// RecordChange is a pull request's address or a Gitlet change's reference.
	RecordChange RecordKind = "change"
	// RecordComment is a comment, by its id.
	RecordComment RecordKind = "comment"
)

// Record is one record behind a finding's evidence.
type Record struct {
	Kind RecordKind `json:"kind"`
	ID   string     `json:"id"`
}

// recordsOf is ids as records of one kind, at most [worklet.MaxPatternExamples].
func recordsOf(kind RecordKind, ids []string) []Record {
	out := []Record{}
	for _, id := range ids {
		if len(out) == worklet.MaxPatternExamples {
			break
		}
		if id != "" {
			out = append(out, Record{Kind: kind, ID: id})
		}
	}
	return out
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
	// Title says what is wrong, in one line (at most 70 characters where this
	// package writes it). Title and Why are the same on every finding of one
	// class, so a class reads as one row: what it is about is Subject, Item,
	// Names, Evidence and Records.
	Title string `json:"title"`
	// Headline is one line that carries the finding's number ("Seats sat
	// idle 11 h on 19 Nov: the model hit its limit 4 times"), where the
	// class or the investigation wrote one; the title stands in for it
	// otherwise (ADR-0166).
	Headline string `json:"headline,omitempty"`
	// Premise is the fact the finding rests on, asked again every hour; a
	// premise that no longer holds resolves the finding (ADR-0166).
	Premise *Premise `json:"premise,omitempty"`
	// Seats are the seats a finding about one cause is about, each with its
	// share of the number, where the class states them (ADR-0166).
	Seats []SeatShare `json:"seats,omitempty"`
	// Why says what it costs, in the owner's words. Where this package writes
	// it, its first sentence, the one a row shows, is at most 90 characters,
	// and detail follows ([TestEveryFindingLeadsWithOneShortLine]).
	Why string `json:"why"`
	// Item is the one thing within the subject the finding is about, as a
	// person reads it in a list: a schedule's name and how often it runs.
	Item string `json:"item,omitempty"`
	// Focus is what within the subject a class that raises several findings
	// on one subject is about, stable across reads: a document path, an
	// item's key, an integration finding's own subject. Part of [Key].
	Focus string `json:"focus,omitempty"`
	// Names is every seat the finding is about by display name, in the order
	// of their handles, where one finding holds several.
	Names []string `json:"names,omitempty"`
	// Path is where in the company document the finding is located.
	Path     string `json:"path"`
	Evidence []Fact `json:"evidence"`
	// Records names at most five records behind the evidence.
	Records []Record `json:"records,omitempty"`
	// Entries names at most [MaxAtlasEntries] atlas entries the finding is
	// about, only for a reader who holds the atlas app.
	Entries []AtlasEntry `json:"entries,omitempty"`
	Fix     Fix          `json:"fix"`
	// WordsCut reports a reconcile finding its surface's record kept without
	// its words: its title, why and fix are no reading of the condition, so
	// a stored finding keeps its own ([Reconcile]). Never stored.
	WordsCut bool `json:"-"`
}

// Answer is every finding about the company at one revision, the most costly
// first. Revision is the one the findings were read at. Unread names what this
// answer could not read, so a list missing it never reads as all clear.
type Answer struct {
	Revision string    `json:"revision"`
	Findings []Finding `json:"findings"`
	Unread   []string  `json:"unread"`
	// Silent is each class that read its sources and still could not judge
	// all of the company, with why (ADR-0166).
	Silent []Silence `json:"silent,omitempty"`
	// Partial is each surface whose findings this reading holds only in
	// part ([Reading.Cuts]).
	Partial []Cut `json:"-"`
	// Shared are the findings of classes the Advisor no longer lists that
	// the usage insights still share by their published class ([Shared]);
	// never listed, never kept.
	Shared []Finding `json:"-"`
}

// SeatShare is one seat a finding is about, and its share of the number.
type SeatShare struct {
	Seat  string  `json:"seat"`
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  Unit    `json:"unit"`
}

// Silence is what a class could not judge, and why, in words.
type Silence struct {
	Class  Class  `json:"class"`
	Reason string `json:"reason"`
}

// [Answer.Unread]'s words: integrations whose last reconcile, the tracker's
// work patterns, the turn ledger, the alarms (this node's evaluation or the
// fleet's marks), the seats' pauses, the executor runs' records, the schedules'
// fires, the held conversations, the budget refusals and the merge
// escalations that could not be read.
const (
	UnreadIntegrations = "integrations"
	UnreadWork         = "work"
	UnreadLedger       = "ledger"
	UnreadAlarms       = "alarms"
	UnreadPauses       = "pauses"
	UnreadRuns         = "runs"
	UnreadFires        = "fires"
	UnreadHolds        = "holds"
	UnreadStops        = "budget_stops"
	UnreadMerges       = "merges"
	UnreadEpisodes     = "alarm_episodes"
	UnreadFlow         = "flow"
	UnreadSkips        = "schedule_skips"
	UnreadStaffing     = "staffing"
	UnreadCapacity     = "capacity"
)

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
	answer := readEach(in)
	answer.Findings = groupCauses(in.Company, answer.Findings)
	stateHeadlines(in.Company, answer.Findings)
	sortFindings(answer.Findings)
	return answer
}

// readEach is [Read] before its causes are grouped: one finding per warning,
// which the evidence's schedule judgement reads schedule by schedule.
func readEach(in Inputs) Answer {
	findings := []Finding{}
	for _, w := range in.Company.Warnings() {
		findings = append(findings, fromWarning(in.Company, w))
	}
	unread := []string{}
	if in.Integrations == nil || slices.ContainsFunc(in.Integrations, func(s integration.State) bool {
		return s.FindingsUnread
	}) {
		unread = append(unread, UnreadIntegrations)
	}
	findings = append(findings, FromIntegrations(in.Integrations)...)
	sortFindings(findings)
	answer := Answer{Revision: in.Revision, Findings: findings, Unread: unread}
	cutIntegrations(&answer, in.Integrations)
	return answer
}

// Cut is one surface whose findings record dropped findings: the path its
// findings name, and whether the drop may have taken findings a person owes
// (Owed) and findings nobody does (Unowed), from the record's counts.
type Cut struct {
	Path   string
	Owed   bool
	Unowed bool
}

// Holds reports whether c may have dropped f, so a reading that does not
// observe it says nothing of it: a reconcile finding on c's path, owed or not
// as c may have dropped. A finding of another source on the path (an inbound
// silence, a stale atlas source) is not in the record at all.
func (c Cut) Holds(f Finding) bool {
	if f.Source != SourceReconcile || f.Path != c.Path {
		return false
	}
	if f.Actor == ActorOwner {
		return c.Owed
	}
	return c.Unowed
}

// cutIntegrations marks on answer each surface whose findings record dropped
// findings ([Cut]): what it carries opens and refreshes, what it may have
// dropped stays open, and the rest of it, like every other surface, concludes.
// A person is told which, and how many, as [ClassIntegrationRecordCut]'s
// silence.
func cutIntegrations(answer *Answer, states []integration.State) {
	var said []string
	for _, s := range states {
		rec := s.FindingsRecord
		if s.Disconnecting || rec.Omitted == 0 {
			continue
		}
		answer.Partial = append(answer.Partial, Cut{Path: IntegrationPath(s.Kind), Owed: rec.OwedOmitted > 0,
			Unowed: rec.Omitted > rec.OwedOmitted})
		noun := "findings"
		if rec.Omitted == 1 {
			noun = "finding"
		}
		said = append(said, fmt.Sprintf("%s's last pass found %d more %s than its record keeps",
			SurfaceName(s.Kind), rec.Omitted, noun))
	}
	if len(said) == 0 {
		return
	}
	held := ", so its open findings the record may have left out stay open until a pass keeps them."
	if len(said) > 1 {
		held = ", so the open findings their records may have left out stay open until a pass keeps them."
	}
	answer.Silent = append(answer.Silent, Silence{Class: ClassIntegrationRecordCut,
		Reason: strings.Join(said, "; ") + held})
}

// sortFindings orders findings the most costly first, then by area and class.
func sortFindings(findings []Finding) {
	slices.SortStableFunc(findings, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.Severity.rank(), b.Severity.rank()),
			cmp.Compare(a.Area, b.Area),
			cmp.Compare(a.Class, b.Class),
			cmp.Compare(a.Path, b.Path),
			cmp.Compare(a.Subject.Seat, b.Subject.Seat),
		)
	})
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
	f.Item = w.Item
	f.Focus = staticFocus(w)
	f.Fix = entry.fix(c, place, w)
	if class == ClassHealthUnheld {
		f.Evidence = append(f.Evidence, Fact{Key: "agents", Value: int64(len(agentSeats(c)))})
	}
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
