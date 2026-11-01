package dashspec

import (
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/words"
)

// THE ENGINE SOURCE CATALOGUE (D62): the named reads a dashboard's `engine`
// datasets may ask, each over one of Crewlet's own domains.
//
// A CLOSED CATALOGUE, NOT A QUERY LANGUAGE. Each entry is one read the domain
// already answers to a screen, with typed parameters and a fixed set of
// columns, so a spec cannot ask a domain for something no screen could, and
// a widget is validated against columns the engine promises rather than
// discovered at run time. What answers each entry, as the viewer, is the
// API's binding (ADR-0043); this table is what an author and an agent read
// (`list_dashboard_sources`, `crewlet artifacts sources`).

// Domain is the engine domain a source reads, which is also whose authority
// decides it.
type Domain string

// The domains the catalogue reads.
const (
	DomainTracker Domain = "tracker"
	DomainSpend   Domain = "spend"
	DomainAgents  Domain = "agents"
)

// Signal is the push a domain sends when what a source reads may have
// changed: the dashboard already receives it, and a dataset over the source
// refetches when it arrives. Empty for a domain that announces no change on
// a push every viewer receives.
type Signal string

// The change signals a source may name, each a push kind the dashboard's
// socket receives.
const (
	SignalNone   Signal = ""
	SignalAgents Signal = "agents"
	SignalTokens Signal = "tokens"
)

// ParamType is what one source parameter takes.
type ParamType string

// The parameter types a source declares.
const (
	ParamText   ParamType = "text"
	ParamNumber ParamType = "number"
	ParamEnum   ParamType = "enum"
)

// SourceParam is one parameter a source takes.
type SourceParam struct {
	Key     string    `json:"key"`
	Type    ParamType `json:"type"`
	Summary string    `json:"summary"`

	// Enum is the closed set an enum takes, the default first.
	Enum []string `json:"enum,omitempty"`

	// Min and Max bound a number, both inclusive.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`

	// MaxBytes bounds a text value.
	MaxBytes int `json:"max_bytes,omitempty"`

	// Default is the value an absent parameter takes, in its text form;
	// empty is "no value", which the source answers as the summary says.
	Default string `json:"default,omitempty"`
}

// EngineSource is one entry of the catalogue.
type EngineSource struct {
	Name    string `json:"name"`
	Domain  Domain `json:"domain"`
	Summary string `json:"summary"`

	// Access says, in words, who may read it: the domain's own rule, which
	// a viewer outside it is refused by.
	Access string `json:"access"`

	Params  []SourceParam `json:"params"`
	Columns []Column      `json:"columns"`
	Key     string        `json:"key"`

	// Signal is the push that announces a change, or none.
	Signal Signal `json:"signal,omitempty"`

	// Windowed is whether the source reads a span of time (spend over
	// company days), so a read with no rows means nothing happened in that
	// span. A source that reads what is true now (open work, every agent)
	// is not, and its empty answer means there is nothing to show, never
	// "nothing in this window".
	Windowed bool `json:"windowed,omitempty"`

	// MaxRows is the most rows one read answers.
	MaxRows int `json:"max_rows"`
}

// Param is s's parameter named key.
func (s EngineSource) Param(key string) (SourceParam, bool) {
	i := slices.IndexFunc(s.Params, func(p SourceParam) bool { return p.Key == key })
	if i < 0 {
		return SourceParam{}, false
	}
	return s.Params[i], true
}

func num(f float64) *float64 { return &f }

// MaxSpendDays is the longest spend window a source reads, the usage domain's
// own bound (tokens.MaxSpendRangeDays, held equal by the datasets surface's
// suite), and MaxSpendBands the bands a daily series names before the rest is
// Other (tokens.DefaultSeriesGroups).
const (
	MaxSpendDays  = 90
	MaxSpendBands = 4
)

// catalogue is the table, in the order every listing follows.
var catalogue = []EngineSource{
	{
		Name:   "tracker.items",
		Domain: DomainTracker,
		Summary: "Work items from Worklet, one row each. `scope` narrows them to the viewer's " +
			"own (`mine`), the viewer's queue, their priorities, the unassigned triage list, the " +
			"blocked or the overdue work, each resolved for whoever is viewing.",
		Access: "people and agents holding Worklet; anybody else is told they cannot read it",
		Params: []SourceParam{
			{Key: "scope", Type: ParamEnum, Summary: "whose work: all, or one of the viewer's own lists",
				Enum:    []string{"all", "mine", "queue", "priorities", "triage", "blocked", "overdue"},
				Default: "all"},
			{Key: "project", Type: ParamText, MaxBytes: 32,
				Summary: "a project key; absent reads every project"},
			{Key: "status_group", Type: ParamEnum, Summary: "one status group; absent reads the open work",
				Enum: []string{"not_started", "active", "done", "closed"}},
			{Key: "limit", Type: ParamNumber, Min: num(1), Max: num(500), Default: "100",
				Summary: "the most rows answered"},
		},
		Columns: []Column{
			{Key: "key", Type: ColumnText, Label: "Key"},
			{Key: "title", Type: ColumnText, Label: "Title"},
			{Key: "status", Type: ColumnText, Label: "Status", Words: WordsTrackerStatus},
			{Key: "status_group", Type: ColumnText, Label: "Status group", Words: WordsTrackerStatusGroup},
			{Key: "priority", Type: ColumnText, Label: "Priority", Words: WordsTrackerPriority},
			{Key: "assignee", Type: ColumnText, Label: "Assignee"},
			{Key: "project", Type: ColumnText, Label: "Project"},
			{Key: "due", Type: ColumnDate, Label: "Due"},
			{Key: "updated", Type: ColumnDate, Label: "Updated"},
			{Key: "points", Type: ColumnNumber, Label: "Points"},
			{Key: "blocked", Type: ColumnBoolean, Label: "Blocked"},
			{Key: "overdue", Type: ColumnBoolean, Label: "Overdue"},
		},
		Key:     "key",
		MaxRows: 500,
	},
	{
		Name:    "tracker.workload",
		Domain:  DomainTracker,
		Summary: "Open work per assignee across every project: how much each person or agent is carrying.",
		Access:  "people and agents holding Worklet; anybody else is told they cannot read it",
		Params: []SourceParam{
			{Key: "unit", Type: ParamText, MaxBytes: 64,
				Summary: "a team's key or name; absent reads everybody"},
		},
		Columns: []Column{
			{Key: "handle", Type: ColumnText, Label: "Assignee"},
			{Key: "open", Type: ColumnNumber, Label: "Open"},
			{Key: "points", Type: ColumnNumber, Label: "Points"},
			{Key: "blocked", Type: ColumnNumber, Label: "Blocked"},
			{Key: "overdue", Type: ColumnNumber, Label: "Overdue"},
			{Key: "unscheduled", Type: ColumnNumber, Label: "Unscheduled"},
		},
		Key:     "handle",
		MaxRows: 500,
	},
	{
		Name:   "spend.seats",
		Domain: DomainSpend,
		Summary: "Token spend per seat over a window of company days, or over the live window when " +
			"`days` is absent.",
		Access: "everybody the company admits: spend is the overview",
		Params: []SourceParam{
			{Key: "days", Type: ParamNumber, Min: num(1), Max: num(MaxSpendDays),
				Summary: "company days ending today; absent reads the live window"},
		},
		Columns: []Column{
			{Key: "handle", Type: ColumnText, Label: "Seat"},
			{Key: "name", Type: ColumnText, Label: "Name"},
			{Key: "person", Type: ColumnBoolean, Label: "Person"},
			{Key: "total_tokens", Type: ColumnNumber, Label: "Tokens"},
			{Key: "input_tokens", Type: ColumnNumber, Label: "Input"},
			{Key: "output_tokens", Type: ColumnNumber, Label: "Output"},
			{Key: "calls", Type: ColumnNumber, Label: "Calls"},
		},
		Key:      "handle",
		Signal:   SignalTokens,
		Windowed: true,
		MaxRows:  500,
	},
	{
		Name:   "spend.daily",
		Domain: DomainSpend,
		Summary: "Token spend per company day, one row per day and band, banded by `group` (the " +
			"busiest bands named, the rest as Other).",
		Access: "everybody the company admits: spend is the overview",
		Params: []SourceParam{
			{Key: "days", Type: ParamNumber, Min: num(1), Max: num(MaxSpendDays), Default: "14",
				Summary: "company days ending today"},
			{Key: "group", Type: ParamEnum, Summary: "what a band is",
				Enum: []string{"seat", "phase", "model", "provider", "unit", "worker"}, Default: "seat"},
		},
		Columns: []Column{
			{Key: "day", Type: ColumnDate, Label: "Day"},
			{Key: "band", Type: ColumnText, Label: "Band"},
			{Key: "total_tokens", Type: ColumnNumber, Label: "Tokens"},
		},
		Signal:   SignalTokens,
		Windowed: true,
		MaxRows:  MaxSpendDays * (MaxSpendBands + 1),
	},
	{
		Name:    "agents.status",
		Domain:  DomainAgents,
		Summary: "Every agent seat and what it is doing now: working, waiting on a person, stopped or idle.",
		Access:  "everybody the company admits: agent status is the overview",
		Params:  []SourceParam{},
		Columns: []Column{
			{Key: "handle", Type: ColumnText, Label: "Seat"},
			{Key: "name", Type: ColumnText, Label: "Name"},
			{Key: "activity", Type: ColumnText, Label: "Activity", Words: WordsAgentActivity},
			{Key: "phase", Type: ColumnText, Label: "Phase"},
			{Key: "paused", Type: ColumnBoolean, Label: "Paused"},
			{Key: "last_turn_at", Type: ColumnDate, Label: "Last turn"},
			{Key: "last_outcome", Type: ColumnText, Label: "Last outcome"},
		},
		Key:     "handle",
		Signal:  SignalAgents,
		MaxRows: 1000,
	},
}

// Catalogue is every engine source this build answers, in order, each a copy
// a caller may keep.
func Catalogue() []EngineSource {
	out := make([]EngineSource, len(catalogue))
	for i, s := range catalogue {
		out[i] = s.clone()
	}
	return out
}

// Source is the catalogue's entry named name, and false for a name this build
// does not answer.
func Source(name string) (EngineSource, bool) {
	i := slices.IndexFunc(catalogue, func(s EngineSource) bool { return s.Name == name })
	if i < 0 {
		return EngineSource{}, false
	}
	return catalogue[i].clone(), true
}

// SourceNames are the catalogue's names, in order.
func SourceNames() []string {
	out := make([]string, len(catalogue))
	for i, s := range catalogue {
		out[i] = s.Name
	}
	return out
}

func (s EngineSource) clone() EngineSource {
	s.Params = slices.Clone(s.Params)
	for i := range s.Params {
		s.Params[i].Enum = slices.Clone(s.Params[i].Enum)
	}
	s.Columns = slices.Clone(s.Columns)
	return s
}

// check reports why value is not one p takes, or "" when it is.
func (p SourceParam) check(value string) string {
	switch p.Type {
	case ParamEnum:
		if !slices.Contains(p.Enum, value) {
			return "is one of " + words.Or(p.Enum)
		}
	case ParamNumber:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f != float64(int64(f)) {
			return "is a whole number"
		}
		if p.Min != nil && f < *p.Min {
			return "is at least " + strconv.FormatFloat(*p.Min, 'f', -1, 64)
		}
		if p.Max != nil && f > *p.Max {
			return "is at most " + strconv.FormatFloat(*p.Max, 'f', -1, 64)
		}
	case ParamText:
		if value == "" {
			return "is not empty"
		}
		if p.MaxBytes > 0 && len(value) > p.MaxBytes {
			return "is at most " + strconv.Itoa(p.MaxBytes) + " bytes"
		}
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return "holds no control character"
			}
		}
	}
	return ""
}
