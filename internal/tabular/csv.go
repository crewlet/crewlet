package tabular

import (
	"encoding/csv"
	"fmt"
	"io"
)

// NewCSV writes rows as RFC 4180 CSV to w, every text cell [Neutralise]d.
//
// Rows are flushed as they are written, so an export read a page at a time is
// written a page at a time rather than held whole.
func NewCSV(w io.Writer) Writer { return &csvWriter{out: csv.NewWriter(w)} }

type csvWriter struct{ out *csv.Writer }

func (c *csvWriter) Write(row []Cell) error {
	fields := make([]string, len(row))
	for i, cell := range row {
		if cell.Numeric {
			fields[i] = cell.Value
			continue
		}
		fields[i] = Neutralise(cell.Value)
	}
	if err := c.out.Write(fields); err != nil {
		return fmt.Errorf("tabular: write a CSV row: %w", err)
	}
	c.out.Flush()
	if err := c.out.Error(); err != nil {
		return fmt.Errorf("tabular: write a CSV row: %w", err)
	}
	return nil
}

func (c *csvWriter) Close() error {
	c.out.Flush()
	if err := c.out.Error(); err != nil {
		return fmt.Errorf("tabular: finish a CSV file: %w", err)
	}
	return nil
}

// WriteCSV writes one whole table as CSV: the header, then the rows.
func WriteCSV(w io.Writer, header []string, rows [][]Cell) error {
	return writeAll(NewCSV(w), header, rows)
}

// writeAll writes a header and rows through out and closes it.
func writeAll(out Writer, header []string, rows [][]Cell) error {
	if err := out.Write(Texts(header...)); err != nil {
		return err
	}
	for _, row := range rows {
		if err := out.Write(row); err != nil {
			return err
		}
	}
	return out.Close()
}
