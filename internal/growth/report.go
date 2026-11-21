package growth

import (
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/period"
)

// The bounds of a report tool's answer.
const (
	// ReportDefaultRows is a report's rows when the call names no limit.
	ReportDefaultRows = 200
	// ReportMaxRows is the most rows one answer carries: 2,000 rows of about
	// ten short columns stays under a tool result's bound, and a model reasons
	// over a few thousand rows at most. Larger questions aggregate.
	ReportMaxRows = 2000
)

// Range is a named date range a report call may give instead of dates.
type Range string

// The named ranges, each cut in the account's own zone.
const (
	RangeYesterday   Range = "yesterday"
	RangeLast7Days   Range = "last_7_days"
	RangeLast28Days  Range = "last_28_days"
	RangeMonthToDate Range = "month_to_date"
)

// Ranges is every named range.
func Ranges() []Range {
	return []Range{RangeYesterday, RangeLast7Days, RangeLast28Days, RangeMonthToDate}
}

// Valid reports whether r is a named range.
func (r Range) Valid() bool { return slices.Contains(Ranges(), r) }

// Days are a report's first and last day, inclusive, as dates in zone.
type Days struct {
	Start, End string
	Zone       *time.Location
}

// Resolve is r's days at now in zone: an account's day is its own zone's,
// never the company's clock or the host's. Yesterday and the trailing ranges
// end yesterday, the last complete day; month_to_date ends today.
func (r Range) Resolve(now time.Time, zone *time.Location) (Days, error) {
	if zone == nil {
		return Days{}, fmt.Errorf("growth: a range is cut in the account's zone, and none was given")
	}
	today := period.At(period.Day, now, zone)
	yesterday := today.Shift(-1)
	label := func(w period.Window) string { return w.Start.Format(time.DateOnly) }
	switch r {
	case RangeYesterday:
		return Days{Start: label(yesterday), End: label(yesterday), Zone: zone}, nil
	case RangeLast7Days:
		return Days{Start: label(today.Shift(-7)), End: label(yesterday), Zone: zone}, nil
	case RangeLast28Days:
		return Days{Start: label(today.Shift(-28)), End: label(yesterday), Zone: zone}, nil
	case RangeMonthToDate:
		return Days{Start: label(period.At(period.Month, now, zone)), End: label(today), Zone: zone}, nil
	}
	return Days{}, fmt.Errorf("growth: %q is not a named range", r)
}

// EmptyReason says why a report has no rows, so an empty answer is never
// read as zero.
type EmptyReason string

// The reasons a report is empty; a vendor's own reason is passed as written.
const (
	EmptyNoData      EmptyReason = "no_data_in_range"
	EmptyNotReady    EmptyReason = "not_ready_yet"
	EmptyThresholded EmptyReason = "thresholded"
)

// ColumnType is what a report column's cells hold.
type ColumnType string

// The column types.
const (
	ColumnText   ColumnType = "text"
	ColumnNumber ColumnType = "number"
	// ColumnMoney cells are decimal strings in the column's currency, never
	// summed across currencies.
	ColumnMoney ColumnType = "money"
	ColumnDate  ColumnType = "date"
)

// Column is one column of a report.
type Column struct {
	Name     string     `json:"name"`
	Type     ColumnType `json:"type"`
	Currency string     `json:"currency,omitempty"`
	// Untrusted marks text somebody else wrote (a search query, a campaign or
	// page name), which the answer fences (ADR-0040).
	Untrusted bool `json:"untrusted,omitempty"`
}

// Report is a report tool's answer: honest about truncation, its zone, how
// fresh it is and why it is empty.
type Report struct {
	Columns []Column `json:"columns"`
	Rows    [][]any  `json:"rows"`
	// Truncated is true when the vendor has more rows than the answer holds.
	Truncated bool `json:"truncated"`
	// TotalRows is the vendor's total where it says one; TotalRowsEstimate
	// where the vendor says its total may be off.
	TotalRows         *int64 `json:"total_rows,omitempty"`
	TotalRowsEstimate *int64 `json:"total_rows_estimate,omitempty"`
	// TimeZone is the zone the report's dates are in.
	TimeZone string `json:"time_zone"`
	// DataThrough is the last complete day; Estimated marks figures the
	// vendor may still restate after it.
	DataThrough string `json:"data_through,omitempty"`
	Estimated   bool   `json:"estimated,omitempty"`
	// EmptyReason says why Rows is empty.
	EmptyReason EmptyReason `json:"empty_reason,omitempty"`
}

// Cell is one cell of a report's rows.
type Cell struct {
	Row, Column int
	Text        string
}

// UntrustedCells are the text cells of r's [Column.Untrusted] columns that
// hold something, which an answer fences.
func (r Report) UntrustedCells() []Cell {
	var out []Cell
	for c, col := range r.Columns {
		if !col.Untrusted {
			continue
		}
		for i, row := range r.Rows {
			if c < len(row) {
				if text, ok := row[c].(string); ok && text != "" {
					out = append(out, Cell{Row: i, Column: c, Text: text})
				}
			}
		}
	}
	return out
}

// RowLimit is the rows a call asked for, held to [ReportMaxRows], and
// [ReportDefaultRows] for none.
func RowLimit(asked int) int {
	switch {
	case asked <= 0:
		return ReportDefaultRows
	case asked > ReportMaxRows:
		return ReportMaxRows
	}
	return asked
}
