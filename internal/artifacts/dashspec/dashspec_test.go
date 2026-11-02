package dashspec

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// dirVersion is a version read from a directory of testdata.
type dirVersion struct {
	root  string
	files map[string]string // path to essence
	over  map[string][]byte // a path's bytes replaced, for a case
}

func boardVersion(t *testing.T) *dirVersion {
	t.Helper()
	return &dirVersion{root: filepath.Join("testdata", "board"), files: map[string]string{
		EntryPath: EssenceJSON, "data/targets.csv": EssenceCSV,
	}, over: map[string][]byte{}}
}

func (v *dirVersion) Paths() []string {
	var out []string
	for p := range v.files {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func (v *dirVersion) Essence(p string) (string, bool) {
	e, ok := v.files[p]
	return e, ok
}

func (v *dirVersion) Read(p string) ([]byte, error) {
	if b, ok := v.over[p]; ok {
		return b, nil
	}
	return os.ReadFile(filepath.Join(v.root, filepath.FromSlash(p)))
}

// edit rewrites the board's spec through a JSON tree, for one case.
func (v *dirVersion) edit(t *testing.T, change func(spec map[string]any)) {
	t.Helper()
	raw, err := v.Read(EntryPath)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	change(spec)
	out, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	v.over[EntryPath] = out
}

func at(spec map[string]any, path ...any) map[string]any {
	var cur any = spec
	for _, p := range path {
		switch k := p.(type) {
		case string:
			cur = cur.(map[string]any)[k]
		case int:
			cur = cur.([]any)[k]
		}
	}
	return cur.(map[string]any)
}

func readBoard(t *testing.T, v Version) *Board {
	t.Helper()
	b, err := Read(v)
	if err != nil {
		t.Fatalf("the board should read: %v", err)
	}
	return b
}

func refusal(t *testing.T, v Version) string {
	t.Helper()
	_, err := Read(v)
	var invalid *Invalid
	if !errors.As(err, &invalid) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("the board should be refused as invalid, got %v", err)
	}
	return invalid.Error()
}

func TestTheExampleBoardReadsWithEveryDatasetShaped(t *testing.T) {
	b := readBoard(t, boardVersion(t))
	for _, d := range b.Spec.Datasets {
		if _, ok := b.Shapes[d.ID]; !ok {
			t.Errorf("dataset %q has no shape", d.ID)
		}
	}
	want := []string{"region", "amount", "deals", "target", "attainment"}
	var got []string
	for _, c := range b.Shapes["by_region"].Columns {
		got = append(got, c.Key)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the calculation's columns are %v, want %v", got, want)
	}
	if b.Shapes["by_region"].Key != "region" {
		t.Errorf("a group by one column keys its rows by it, got %q", b.Shapes["by_region"].Key)
	}
	if b.Shapes["deals"].Key != StoreIDColumn || b.Shapes["deals"].Columns[0].Key != StoreIDColumn {
		t.Errorf("a store dataset carries and is keyed by the document id: %+v", b.Shapes["deals"])
	}
	if rows := b.Files["targets"].Rows; len(rows) != 2 || rows[1]["target"] != 250000.0 ||
		rows[0]["owner"] != nil {
		t.Errorf("the file dataset reads its declared columns, coerced, and nothing else: %v", rows)
	}
}

func TestABoardOutsideTheFormatIsRefusedNamingTheField(t *testing.T) {
	cases := []struct {
		name   string
		change func(spec map[string]any)
		want   string
	}{
		{"an unknown field", func(s map[string]any) { s["widget"] = []any{} }, "widget is not a key the dashboard format carries: the top level of dashboard.json takes v, title, params, datasets, widgets"},
		{"another format version", func(s map[string]any) { s["v"] = 2 }, "v: is 2"},
		{"a widget over no dataset", func(s map[string]any) { at(s, "widgets", 0)["dataset"] = "nope" },
			`widgets[0].dataset: "nope" is not a dataset`},
		{"a stat over a text column", func(s map[string]any) {
			at(s, "widgets", 0)["dataset"] = "deals"
			at(s, "widgets", 0)["value"] = "stage"
		}, `"stage" is a text column`},
		{"a field the widget type does not read", func(s map[string]any) {
			at(s, "widgets", 0)["x"] = "closed"
		}, "a stat widget takes no `x`"},
		{"an engine source the catalogue lacks", func(s map[string]any) {
			at(s, "datasets", 0, "engine")["name"] = "tracker.everything"
		}, `"tracker.everything" is not a source`},
		{"an engine parameter out of range", func(s map[string]any) {
			at(s, "datasets", 0, "engine", "params")["limit"] = 5000
		}, "datasets[0].engine.params.limit: is at most 500"},
		{"a filter bound to choices the source refuses", func(s map[string]any) {
			at(s, "params", 0)["options"] = []any{map[string]any{"value": "mine", "label": "Mine"},
				map[string]any{"value": "theirs", "label": "Theirs"}}
		}, `whose choice "theirs" is not one tracker.items.scope takes`},
		{"an interval under a minute", func(s map[string]any) {
			at(s, "datasets", 0, "refresh")["every_seconds"] = 10
		}, "an interval is 60 to 86400 seconds"},
		{"an engine dataset that declares columns", func(s map[string]any) {
			at(s, "datasets", 1)["columns"] = []any{map[string]any{"key": "x", "type": "text"}}
		}, "is the catalogue's for an engine dataset"},
		{"a calculation that reads itself", func(s map[string]any) {
			at(s, "datasets", 4, "calc")["from"] = "total"
		}, "reads itself"},
		{"a step over a column the rows lack", func(s map[string]any) {
			at(s, "datasets", 4, "calc", "steps", 3)["right"] = "quota"
		}, `right "quota" is not one`},
		{"a filter default that is not a choice", func(s map[string]any) { at(s, "params", 1)["default"] = "apac" },
			`params[1].default: "apac" is not one`},
		{"a store collection the datastore reserves", func(s map[string]any) {
			at(s, "datasets", 2, "store")["collection"] = "schema"
		}, "is not a collection a store holds"},
		{"two widgets under one id", func(s map[string]any) { at(s, "widgets", 1)["id"] = "total_stat" },
			`"total_stat" is another widget's id`},
		{"a group inside a group", func(s map[string]any) {
			at(s, "widgets", 3, "children", 1)["type"] = "group"
			delete(at(s, "widgets", 3, "children", 1), "text")
			at(s, "widgets", 3, "children", 1)["children"] = []any{map[string]any{"id": "deep", "type": "note", "text": "x", "w": 3}}
		}, "never another group"},
		{"a widget wider than the grid", func(s map[string]any) { at(s, "widgets", 0)["w"] = 13 },
			"a widget is 1 to 12 columns wide"},
		{"a file dataset over a missing file", func(s map[string]any) {
			at(s, "datasets", 3, "file")["path"] = "data/missing.csv"
		}, `"data/missing.csv" is not a file of this version`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := boardVersion(t)
			v.edit(t, tc.change)
			if got := refusal(t, v); !strings.Contains(got, tc.want) {
				t.Errorf("the refusal %q does not say %q", got, tc.want)
			}
		})
	}
}

func TestEveryFindingIsReportedInOnePass(t *testing.T) {
	v := boardVersion(t)
	v.edit(t, func(s map[string]any) {
		at(s, "widgets", 0)["w"] = 0
		at(s, "widgets", 1)["dataset"] = "nope"
		at(s, "params", 1)["default"] = "apac"
	})
	_, err := Read(v)
	var invalid *Invalid
	if !errors.As(err, &invalid) || len(invalid.Findings) != 3 {
		t.Fatalf("want three findings, got %v", err)
	}
}

func TestAFileDatasetIsHeldToItsColumnsNamingTheLine(t *testing.T) {
	v := boardVersion(t)
	v.over["data/targets.csv"] = []byte("region,target\nemea,100000\namer,lots\n")
	if got := refusal(t, v); !strings.Contains(got, "data/targets.csv:3") || !strings.Contains(got, `"lots" is not a number`) {
		t.Errorf("the refusal should name the file, the line and the value: %s", got)
	}
	v.over["data/targets.csv"] = []byte("region,target\nemea,1\nemea,2\n")
	if got := refusal(t, v); !strings.Contains(got, "the key emea is the row's at line 2 too") {
		t.Errorf("a key twice is refused: %s", got)
	}
	v.over["data/targets.csv"] = []byte("place,target\nemea,1\n")
	if got := refusal(t, v); !strings.Contains(got, `has no "region" in its header`) {
		t.Errorf("a missing column is refused: %s", got)
	}
}

func TestAJSONFileDatasetReadsAnArrayOfObjects(t *testing.T) {
	d := Dataset{ID: "t", Source: SourceFile, File: &FileSource{Path: "t.json"},
		Columns: []Column{{Key: "day", Type: ColumnDate}, {Key: "n", Type: ColumnNumber}}}
	tb, err := ParseFile(d, EssenceJSON, []byte(`[{"day":"2026-10-01","n":3},{"day":"2026-10-02T10:00:00+02:00","n":"4"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if tb.Rows[1]["day"] != "2026-10-02T08:00:00Z" || tb.Rows[1]["n"] != 4.0 {
		t.Errorf("values are coerced to their canonical forms: %v", tb.Rows)
	}
	if _, err := ParseFile(d, EssenceJSON, []byte(`[{"day":"2026-10-01","n":{"x":1}}]`)); err == nil ||
		!strings.Contains(err.Error(), "holds an object or an array") {
		t.Errorf("a nested value is refused, got %v", err)
	}
}

func dealsTable() Table {
	return Table{
		Columns: []Column{{Key: StoreIDColumn, Type: ColumnText}, {Key: "name", Type: ColumnText},
			{Key: "stage", Type: ColumnText}, {Key: "amount", Type: ColumnNumber},
			{Key: "region", Type: ColumnText}, {Key: "closed", Type: ColumnDate}},
		Key: StoreIDColumn,
		Rows: []Row{
			{StoreIDColumn: "d1", "region": "emea", "amount": 50000.0, "closed": "2026-10-05"},
			{StoreIDColumn: "d2", "region": "amer", "amount": 100000.0, "closed": "2026-10-07"},
			{StoreIDColumn: "d3", "region": "emea", "amount": 30000.0, "closed": "2026-10-12"},
			{StoreIDColumn: "d4", "region": "amer", "amount": nil},
		},
	}
}

func boardInputs(t *testing.T, b *Board) Inputs {
	return func(id string) (Table, error) {
		switch id {
		case "deals":
			return dealsTable(), nil
		case "targets":
			return b.Files["targets"], nil
		}
		d, _ := b.Spec.Dataset(id)
		return Run(d, map[string]string{"region": "", "scope": "mine"}, boardInputs(t, b))
	}
}

func TestACalculationFiltersGroupsJoinsComputesAndSorts(t *testing.T) {
	b := readBoard(t, boardVersion(t))
	d, _ := b.Spec.Dataset("by_region")
	got, err := Run(d, map[string]string{"region": ""}, boardInputs(t, b))
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{"region": "emea", "amount": 80000.0, "deals": 2.0, "target": 100000.0, "attainment": 0.8},
		{"region": "amer", "amount": 100000.0, "deals": 1.0, "target": 250000.0, "attainment": 0.4},
	}
	if !reflect.DeepEqual(got.Rows, want) {
		t.Errorf("rows\n got %v\nwant %v", got.Rows, want)
	}
	only, err := Run(d, map[string]string{"region": "amer"}, boardInputs(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Rows) != 1 || only.Rows[0]["region"] != "amer" {
		t.Errorf("a condition bound to a filter keeps the filter's rows: %v", only.Rows)
	}
	total, _ := b.Spec.Dataset("total")
	sum, err := Run(total, nil, boardInputs(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Rows) != 1 || sum.Rows[0]["amount"] != 180000.0 {
		t.Errorf("a whole-set group folds to one row: %v", sum.Rows)
	}
}

func TestACalculationIsDeterministic(t *testing.T) {
	b := readBoard(t, boardVersion(t))
	d, _ := b.Spec.Dataset("by_region")
	first, err := Run(d, map[string]string{"region": ""}, boardInputs(t, b))
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		again, _ := Run(d, map[string]string{"region": ""}, boardInputs(t, b))
		if !reflect.DeepEqual(first, again) {
			t.Fatal("one input and one spec answered two tables")
		}
	}
}

func TestARefusedInputIsTheCalculationsRefusal(t *testing.T) {
	b := readBoard(t, boardVersion(t))
	d, _ := b.Spec.Dataset("by_region")
	closed := errors.New("you cannot read the deals")
	_, err := Run(d, nil, func(string) (Table, error) { return Table{}, closed })
	if !errors.Is(err, closed) {
		t.Errorf("an input's failure should be the calculation's, got %v", err)
	}
}

func TestDateStepsCutToTheDayWeekAndMonth(t *testing.T) {
	in := Table{Columns: []Column{{Key: "at", Type: ColumnDate}}, Rows: []Row{{"at": "2026-10-18T23:30:00Z"}}}
	for fn, want := range map[ComputeFn]string{FnDay: "2026-10-18", FnWeek: "2026-10-12", FnMonth: "2026-10-01"} {
		d := Dataset{ID: "c", Source: SourceCalc, Calc: &Calc{From: "in", Steps: []Step{
			{Op: OpCompute, Column: "bucket", Fn: fn, Left: &Operand{Column: "at"}}}}}
		got, err := Run(d, nil, func(string) (Table, error) { return in, nil })
		if err != nil {
			t.Fatal(err)
		}
		if got.Rows[0]["bucket"] != want {
			t.Errorf("%s of a Sunday night is %v, want %s", fn, got.Rows[0]["bucket"], want)
		}
	}
}

func TestDivisionByZeroIsAbsentNotInfinite(t *testing.T) {
	in := Table{Columns: []Column{{Key: "a", Type: ColumnNumber}, {Key: "b", Type: ColumnNumber}},
		Rows: []Row{{"a": 1.0, "b": 0.0}}}
	d := Dataset{ID: "c", Source: SourceCalc, Calc: &Calc{From: "in", Steps: []Step{
		{Op: OpCompute, Column: "r", Fn: FnDiv, Left: &Operand{Column: "a"}, Right: &Operand{Column: "b"}}}}}
	got, err := Run(d, nil, func(string) (Table, error) { return in, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Rows[0]["r"] != nil {
		t.Errorf("a ratio over zero is nil, got %v", got.Rows[0]["r"])
	}
}

func TestFiltersAreAClosedSet(t *testing.T) {
	b := readBoard(t, boardVersion(t))
	got, err := b.Spec.ResolveParams(map[string]string{"region": "emea"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]string{"scope": "mine", "region": "emea"}) {
		t.Errorf("an unsent filter takes its default: %v", got)
	}
	if _, err := b.Spec.ResolveParams(map[string]string{"region": "mars"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a value the filter does not offer is refused, got %v", err)
	}
	if _, err := b.Spec.ResolveParams(map[string]string{"owner": "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a key the board has no filter for is refused, got %v", err)
	}
}

func TestEngineArgumentsCarryLiteralsBindingsAndDefaults(t *testing.T) {
	b := readBoard(t, boardVersion(t))
	d, _ := b.Spec.Dataset("work")
	got, err := EngineArgs(d, map[string]string{"scope": "all", "region": ""})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]string{"scope": "all", "limit": "200"}) {
		t.Errorf("args are %v", got)
	}
}

func TestEachDatasetSaysHowItIsKeptCurrent(t *testing.T) {
	b := readBoard(t, boardVersion(t))
	want := map[string]Freshness{
		"work":      {Mode: RefreshInterval, EverySeconds: 120},
		"spend":     {Mode: RefreshSignal, Signal: SignalTokens},
		"deals":     {Mode: RefreshLive, Collection: "deals"},
		"targets":   {Mode: RefreshFixed},
		"by_region": {Mode: RefreshDerived, Inputs: []string{"deals", "targets"}},
	}
	for id, w := range want {
		d, _ := b.Spec.Dataset(id)
		if got := RefreshOf(d); !reflect.DeepEqual(got, w) {
			t.Errorf("%s refreshes %+v, want %+v", id, got, w)
		}
	}
}

func TestTheOutlineHoldsNoNumber(t *testing.T) {
	b := readBoard(t, boardVersion(t))
	o := b.Spec.Outline()
	if len(o.Stats) != 2 || o.Stats[0].DatasetTitle != "Pipeline total" {
		t.Errorf("the cover's stats are the first stats with their datasets' titles: %+v", o.Stats)
	}
	if len(o.Widgets) != 7 || o.Widgets[4].Group != "spend_group" {
		t.Errorf("the outline lists every widget, a group's children under it: %+v", o.Widgets)
	}
	raw, _ := json.Marshal(o)
	if strings.Contains(string(raw), "100000") {
		t.Error("the outline carries a value, which is the viewer's alone")
	}
	if text := b.Spec.Text(); !strings.Contains(text, "Spend is the last seven company days.") ||
		strings.Contains(text, "250000") {
		t.Errorf("the search text is the board's words and never a value: %q", text)
	}
}

func TestAnAnchorIsAWidgetOrOneOfItsDataPoints(t *testing.T) {
	b := readBoard(t, boardVersion(t))
	for _, ok := range []Anchor{{Widget: "attain"}, {Widget: "attain", Row: "emea", Field: "attainment"},
		{Widget: "spend_note"}} {
		if err := b.Check(ok); err != nil {
			t.Errorf("%+v should anchor: %v", ok, err)
		}
	}
	for _, bad := range []Anchor{{Widget: "nope"}, {Widget: "attain", Field: "quota"},
		{Widget: "spend_note", Row: "x"}} {
		if err := b.Check(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v should be refused, got %v", bad, err)
		}
	}
	if (Anchor{Widget: "attain"}).Region() != "widget:attain" {
		t.Error("an anchor's presence region is widget:<id>")
	}
}

func TestTheCatalogueIsACopy(t *testing.T) {
	c := Catalogue()
	c[0].Params[0].Enum[0] = "changed"
	if src, _ := Source(c[0].Name); src.Params[0].Enum[0] == "changed" {
		t.Error("a caller's copy of the catalogue changed the catalogue")
	}
	for _, s := range Catalogue() {
		if s.Key != "" && !slices.ContainsFunc(s.Columns, func(c Column) bool { return c.Key == s.Key }) {
			t.Errorf("%s is keyed by %q, which it does not answer", s.Name, s.Key)
		}
	}
}

// A SOURCE IS WINDOWED EXACTLY WHEN IT READS A SPAN OF DAYS: the spend
// sources, whose empty answer a figure words as "nothing in this window",
// and never a source that reads what is true now, where that wording would
// promise a window the reader cannot find.
func TestASourceIsWindowedExactlyWhenItReadsASpanOfDays(t *testing.T) {
	want := map[string]bool{"spend.seats": true, "spend.daily": true}
	for _, src := range Catalogue() {
		if src.Windowed != want[src.Name] {
			t.Errorf("%s is windowed %v, want %v", src.Name, src.Windowed, want[src.Name])
		}
		if _, days := src.Param("days"); days != src.Windowed {
			t.Errorf("%s is windowed %v and takes days %v: a span is a parameter of the read",
				src.Name, src.Windowed, days)
		}
	}
}

// A DOMAIN'S IDS ARE DRAWN IN ITS OWN WORDS: the tracker's status, status
// group and priority, and an agent's activity, carry the vocabulary a reader
// is shown them in, while their values stay the ids a filter compares.
func TestTheCatalogueNamesTheVocabularyItsIDsAreDrawnIn(t *testing.T) {
	want := map[string]map[string]Words{
		"tracker.items": {"status": WordsTrackerStatus, "status_group": WordsTrackerStatusGroup,
			"priority": WordsTrackerPriority},
		"agents.status": {"activity": WordsAgentActivity},
	}
	for name, cols := range want {
		src, _ := Source(name)
		for key, words := range cols {
			i := slices.IndexFunc(src.Columns, func(c Column) bool { return c.Key == key })
			if i < 0 || src.Columns[i].Words != words {
				t.Errorf("%s.%s is drawn in %q, want %q", name, key, src.Columns[i].Words, words)
			}
		}
	}
}

func TestADeclaredVocabularyIsHeldToTheClosedSetAndToText(t *testing.T) {
	for change, want := range map[string]string{
		"guessed": `"guessed" is not one of`,
		"number":  "names a text column's vocabulary",
	} {
		v := boardVersion(t)
		v.edit(t, func(s map[string]any) {
			col := at(s, "datasets", 2, "columns", 2)
			if change == "number" {
				col["words"] = "tracker_status"
			} else {
				at(s, "datasets", 2, "columns", 1)["words"] = change
			}
		})
		if got := refusal(t, v); !strings.Contains(got, want) {
			t.Errorf("%s: the refusal %q does not say %q", change, got, want)
		}
	}
	v := boardVersion(t)
	v.edit(t, func(s map[string]any) { at(s, "datasets", 2, "columns", 1)["words"] = "tracker_status" })
	if b := readBoard(t, v); b.Shapes["deals"].Columns[2].Words != WordsTrackerStatus {
		t.Errorf("a store column declared in the tracker's words keeps them: %+v", b.Shapes["deals"].Columns)
	}
}

// A SOURCE READ DIRECTLY takes only the catalogue's parameters, each held to
// its type, and the source's defaults for the rest.
func TestSourceArgsHoldsADirectReadToTheCatalogue(t *testing.T) {
	src, _ := Source("spend.daily")
	got, err := SourceArgs(src, map[string]string{"group": "model"})
	if err != nil || got["group"] != "model" || got["days"] != "14" {
		t.Errorf("args = %v, %v; want the sent group and the default days", got, err)
	}
	for _, sent := range []map[string]string{{"owner": "cto"}, {"group": "everyone"}, {"days": "0"},
		{"days": "1.5"}} {
		if _, err := SourceArgs(src, sent); !errors.Is(err, ErrInvalid) {
			t.Errorf("SourceArgs(%v) = %v, want a refusal", sent, err)
		}
	}
}

// A COUNT OR A SUM OF NO ROWS IS ZERO, because the source answered and a
// total of nothing is a measured nought; rows that hold no value total
// nothing anybody measured, and a fold that picks or averages has nothing to
// pick. The dashboard's own fold is held to the same cases.
func TestAFoldOverNoRowsIsZeroOnlyWhereItCounts(t *testing.T) {
	if got := Fold(AggSum, ColumnNumber, nil, "amount"); got != 0.0 {
		t.Errorf("a sum of no rows is %v, want 0", got)
	}
	if got := Fold(AggCount, ColumnNumber, nil, ""); got != 0.0 {
		t.Errorf("a count of no rows is %v, want 0", got)
	}
	if got := Fold(AggSum, ColumnNumber, []Row{{"amount": nil}}, "amount"); got != nil {
		t.Errorf("a sum of rows holding no value is %v, want none", got)
	}
	for _, agg := range []Agg{AggAvg, AggMin, AggMax, AggFirst, AggLast} {
		if got := Fold(agg, ColumnNumber, nil, "amount"); got != nil {
			t.Errorf("%s of no rows is %v, want none", agg, got)
		}
	}
}
