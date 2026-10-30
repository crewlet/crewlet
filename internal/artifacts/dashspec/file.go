package dashspec

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/words"
)

// ---- file datasets -------------------------------------------------------- //

// The essences a file dataset may read.
const (
	EssenceCSV  = "text/csv"
	EssenceJSON = "application/json"
)

// FileEssences are every type a file dataset reads.
var FileEssences = []string{EssenceCSV, EssenceJSON}

// Version is a version's files as the format reads them: the same three
// questions every native format asks of a version, whichever surface holds
// its bytes (the publish pipeline's composed version, or a stored one).
type Version interface {
	// Paths are every file of the version.
	Paths() []string

	// Essence is the media type essence of the file at path, false where
	// the version holds none.
	Essence(path string) (string, bool)

	// Read is the bytes of the file at path.
	Read(path string) ([]byte, error)
}

// ParseFile is a file dataset's table from the file's bytes: a CSV whose
// first record is its header, or a JSON array of objects. Every declared
// column must be in the file (a header, or a key some object holds); a column
// the spec does not declare is not answered. A value that is not one of its
// column's type is REFUSED, naming the line, since a file is fixed with its
// version and a publish is the one moment its author can fix it.
func ParseFile(d Dataset, essence string, raw []byte) (Table, error) {
	if d.Source != SourceFile || d.File == nil {
		return Table{}, fmt.Errorf("%w: %q is not a file dataset", ErrInvalid, d.ID)
	}
	if int64(len(raw)) > artifacts.MaxFileDatasetBytes {
		return Table{}, fileFinding(d, 0, fmt.Sprintf("is %d bytes, over the %d a file dataset reads",
			len(raw), artifacts.MaxFileDatasetBytes))
	}
	if !utf8.Valid(raw) {
		return Table{}, fileFinding(d, 0, "is not UTF-8 text")
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var (
		records []map[string]any
		lines   []int
		err     error
	)
	switch essence {
	case EssenceCSV:
		records, lines, err = readCSV(d, raw)
	case EssenceJSON:
		records, lines, err = readJSON(d, raw)
	default:
		return Table{}, fileFinding(d, 0, fmt.Sprintf("is %s, and a file dataset reads %s", essence,
			words.Or(FileEssences)))
	}
	if err != nil {
		return Table{}, err
	}
	if len(records) > artifacts.MaxDatasetRows {
		return Table{}, fileFinding(d, 0, fmt.Sprintf("holds %d rows, over the %d one dataset answers",
			len(records), artifacts.MaxDatasetRows))
	}
	t := Table{Columns: slices.Clone(d.Columns), Key: d.Key, Rows: make([]Row, len(records))}
	keys := map[string]int{}
	for i, rec := range records {
		row := make(Row, len(d.Columns))
		for _, c := range d.Columns {
			v, err := Coerce(c.Type, rec[c.Key])
			if err != nil {
				return Table{}, fileFinding(d, lines[i], fmt.Sprintf("column %q: %v", c.Key, err))
			}
			row[c.Key] = v
		}
		if d.Key != "" {
			k := joinKey(row[d.Key])
			switch first, dup := keys[k]; {
			case row[d.Key] == nil:
				return Table{}, fileFinding(d, lines[i], fmt.Sprintf("the key column %q is empty", d.Key))
			case dup:
				return Table{}, fileFinding(d, lines[i], fmt.Sprintf("the key %v is the row's at line %d too",
					row[d.Key], first))
			}
			keys[k] = lines[i]
		}
		t.Rows[i] = row
	}
	return t, nil
}

// fileFinding is a refusal of a file dataset's file.
func fileFinding(d Dataset, line int, msg string) error {
	return &Invalid{Findings: []Finding{{Path: d.File.Path, Line: line,
		Field: "datasets." + d.ID, Message: msg}}}
}

// readCSV reads a CSV's records, each keyed by its header, with the line each
// starts on.
func readCSV(d Dataset, raw []byte) ([]map[string]any, []int, error) {
	r := csv.NewReader(bytes.NewReader(raw))
	r.ReuseRecord = false
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, fileFinding(d, 1, "has no header record")
	}
	if err != nil {
		return nil, nil, fileFinding(d, csvLine(err), "is not CSV: "+err.Error())
	}
	for _, c := range d.Columns {
		if !slices.Contains(header, c.Key) {
			return nil, nil, fileFinding(d, 1, fmt.Sprintf("has no %q in its header (%s)", c.Key,
				words.And(header)))
		}
	}
	var out []map[string]any
	var lines []int
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, lines, nil
		}
		if err != nil {
			return nil, nil, fileFinding(d, csvLine(err), "is not CSV: "+err.Error())
		}
		line, _ := r.FieldPos(0)
		m := make(map[string]any, len(header))
		for i, h := range header {
			m[h] = rec[i]
		}
		out = append(out, m)
		lines = append(lines, line)
		if len(out) > artifacts.MaxDatasetRows {
			return out, lines, nil
		}
	}
}

// csvLine is the line a CSV error names.
func csvLine(err error) int {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return pe.Line
	}
	return 0
}

// readJSON reads a JSON array of objects, with the line each starts on.
func readJSON(d Dataset, raw []byte) ([]map[string]any, []int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('[') {
		return nil, nil, fileFinding(d, 1, "is not a JSON array of objects")
	}
	var out []map[string]any
	var lines []int
	for dec.More() {
		at := dec.InputOffset()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			return nil, nil, fileFinding(d, lineAt(raw, at), "holds an element that is not an object: "+
				err.Error())
		}
		for k, v := range obj {
			switch v.(type) {
			case map[string]any, []any:
				return nil, nil, fileFinding(d, lineAt(raw, at), fmt.Sprintf("the key %q holds an object "+
					"or an array; a row's values are text, numbers, booleans or null", k))
			}
		}
		out = append(out, obj)
		lines = append(lines, lineAt(raw, at))
		if len(out) > artifacts.MaxDatasetRows {
			return out, lines, nil
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, fileFinding(d, 0, "is not a JSON array of objects: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, fileFinding(d, 0, "holds more than one JSON value")
	}
	for _, c := range d.Columns {
		if len(out) > 0 && !slices.ContainsFunc(out, func(m map[string]any) bool { _, ok := m[c.Key]; return ok }) {
			return nil, nil, fileFinding(d, 1, fmt.Sprintf("has no row holding %q", c.Key))
		}
	}
	return out, lines, nil
}

// lineAt is the line offset is on, skipping the separator a decoder's offset
// may still point before.
func lineAt(raw []byte, offset int64) int {
	if offset > int64(len(raw)) {
		offset = int64(len(raw))
	}
	rest := raw[offset:]
	skip := len(rest) - len(bytes.TrimLeft(rest, " \t\r\n,"))
	return bytes.Count(raw[:offset+int64(skip)], []byte("\n")) + 1
}
