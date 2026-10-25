package tabular

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// THE DASHBOARD ASSISTANT'S TABLE CARD AS A DOWNLOAD: its columns as the
// header, its rows, its total where it has one, and a last row saying the
// instant the table was read, so a file passed on later still says how old its
// numbers are. A numeric column (a number, a token count, a duration in
// milliseconds) is written as numbers a spreadsheet sums; every other cell is
// text, neutralised as every text cell is.

// Tables writes table cards as downloads: the value internal/assistant's
// export route holds, beside [CSV] and [XLSX] themselves.
type Tables struct{}

// CSV is [CSV].
func (Tables) CSV(w io.Writer, t wire.TableCard) error { return CSV(w, t) }

// XLSX is [XLSX].
func (Tables) XLSX(w io.Writer, t wire.TableCard) error { return XLSX(w, t) }

// CSV writes a table card as CSV.
func CSV(w io.Writer, t wire.TableCard) error {
	return writeCard(NewCSV(w), t)
}

// XLSX writes a table card as a one-sheet XLSX workbook named for its title.
func XLSX(w io.Writer, t wire.TableCard) error {
	out, err := NewXLSX(w, t.Title)
	if err != nil {
		return err
	}
	return writeCard(out, t)
}

// AsOfLabel opens the row that says when a card's table was read.
const AsOfLabel = "As of"

func writeCard(out Writer, t wire.TableCard) error {
	header := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		header[i] = c.Label
	}
	if err := out.Write(Texts(header...)); err != nil {
		return err
	}
	rows := t.Rows
	if t.Total != nil {
		rows = append(rows[:len(rows):len(rows)], t.Total)
	}
	for i, row := range rows {
		if len(row) != len(t.Columns) {
			return fmt.Errorf("tabular: row %d of %q has %d cells for %d columns", i, t.Title,
				len(row), len(t.Columns))
		}
		cells := make([]Cell, len(row))
		for j, v := range row {
			cells[j] = cardCell(t.Columns[j].Type, v)
		}
		if err := out.Write(cells); err != nil {
			return err
		}
	}
	if err := out.Write(Texts(AsOfLabel, t.AsOf.UTC().Format(time.RFC3339))); err != nil {
		return err
	}
	return out.Close()
}

// cardCell is one card cell as a file carries it.
func cardCell(typ wire.ColumnType, v any) Cell {
	var s string
	switch n := v.(type) {
	case nil:
		return Text("")
	case string:
		s = n
	case bool:
		s = strconv.FormatBool(n)
	case json.Number:
		s = n.String()
	case float64:
		s = strconv.FormatFloat(n, 'f', -1, 64)
	case int:
		s = strconv.Itoa(n)
	case int64:
		s = strconv.FormatInt(n, 10)
	default:
		s = fmt.Sprint(n)
	}
	switch typ {
	case wire.ColumnNumber, wire.ColumnTokens, wire.ColumnDuration:
		return Number(s)
	}
	return Text(s)
}
