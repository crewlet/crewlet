package tabular

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// The SpreadsheetML limits a file is held to, from the format's own
// specification as every spreadsheet enforces it.
const (
	// MaxXLSXRows is a worksheet's rows, the header included.
	MaxXLSXRows = 1 << 20
	// MaxXLSXColumns is a worksheet's columns.
	MaxXLSXColumns = 1 << 14
	// MaxXLSXCellChars is one cell's text, in UTF-16 code units.
	MaxXLSXCellChars = 32767
	// maxSheetName is a worksheet name's length, in characters.
	maxSheetName = 31
)

// zipTime stamps every entry, so one table is one file byte for byte: a
// download of the same snapshot twice is the same download.
var zipTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// The package's fixed parts, in the order they are written. The worksheet is
// last because it is streamed: a zip archive is written one entry at a time.
var xlsxParts = []struct{ name, body string }{
	{"[Content_Types].xml", xml.Header + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
		`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
		`</Types>`},
	{"_rels/.rels", xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
		`</Relationships>`},
	{"xl/_rels/workbook.xml.rels", xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
		`</Relationships>`},
}

// NewXLSX writes rows as a one-sheet XLSX workbook to w, named sheet.
//
// A MINIMAL PACKAGE over archive/zip and nothing else: a workbook, one
// worksheet, inline strings and numbers, no shared strings, no styles and no
// formulas. A text cell is an inline string, which a spreadsheet never
// evaluates, and is [Neutralise]d as a CSV cell is. A cell, a row or a column
// past the format's limits is refused with [ErrTooLarge] rather than cut.
func NewXLSX(w io.Writer, sheet string) (Writer, error) {
	x := &xlsxWriter{zip: zip.NewWriter(w)}
	for _, part := range xlsxParts {
		if err := x.entry(part.name, part.body); err != nil {
			return nil, err
		}
	}
	workbook := xml.Header + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<sheets><sheet name="` + escape(SheetName(sheet)) + `" sheetId="1" r:id="rId1"/></sheets></workbook>`
	if err := x.entry("xl/workbook.xml", workbook); err != nil {
		return nil, err
	}
	sheetPart, err := x.create("xl/worksheets/sheet1.xml")
	if err != nil {
		return nil, err
	}
	x.sheet = sheetPart
	if _, err := io.WriteString(x.sheet, xml.Header+
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`); err != nil {
		return nil, fmt.Errorf("tabular: write an XLSX worksheet: %w", err)
	}
	return x, nil
}

type xlsxWriter struct {
	zip   *zip.Writer
	sheet io.Writer
	rows  int
}

func (x *xlsxWriter) create(name string) (io.Writer, error) {
	part, err := x.zip.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate,
		Modified: zipTime})
	if err != nil {
		return nil, fmt.Errorf("tabular: write the XLSX part %s: %w", name, err)
	}
	return part, nil
}

func (x *xlsxWriter) entry(name, body string) error {
	part, err := x.create(name)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(part, body); err != nil {
		return fmt.Errorf("tabular: write the XLSX part %s: %w", name, err)
	}
	return nil
}

func (x *xlsxWriter) Write(row []Cell) error {
	switch {
	case x.rows >= MaxXLSXRows:
		return fmt.Errorf("%w: a worksheet holds at most %d rows", ErrTooLarge, MaxXLSXRows)
	case len(row) > MaxXLSXColumns:
		return fmt.Errorf("%w: a row of %d cells, and a worksheet holds at most %d columns",
			ErrTooLarge, len(row), MaxXLSXColumns)
	}
	x.rows++
	var b strings.Builder
	number := strconv.Itoa(x.rows)
	b.WriteString(`<row r="` + number + `">`)
	for i, cell := range row {
		ref := ColumnName(i) + number
		if cell.Numeric {
			b.WriteString(`<c r="` + ref + `"><v>` + cell.Value + `</v></c>`)
			continue
		}
		text := Neutralise(cell.Value)
		if n := len(utf16.Encode([]rune(text))); n > MaxXLSXCellChars {
			return fmt.Errorf("%w: cell %s is %d characters, and a cell holds at most %d",
				ErrTooLarge, ref, n, MaxXLSXCellChars)
		}
		b.WriteString(`<c r="` + ref + `" t="inlineStr"><is><t xml:space="preserve">` +
			escape(text) + `</t></is></c>`)
	}
	b.WriteString(`</row>`)
	if _, err := io.WriteString(x.sheet, b.String()); err != nil {
		return fmt.Errorf("tabular: write an XLSX row: %w", err)
	}
	return nil
}

func (x *xlsxWriter) Close() error {
	if _, err := io.WriteString(x.sheet, `</sheetData></worksheet>`); err != nil {
		return fmt.Errorf("tabular: finish an XLSX worksheet: %w", err)
	}
	if err := x.zip.Close(); err != nil {
		return fmt.Errorf("tabular: finish an XLSX file: %w", err)
	}
	return nil
}

// WriteXLSX writes one whole table as a one-sheet XLSX workbook: the header,
// then the rows.
func WriteXLSX(w io.Writer, sheet string, header []string, rows [][]Cell) error {
	out, err := NewXLSX(w, sheet)
	if err != nil {
		return err
	}
	return writeAll(out, header, rows)
}

// ColumnName is the spreadsheet's name for the zero-based column i: A to Z,
// then AA, AB and on.
func ColumnName(i int) string {
	name := ""
	for i++; i > 0; i = (i - 1) / 26 {
		name = string(rune('A'+(i-1)%26)) + name
	}
	return name
}

// SheetName is name as a worksheet may be called: the characters the format
// forbids (`[]:*?/\`) as spaces, no apostrophe at either end, at most 31
// characters, and "Sheet1" for a name that is empty after that.
func SheetName(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) || r < 0x20 {
			return ' '
		}
		return r
	}, name)
	if utf8.RuneCountInString(name) > maxSheetName {
		name = string([]rune(name)[:maxSheetName])
	}
	name = strings.TrimSpace(strings.Trim(name, "'"))
	if name == "" {
		return "Sheet1"
	}
	return name
}

// escape is s as XML character data, with every character XML 1.0 cannot
// carry (the C0 controls but tab, line feed and carriage return, and the
// non-characters) replaced by U+FFFD, since a part holding one is a file no
// spreadsheet opens.
func escape(s string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r == '\t', r == '\n', r == '\r':
			return r
		case r < 0x20, r == 0xFFFE, r == 0xFFFF, r >= 0xD800 && r <= 0xDFFF:
			return utf8.RuneError
		}
		return r
	}, s)
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(clean)); err != nil {
		// EscapeText writes to a strings.Builder, which never fails.
		panic(fmt.Sprintf("tabular: escape XML text: %v", err))
	}
	return b.String()
}
