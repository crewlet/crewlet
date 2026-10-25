package tabular

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// Cell is one value of a row: text, or a number a spreadsheet may sum.
//
// A NUMBER IS DECLARED BY ITS WRITER and checked here: a column the writer
// knows is numeric hands [Number], and a value that does not parse as a finite
// decimal is written as text instead, so a cell is never a number a reader
// cannot read back.
type Cell struct {
	Value   string
	Numeric bool
}

// Text is a text cell.
func Text(s string) Cell { return Cell{Value: s} }

// Number is a numeric cell for a finite decimal, and a text cell for anything
// else.
func Number(s string) Cell {
	if !numeric(s) {
		return Text(s)
	}
	return Cell{Value: s, Numeric: true}
}

// Texts is a row of text cells, the header row's shape.
func Texts(values ...string) []Cell {
	row := make([]Cell, len(values))
	for i, v := range values {
		row[i] = Text(v)
	}
	return row
}

// Writer writes rows of cells to a file, the first row the header.
type Writer interface {
	// Write appends one row.
	Write(row []Cell) error
	// Close finishes the file. Nothing written before it is a complete
	// file.
	Close() error
}

// Neutralise is a text cell as it is written: a value a spreadsheet would run
// as a formula, one opening with `=`, `+`, `-`, `@`, a tab or a carriage
// return, gains a leading apostrophe, which every spreadsheet reads as "this
// is text". A plain number is left as it is, so a negative one stays a number.
//
// THE ONE RULE FOR BOTH FORMATS. A value somebody else typed into a shared
// record as `=HYPERLINK(...)` must not execute on the machine of whoever opens
// the download (CSV injection), and an XLSX inline string, which a spreadsheet
// does not evaluate, is written the same way so a cell copied out of it into
// another sheet does not become one either.
func Neutralise(s string) string {
	if s == "" || !strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return s
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	return "'" + s
}

// numeric reports whether s is a finite decimal number a spreadsheet reads as
// one: no hexadecimal, no infinity, no NaN, no underscores.
func numeric(s string) bool {
	if s == "" || strings.ContainsAny(s, "xXpP_iInN") {
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// ErrTooLarge is a row or a cell past what the format can hold. It is refused
// rather than cut, because a download that silently dropped the end of a
// value or a row would be read as the whole of it.
var ErrTooLarge = errors.New("tabular: past what the format holds")
