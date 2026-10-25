package artifactdata

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/tabular"
)

// THE CSV EXPORT of one collection: a header row naming the document id, every
// column (a schemaless collection's every top-level field, sorted) and the
// row's attribution, then one row per live document the reader may see, in id
// order. A PERSON READS IT: a declared column is headed by its label (its key
// where it has none), option ids are written as their labels, a person cell as
// the seat's name ([Names]), and a list as its elements joined by "; ". No
// path reads an export back in, so nothing keys on the stored field names.
//
// A CELL A SPREADSHEET WOULD RUN AS A FORMULA is written with a leading
// apostrophe: a value somebody typed as `=HYPERLINK(...)` into a shared sheet
// must not execute on the machine of whoever opens the export (CSV injection).
// That rule is internal/tabular's, which every download the engine writes
// goes through, so this export and the Dashboard Assistant's table downloads
// cannot disagree about it.
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

// Names answers the name a person cell's seat handle is written as: the
// seat's display name in the running org, or the handle itself for a seat the
// org does not hold (a retired one). Nil writes every handle as it is.
type Names func(handle string) string

// Export writes one collection as CSV to w, under the collection's read rule,
// with person cells named by names.
func Export(ctx context.Context, r ExportSource, id artifacts.ID, rd Reading, collection string,
	names Names, w io.Writer) error {
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
	if names == nil {
		names = func(handle string) string { return handle }
	}
	out := tabular.NewCSV(w)
	header := []string{"ID"}
	for _, k := range keys {
		header = append(header, columnTitle(coll, k))
	}
	header = append(header, "Created by", "Created at", "Updated by", "Updated at", "Version")
	if err := out.Write(tabular.Texts(header...)); err != nil {
		return fmt.Errorf("artifactdata: write the export of %s: %w", collection, err)
	}
	cursor := ""
	for {
		page, err := r.Docs(ctx, id, rd, collection, 0, cursor, exportPage)
		if err != nil {
			return err
		}
		for _, d := range page.Docs {
			row, err := exportRow(coll, keys, names, d)
			if err != nil {
				return err
			}
			if err := out.Write(row); err != nil {
				return fmt.Errorf("artifactdata: write the export of %s: %w", collection, err)
			}
		}
		if page.After == "" {
			if err := out.Close(); err != nil {
				return fmt.Errorf("artifactdata: write the export of %s: %w", collection, err)
			}
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

// columnTitle is a field column's header: a declared column's label, or its
// key where it has none (and every field of a schemaless collection).
func columnTitle(c *collState, key string) string {
	if col, declared := c.columns[key]; declared && col.Label != "" {
		return col.Label
	}
	return key
}

// exportRow is one document's CSV row.
func exportRow(c *collState, keys []string, names Names, d Doc) ([]tabular.Cell, error) {
	v, err := canon.Parse(d.Fields, canon.MaxDepth)
	if err != nil {
		return nil, fmt.Errorf("artifactdata: decode %s/%s: %w", c.spec.Name, d.ID, err)
	}
	fields := map[string]canon.Value{}
	for _, m := range v.Members {
		fields[m.Key] = m.Value
	}
	row := []tabular.Cell{tabular.Text(d.ID)}
	for _, k := range keys {
		val, held := fields[k]
		if !held {
			row = append(row, tabular.Text(""))
			continue
		}
		row = append(row, tabular.Text(render(c.columns[k], names, val)))
	}
	return append(row, tabular.Text(d.CreatedBy.Name),
		tabular.Text(d.CreatedAt.UTC().Format(time.RFC3339)), tabular.Text(d.UpdatedBy.Name),
		tabular.Text(d.UpdatedAt.UTC().Format(time.RFC3339)),
		tabular.Number(strconv.FormatInt(d.Version, 10))), nil
}

// render is one value as a spreadsheet cell reads it.
func render(col artifacts.Column, names Names, v canon.Value) string {
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
		switch {
		case hasOptions(col.Type):
			return label(v.String)
		case col.Type == artifacts.ColumnPerson:
			return names(v.String)
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
			parts = append(parts, render(col, names, e))
		}
		if scalar {
			return strings.Join(parts, "; ")
		}
	}
	return string(canon.Encode(v))
}
