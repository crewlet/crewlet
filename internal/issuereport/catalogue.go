package issuereport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/store"
)

// maxMigrationNumber bounds a migration's number: its four digits.
const maxMigrationNumber = 9999

// Class is a catalogue id: which defect a report describes.
type Class string

// The defects the engine detects. An exploitable one is never here: it goes
// through SECURITY.md, privately, and into no public issue.
const (
	ClassContextExhaustedLoop Class = "executor.context_exhausted_loop"
	ClassFailureLoop          Class = "turn.failure_loop"
	ClassDeadLettered         Class = "queue.dead_lettered"
	ClassEnginePanicked       Class = "engine.panicked"
	ClassProcessCrashed       Class = "process.crashed"
	ClassMigrationFailed      Class = "store.migration_failed"
	ClassRunStranded          Class = "executor.run_stranded"
)

// Valid reports whether the class is one this build names.
func (c Class) Valid() bool {
	_, ok := entryOf(c)
	return ok
}

// FactKey names one fact a class declares.
type FactKey string

// The facts a report may carry.
const (
	FactContextWindowTokens FactKey = "context_window_tokens"
	FactSessionSizeBucket   FactKey = "session_size_bucket"
	FactSessionContinued    FactKey = "session_continued"
	FactMessageLost         FactKey = "message_lost"
	FactDeferCause          FactKey = "defer_cause"
	FactReason              FactKey = "reason"
	FactScope               FactKey = "scope"
	FactPanicKind           FactKey = "panic_kind"
	FactFatalKind           FactKey = "fatal_kind"
	FactEstate              FactKey = "estate"
	FactFromMigration       FactKey = "from_migration"
	FactSQLErrorClass       FactKey = "sql_error_class"
)

// IdentityHarnessName is the one identity path that is no fact: the name in a
// report's harness block.
const IdentityHarnessName FactKey = "harness.name"

// FactType is the JSON type of a fact's value.
type FactType string

// The types a fact may have.
const (
	FactInteger FactType = "integer"
	FactBoolean FactType = "boolean"
	FactEnum    FactType = "enum"
)

// Fact declares one fact of a class: its key, its type and what it may hold.
type Fact struct {
	Key  FactKey  `json:"key"`
	Type FactType `json:"type"`
	// Enum is an enum fact's closed set.
	Enum []string `json:"enum,omitempty"`
	// Max bounds an integer fact, which is never negative.
	Max int64 `json:"max,omitempty"`
	// Optional is a fact a wired signal may have nothing to say for, on a path
	// the class names. Every other declared fact is required: see [Entry.checked].
	Optional bool `json:"optional,omitempty"`
	// example is what a preview shows for it.
	example FactValue
}

// SiteKind is what a varying site names: a function, unless it says otherwise.
type SiteKind string

// SiteMigration is a site that is a schema file under internal/store/schema/,
// which the relay checks exists at the report's commit.
const SiteMigration SiteKind = "migration"

// Threshold is when repeated failures are a defect rather than noise: Count
// of them in a row within WindowHours. One names a failure that is a defect
// the first time.
type Threshold struct {
	Count       int `json:"count"`
	WindowHours int `json:"window_hours"`
}

// Window is the threshold's window as a duration.
func (t Threshold) Window() time.Duration { return time.Duration(t.WindowHours) * time.Hour }

// Entry is one class: everything a report of it says beyond its typed fields.
type Entry struct {
	ID Class `json:"id"`
	// Title is the issue's fixed title, and Summary its fixed paragraph.
	Title   string `json:"title"`
	Summary string `json:"summary"`
	// Component is the engine package that detects it. Where SiteVaries, a
	// report names the package of the site its signal found: see [Entry.placeOf].
	Component Component `json:"component"`
	// Site is what a report names, and the relay holds it to exactly this. A STABLE
	// TOKEN: renaming the function it stands for leaves it alone, so an issue is
	// not orphaned, and `liveSites` in the suite maps it to today's function. Where
	// SiteVaries, a report names the function its signal found, and this is what it
	// falls back to.
	Site       Site `json:"site"`
	SiteVaries bool `json:"site_varies,omitempty"`
	// SiteKind is what a varying site names, omitted for a function. A schema
	// file is its own declaration, so it carries no `site_file`, and its
	// component stays the class's.
	SiteKind SiteKind `json:"site_kind,omitempty"`
	// Frames says a report may carry the stack's module frames: only a class
	// whose site varies, since a frame is the place its site was read from.
	Frames bool   `json:"frames,omitempty"`
	Facts  []Fact `json:"facts"`
	// Identity is what feeds the fingerprint beyond the class, component and site,
	// sorted: required boolean or closed-enum facts, or [IdentityHarnessName], so
	// one class can be several defects. Never a number, a version or a pattern: a
	// value that varies per engine would split one defect into many issues.
	Identity  []FactKey `json:"identity,omitempty"`
	Threshold Threshold `json:"threshold"`
	// Security is always false: see [Class].
	Security bool `json:"security"`
	// Harness says a report of it carries the coding CLI that ran, and no
	// other class's may.
	Harness bool `json:"harness"`
}

// The buckets a kept session's size falls in, and what is said for no size.
var sessionSizeBuckets = []string{
	"unknown", "0-64KiB", "64KiB-256KiB", "256KiB-512KiB", "512KiB-1MiB", "1MiB-4MiB", "4MiB-16MiB", "16MiB+",
}

// kindError is the turn loop's one kind: what nothing classified.
const kindError = types.ErrorKindUnclassified

// maxWindowFact bounds the integer facts.
const maxWindowFact = 1 << 24

// deferCauses is the closed set a hand-back's cause takes ([queue.DeferCauses]).
func deferCauses() []string { return enumStrings(queue.DeferCauses) }

// catalogue is the one table. Each threshold is a judgment recorded here: see
// `docs/guides/engine-issue-reports.md` for what each class reports.
var catalogue = []Entry{{
	ID:    ClassContextExhaustedLoop,
	Title: "Executor runs loop on an exhausted context",
	Summary: "A conversation's executor runs failed on context exhaustion several times in a row. " +
		"The engine retires a session that runs out, so each launch should run on another session: with " +
		"`session_continued` false, none of them fit its model's context, and the brief or the window is the " +
		"likelier cause; with it true, a session that ran out was continued anyway.",
	Component: "internal/sandbox",
	Site:      "internal/sandbox.collectedRecord",
	Facts: []Fact{
		// OPTIONAL: only a Claude Code record carries the window its run was pinned to.
		{Key: FactContextWindowTokens, Type: FactInteger, Max: maxWindowFact, Optional: true, example: intFact(1_000_000)},
		{Key: FactSessionSizeBucket, Type: FactEnum, Enum: sessionSizeBuckets, example: enumFact("512KiB-1MiB")},
		{Key: FactSessionContinued, Type: FactBoolean, example: boolFact(true)},
	},
	// TWO DEFECTS ON ONE SITE, split by whether the session was continued, per
	// harness, since each harness keeps its sessions its own way.
	Identity: []FactKey{IdentityHarnessName, FactSessionContinued},
	// THREE IN SIX HOURS: one is a long conversation whose session is retired
	// and a second can be the session it fell back on; a third is a conversation
	// no session of its own fits, which retiring them does not end.
	Threshold: Threshold{Count: 3, WindowHours: 6},
	Harness:   true,
}, {
	ID:    ClassFailureLoop,
	Title: "A seat's turns fail in a row at one place in the engine",
	Summary: "A seat's turns failed in a row at the same function, or a message on its inbox was " +
		"lost to such a failure, for a cause the engine could not attribute to a provider, a " +
		"credential, a budget, a box or its environment. The report names the function the failure " +
		"left the engine's code through; a report at this class's own site means no function on the " +
		"way out recorded it yet.",
	Component: "internal/engine",
	Site:      "internal/engine.(*Engine).publishTurnCompleted",
	// EACH PLACE A FAILURE LEAVES THE ENGINE IS ITS OWN DEFECT, read off the
	// stamp on the error (internal/origin).
	SiteVaries: true,
	Facts: []Fact{
		{Key: FactMessageLost, Type: FactBoolean, example: boolFact(false)},
	},
	// FIVE IN AN HOUR: a failed turn is redelivered within minutes, so five in
	// a row at one place outlasts any blip that clears on its own retries. A
	// lost message is one: its whole delivery budget is spent already.
	Threshold: Threshold{Count: 5, WindowHours: 1},
}, {
	ID:    ClassDeadLettered,
	Title: "A seat's message was dead-lettered",
	Summary: "A message on a seat's inbox used up its whole delivery budget on hand-backs, the " +
		"deliveries the engine returned to the queue rather than ran, while no node was draining and " +
		"every node ran one build, and was moved to the dead-letter subject. The person or system " +
		"that sent it was not answered. The report names why its last delivery was handed back.",
	Component: "internal/engine",
	Site:      "internal/engine.(*Dispatcher).Dispatch",
	Facts: []Fact{
		{Key: FactDeferCause, Type: FactEnum, Enum: deferCauses(), example: enumFact(string(queue.DeferSeatMoved))},
	},
	// EACH HAND-BACK PATH IS ITS OWN DEFECT.
	Identity: []FactKey{FactDeferCause},
	// ONE: reaching the boundary already took every delivery the budget has.
	Threshold: Threshold{Count: 1, WindowHours: 24},
}, {
	ID:    ClassEnginePanicked,
	Title: "The engine panicked",
	Summary: "A panic was recovered where the engine contains one, and the `scope` says in what: a turn, a " +
		"tool call, a queue handler, a seat's loop, an API request. A panic is a defect in the engine rather " +
		"than in anything it talks to. The report names the function that panicked and up to five of the " +
		"engine's own frames under it, each a function, its file in the public source and a line; the panic's " +
		"value stays on the node's log.",
	Component: "internal/panics",
	Site:      "internal/panics.Recovered",
	// EACH FUNCTION THAT PANICS IS ITS OWN DEFECT, its frames how a maintainer
	// finds the caller that handed it the input.
	SiteVaries: true,
	Frames:     true,
	Facts: []Fact{
		{Key: FactScope, Type: FactEnum, Enum: enumStrings(panics.Scopes()), example: enumFact(string(panics.ScopeTurn))},
		{Key: FactPanicKind, Type: FactEnum, Enum: enumStrings(panics.Kinds()), example: enumFact(string(panics.KindNilDereference))},
	},
	// ONE: redelivering a panic runs the same code on the same input.
	Threshold: Threshold{Count: 1, WindowHours: 24},
}, {
	ID:    ClassProcessCrashed,
	Title: "The engine crashed",
	Summary: "The engine's process ended on a crash the Go runtime could not recover from, and the node's next " +
		"start found its output. The `fatal_kind` says what it was: a panic nothing recovered, a map used by two " +
		"goroutines at once, a lock or a wait group misused, a stack past its limit, a native library's fault, " +
		"every goroutine asleep, or another fatal error. A host out of memory or threads is never reported. The " +
		"report names the engine function on top of the crashing goroutine and up to five of its frames; the rest " +
		"of the output is deleted on the node, unsent.",
	Component: "internal/issuereport",
	Site:      "internal/issuereport.readCrash",
	// THE FUNCTION ON TOP OF THE CRASHING GOROUTINE, and the class's own site for
	// a crash with no frame of the engine's, a native library's own thread.
	SiteVaries: true,
	Frames:     true,
	Facts: []Fact{
		{Key: FactFatalKind, Type: FactEnum, Enum: enumStrings(fatalKinds), example: enumFact(string(FatalConcurrentMap))},
		// OPTIONAL: a panic's alone.
		{Key: FactPanicKind, Type: FactEnum, Enum: enumStrings(panics.Kinds()), Optional: true,
			example: enumFact(string(panics.KindNilMapWrite))},
	},
	// A CONCURRENT WRITE AND AN OVERFLOW IN ONE FUNCTION ARE TWO FIXES.
	Identity: []FactKey{FactFatalKind},
	// ONE: the process is already gone, and a crash loop is one report.
	Threshold: Threshold{Count: 1, WindowHours: 24},
}, {
	ID:    ClassMigrationFailed,
	Title: "A schema migration failed",
	Summary: "A numbered schema migration of a tagged release failed on a node's database, and the node " +
		"reported it on its next successful start. The site is the migration file; `from_migration` is the " +
		"last file the database had applied, so the two numbers reproduce the upgrade, and `sql_error_class` " +
		"is the driver's class of refusal. The data that made it fail never leaves the node.",
	Component: "internal/store",
	Site:      "internal/store.(*DB).migrate",
	// THE FILE THAT FAILED, each its own defect.
	SiteVaries: true,
	SiteKind:   SiteMigration,
	Facts: []Fact{
		{Key: FactEstate, Type: FactEnum, Enum: enumStrings(store.Estates), example: enumFact(string(store.EstateNode))},
		{Key: FactFromMigration, Type: FactInteger, Max: maxMigrationNumber, example: intFact(147)},
		{Key: FactSQLErrorClass, Type: FactEnum, Enum: enumStrings(store.MigrationFailures()),
			example: enumFact(string(store.MigrationConstraint))},
	},
	// ONE: the node could not start on it.
	Threshold: Threshold{Count: 1, WindowHours: 24},
}, {
	ID:    ClassRunStranded,
	Title: "An executor run was stranded by the engine",
	Summary: "An executor's run ended with no turn able to resume it, for a reason the engine documents as an " +
		"invariant of its own and not as its environment or a person's act: the run's record held none of its " +
		"turn's parked state, its turn suspended with no conversation recorded, every attempt to resume a " +
		"finished job failed on an error the engine could not attribute elsewhere, or its parked state was " +
		"saved in a format an upgrade retired. The `reason` says which.",
	Component: "internal/sandbox",
	Site:      "internal/sandbox.(*Coordinator).settleFailed",
	Facts: []Fact{
		{Key: FactReason, Type: FactEnum, Enum: []string{
			types.ExecutorFailureNoExecuteState, types.ExecutorFailureSuspensionUnrecorded, types.ExecutorFailureResumeFailing,
			types.ExecutorFailureStateRetired,
		}, example: enumFact(types.ExecutorFailureNoExecuteState)},
	},
	// EACH REASON IS ITS OWN DEFECT: four invariants, four issues.
	Identity: []FactKey{FactReason},
	// ONE: a stranded run is the broken invariant itself, with nothing to wait out.
	Threshold: Threshold{Count: 1, WindowHours: 24},
}}

// fingerprintOf is the identity of a report of this class at this place, with its
// declared identity read off what the report says. A value a required fact left
// out reads as nothing here and stops the report at [Entry.checked].
func (e Entry) fingerprintOf(component Component, site Site, harness *Harness, facts map[FactKey]FactValue) Fingerprint {
	identity := make([]string, 0, len(e.Identity))
	for _, path := range e.Identity {
		value := ""
		if path == IdentityHarnessName {
			if harness != nil {
				value = string(harness.Name)
			}
		} else if fact, present := facts[path]; present {
			value = fact.identityValue()
		}
		identity = append(identity, string(path)+"="+value)
	}
	return FingerprintOf(e.ID, component, site, identity...)
}

// placeOf is where a report of this class says it happened: the class's own
// place, unless the class varies and its signal found another. A component is
// always its site's, so triage reads the package that broke, not the detector.
func (e Entry) placeOf(found Location) (Component, Location) {
	if e.SiteKind == SiteMigration && found.Site != "" && found.Site != e.Site {
		return e.Component, Location{Site: found.Site}
	}
	if e.SiteVaries && found.Site != "" && found.Site != e.Site {
		return found.Site.Package(), found
	}
	return e.Component, Location{Site: e.Site}
}

func entryOf(c Class) (Entry, bool) {
	i := slices.IndexFunc(catalogue, func(e Entry) bool { return e.ID == c })
	if i < 0 {
		return Entry{}, false
	}
	return catalogue[i], true
}

// Catalogue is every class, in the table's order.
func Catalogue() []Entry { return slices.Clone(catalogue) }

// catalogueExport is the file a relay vendors: the classes and the closed sets
// its validation holds a report to.
type catalogueExport struct {
	Schema   int                 `json:"schema"`
	Enums    map[string][]string `json:"enums"`
	Patterns map[string]string   `json:"patterns"`
	Limits   map[string]int      `json:"limits"`
	Classes  []Entry             `json:"classes"`
}

// CatalogueJSON is the generated export, `schema/issue-report-catalogue.json`.
func CatalogueJSON() ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(catalogueExport{Schema: SchemaVersion, Enums: map[string][]string{
		"engine.os":             enumStrings(knownOS),
		"engine.arch":           enumStrings(knownArch),
		"harness.name":          enumStrings(knownHarnesses),
		"topology.nodes":        enumStrings(knownNodeBuckets),
		"topology.stream":       enumStrings(knownStreams),
		"topology.coordination": enumStrings(knownCoordination),
		"topology.sandbox":      enumStrings(knownSandboxes),
	}, Patterns: map[string]string{
		"engine.version":  versionShape().String(),
		"engine.commit":   commitShape().String(),
		"engine.go":       goShape().String(),
		"harness.version": harnessShape().String(),
		"site":            siteShape().String(),
		"source.file":     sourceFileShape().String(),
		"site.migration":  migrationFileShape().String(),
	}, Limits: map[string]int{
		"occurrence.count": maxOccurrenceCount,
		"site.bytes":       maxSiteBytes,
		"frames.count":     maxFrames,
		"frame.line":       maxFrameLine,
		"report.bytes":     coord.MaxIssueReportPayloadBytes,
	}, Classes: catalogue})
	if err != nil {
		return nil, fmt.Errorf("issuereport: encode the catalogue: %w", err)
	}
	return out.Bytes(), nil
}

func enumStrings[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}
