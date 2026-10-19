package dashspec

import "slices"

// WidgetType is what a widget draws.
type WidgetType string

// The widgets a board may hold, each drawn by one uilet primitive.
const (
	// WidgetStat is one number with its label (StatCard): `value` folded
	// by `agg` over the dataset's rows.
	WidgetStat WidgetType = "stat"

	// WidgetMeter is a number against a maximum (Meter): `value` folded by
	// `agg`, over `max` or `max_field`.
	WidgetMeter WidgetType = "meter"

	// WidgetTimeSeries is lines over time (TimeSeries): `x` a date column,
	// `y` one to six number columns, or one with `by` splitting it into a
	// series per value.
	WidgetTimeSeries WidgetType = "timeseries"

	// WidgetColumns is stacked columns over time (StackedColumns): as a
	// time series.
	WidgetColumns WidgetType = "columns"

	// WidgetActivity is one value over time as a strip (ActivityStrip):
	// `x` a date column and exactly one `y`.
	WidgetActivity WidgetType = "activity"

	// WidgetBars is a ranked list of bars (BarList): `label` a text column
	// and `value` a number column, the first `limit` rows.
	WidgetBars WidgetType = "bars"

	// WidgetShare is the parts of a whole (StackedBar): as bars, at most
	// [MaxShareParts] parts.
	WidgetShare WidgetType = "share"

	// WidgetTable is the rows themselves (DataTable): `columns` named, the
	// first `limit` rows.
	WidgetTable WidgetType = "table"

	// WidgetNote is the author's words (Note), and no dataset.
	WidgetNote WidgetType = "note"

	// WidgetGroup holds other widgets under a title (ArrangeGroup).
	WidgetGroup WidgetType = "group"
)

// WidgetTypes are every widget this build draws.
var WidgetTypes = []WidgetType{
	WidgetStat, WidgetMeter, WidgetTimeSeries, WidgetColumns, WidgetActivity, WidgetBars,
	WidgetShare, WidgetTable, WidgetNote, WidgetGroup,
}

// Valid reports whether t is a widget this build draws.
func (t WidgetType) Valid() bool { return slices.Contains(WidgetTypes, t) }

// Agg folds a column over a dataset's rows into one value.
type Agg string

// The folds a stat or a meter may take.
const (
	AggFirst Agg = "first" // the first row's value
	AggLast  Agg = "last"  // the last row's value
	AggSum   Agg = "sum"
	AggAvg   Agg = "avg"
	AggMin   Agg = "min"
	AggMax   Agg = "max"
	AggCount Agg = "count" // rows whose value is present; every row when no column is named
)

// Aggs are every fold this build computes.
var Aggs = []Agg{AggFirst, AggLast, AggSum, AggAvg, AggMin, AggMax, AggCount}

// Valid reports whether a is a fold this build computes.
func (a Agg) Valid() bool { return slices.Contains(Aggs, a) }

// Format is how a number is written.
type Format string

// The number formats a widget may take.
const (
	FormatNumber  Format = "number"  // grouped, up to two decimals
	FormatInteger Format = "integer" // grouped, rounded
	FormatPercent Format = "percent" // a ratio, 0.25 written 25%
	FormatCompact Format = "compact" // 12.3k, 4.5M
)

// Formats are every number format this build writes.
var Formats = []Format{FormatNumber, FormatInteger, FormatPercent, FormatCompact}

// Valid reports whether f is a number format this build writes.
func (f Format) Valid() bool { return slices.Contains(Formats, f) }

// Tones are the hues a group may take: uilet's node hues, and none.
var Tones = []string{"none", "purple", "cyan", "green", "amber", "rose", "blue"}

// Widget is one figure of the board. A FLAT shape: every type reads the
// fields it names and refuses the rest, so an agent writes one object per
// widget rather than a nested block per type.
type Widget struct {
	ID      string     `json:"id"`
	Type    WidgetType `json:"type"`
	Title   string     `json:"title,omitempty"`
	Dataset string     `json:"dataset,omitempty"`

	// W is the widget's width in grid columns, 1 to 12; H its height in
	// grid rows, absent for the height of its content.
	W int  `json:"w"`
	H *int `json:"h,omitempty"`

	// A stat's and a meter's number.
	Value  string `json:"value,omitempty"`
	Agg    Agg    `json:"agg,omitempty"`
	Format Format `json:"format,omitempty"`
	Unit   string `json:"unit,omitempty"`

	// A meter's maximum: a number, or a column folded as Value is.
	Max      *float64 `json:"max,omitempty"`
	MaxField string   `json:"max_field,omitempty"`

	// A chart over time.
	X  string   `json:"x,omitempty"`
	Y  []string `json:"y,omitempty"`
	By string   `json:"by,omitempty"`

	// Bars and a share.
	Label string `json:"label,omitempty"`
	Limit int    `json:"limit,omitempty"`

	// A table's columns.
	Columns []string `json:"columns,omitempty"`

	// A note's words.
	Text string `json:"text,omitempty"`

	// A group's hue and its widgets.
	Tone     string   `json:"tone,omitempty"`
	Children []Widget `json:"children,omitempty"`
}
