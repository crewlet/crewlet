package tabular_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/tabular"
)

// card is a table card as the assistant stores it: a formula somebody typed
// into a title, a token count, a total.
func card() wire.TableCard {
	return wire.TableCard{Title: "Spend by agent", AsOf: time.Date(2026, 10, 24, 9, 41, 0, 0, time.UTC),
		Columns: []wire.Column{{Key: "agent", Label: "Agent", Type: wire.ColumnText},
			{Key: "tokens", Label: "Tokens", Type: wire.ColumnTokens}},
		Rows: [][]any{{"=HYPERLINK(\"http://example.com\")", json.Number("1200")},
			{"pm", float64(300)}},
		Total: []any{"Total", json.Number("1500")}, RowCount: 2}
}

// A TABLE CARD DOWNLOADS WHOLE: its columns as the header, every row, its
// total, and the instant it was read, with a formula never run and a count left
// a number. Mutation: drop the As of row, or write the token column as text,
// and this fails.
func TestATableCardDownloadsWithItsTotalAndItsInstant(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := (tabular.Tables{}).CSV(&buf, card()); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"Agent", "Tokens"}, {"'=HYPERLINK(\"http://example.com\")", "1200"},
		{"pm", "300"}, {"Total", "1500"}, {tabular.AsOfLabel, "2026-10-24T09:41:00Z"}}
	if len(records) != len(want) {
		t.Fatalf("the CSV is %v", records)
	}
	for i := range want {
		for j := range want[i] {
			if records[i][j] != want[i][j] {
				t.Errorf("cell %d,%d is %q, want %q", i, j, records[i][j], want[i][j])
			}
		}
	}
	buf.Reset()
	if err := (tabular.Tables{}).XLSX(&buf, card()); err != nil {
		t.Fatal(err)
	}
	got := cells(t, part(t, buf.Bytes(), "xl/worksheets/sheet1.xml"))
	if c := got[1][1]; c.kind != "" || c.text != "1200" {
		t.Errorf("the token cell is %+v, want the number 1200", c)
	}
	if c := got[1][0]; c.kind != "inlineStr" || c.text != "'=HYPERLINK(\"http://example.com\")" {
		t.Errorf("the formula cell is %+v", c)
	}
	if wb := part(t, buf.Bytes(), "xl/workbook.xml"); !bytes.Contains([]byte(wb), []byte(`name="Spend by agent"`)) {
		t.Errorf("the sheet is not named for the card: %s", wb)
	}
	bad := card()
	bad.Rows = append(bad.Rows, []any{"short"})
	if err := tabular.CSV(io.Discard, bad); err == nil {
		t.Error("a row short of its columns was written")
	}
}
