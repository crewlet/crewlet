package query

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
)

// stamped is every field of code.GitAnswer that Read stamps on an answer —
// a page from the memo and one cut alike — and that no page holds; partOf is
// the field each kind answers in, an interdiff in a diff's; and unkept is
// every part no page holds, since its kind is never memoized (memoizable).
var (
	stamped = []string{"RepoID", "Position", "Memo", "PushedBy", "Coverage"}
	partOf  = map[code.GitReadKind]string{code.GitTree: "Tree", code.GitBlob: "Blob", code.GitDiff: "Diff",
		code.GitCompare: "Compare", code.GitLandedIn: "LandedIn", code.GitRevisionFacts: "RevisionFacts",
		code.GitReanchor: "Reanchor", code.GitInterdiff: "Diff", code.GitLog: "Log", code.GitBlame: "Blame",
		code.GitChecksFile: "ChecksFile", code.GitSearch: "Search"}
	unkept = []string{"LandedIn", "RevisionFacts", "Reanchor", "ChecksFile"}
)

// memoized is every kind memoizable answers from the memo.
func memoized() []code.GitReadKind {
	var out []code.GitReadKind
	for _, k := range code.GitReadKinds() {
		if memoizable(code.GitQuery{Kind: k}) {
			out = append(out, k)
		}
	}
	return out
}

// EVERY KIND'S PART IS KEPT EXACTLY WHEN ITS KIND IS MEMOIZED: the field list
// the two cases below read a page's shape by is held to memoizable, both
// ways, so a kind made memoizable has its part encoded and fingerprinted, and
// one made not leaves the shape it no longer has.
func TestThePartsAPageHoldsAreTheMemoizedKinds(t *testing.T) {
	t.Parallel()
	kept := map[string]bool{}
	for _, k := range memoized() {
		kept[partOf[k]] = true
	}
	for _, k := range code.GitReadKinds() {
		part, ok := partOf[k]
		switch {
		case !ok:
			t.Errorf("kind %s answers in no part partOf names", k)
		case kept[part] == slices.Contains(unkept, part):
			t.Errorf("part %s of kind %s is kept %t, and unkept lists it %t: one of the two is wrong", part, k,
				kept[part], slices.Contains(unkept, part))
		}
	}
}

// filled is v with every field set, recursively, to a value of its own: a
// string its path, a number counted up, a flag set, a slice of two, a map of
// two, a pointer to a filled value, an instant counted up to the nanosecond —
// so a field the encoding leaves out reads back as its zero, which no field
// here is.
func filled(v reflect.Value, path string, n *int) {
	if v.Type() == reflect.TypeFor[time.Time]() {
		*n++
		v.Set(reflect.ValueOf(time.Unix(int64(*n), int64(*n)).UTC()))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(path)
	case reflect.Int, reflect.Int64:
		*n++
		v.SetInt(int64(*n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := range 2 {
			filled(s.Index(i), fmt.Sprintf("%s[%d]", path, i), n)
		}
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		for i := range 2 {
			k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
			filled(k, fmt.Sprintf("%s{%d}", path, i), n)
			filled(e, fmt.Sprintf("%s[%d]", path, i), n)
			m.SetMapIndex(k, e)
		}
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		filled(p.Elem(), path, n)
		v.Set(p)
	case reflect.Struct:
		for i := range v.NumField() {
			filled(v.Field(i), path+"."+v.Type().Field(i).Name, n)
		}
	default:
		panic(fmt.Sprintf("filled: %s is a %s, which no page holds: teach the encoding it", path, v.Kind()))
	}
}

// tailOf is what a run would append to a's page: everything of it the run
// learns only at its end.
func tailOf(a code.GitAnswer) memoTail {
	switch a.Kind {
	case code.GitTree:
		return memoTail{total: a.Tree.Total}
	case code.GitBlob:
		return memoTail{lines: a.Blob.Lines}
	case code.GitDiff, code.GitInterdiff:
		t := memoTail{total: a.Diff.Total, added: a.Diff.Added, deleted: a.Diff.Deleted}
		for _, f := range a.Diff.Files {
			t.files = append(t.files, fileCounts{added: f.Added, deleted: f.Deleted, binary: f.Binary})
		}
		return t
	}
	return memoTail{}
}

// A MEMO ENTRY HOLDS EVERY FIELD OF ITS ANSWER. A page of every kind the memo
// keeps — every field of its answer set, but those Read stamps — is written as
// a run writes it (appendPage, then its tail) and read back whole: so a field
// added to an answer and left out of the encoding reads back as nothing,
// here, rather than vanishing from every page a node answers from its memo.
//
// Mutation: leave a field out of appendPage (a tree entry's PathB64) — its
// page reads back without it.
func TestAMemoEntryHoldsEveryFieldOfItsAnswer(t *testing.T) {
	t.Parallel()
	for _, kind := range memoized() {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			a := code.GitAnswer{Kind: kind}
			v := reflect.ValueOf(&a).Elem()
			n := 0
			for i := range v.NumField() {
				f := v.Type().Field(i)
				switch {
				case f.Name == "Kind":
				case slices.Contains(stamped, f.Name), slices.Contains(unkept, f.Name):
				case f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct:
					if f.Name == partOf[kind] {
						filled(v.Field(i), f.Name, &n)
					}
				default:
					filled(v.Field(i), f.Name, &n)
				}
			}
			p := memoPage{answer: a, commits: []string{"one commit", "another"}, budget: 4242}
			body := appendTail(appendPage(nil, p), kind, tailOf(a))
			got, err := decodePage(body)
			if err != nil {
				t.Fatalf("decodePage: %v", err)
			}
			if !reflect.DeepEqual(got, p) {
				t.Errorf("the page read back =\n%+v\nwant every field written\n%+v", got, p)
			}
			// A stat writes a page's counts as it counts them, and its tail the
			// totals alone: the same bytes.
			if partOf[kind] == "Diff" {
				tail := tailOf(a)
				counted := appendTail(appendCounts(appendPage(nil, p), tail.files), kind,
					memoTail{total: tail.total, added: tail.added, deleted: tail.deleted, counted: true})
				if string(counted) != string(body) {
					t.Error("a page whose counts were written as they were counted is not the page its tail ends")
				}
			}
			if _, err := decodePage(body[:len(body)-1]); err == nil {
				t.Error("decodePage read a page cut short")
			}
			if _, err := decodePage(append(body, 0)); err == nil {
				t.Error("decodePage read a page with a byte past its end")
			}
		})
	}
}

// shapeOf is the shape of the answer types a page holds — every field's name
// and type, recursively — as a fingerprint: what memoFormat names.
func shapeOf() string {
	var b strings.Builder
	seen := map[reflect.Type]bool{}
	var walk func(ty reflect.Type)
	walk = func(ty reflect.Type) {
		// A MAP'S VALUES are what it holds, as a slice's elements are; its
		// keys are strings. An instant is one value, whatever the standard
		// library keeps inside it.
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] || ty == reflect.TypeFor[time.Time]() {
			return
		}
		seen[ty] = true
		fmt.Fprintf(&b, "%s{", ty)
		for i := range ty.NumField() {
			f := ty.Field(i)
			if ty == reflect.TypeFor[code.GitAnswer]() && (slices.Contains(stamped, f.Name) || slices.Contains(unkept, f.Name)) {
				continue
			}
			fmt.Fprintf(&b, "%s %s;", f.Name, f.Type)
		}
		b.WriteString("}")
		for i := range ty.NumField() {
			f := ty.Field(i)
			if ty == reflect.TypeFor[code.GitAnswer]() && (slices.Contains(stamped, f.Name) || slices.Contains(unkept, f.Name)) {
				continue
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeFor[code.GitAnswer]())
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// memoShapes is the shape of the answers each memoFormat this build reads was
// written over: a format names one shape, for ever.
var memoShapes = map[string]string{
	"page/4": "4a3450124092111d",
}

// AN ANSWER'S SHAPE IS ITS MEMO FORMAT. An entry a node keeps outlives the
// build that wrote it, so an answer type reshaped — a field added, renamed
// or retyped — under the same memoFormat would have every entry an older
// build wrote read as a page of the new shape. memoFormat names the shape it
// was written over, here, so a change to an answer's fields fails until the
// format moves with it — and the encoding with it (the case above).
func TestAnAnswersShapeIsItsMemoFormat(t *testing.T) {
	t.Parallel()
	if got, want := shapeOf(), memoShapes[memoFormat]; got != want {
		t.Errorf("the answers a page holds are of shape %s, and memoFormat %q was written over %q: give the "+
			"encoding the change, move memoFormat, and name the new format's shape here", got, memoFormat, want)
	}
}

// fakeCache answers a recall from a memo of one entry, and nothing else.
type fakeCache struct {
	body []byte
}

func (fakeCache) Holds(string) bool { return true }
func (fakeCache) Sync(context.Context, string, statelog.Freshness) (cache.Synced, error) {
	return cache.Synced{}, nil
}
func (fakeCache) ServeConfig() []gitproc.KV                         { return nil }
func (fakeCache) Provenance(cache.Synced) (cache.Provenance, error) { return cache.Provenance{}, nil }
func (fakeCache) WantHistory(string)                                {}
func (f fakeCache) MemoGet(cache.Synced, cache.MemoKey) ([]byte, bool) {
	return f.body, f.body != nil
}
func (fakeCache) Memo(cache.Synced) *cache.MemoBatch                  { return nil }
func (fakeCache) Spool() (*os.File, error)                            { return nil, os.ErrInvalid }
func (fakeCache) IndexTip(cache.Synced) (cache.IndexedTip, bool)      { return cache.IndexedTip{}, false }
func (fakeCache) CodeIndex(cache.Synced) (*cache.CodeIndexView, bool) { return nil, false }

// A PAGE FROM THE MEMO FITS WHATEVER IT IS STAMPED WITH. A page carries its
// budget — the most the fields Read stamps may encode to and the page still
// fit — and a read whose stamped fields would take more is answered by a cut
// of its own rather than by the page: here a copy whose position, stamped on
// every answer, encodes past what the page left.
//
// Mutation: answer a hit whatever its budget (recall's budget check) — the
// page is answered past its bound.
func TestAPageFromTheMemoFitsWhateverItIsStamped(t *testing.T) {
	t.Parallel()
	q := code.GitQuery{Kind: code.GitTree, RepoID: "0e8bd7a4-5a04-4e17-8f3c-7e7d4b2c9a11",
		Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo"}, Tree: &code.TreeQuery{
			Rev: strings.Repeat("a", 40)}}
	near := statelog.Position{Stream: "s", Generation: 1, Seq: 7}
	far := statelog.Position{Stream: strings.Repeat("a stream named at length ", 4), Generation: 1, Seq: 7}
	page := code.GitAnswer{Kind: code.GitTree, Rev: q.Tree.Rev, Tree: &code.TreeAnswer{Tree: q.Tree.Rev,
		Entries: []code.TreeEntry{}}}

	cut := &read{s: &Server{pageBytes: code.MaxGitAnswerBytes}, q: q, synced: cache.Synced{Position: near}}
	budget := cut.stampCost() + 10 // ten bytes of the room left
	body := appendTail(appendPage(nil, memoPage{answer: page, budget: budget}), code.GitTree, memoTail{total: 3})
	for _, tc := range []struct {
		at   statelog.Position
		fits bool
	}{{near, true}, {far, false}} {
		r := &read{s: &Server{o: Options{Cache: fakeCache{body: body}}, pageBytes: code.MaxGitAnswerBytes}, q: q,
			synced: cache.Synced{Position: tc.at}}
		a, ok := r.recall(map[string]string{q.Tree.Rev: q.Tree.Rev}, cursor{})
		if ok != tc.fits {
			t.Errorf("a page left %d bytes for stamps costing %d was answered %t, want %t", budget, r.stampCost(),
				ok, tc.fits)
		}
		if ok && (!a.Memo || a.Tree.Total != 3) {
			t.Errorf("the page from the memo = %+v, want it marked and its tail read", a)
		}
	}
}

// A STAT'S RUN HOLDS ONE PAGE'S COUNTS AT A TIME. A stat cuts every page past
// the asked one before git counts a line, and its counts follow every file:
// filed under each staged page and held to the run's end, they were some 24
// bytes for every file of the stat past the asked page, under no bound. So
// each page's counts are handed on — into its staged entry — as soon as the
// numstat half has counted past it: here fifty pages of twenty files, each
// handed once, in order, its own twenty counts, with never more than one
// page's held; and done hands on what is left, a page the half wrote no count
// for among it.
//
// Mutation: hand nothing on until done (add filing every count) — a thousand
// counts are held at once.
func TestAStatsRunHoldsOnePagesCountsAtATime(t *testing.T) {
	t.Parallel()
	const pages, files = 50, 20
	type handed struct {
		page   int
		counts []fileCounts
	}
	var got []handed
	pc := pageCounts{hand: func(page int, counts []fileCounts) {
		got = append(got, handed{page: page, counts: slices.Clone(counts)})
	}}
	for p := range pages + 1 {
		limit := maxInt
		if p < pages {
			limit = (p + 1) * files
		}
		pc.page(p*files, limit)
	}
	for i := range pages * files {
		pc.add(i, fileCounts{added: i, deleted: 2 * i})
	}
	pc.done()
	if pc.most > files {
		t.Errorf("the stat held %d counts at once, past the %d of the page it counted", pc.most, files)
	}
	if len(got) != pages+1 {
		t.Fatalf("%d pages were handed on, want each of the %d once", len(got), pages+1)
	}
	for p, h := range got {
		want := files
		if p == pages {
			want = 0 // the last page staged, which the half wrote no count for
		}
		if h.page != p || len(h.counts) != want {
			t.Fatalf("hand %d was page %d with %d counts, want page %d with %d", p, h.page, len(h.counts), p, want)
		}
		for j, c := range h.counts {
			if i := p*files + j; c.added != i || c.deleted != 2*i {
				t.Fatalf("page %d's count %d is %+v, want file %d's", p, j, c, i)
			}
		}
	}
}
