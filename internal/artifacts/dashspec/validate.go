package dashspec

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/jsonform"
	"github.com/crewlet/crewlet/internal/words"
)

// ---- validation ------------------------------------------------------------ //

// Board is a version's dashboard as this build draws it: the spec, every
// dataset's shape (its columns and key, the catalogue's for an engine
// dataset and the steps' for a calculated one), and every file dataset's
// rows, read once with the version they are fixed in.
type Board struct {
	Spec   *Spec
	Shapes map[string]Shape
	Files  map[string]Table

	// Weight is the bytes the board was read from (the spec and its file
	// datasets): what a cache of boards weighs one by.
	Weight int64
}

// Shape is a dataset's columns and the column naming a row.
type Shape struct {
	Columns []Column `json:"columns"`
	Key     string   `json:"key,omitempty"`
}

// column is s's column named key.
func (s Shape) column(key string) (Column, bool) {
	i := slices.IndexFunc(s.Columns, func(c Column) bool { return c.Key == key })
	if i < 0 {
		return Column{}, false
	}
	return s.Columns[i], true
}

// Read is the version's board: its spec parsed and held to the format, every
// file dataset read and held to its declared columns. A board outside the
// format is an [*Invalid] carrying every finding, so an author fixes them in
// one pass; any other error is a file that could not be read.
//
// The spec is read as stored ([jsonform.Stored]), so a board accepted
// before ADR-0059 tightened its keys reads as it did; [Check] is the publish's.
func Read(v Version) (*Board, error) { return read(v, jsonform.Stored(jsonform.UntilEOF)) }

// Check is [Read] as a publish holds a board whose spec it writes ([Parse]).
func Check(v Version) (*Board, error) { return read(v, jsonform.Decode) }

func read(v Version, decode jsonform.Reader) (*Board, error) {
	raw, err := v.Read(EntryPath)
	if err != nil {
		return nil, fmt.Errorf("dashspec: read %s: %w", EntryPath, err)
	}
	s, err := parse(raw, decode)
	if err != nil {
		return nil, err
	}
	b := &Board{Spec: s, Shapes: map[string]Shape{}, Files: map[string]Table{}, Weight: int64(len(raw))}
	c := &checker{board: b, version: v}
	c.spec()
	if c.err != nil {
		return nil, c.err
	}
	if len(c.found) > 0 {
		return nil, &Invalid{Findings: c.found}
	}
	return b, nil
}

// checker gathers a board's findings.
type checker struct {
	board   *Board
	version Version
	found   []Finding

	// err is a failure that is not a finding: a file the version names
	// whose bytes could not be read.
	err error
}

func (c *checker) add(field, format string, args ...any) {
	c.found = append(c.found, Finding{Path: EntryPath, Field: field, Message: fmt.Sprintf(format, args...)})
}

// text holds a person-readable string to its bounds.
func (c *checker) text(field, value string, required bool, maxBytes int) {
	switch {
	case required && strings.TrimSpace(value) == "":
		c.add(field, "is required")
	case len(value) > maxBytes:
		c.add(field, "is %d bytes, over the %d it may be", len(value), maxBytes)
	case !utf8.ValidString(value):
		c.add(field, "is not UTF-8 text")
	case strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\t' }):
		c.add(field, "holds a control character")
	}
}

func (c *checker) spec() {
	s := c.board.Spec
	if s.Version != FormatVersion {
		c.add("v", "is %d, and this build reads the dashboard format %d", s.Version, FormatVersion)
	}
	c.text("title", s.Title, false, int(artifacts.MaxTitleBytes))
	c.params()
	c.datasets()
	c.widgets()
}

// ---- filters -------------------------------------------------------------- //

// maxOptionBytes bounds one filter choice's value: a choice is a key a source
// parameter or a condition takes, never prose.
const maxOptionBytes = 64

func (c *checker) params() {
	s := c.board.Spec
	if len(s.Params) > artifacts.MaxDashboardParams {
		c.add("params", "holds %d filters, over the %d a board offers", len(s.Params), artifacts.MaxDashboardParams)
	}
	seen := map[string]bool{}
	for i, p := range s.Params {
		f := fmt.Sprintf("params[%d]", i)
		switch {
		case !ValidID(p.Key):
			c.add(f+".key", "%q is a lowercase letter, then lowercase letters, digits and `_`, at most 40", p.Key)
		case seen[p.Key]:
			c.add(f+".key", "%q is another filter's key too", p.Key)
		}
		seen[p.Key] = true
		c.text(f+".label", p.Label, true, int(artifacts.MaxTitleBytes))
		if len(p.Options) == 0 || len(p.Options) > artifacts.MaxParamOptions {
			c.add(f+".options", "holds %d choices, and a filter offers 1 to %d", len(p.Options),
				artifacts.MaxParamOptions)
		}
		values := map[string]bool{}
		for j, o := range p.Options {
			of := fmt.Sprintf("%s.options[%d]", f, j)
			switch {
			case values[o.Value]:
				c.add(of+".value", "%q is another choice's value too", o.Value)
			case len(o.Value) > maxOptionBytes:
				c.add(of+".value", "is %d bytes, over the %d a choice's value may be", len(o.Value), maxOptionBytes)
			case strings.ContainsFunc(o.Value, func(r rune) bool { return r < 0x20 || r == 0x7f }):
				c.add(of+".value", "holds a control character")
			}
			values[o.Value] = true
			c.text(of+".label", o.Label, true, int(artifacts.MaxTitleBytes))
		}
		if !values[p.Default] {
			c.add(f+".default", "%q is not one of the filter's choices", p.Default)
		}
	}
}

// ---- datasets ------------------------------------------------------------- //

// maxDatasetColumns bounds a store or file dataset's declared columns: a
// store collection's own bound.
const maxDatasetColumns = artifacts.MaxCollections

func (c *checker) datasets() {
	s := c.board.Spec
	if len(s.Datasets) == 0 || len(s.Datasets) > artifacts.MaxDashboardDatasets {
		c.add("datasets", "holds %d datasets, and a board holds 1 to %d", len(s.Datasets),
			artifacts.MaxDashboardDatasets)
	}
	seen := map[string]bool{}
	var calcs []int
	for i, d := range s.Datasets {
		f := fmt.Sprintf("datasets[%d]", i)
		switch {
		case !ValidID(d.ID):
			c.add(f+".id", "%q is a lowercase letter, then lowercase letters, digits and `_`, at most 40", d.ID)
		case seen[d.ID]:
			c.add(f+".id", "%q is another dataset's id too", d.ID)
		}
		seen[d.ID] = true
		c.text(f+".title", d.Title, true, int(artifacts.MaxTitleBytes))
		if !d.Source.Valid() {
			c.add(f+".source", "%q is not one of %s", d.Source, words.Or(SourceKinds))
			continue
		}
		c.blocks(f, d)
		if d.Refresh != nil && d.Source != SourceEngine {
			c.add(f+".refresh", "is an engine dataset's; a %s dataset is %s", d.Source, RefreshOf(d).Mode)
		}
		switch d.Source {
		case SourceStore:
			c.store(f, d)
		case SourceFile:
			c.file(f, d)
		case SourceEngine:
			c.engine(f, d)
		case SourceCalc:
			calcs = append(calcs, i)
		}
	}
	c.calcs(calcs)
}

// blocks holds a dataset to exactly the one block its source names.
func (c *checker) blocks(f string, d Dataset) {
	for _, b := range []struct {
		source SourceKind
		set    bool
	}{
		{SourceStore, d.Store != nil}, {SourceFile, d.File != nil},
		{SourceEngine, d.Engine != nil}, {SourceCalc, d.Calc != nil},
	} {
		switch {
		case b.source == d.Source && !b.set:
			c.add(f+"."+string(b.source), "is required for a %s dataset", d.Source)
		case b.source != d.Source && b.set:
			c.add(f+"."+string(b.source), "is a %s dataset's, and this one is %s", b.source, d.Source)
		}
	}
}

// declared holds a store or file dataset's declared columns and key, and
// records its shape.
func (c *checker) declared(f string, d Dataset, implicit []Column, defaultKey string) {
	if len(d.Columns) == 0 || len(d.Columns) > maxDatasetColumns {
		c.add(f+".columns", "declares %d columns, and a %s dataset declares 1 to %d", len(d.Columns),
			d.Source, maxDatasetColumns)
	}
	cols := slices.Clone(implicit)
	for j, col := range d.Columns {
		cf := fmt.Sprintf("%s.columns[%d]", f, j)
		switch {
		case !ValidColumnKey(col.Key):
			c.add(cf+".key", "%q is a letter or `_`, then letters, digits and `_`, at most 64", col.Key)
		case slices.ContainsFunc(cols, func(o Column) bool { return o.Key == col.Key }):
			c.add(cf+".key", "%q is declared twice, or is a column every %s dataset has", col.Key, d.Source)
		}
		if !col.Type.Valid() {
			c.add(cf+".type", "%q is not one of %s", col.Type, words.Or(ColumnTypes))
		}
		c.text(cf+".label", col.Label, false, int(artifacts.MaxTitleBytes))
		switch {
		case !col.Words.Valid():
			c.add(cf+".words", "%q is not one of %s", col.Words, words.Or(WordSets))
		case col.Words != WordsNone && col.Type != ColumnText:
			c.add(cf+".words", "names a text column's vocabulary, and %q is a %s", col.Key, col.Type)
		}
		cols = append(cols, col)
	}
	key := d.Key
	if key == "" {
		key = defaultKey
	}
	if key != "" && !slices.ContainsFunc(cols, func(o Column) bool { return o.Key == key }) {
		c.add(f+".key", "%q is not one of the dataset's columns", key)
	}
	c.board.Shapes[d.ID] = Shape{Columns: cols, Key: key}
}

func (c *checker) store(f string, d Dataset) {
	if d.Store != nil && !artifacts.ValidCollectionName(d.Store.Collection) {
		c.add(f+".store.collection", "%q is not a collection a store holds: a lowercase letter, then "+
			"lowercase letters, digits and `_`, at most %d bytes, and not one of %s", d.Store.Collection,
			artifacts.MaxCollectionNameBytes, words.Or(artifacts.ReservedCollections))
	}
	c.declared(f, d, []Column{{Key: StoreIDColumn, Type: ColumnText, Label: "Document"}}, StoreIDColumn)
}

func (c *checker) file(f string, d Dataset) {
	c.declared(f, d, nil, "")
	if d.File == nil {
		return
	}
	essence, ok := c.version.Essence(d.File.Path)
	switch {
	case d.File.Path == EntryPath:
		c.add(f+".file.path", "is the spec itself")
		return
	case !ok:
		c.add(f+".file.path", "%q is not a file of this version", d.File.Path)
		return
	case !slices.Contains(FileEssences, essence):
		c.add(f+".file.path", "%q is %s, and a file dataset reads %s", d.File.Path, essence,
			words.Or(FileEssences))
		return
	}
	if len(c.found) > 0 {
		// THE FILE IS READ ONLY FOR A SPEC THAT IS OTHERWISE SOUND: its
		// columns are what it is held to, and a refusal already found
		// costs no further reads.
		return
	}
	raw, err := c.version.Read(d.File.Path)
	if err != nil {
		c.err = fmt.Errorf("dashspec: read %s: %w", d.File.Path, err)
		return
	}
	t, err := ParseFile(d, essence, raw)
	var invalid *Invalid
	switch {
	case errors.As(err, &invalid):
		c.found = append(c.found, invalid.Findings...)
	case err != nil:
		c.err = err
	default:
		c.board.Files[d.ID] = t
		c.board.Weight += int64(len(raw))
	}
}

func (c *checker) engine(f string, d Dataset) {
	if len(d.Columns) > 0 {
		c.add(f+".columns", "is the catalogue's for an engine dataset, never declared")
	}
	if d.Key != "" {
		c.add(f+".key", "is the catalogue's for an engine dataset, never declared")
	}
	if d.Refresh != nil && (d.Refresh.EverySeconds < artifacts.MinRefreshSeconds ||
		d.Refresh.EverySeconds > artifacts.MaxRefreshSeconds) {
		c.add(f+".refresh.every_seconds", "is %d, and an interval is %d to %d seconds",
			d.Refresh.EverySeconds, artifacts.MinRefreshSeconds, artifacts.MaxRefreshSeconds)
	}
	if d.Engine == nil {
		return
	}
	src, ok := Source(d.Engine.Name)
	if !ok {
		c.add(f+".engine.name", "%q is not a source this build answers: one of %s", d.Engine.Name,
			words.Or(SourceNames()))
		return
	}
	for _, key := range sortedKeys(d.Engine.Params) {
		a := d.Engine.Params[key]
		pf := f + ".engine.params." + key
		p, ok := src.Param(key)
		if !ok {
			c.add(pf, "is not a parameter of %s", src.Name)
			continue
		}
		if a.Param == "" {
			if why := p.check(a.Literal); why != "" {
				c.add(pf, "%s", why)
			}
			continue
		}
		filter, ok := c.board.Spec.Param(a.Param)
		if !ok {
			c.add(pf, "is bound to the filter %q, which the board does not offer", a.Param)
			continue
		}
		for _, o := range filter.Options {
			if o.Value == "" {
				continue
			}
			if why := p.check(o.Value); why != "" {
				c.add(pf, "is bound to the filter %q, whose choice %q is not one %s.%s takes: it %s",
					a.Param, o.Value, src.Name, key, why)
			}
		}
	}
	c.board.Shapes[d.ID] = Shape{Columns: slices.Clone(src.Columns), Key: src.Key}
}

// calcs holds every calculated dataset to its steps, in an order where each
// input is shaped before the dataset that reads it, refusing a cycle and a
// chain deeper than [artifacts.MaxCalcDepth].
func (c *checker) calcs(indexes []int) {
	s := c.board.Spec
	depth := map[string]int{}
	var visit func(id string, path []string) int
	visit = func(id string, path []string) int {
		if dd, done := depth[id]; done {
			return dd
		}
		d, ok := s.Dataset(id)
		if !ok || d.Source != SourceCalc || d.Calc == nil {
			return 0
		}
		if slices.Contains(path, id) {
			return -1
		}
		path = append(path, id)
		deepest := 0
		for _, in := range CalcInputs(d) {
			sub := visit(in, path)
			if sub < 0 {
				return -1
			}
			deepest = max(deepest, sub)
		}
		depth[id] = deepest + 1
		return deepest + 1
	}
	order := slices.Clone(indexes)
	for _, i := range indexes {
		d := s.Datasets[i]
		if d.Calc == nil {
			continue
		}
		switch n := visit(d.ID, nil); {
		case n < 0:
			c.add(fmt.Sprintf("datasets[%d].calc", i), "reads itself, through %s", strings.Join(CalcInputs(d), ", "))
			return
		case n > artifacts.MaxCalcDepth:
			c.add(fmt.Sprintf("datasets[%d].calc", i), "stands on %d calculated datasets, over the %d a "+
				"calculation may", n, artifacts.MaxCalcDepth)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int { return depth[s.Datasets[a].ID] - depth[s.Datasets[b].ID] })
	for _, i := range order {
		c.calc(i)
	}
}

func (c *checker) calc(i int) {
	d := c.board.Spec.Datasets[i]
	f := fmt.Sprintf("datasets[%d]", i)
	if len(d.Columns) > 0 || d.Key != "" {
		c.add(f+".columns", "a calculated dataset's columns and key are its steps', never declared")
	}
	if d.Calc == nil {
		return
	}
	in, ok := c.board.Shapes[d.Calc.From]
	switch {
	case d.Calc.From == d.ID:
		c.add(f+".calc.from", "is the dataset itself")
		return
	case !ok:
		c.add(f+".calc.from", "%q is not a dataset of the board, or one refused above", d.Calc.From)
		return
	}
	if len(d.Calc.Steps) == 0 || len(d.Calc.Steps) > artifacts.MaxCalcSteps {
		c.add(f+".calc.steps", "holds %d steps, and a calculation takes 1 to %d", len(d.Calc.Steps),
			artifacts.MaxCalcSteps)
		return
	}
	cols, key := in.Columns, in.Key
	for j, st := range d.Calc.Steps {
		sf := fmt.Sprintf("%s.calc.steps[%d]", f, j)
		if why := stepFields(st); why != "" {
			c.add(sf, "%s", why)
			return
		}
		var with *Table
		if st.Op == OpJoin {
			w, ok := c.board.Shapes[st.With]
			if !ok || st.With == d.ID {
				c.add(sf+".with", "%q is not another dataset of the board", st.With)
				return
			}
			with = &Table{Columns: w.Columns, Key: w.Key}
		}
		next, nextKey, why := stepColumns(st, cols, key, with)
		if why != "" {
			c.add(sf, "%s", why)
			return
		}
		if st.Op == OpFilter {
			c.boundConditions(sf, st, cols)
		}
		cols, key = next, nextKey
	}
	c.board.Shapes[d.ID] = Shape{Columns: cols, Key: key}
}

// boundConditions holds every filter-bound condition's filter to a choice set
// the column takes.
func (c *checker) boundConditions(f string, st Step, cols []Column) {
	for k, w := range st.Where {
		if w.Value == nil || w.Value.Param == "" {
			continue
		}
		filter, ok := c.board.Spec.Param(w.Value.Param)
		if !ok {
			c.add(fmt.Sprintf("%s.where[%d].value", f, k), "is bound to the filter %q, which the board "+
				"does not offer", w.Value.Param)
			continue
		}
		col, _ := Shape{Columns: cols}.column(w.Column)
		for _, o := range filter.Options {
			if o.Value == "" {
				continue
			}
			if _, err := Coerce(col.Type, o.Value); err != nil {
				c.add(fmt.Sprintf("%s.where[%d].value", f, k), "is bound to the filter %q, whose choice %q "+
					"is not a %s", w.Value.Param, o.Value, col.Type)
			}
		}
	}
}

// stepFields refuses a field a step's op does not read.
func stepFields(st Step) string {
	if !st.Op.Valid() {
		return "op is one of " + words.Or(Ops)
	}
	set := map[string]bool{
		"where": len(st.Where) > 0, "by": len(st.By) > 0, "aggregates": len(st.Aggregates) > 0,
		"column": st.Column != "", "fn": st.Fn != "", "left": st.Left != nil, "right": st.Right != nil,
		"with": st.With != "", "on": st.On != "", "columns": len(st.Columns) > 0,
		"order": len(st.Order) > 0, "count": st.Count != 0,
	}
	allowed := map[Op][]string{
		OpFilter:  {"where"},
		OpGroup:   {"by", "aggregates"},
		OpCompute: {"column", "fn", "left", "right"},
		OpJoin:    {"with", "on", "columns"},
		OpSort:    {"order"},
		OpLimit:   {"count"},
		OpSelect:  {"columns"},
	}[st.Op]
	return refuseRest(string(st.Op)+" step", set, allowed)
}

// refuseRest names the first field set that allowed does not hold, in a
// fixed order.
func refuseRest(what string, set map[string]bool, allowed []string) string {
	for _, k := range sortedKeys(set) {
		if set[k] && !slices.Contains(allowed, k) {
			return fmt.Sprintf("a %s takes no `%s`", what, k)
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ---- widgets -------------------------------------------------------------- //

func (c *checker) widgets() {
	s := c.board.Spec
	total := 0
	s.EachWidget(func(Widget, string) bool { total++; return true })
	if total == 0 || total > artifacts.MaxDashboardWidgets {
		c.add("widgets", "holds %d widgets, a group's counted, and a board holds 1 to %d", total,
			artifacts.MaxDashboardWidgets)
	}
	seen := map[string]bool{}
	for i, w := range s.Widgets {
		f := fmt.Sprintf("widgets[%d]", i)
		c.widget(f, w, seen, false)
		for j, child := range w.Children {
			c.widget(fmt.Sprintf("%s.children[%d]", f, j), child, seen, true)
		}
	}
}

// widget holds one widget to its type's fields.
func (c *checker) widget(f string, w Widget, seen map[string]bool, nested bool) {
	switch {
	case !ValidID(w.ID):
		c.add(f+".id", "%q is a lowercase letter, then lowercase letters, digits and `_`, at most 40", w.ID)
	case seen[w.ID]:
		c.add(f+".id", "%q is another widget's id too", w.ID)
	}
	seen[w.ID] = true
	if !w.Type.Valid() {
		c.add(f+".type", "%q is not one of %s", w.Type, words.Or(WidgetTypes))
		return
	}
	if w.W < 1 || w.W > artifacts.DashboardColumns {
		c.add(f+".w", "is %d, and a widget is 1 to %d columns wide", w.W, artifacts.DashboardColumns)
	}
	if w.H != nil && (*w.H < artifacts.MinWidgetRows || *w.H > artifacts.MaxWidgetRows) {
		c.add(f+".h", "is %d, and a widget is %d to %d rows tall, or absent for its content's height",
			*w.H, artifacts.MinWidgetRows, artifacts.MaxWidgetRows)
	}
	c.text(f+".title", w.Title, w.Type != WidgetNote, int(artifacts.MaxTitleBytes))
	c.text(f+".unit", w.Unit, false, artifacts.MaxUnitBytes)
	if w.Format != "" && !w.Format.Valid() {
		c.add(f+".format", "%q is not one of %s", w.Format, words.Or(Formats))
	}
	if why := widgetFields(w); why != "" {
		c.add(f, "%s", why)
		return
	}
	switch w.Type {
	case WidgetGroup:
		if nested {
			c.add(f+".type", "a group holds widgets, never another group")
		}
		if len(w.Children) == 0 {
			c.add(f+".children", "a group holds at least one widget")
		}
		if w.Tone != "" && !slices.Contains(Tones, w.Tone) {
			c.add(f+".tone", "%q is not one of %s", w.Tone, words.Or(Tones))
		}
		return
	case WidgetNote:
		c.text(f+".text", w.Text, true, artifacts.MaxNoteBytes)
		return
	}
	shape, ok := c.board.Shapes[w.Dataset]
	if !ok {
		if _, exists := c.board.Spec.Dataset(w.Dataset); !exists {
			c.add(f+".dataset", "%q is not a dataset of the board", w.Dataset)
		}
		return
	}
	need := func(field, key string, types ...ColumnType) {
		col, ok := shape.column(key)
		switch {
		case key == "":
			c.add(f+"."+field, "is required for a %s", w.Type)
		case !ok:
			c.add(f+"."+field, "%q is not a column of %q", key, w.Dataset)
		case !slices.Contains(types, col.Type):
			c.add(f+"."+field, "%q is a %s column, and a %s's %s is %s", key, col.Type, w.Type, field,
				words.Or(types))
		}
	}
	bound := func(field string, n, lo, hi int) {
		if n != 0 && (n < lo || n > hi) {
			c.add(f+"."+field, "is %d, and a %s takes %d to %d", n, w.Type, lo, hi)
		}
	}
	switch w.Type {
	case WidgetStat, WidgetMeter:
		agg := w.EffectiveAgg()
		if !agg.Valid() {
			c.add(f+".agg", "%q is not one of %s", w.Agg, words.Or(Aggs))
		} else if agg != AggCount || w.Value != "" {
			if agg == AggCount {
				if _, ok := shape.column(w.Value); !ok {
					c.add(f+".value", "%q is not a column of %q", w.Value, w.Dataset)
				}
			} else {
				need("value", w.Value, ColumnNumber)
			}
		}
		if w.Type == WidgetMeter {
			switch {
			case (w.Max == nil) == (w.MaxField == ""):
				c.add(f, "a meter names its maximum as exactly one of `max` or `max_field`")
			case w.Max != nil && *w.Max <= 0:
				c.add(f+".max", "is a positive number")
			case w.MaxField != "":
				need("max_field", w.MaxField, ColumnNumber)
			}
		}
	case WidgetTimeSeries, WidgetColumns, WidgetActivity:
		need("x", w.X, ColumnDate)
		lo, hi := 1, artifacts.MaxChartSeries
		if w.Type == WidgetActivity {
			hi = 1
		}
		if len(w.Y) < lo || len(w.Y) > hi {
			c.add(f+".y", "names %d columns, and a %s draws %d to %d", len(w.Y), w.Type, lo, hi)
		}
		for k, y := range w.Y {
			need(fmt.Sprintf("y[%d]", k), y, ColumnNumber)
		}
		if w.By != "" {
			if w.Type == WidgetActivity || len(w.Y) != 1 {
				c.add(f+".by", "splits one `y` into a series per value, on a timeseries or columns")
			} else {
				need("by", w.By, ColumnText)
			}
		}
	case WidgetBars, WidgetShare:
		need("label", w.Label, ColumnText)
		need("value", w.Value, ColumnNumber)
		hi := artifacts.MaxBarRows
		if w.Type == WidgetShare {
			hi = artifacts.MaxShareParts
		}
		bound("limit", w.Limit, 1, hi)
	case WidgetTable:
		if len(w.Columns) == 0 || len(w.Columns) > artifacts.MaxTableColumns {
			c.add(f+".columns", "names %d columns, and a table draws 1 to %d", len(w.Columns),
				artifacts.MaxTableColumns)
		}
		for k, col := range w.Columns {
			if _, ok := shape.column(col); !ok {
				c.add(fmt.Sprintf("%s.columns[%d]", f, k), "%q is not a column of %q", col, w.Dataset)
			}
		}
		bound("limit", w.Limit, 1, artifacts.MaxTableRows)
	}
}

// widgetFields refuses a field a widget's type does not read.
func widgetFields(w Widget) string {
	set := map[string]bool{
		"dataset": w.Dataset != "", "value": w.Value != "", "agg": w.Agg != "",
		"format": w.Format != "", "unit": w.Unit != "", "max": w.Max != nil, "max_field": w.MaxField != "",
		"x": w.X != "", "y": len(w.Y) > 0, "by": w.By != "", "label": w.Label != "",
		"limit": w.Limit != 0, "columns": len(w.Columns) > 0, "text": w.Text != "",
		"tone": w.Tone != "", "children": len(w.Children) > 0,
	}
	chart := []string{"dataset", "x", "y", "by", "format", "unit"}
	allowed := map[WidgetType][]string{
		WidgetStat:       {"dataset", "value", "agg", "format", "unit"},
		WidgetMeter:      {"dataset", "value", "agg", "format", "unit", "max", "max_field"},
		WidgetTimeSeries: chart,
		WidgetColumns:    chart,
		WidgetActivity:   {"dataset", "x", "y", "format", "unit"},
		WidgetBars:       {"dataset", "label", "value", "limit", "format", "unit"},
		WidgetShare:      {"dataset", "label", "value", "limit", "format", "unit"},
		WidgetTable:      {"dataset", "columns", "limit"},
		WidgetNote:       {"text"},
		WidgetGroup:      {"tone", "children"},
	}[w.Type]
	if why := refuseRest(string(w.Type)+" widget", set, allowed); why != "" {
		return why
	}
	if w.Type != WidgetNote && w.Type != WidgetGroup && w.Dataset == "" {
		return fmt.Sprintf("a %s widget names the dataset it draws in `dataset`", w.Type)
	}
	return ""
}

// EffectiveAgg is the fold a stat or a meter takes: its own, else the last
// row's value, which is what a dataset of one row (a calculated total) means.
func (w Widget) EffectiveAgg() Agg {
	if w.Agg == "" {
		return AggLast
	}
	return w.Agg
}

// EffectiveLimit is the rows a bars, share or table widget draws: its own, else the
// type's default.
func (w Widget) EffectiveLimit() int {
	switch {
	case w.Limit > 0:
		return w.Limit
	case w.Type == WidgetShare:
		return artifacts.MaxShareParts
	case w.Type == WidgetTable:
		return artifacts.DefaultTableRows
	}
	return artifacts.DefaultBarRows
}
