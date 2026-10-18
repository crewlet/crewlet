package artifactdata

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE CSV EXPORT of one collection: a header row naming the document id, every
// column (a schemaless collection's every top-level field, sorted) and the
// row's attribution, then one row per live document the reader may see, in id
// order. Option ids are written as their labels and a list as its elements
// joined by "; ", because a spreadsheet is what reads it.
//
// A CELL A SPREADSHEET WOULD RUN AS A FORMULA is written with a leading
// apostrophe: a value somebody typed as `=HYPERLINK(...)` into a shared sheet
// must not execute on the machine of whoever opens the export (CSV injection).
//
// The export is read a page at a time, each page at the reading's level, so a
// store being written while it is exported is exported as each page found it;
// what the first page read is named by the caller's own position.

// exportPage is how many documents one read of an export takes.
const exportPage = artifacts.MaxQueryLimit

// ExportSource is what an export reads: the store's schema and its pages, from
// this node's reader or from a data node's through the estate router, which
// is why the export is a function over the reads rather than a reader's
// method a router could not carry.
type ExportSource interface {
	Schema(ctx context.Context, id artifacts.ID) ([]artifacts.Collection, error)
	Docs(ctx context.Context, id artifacts.ID, rd Reading, collection string, since int64,
		cursor string, limit int) (DocPage, error)
}

// Export writes one collection as CSV to w, under the collection's read rule.
func Export(ctx context.Context, r ExportSource, id artifacts.ID, rd Reading, collection string, w io.Writer) error {
	schema, err := r.Schema(ctx, id)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(schema, func(c artifacts.Collection) bool { return c.Name == collection })
	if i < 0 {
		return fmt.Errorf("%w: collection %q", ErrNotFound, collection)
	}
	spec := schema[i]
	coll := newCollState(spec, 0, false, true, 0)
	keys, err := exportKeys(ctx, r, id, rd, coll)
	if err != nil {
		return err
	}
	out := csv.NewWriter(w)
	header := append([]string{"id"}, keys...)
	header = append(header, "created_by", "created_at", "updated_by", "updated_at", "version")
	if err := out.Write(header); err != nil {
		return fmt.Errorf("artifactdata: write the export of %s: %w", collection, err)
	}
	cursor := ""
	for {
		page, err := r.Docs(ctx, id, rd, collection, 0, cursor, exportPage)
		if err != nil {
			return err
		}
		for _, d := range page.Docs {
			row, err := exportRow(coll, keys, d)
			if err != nil {
				return err
			}
			if err := out.Write(row); err != nil {
				return fmt.Errorf("artifactdata: write the export of %s: %w", collection, err)
			}
		}
		out.Flush()
		if err := out.Error(); err != nil {
			return fmt.Errorf("artifactdata: write the export of %s: %w", collection, err)
		}
		if page.After == "" {
			return nil
		}
		cursor = page.After
	}
}

// exportKeys is the field columns of an export: a schema's columns in their
// declared order, or a schemaless collection's every top-level field the
// reader's rows hold, sorted.
func exportKeys(ctx context.Context, r ExportSource, id artifacts.ID, rd Reading, c *collState) ([]string, error) {
	if !c.schemaless() {
		keys := make([]string, 0, len(c.spec.Columns))
		for _, col := range c.spec.Columns {
			keys = append(keys, col.Key)
		}
		return keys, nil
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := r.Docs(ctx, id, rd, c.spec.Name, 0, cursor, exportPage)
		if err != nil {
			return nil, err
		}
		for _, d := range page.Docs {
			v, err := canon.Parse(d.Fields, canon.MaxDepth)
			if err != nil {
				return nil, fmt.Errorf("artifactdata: decode %s/%s: %w", c.spec.Name, d.ID, err)
			}
			for _, m := range v.Members {
				seen[m.Key] = true
			}
		}
		if page.After == "" {
			break
		}
		cursor = page.After
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, nil
}

// exportRow is one document's CSV row.
func exportRow(c *collState, keys []string, d Doc) ([]string, error) {
	v, err := canon.Parse(d.Fields, canon.MaxDepth)
	if err != nil {
		return nil, fmt.Errorf("artifactdata: decode %s/%s: %w", c.spec.Name, d.ID, err)
	}
	fields := map[string]canon.Value{}
	for _, m := range v.Members {
		fields[m.Key] = m.Value
	}
	row := []string{cell(d.ID)}
	for _, k := range keys {
		val, held := fields[k]
		if !held {
			row = append(row, "")
			continue
		}
		row = append(row, cell(render(c.columns[k], val)))
	}
	return append(row, cell(d.CreatedBy.Name), d.CreatedAt.UTC().Format(time.RFC3339),
		cell(d.UpdatedBy.Name), d.UpdatedAt.UTC().Format(time.RFC3339),
		strconv.FormatInt(d.Version, 10)), nil
}

// render is one value as a spreadsheet cell reads it.
func render(col artifacts.Column, v canon.Value) string {
	label := func(id string) string {
		for _, o := range col.Options {
			if o.ID == id && o.Label != "" {
				return o.Label
			}
		}
		return id
	}
	switch v.Kind {
	case canon.KindString:
		if hasOptions(col.Type) {
			return label(v.String)
		}
		return v.String
	case canon.KindNumber:
		return v.Number
	case canon.KindBool:
		return strconv.FormatBool(v.Bool)
	case canon.KindNull:
		return ""
	case canon.KindArray:
		parts := make([]string, 0, len(v.Array))
		scalar := true
		for _, e := range v.Array {
			if e.Kind == canon.KindArray || e.Kind == canon.KindObject {
				scalar = false
				break
			}
			parts = append(parts, render(col, e))
		}
		if scalar {
			return strings.Join(parts, "; ")
		}
	}
	return string(canon.Encode(v))
}

// cell neutralises a value a spreadsheet would run as a formula. A plain
// number is left as it is, so a negative one stays a number.
func cell(s string) string {
	if s == "" || !strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return s
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	return "'" + s
}
