package issuereport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// Class is a catalogue id: which defect a report describes.
type Class string

// The defects the engine detects. An exploitable one is never here: it goes
// through SECURITY.md, privately, and into no public issue.
const (
	ClassContextExhaustedLoop Class = "executor.context_exhausted_loop"
	ClassIdenticalFailureLoop Class = "turn.identical_failure_loop"
	ClassDeadLettered         Class = "queue.dead_lettered"
	ClassTurnPanicked         Class = "turn.panicked"
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
	FactErrorKind           FactKey = "error_kind"
	FactOutcome             FactKey = "outcome"
)

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
	// example is what a preview shows for it.
	example FactValue
}

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
	// Component is the engine package that detects it.
	Component Component `json:"component"`
	// Site is the engine function a report names, and the relay holds it to
	// exactly this. Where SiteVaries, a report names the function its signal
	// found, and this is what it falls back to.
	Site       Site      `json:"site"`
	SiteVaries bool      `json:"site_varies,omitempty"`
	Facts      []Fact    `json:"facts"`
	Threshold  Threshold `json:"threshold"`
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

// The values [FactErrorKind] and [FactOutcome] take.
const (
	kindError     = types.ErrorKindUnclassified
	OutcomeNak    = "nak"
	OutcomeDefer  = "defer"
	maxWindowFact = 1 << 24
)

// catalogue is the one table. Each threshold is a judgment recorded here: see
// `docs/guides/engine-issue-reports.md` for what each class reports.
var catalogue = []Entry{{
	ID:    ClassContextExhaustedLoop,
	Title: "Executor runs loop on an exhausted context",
	Summary: "A conversation's executor runs failed on context exhaustion several times in a row. " +
		"The engine retires a session that runs out, so each launch ran on another session and still " +
		"did not fit its model's context: the brief or the window is the likelier cause.",
	Component: "internal/sandbox",
	Site:      "internal/sandbox.collectedRecord",
	Facts: []Fact{
		{Key: FactContextWindowTokens, Type: FactInteger, Max: maxWindowFact, example: intFact(100_000)},
		{Key: FactSessionSizeBucket, Type: FactEnum, Enum: sessionSizeBuckets, example: enumFact("512KiB-1MiB")},
		{Key: FactSessionContinued, Type: FactBoolean, example: boolFact(true)},
	},
	// THREE IN SIX HOURS: one is a long conversation whose session is retired
	// and a second can be the session it fell back on; a third is a conversation
	// no session of its own fits, which retiring them does not end.
	Threshold: Threshold{Count: 3, WindowHours: 6},
	Harness:   true,
}, {
	ID:    ClassIdenticalFailureLoop,
	Title: "A seat's turns loop on an unclassified error",
	Summary: "A seat's turns failed in a row with the same error kind, one the engine could not " +
		"attribute to a provider, a credential, a budget or a box. The failure is the engine's own " +
		"until shown otherwise.",
	Component: "internal/engine",
	Site:      "internal/engine.(*Engine).publishTurnCompleted",
	Facts: []Fact{
		{Key: FactErrorKind, Type: FactEnum, Enum: []string{kindError}, example: enumFact(kindError)},
	},
	// FIVE IN AN HOUR: a failed turn is redelivered within minutes, so five in
	// a row outlasts any blip that clears on its own retries.
	Threshold: Threshold{Count: 5, WindowHours: 1},
}, {
	ID:    ClassDeadLettered,
	Title: "A seat's message was dead-lettered",
	Summary: "A message on a seat's inbox used up its whole delivery budget and was moved to the " +
		"dead-letter subject, for a cause the engine could not attribute to a provider, a credential " +
		"or a host. The person or system that sent it was not answered.",
	Component: "internal/engine",
	Site:      "internal/engine.(*Dispatcher).Dispatch",
	Facts: []Fact{
		{Key: FactOutcome, Type: FactEnum, Enum: []string{OutcomeNak, OutcomeDefer}, example: enumFact(OutcomeNak)},
	},
	// ONE: reaching the boundary already took every delivery the budget has.
	Threshold: Threshold{Count: 1, WindowHours: 24},
}, {
	ID:    ClassTurnPanicked,
	Title: "A turn panicked",
	Summary: "A panic was recovered while handling a seat's turn. A panic is a defect in the engine " +
		"rather than in anything it talks to. The report names the function that panicked, from the " +
		"engine's public source; the panic's value and stack stay on the node's log.",
	Component: "internal/agent/turn",
	Site:      "internal/agent/turn.Recovered",
	// EACH FUNCTION THAT PANICS IS ITS OWN DEFECT, and the one class whose site
	// is not fixed.
	SiteVaries: true,
	Facts:      []Fact{},
	// ONE: redelivering a panic runs the same code on the same input.
	Threshold: Threshold{Count: 1, WindowHours: 24},
}}

// siteOf is the function a report of this class names: its own, unless the class
// varies and its signal found one.
func (e Entry) siteOf(found Site) Site {
	if e.SiteVaries && found != "" {
		return found
	}
	return e.Site
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
		"topology.stream":       enumStrings(knownStreams),
		"topology.coordination": enumStrings(knownCoordination),
		"topology.sandbox":      enumStrings(knownSandboxes),
	}, Patterns: map[string]string{
		"engine.version":  versionShape.String(),
		"engine.commit":   commitShape.String(),
		"engine.go":       goShape.String(),
		"harness.version": harnessShape.String(),
		"site":            siteShape.String(),
	}, Limits: map[string]int{
		"occurrence.count": maxOccurrenceCount,
		"topology.nodes":   maxTopologyNodes,
		"site.bytes":       maxSiteBytes,
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
