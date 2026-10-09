package rendezvous_test

import (
	"bytes"
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestNoPackageOrdersByAHashOfItsOwn fails the build when any package but
// this one ORDERS two values derived from a hash — a second rendezvous order,
// or a hash ring, written privately beside the one this package is (ADR-0008).
//
// # Why ordering, and not "hashing a node", is the subject
//
// What the rule forbids is a second placement rule, and no static check can
// see placement: nothing tells a node id from any other string, and a private
// copy calls its candidates whatever it likes. What no rendezvous or
// hash-ring implementation can avoid, whatever its candidates are called, is
// RANKING values a hash produced — comparing two weights, or sorting, maxing
// or searching a collection of them. internal/estate's router did exactly that
// before this package existed (`weight(r.self, a) > weight(r.self, b)`), and
// it was the copy with the bug: plain FNV-1a, which ranked nodes named alike
// by their last character's low bits.
//
// # What it flags
//
// In every .go file under internal/ and cmd/, production and test alike — a
// test that ranks by a hash of its own is a second implementation used as an
// oracle — outside internal/rendezvous:
//
//   - <, >, <= and >= with BOTH operands derived from a hash;
//   - cmp.Compare, cmp.Less, bytes.Compare and strings.Compare, and the min
//     and max builtins, over two such values;
//   - slices.Sort, SortFunc, SortStableFunc, IsSorted, IsSortedFunc, Max,
//     MaxFunc, Min, MinFunc, BinarySearch and BinarySearchFunc, and sort.Ints,
//     Float64s and Strings with their AreSorted and Search forms, Sort,
//     Stable, Slice and SliceStable, over a collection of hash-derived values.
//
// "Derived from a hash" is an integer or a float — a WEIGHT — or the raw bytes
// of a DIGEST, and an ordering of either is flagged alike. It starts at a call
// into a hash package (the table below), at Sum64 or Sum32 on anything, and at
// a constant a hand-rolled hash cannot be written without — FNV's offset bases
// and primes, MurmurHash3's and SplitMix64's multipliers — spelled in the file
// or exported by another package. It is followed, flow-insensitively, through
// a function body and its closures; through a function, method, closure or
// function variable that returns one, across files of a package and across
// packages for an exported function; through a struct field one was stored
// in, by a keyed or a positional literal or an assignment; and through
// arithmetic — division included: a quotient SCALES a hash and keeps its
// order, and weighted rendezvous ranks by -weight / ln(h / 2^64) — indexing,
// slicing, conversions, append, a slice or map literal, encoding/binary's and
// math/bits' integer reads, and package math's functions (a logarithm, a root,
// a float's bits), bar Mod and Remainder.
//
// # What it never flags, and why the tree's own hashes stay out
//
// Taint ENDS where a hash is WRAPPED into a bucket — at % and &, and at
// math.Mod and math.Remainder, whose remainder or low bits start again at zero
// so that a bucket's order is not the hash's — at a comparison, which is a
// bool, and at every other call: an encoded digest is a NAME, compared for a
// hundred reasons none of which is placement. So search.ShardOf (FNV-1a % 64),
// search.IVFSeed (a seed, never compared), notify's phrase pick (% the pool),
// the embeddings fake (% the width), jetstream.PeerIDOf (% the alphabet),
// consumer names, digests encoded into names and then sorted, and object
// digests compared for equality are none of its business. Nor is a one-sided
// threshold (`h.Sum64() < limit`, a sample).
//
// A canonical order of RAW digests is NOT spared, however the comparator
// reaches them — its own parameters, the collection by index, a field of the
// element: bytes.Compare over two SHA-256 sums is the same expression whether
// it sorts a set of them or picks the node a key prefers (rendezvous over
// SHA-256), and a sorted set of sums searched for a key's successor is a hash
// ring. Encode them first, or see "No allowance list".
//
// # Coverage boundary, stated rather than assumed
//
// It does not see a hash reached through a hash.Hash handed in from elsewhere
// and summed with Sum(nil), then read by a call it does not follow; a hash
// library missing from the table (adding one to go.mod is the moment to add it
// here); a dot-imported hash package; a magic constant written with digit
// separators or in mixed-case hex, unless the file is judged for another
// reason; a positional literal whose struct is declared in a file the
// prefilter did not parse; a method or a field of ANOTHER package's type; a
// hash masked with & to its HIGH bits, which keeps a coarse order but is taken
// for a bucket (the coarse order is written with a shift or a division, both
// followed); or a value that travels through an interface, reflection or a
// type parameter. Those are the paths a deliberate evasion would take rather
// than the ones a private copy is written in, and the controls in
// TestTheHashOrderWalkJudgesAKnownTree prove the shapes rendezvous and ring
// implementations do take — the router's own among them — are caught.
//
// # No allowance list
//
// If an ordering by hash is genuinely not a placement — a HyperLogLog
// register, sampling between two hashes, a canonical order of hashes or raw
// digests (a pack index's object ids computed here rather than read from git)
// — change this gate in the same diff and say why there. It keeps no
// allowance list on purpose: an entry is a place a second ring hides behind a
// stale excuse. If the rule ever legitimately goes away, DELETE this guard
// rather than weakening it.
//
// # Cost
//
// It reads every .go file under internal/ and cmd/ once, parses each one's
// imports, and parses whole only the files that import a hash package, spell a
// magic constant, or mention what one of those produces: 77 of some 2,500
// files, in about 0.4 s, 1.9 s under the race detector.
func TestNoPackageOrdersByAHashOfItsOwn(t *testing.T) {
	t.Parallel()
	found := hashOrders(t, sourcetree.Root(t))

	if found.read == 0 || found.parsed == 0 {
		t.Fatalf("read the imports of %d files and judged %d — this guard is certifying "+
			"nothing. Check the module root and the internal/ and cmd/ walks",
			found.read, found.parsed)
	}
	// A guard asserting an ABSENCE passes identically when the thing is
	// absent and when the matcher has gone inert. The one implementation is
	// KNOWN to rank by its own weight, so a walk that cannot see that has
	// stopped seeing anything, and its silence below would mean nothing.
	if found.inRendezvous == 0 {
		t.Fatalf("found no ordering by hash in %s's own source. Either the matcher has gone "+
			"inert — and every verdict below is worthless — or the package stopped ranking "+
			"by its weight, in which case this guard's subject moved and it moves with it",
			rendezvousDir)
	}
	for _, h := range found.hits {
		t.Errorf("%s:%d in %s: %s orders two values derived from a hash.\n"+
			"\tThat is rendezvous or a hash ring whatever the candidates are called, and "+
			"this tree has one: rank them with rendezvous.Order. If this ordering is "+
			"genuinely not a placement (a HyperLogLog register, sampling between two "+
			"hashes), change this gate in the same diff and say why — it keeps no "+
			"allowance list on purpose", h.file, h.line, h.fn, h.construct)
	}
	t.Logf("read the imports of %d files under internal/ and cmd/ and judged %d; %d "+
		"producer(s) of a hash-derived value, %d ordering(s) by one inside %s",
		found.read, found.parsed, found.producers, found.inRendezvous, rendezvousDir)
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: every shape a second rendezvous
// order or hash ring takes in the wild must be reported, every way this tree
// legitimately uses a hash must not be, and what the walk must never read —
// testdata, a nested checkout, a file that is not Go past its imports — is
// there to be read by mistake.
func TestTheHashOrderWalkJudgesAKnownTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	goFiles := 0
	for path, body := range hashOrderFixture {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(path, ".go") {
			goFiles++
		}
	}
	found := hashOrders(t, root)

	got := make([]string, 0, len(found.hits))
	for _, h := range found.hits {
		got = append(got, h.pkg+"."+h.fn+": "+h.construct)
	}
	// A MULTISET: one declaration can order by hash twice, and each is a
	// verdict of its own.
	count := map[string]int{}
	for _, w := range hashOrderFixtureHits {
		count[w]++
	}
	for _, g := range got {
		count[g]--
	}
	for _, verdict := range slices.Sorted(maps.Keys(count)) {
		switch n := count[verdict]; {
		case n > 0:
			t.Errorf("missed %s (%d time(s)) — a shape a second rendezvous order takes "+
				"went unreported", verdict, n)
		case n < 0:
			t.Errorf("reported %s (%d time(s) too many), which orders nothing by hash "+
				"or was never to be read", verdict, -n)
		}
	}
	// The fixture's rendezvous ranks by its own weight once in source and
	// once in a test; only the first is the implementation recognised.
	if found.inRendezvous != 1 {
		t.Errorf("recognised %d ordering(s) by hash inside %s, want the 1 in its source",
			found.inRendezvous, rendezvousDir)
	}
	if want := goFiles - len(hashOrderNeverRead); found.read != want {
		t.Errorf("read the imports of %d files, want %d: every .go file but %v",
			found.read, want, hashOrderNeverRead)
	}
}

// rendezvousDir is the one package allowed to order by a hash, relative to the
// module root: both its packages, because its own tests pin the arithmetic
// against an independent reference.
const rendezvousDir = "internal/rendezvous"

// modulePath is this module's import path: an exported producer of one
// package is recognised in another by the import naming it.
const modulePath = "github.com/crewlet/crewlet"

// hashPackages are the import paths whose functions compute a hash: the
// standard library's, and every hash library already in this module's graph
// (go.mod). Adding a hash library to go.mod is the moment to add it here.
var hashPackages = map[string]bool{
	"hash/fnv": true, "hash/crc32": true, "hash/crc64": true, "hash/adler32": true,
	"hash/maphash": true, "crypto/md5": true, "crypto/sha1": true, "crypto/sha256": true,
	"crypto/sha512": true, "crypto/sha3": true, "crypto/hmac": true,
	"github.com/cespare/xxhash/v2": true, "github.com/minio/highwayhash": true,
	"golang.org/x/crypto/blake2b": true, "golang.org/x/crypto/blake2s": true,
	"golang.org/x/crypto/sha3": true,
}

// magicConstants are what a hand-rolled hash cannot be written without, so a
// file spelling one is judged although it imports no hash package: FNV-64's
// offset basis and prime, FNV-32's, MurmurHash3's fmix64 multipliers and
// SplitMix64's.
var magicConstants = []uint64{
	0xcbf29ce484222325, 0x100000001b3,
	0x811c9dc5, 0x01000193,
	0xff51afd7ed558ccd, 0xc4ceb9fe1a85ec53,
	0x9e3779b97f4a7c15, 0xbf58476d1ce4e5b9, 0x94d049bb133111eb,
}

// magicSpellings is every way the prefilter looks for a magic constant in a
// file's bytes: lower-case hex digits, upper-case hex digits and decimal.
// Searched for as they are, never in a lowered copy of the source: lowering
// every file more than doubled the whole walk under the race detector (4.4 s
// against 2.0 s).
var magicSpellings = func() [][]byte {
	var out [][]byte
	for _, v := range magicConstants {
		hex := strconv.FormatUint(v, 16)
		out = append(out, []byte(hex), []byte(strings.ToUpper(hex)),
			[]byte(strconv.FormatUint(v, 10)))
	}
	return out
}()

// collectionSinks order a collection by its elements' own values, so one of
// hash-derived values — weights or digests — is ordered by hash whatever
// comparator it is handed. Package sort's typed helpers are here whole — a
// ring sorted with sort.Ints and searched with SearchInts is the same ring as
// one sorted and searched through slices.
var collectionSinks = map[string]map[string]bool{
	"slices": {"Sort": true, "SortFunc": true, "SortStableFunc": true, "IsSorted": true,
		"IsSortedFunc": true, "Max": true, "MaxFunc": true, "Min": true, "MinFunc": true,
		"BinarySearch": true, "BinarySearchFunc": true},
	"sort": {"Ints": true, "IntsAreSorted": true, "SearchInts": true, "Float64s": true,
		"Float64sAreSorted": true, "SearchFloat64s": true, "Strings": true,
		"StringsAreSorted": true, "SearchStrings": true, "Sort": true, "Stable": true,
		"Slice": true, "SliceStable": true},
}

// orderingSinks rank their two arguments against each other.
var orderingSinks = map[string]map[string]bool{
	"cmp":     {"Compare": true, "Less": true},
	"bytes":   {"Compare": true},
	"strings": {"Compare": true},
}

// numericTypes are the conversions that read a hash-derived value as an
// integer — a digest's byte included.
var numericTypes = map[string]bool{
	"uint64": true, "uint32": true, "uint16": true, "uint8": true, "uint": true,
	"int64": true, "int32": true, "int16": true, "int8": true, "int": true,
	"byte": true, "rune": true, "uintptr": true, "float64": true, "float32": true,
}

// hashOrderWalk is what one walk of a tree found.
type hashOrderWalk struct {
	// read is every .go file whose imports were read; parsed, every one
	// parsed whole and judged.
	read, parsed int
	// producers is every function, method and function variable found to
	// return a hash-derived value.
	producers int
	// inRendezvous counts the orderings by hash in internal/rendezvous's own
	// non-test source: the one implementation, recognised.
	inRendezvous int
	hits         []hashOrderHit
}

// hashOrderHit is one ordering by hash outside internal/rendezvous.
type hashOrderHit struct {
	file      string // relative to the root
	line      int
	pkg       string // the package's directory, relative to the root
	fn        string // the declaration it sits in: a function, Type.method or variable
	construct string // what orders: the operator, the builtin or the call
}

// taint is what a hash contributed to a value.
type taint uint8

const (
	// hashWeight is an integer derived from a hash: what a rendezvous order
	// or a ring ranks by.
	hashWeight taint = 1 << iota
	// hashDigest is bytes derived from a hash, or a hasher only a Sum turns
	// into a value.
	hashDigest
)

// hashTree is one walk's state: every file read, and what the parsed ones
// were found to declare.
type hashTree struct {
	root   string
	fset   *token.FileSet
	files  []*hashFile
	scopes map[scopeKey]*hashScope
	// exportedFuncs and exportedMagic are every exported producer, by the
	// import path its package is imported by.
	exportedFuncs map[string]map[string]taint
	exportedMagic map[string]map[string]bool
	// changed is set by any summary that grew, so summarise runs to a fixed
	// point.
	changed bool
}

// scopeKey is one package as the compiler sees it: a directory AND a package
// clause, so an external test package (x_test) is a scope of its own beside
// the package it tests.
type scopeKey struct{ dir, name string }

// hashScope is what one package's parsed files declare.
type hashScope struct {
	key  scopeKey
	path string // the import path
	// funcs are package-level functions and function variables returning a
	// hash-derived value; methods, the same per receiver type.
	funcs   map[string]taint
	methods map[string]map[string]taint
	// magic names the package-level constants and variables holding a magic
	// constant.
	magic map[string]bool
	// types is every type the parsed files declare, and structs the fields
	// of the struct ones, in order.
	types   map[string]ast.Expr
	structs map[string][]string
	// fields is what a hash contributed to each struct field it was stored
	// in: a weight, a digest, or both.
	fields map[string]map[string]taint
}

// hashFile is one .go file the walk read.
type hashFile struct {
	path    string
	src     []byte
	test    bool
	scope   *hashScope
	imports map[string]string // the name an import is used by -> its path
	// parsed is set once the file has been parsed whole (ast is nil when it
	// failed to parse, which has already failed the test).
	parsed bool
	ast    *ast.File
}

// fnUnit is one body the analysis reads: a function or method declaration, or
// a function literal a package-level variable holds.
type fnUnit struct {
	file     *hashFile
	name     string
	recv     string // the receiver's type, for a method
	typ      *ast.FuncType
	params   []*ast.FieldList
	body     *ast.BlockStmt
	callable bool // the unit is what a call of name runs
}

// label is how a hit names the declaration it sits in.
func (u fnUnit) label() string {
	if u.recv != "" {
		return u.recv + "." + u.name
	}
	return u.name
}

// hashOrders walks root's internal/ and cmd/ trees and judges every ordering
// by hash in them.
//
// THE IMPORTS ARE READ FIRST, for every file, and a file is parsed whole only
// if it imports a hash package or spells a magic constant — then, to a fixed
// point, if it mentions a producer one of those declared: a name of its own
// package, or an exported name of a package it imports. A file that does none
// of that holds no hash-derived value this analysis could see.
func hashOrders(t *testing.T, root string) hashOrderWalk {
	t.Helper()
	tr := &hashTree{root: root, fset: token.NewFileSet(), scopes: map[scopeKey]*hashScope{},
		exportedFuncs: map[string]map[string]taint{}, exportedMagic: map[string]map[string]bool{}}
	for _, sub := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, sub), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// testdata is not compiled.
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") {
				tr.read(t, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	for _, f := range tr.files {
		if f.importsAHash() || spellsAMagicConstant(f.src) {
			tr.parse(t, f)
		}
	}
	for {
		tr.summarise()
		if !tr.grow(t) {
			return tr.judge()
		}
	}
}

// read records one file and its imports.
func (tr *hashTree) read(t *testing.T, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read %s: %v", tr.rel(path), err)
		return
	}
	// The imports are PARSED rather than searched for as text: an import
	// path is a string literal, and a string can be spelled with escapes
	// its bytes never show. ImportsOnly stops at the first declaration that
	// is not an import, a fraction of the file.
	head, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
	if err != nil {
		t.Errorf("parse the imports of %s: %v", tr.rel(path), err)
		return
	}
	dir := tr.rel(filepath.Dir(path))
	key := scopeKey{dir: dir, name: head.Name.Name}
	sc := tr.scopes[key]
	if sc == nil {
		sc = &hashScope{key: key, path: modulePath + "/" + dir, funcs: map[string]taint{},
			methods: map[string]map[string]taint{}, magic: map[string]bool{},
			types: map[string]ast.Expr{}, structs: map[string][]string{},
			fields: map[string]map[string]taint{}}
		tr.scopes[key] = sc
	}
	f := &hashFile{path: path, src: src, test: strings.HasSuffix(path, "_test.go"), scope: sc,
		imports: map[string]string{}}
	for _, imp := range head.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := importName(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		// A blank import names nothing, and a dot import would need
		// identifier resolution this walk does not do (see the coverage
		// boundary).
		if name != "_" && name != "." {
			f.imports[name] = p
		}
	}
	tr.files = append(tr.files, f)
}

// importName is the name an import path is used by when nothing renames it:
// its last element, or the one before a major-version suffix
// (math/rand/v2, xxhash/v2). Every package in this module is named for its
// directory, so the rule holds for the module's own imports too.
func importName(path string) string {
	elems := strings.Split(path, "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && len(last) > 1 && last[0] == 'v' {
		if _, err := strconv.Atoi(last[1:]); err == nil {
			return elems[len(elems)-2]
		}
	}
	return last
}

func (f *hashFile) importsAHash() bool {
	for _, p := range f.imports {
		if hashPackages[p] {
			return true
		}
	}
	return false
}

func spellsAMagicConstant(src []byte) bool {
	for _, m := range magicSpellings {
		if bytes.Contains(src, m) {
			return true
		}
	}
	return false
}

// parse parses one file whole and records what it declares at package level.
func (tr *hashTree) parse(t *testing.T, f *hashFile) {
	t.Helper()
	f.parsed = true
	file, err := parser.ParseFile(tr.fset, f.path, f.src, parser.SkipObjectResolution)
	if err != nil {
		t.Errorf("parse %s: %v", tr.rel(f.path), err)
		return
	}
	f.ast = file
	sc := f.scope
	for _, decl := range file.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range g.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				for i, v := range s.Values {
					if i < len(s.Names) && holdsAMagicConstant(v) {
						tr.markMagic(sc, s.Names[i].Name)
					}
				}
			case *ast.TypeSpec:
				sc.types[s.Name.Name] = s.Type
				if st, ok := s.Type.(*ast.StructType); ok {
					sc.structs[s.Name.Name] = structFields(st)
				}
			}
		}
	}
}

// structFields are a struct's field names in declaration order — an embedded
// field by its type's name — which is what a positional literal assigns.
func structFields(st *ast.StructType) []string {
	var out []string
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			out = append(out, typeName(field.Type))
		}
		for _, n := range field.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

func (tr *hashTree) markMagic(sc *hashScope, name string) {
	sc.magic[name] = true
	if ast.IsExported(name) && sc.importable() {
		if tr.exportedMagic[sc.path] == nil {
			tr.exportedMagic[sc.path] = map[string]bool{}
		}
		tr.exportedMagic[sc.path][name] = true
	}
}

// importable reports whether another package can import this scope: an
// external test package and a command cannot be.
func (sc *hashScope) importable() bool {
	return !strings.HasSuffix(sc.key.name, "_test") && sc.key.name != "main"
}

func holdsAMagicConstant(e ast.Node) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && isMagic(lit) {
			found = true
		}
		return !found
	})
	return found
}

func isMagic(lit *ast.BasicLit) bool {
	if lit.Kind != token.INT {
		return false
	}
	v, err := strconv.ParseUint(strings.ReplaceAll(lit.Value, "_", ""), 0, 64)
	return err == nil && slices.Contains(magicConstants, v)
}

// units are every body one file declares.
func units(f *hashFile) []fnUnit {
	var out []fnUnit
	for _, decl := range f.ast.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			u := fnUnit{file: f, name: d.Name.Name, typ: d.Type, body: d.Body,
				params: []*ast.FieldList{d.Recv, d.Type.Params, d.Type.Results}, callable: true}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				u.recv = typeName(d.Recv.List[0].Type)
			}
			out = append(out, u)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, v := range vs.Values {
					name := "_"
					if i < len(vs.Names) {
						name = vs.Names[i].Name
					}
					ast.Inspect(v, func(n ast.Node) bool {
						lit, ok := n.(*ast.FuncLit)
						if !ok {
							return true
						}
						out = append(out, fnUnit{file: f, name: name, typ: lit.Type, body: lit.Body,
							params:   []*ast.FieldList{lit.Type.Params, lit.Type.Results},
							callable: n == ast.Node(v)})
						return false
					})
				}
			}
		}
	}
	return out
}

// summarise finds every function, method and function variable returning a
// hash-derived value and every struct field a weight is stored in, to a fixed
// point across every parsed file: a producer found in one file makes a value
// in another tainted, which can make a third function a producer.
func (tr *hashTree) summarise() {
	for {
		tr.changed = false
		for _, f := range tr.files {
			if f.ast == nil {
				continue
			}
			for _, u := range units(f) {
				fs := tr.analyse(u)
				if k := fs.returns(u.typ, u.body); k != 0 && u.callable {
					tr.produce(f.scope, u, k)
				}
			}
		}
		if !tr.changed {
			return
		}
	}
}

func (tr *hashTree) produce(sc *hashScope, u fnUnit, k taint) {
	if u.recv != "" {
		m := sc.methods[u.recv]
		if m == nil {
			m = map[string]taint{}
			sc.methods[u.recv] = m
		}
		if m[u.name]|k != m[u.name] {
			m[u.name] |= k
			tr.changed = true
		}
		return
	}
	if sc.funcs[u.name]|k == sc.funcs[u.name] {
		return
	}
	sc.funcs[u.name] |= k
	tr.changed = true
	if ast.IsExported(u.name) && sc.importable() {
		if tr.exportedFuncs[sc.path] == nil {
			tr.exportedFuncs[sc.path] = map[string]taint{}
		}
		tr.exportedFuncs[sc.path][u.name] |= k
	}
}

// grow parses every file not yet parsed that mentions a producer it could
// reach — one of its own package's, or an exported one of a package it
// imports — and reports whether it parsed any.
func (tr *hashTree) grow(t *testing.T) bool {
	t.Helper()
	grew := false
	for _, f := range tr.files {
		if f.parsed {
			continue
		}
		if tr.mentionsAProducer(f) {
			tr.parse(t, f)
			grew = true
		}
	}
	return grew
}

func (tr *hashTree) mentionsAProducer(f *hashFile) bool {
	sc := f.scope
	mentions := func(name string) bool { return bytes.Contains(f.src, []byte(name)) }
	for name := range sc.funcs {
		if mentions(name) {
			return true
		}
	}
	for _, methods := range sc.methods {
		for name := range methods {
			if mentions(name) {
				return true
			}
		}
	}
	for name := range sc.magic {
		if mentions(name) {
			return true
		}
	}
	for typ := range sc.fields {
		if mentions(typ) {
			return true
		}
	}
	for _, path := range f.imports {
		for name := range tr.exportedFuncs[path] {
			if mentions(name) {
				return true
			}
		}
		for name := range tr.exportedMagic[path] {
			if mentions(name) {
				return true
			}
		}
	}
	return false
}

// judge finds every ordering by hash in every parsed file.
func (tr *hashTree) judge() hashOrderWalk {
	out := hashOrderWalk{read: len(tr.files)}
	for _, sc := range tr.scopes {
		out.producers += len(sc.funcs)
		for _, methods := range sc.methods {
			out.producers += len(methods)
		}
	}
	for _, f := range tr.files {
		if f.ast == nil {
			continue
		}
		out.parsed++
		exempt := f.scope.key.dir == rendezvousDir
		for _, u := range units(f) {
			fs := tr.analyse(u)
			ast.Inspect(u.body, func(n ast.Node) bool {
				construct := fs.orders(n)
				switch {
				case construct == "":
				case exempt && !f.test:
					out.inRendezvous++
				case exempt:
					// Its own tests rank by the weight against an
					// independent reference, which is the point of them.
				default:
					pos := tr.fset.Position(n.Pos())
					out.hits = append(out.hits, hashOrderHit{file: tr.rel(pos.Filename),
						line: pos.Line, pkg: f.scope.key.dir, fn: u.label(), construct: construct})
				}
				return true
			})
		}
	}
	slices.SortFunc(out.hits, func(a, b hashOrderHit) int {
		return cmp.Or(strings.Compare(a.file, b.file), cmp.Compare(a.line, b.line),
			strings.Compare(a.construct, b.construct))
	})
	return out
}

func (tr *hashTree) rel(path string) string {
	rel, err := filepath.Rel(tr.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// fnScope is one body's taint: FLOW-INSENSITIVE, so a name assigned a
// hash-derived value anywhere in the body holds one everywhere in it,
// closures included — a comparator is a closure over the names of the
// function that sorts with it.
type fnScope struct {
	tr       *hashTree
	file     *hashFile
	names    map[string]taint
	closures map[string]taint // local function values returning a hash-derived value
	types    map[string]ast.Expr
	changed  bool
}

func (tr *hashTree) analyse(u fnUnit) *fnScope {
	fs := &fnScope{tr: tr, file: u.file, names: map[string]taint{}, closures: map[string]taint{},
		types: map[string]ast.Expr{}}
	for _, fl := range u.params {
		fs.declare(fl)
	}
	// What each name's type is, as far as the syntax says: enough to
	// resolve a selector to a struct field and a call to a method.
	ast.Inspect(u.body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			fs.declare(x.Type.Params)
			fs.declare(x.Type.Results)
		case *ast.ValueSpec:
			for i, name := range x.Names {
				switch {
				case x.Type != nil:
					fs.types[name.Name] = x.Type
				case i < len(x.Values) && len(x.Values) == len(x.Names):
					if typ := fs.typeOf(x.Values[i]); typ != nil {
						fs.types[name.Name] = typ
					}
				}
			}
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE && len(x.Lhs) == len(x.Rhs) {
				for i, l := range x.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						if typ := fs.typeOf(x.Rhs[i]); typ != nil {
							fs.types[id.Name] = typ
						}
					}
				}
			}
		case *ast.RangeStmt:
			if id, ok := x.Value.(*ast.Ident); ok {
				if typ := fs.elem(fs.typeOf(x.X)); typ != nil {
					fs.types[id.Name] = typ
				}
			}
		}
		return true
	})
	for {
		fs.changed = false
		ast.Inspect(u.body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				fs.assignStmt(x)
			case *ast.ValueSpec:
				fs.valueSpec(x)
			case *ast.RangeStmt:
				if x.Value != nil {
					fs.assign(x.Value, fs.kind(x.X))
				}
			case *ast.CompositeLit:
				fs.literal(x, x.Type)
			}
			return true
		})
		if !fs.changed {
			return fs
		}
	}
}

func (fs *fnScope) declare(fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, field := range fl.List {
		for _, n := range field.Names {
			fs.types[n.Name] = field.Type
		}
	}
}

func (fs *fnScope) assignStmt(s *ast.AssignStmt) {
	// %= and &= wrap a hash into a bucket, exactly as % and & do; /= scales
	// one, as / does, so it carries the right-hand side's taint like *=.
	if s.Tok == token.REM_ASSIGN || s.Tok == token.AND_ASSIGN {
		return
	}
	if len(s.Lhs) == len(s.Rhs) {
		for i, l := range s.Lhs {
			if lit, ok := s.Rhs[i].(*ast.FuncLit); ok {
				if id, ok := l.(*ast.Ident); ok {
					fs.closure(id.Name, lit)
				}
				continue
			}
			fs.assign(l, fs.kind(s.Rhs[i]))
		}
		return
	}
	if len(s.Rhs) == 1 {
		k := fs.kind(s.Rhs[0])
		for _, l := range s.Lhs {
			fs.assign(l, k)
		}
	}
}

func (fs *fnScope) valueSpec(s *ast.ValueSpec) {
	if len(s.Values) == len(s.Names) {
		for i, v := range s.Values {
			if lit, ok := v.(*ast.FuncLit); ok {
				fs.closure(s.Names[i].Name, lit)
				continue
			}
			fs.assign(s.Names[i], fs.kind(v))
		}
		return
	}
	if len(s.Values) == 1 {
		k := fs.kind(s.Values[0])
		for _, n := range s.Names {
			fs.assign(n, k)
		}
	}
}

func (fs *fnScope) closure(name string, lit *ast.FuncLit) {
	k := fs.returns(lit.Type, lit.Body)
	if fs.closures[name]|k != fs.closures[name] {
		fs.closures[name] |= k
		fs.changed = true
	}
}

// returns is what a body's own return statements yield — a bare one, its
// named results — while its nested function literals return for themselves.
func (fs *fnScope) returns(typ *ast.FuncType, body *ast.BlockStmt) taint {
	var k taint
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			if len(x.Results) == 0 && typ.Results != nil {
				for _, field := range typ.Results.List {
					for _, name := range field.Names {
						k |= fs.names[name.Name]
					}
				}
			}
			for _, r := range x.Results {
				k |= fs.kind(r)
			}
		}
		return true
	})
	return k
}

func (fs *fnScope) assign(lhs ast.Expr, k taint) {
	if k == 0 {
		return
	}
	switch x := lhs.(type) {
	case *ast.Ident:
		if x.Name != "_" && fs.names[x.Name]|k != fs.names[x.Name] {
			fs.names[x.Name] |= k
			fs.changed = true
		}
	case *ast.IndexExpr:
		fs.assign(x.X, k)
	case *ast.StarExpr:
		fs.assign(x.X, k)
	case *ast.ParenExpr:
		fs.assign(x.X, k)
	case *ast.SelectorExpr:
		if typ := typeName(fs.typeOf(x.X)); typ != "" {
			fs.markField(typ, x.Sel.Name, k)
		}
	}
}

func (fs *fnScope) markField(typ, field string, k taint) {
	// Nothing recorded for a field no hash reached: a type in sc.fields is
	// one grow parses every file mentioning.
	if k == 0 {
		return
	}
	sc := fs.file.scope
	if sc.fields[typ] == nil {
		sc.fields[typ] = map[string]taint{}
	}
	if sc.fields[typ][field]|k != sc.fields[typ][field] {
		sc.fields[typ][field] |= k
		fs.changed = true
		fs.tr.changed = true
	}
}

// literal records the struct fields a composite literal of typ stores a
// hash-derived value in — keyed or positional — and descends into a slice or
// map literal whose elements elide their struct type.
func (fs *fnScope) literal(c *ast.CompositeLit, typ ast.Expr) {
	if typ == nil {
		return
	}
	if elem := fs.elem(typ); elem != nil {
		for _, e := range c.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				e = kv.Value
			}
			if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil {
				fs.literal(inner, elem)
			}
		}
		return
	}
	name := typeName(typ)
	if name == "" {
		return
	}
	decl := fs.file.scope.structs[name]
	for i, e := range c.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok {
				fs.markField(name, key.Name, fs.kind(kv.Value))
			}
			continue
		}
		if i < len(decl) {
			fs.markField(name, decl[i], fs.kind(e))
		}
	}
}

// typeName is the package-local type a type expression names, through
// pointers and type arguments.
func typeName(t ast.Expr) string {
	switch x := t.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return typeName(x.X)
	case *ast.ParenExpr:
		return typeName(x.X)
	case *ast.IndexExpr:
		return typeName(x.X)
	case *ast.IndexListExpr:
		return typeName(x.X)
	}
	return ""
}

// elem is the element type of a slice, array or map type — a type this
// package declares as one included — or nil.
func (fs *fnScope) elem(t ast.Expr) ast.Expr {
	switch x := t.(type) {
	case *ast.ArrayType:
		return x.Elt
	case *ast.MapType:
		return x.Value
	case *ast.StarExpr:
		return fs.elem(x.X)
	case *ast.ParenExpr:
		return fs.elem(x.X)
	case *ast.Ident:
		if decl, ok := fs.file.scope.types[x.Name]; ok {
			if _, isStruct := decl.(*ast.StructType); !isStruct {
				return fs.elem(decl)
			}
		}
	}
	return nil
}

// typeOf is an expression's type as far as the syntax says, or nil.
func (fs *fnScope) typeOf(e ast.Expr) ast.Expr {
	switch x := e.(type) {
	case *ast.Ident:
		return fs.types[x.Name]
	case *ast.CompositeLit:
		return x.Type
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return fs.typeOf(x.X)
		}
	case *ast.ParenExpr:
		return fs.typeOf(x.X)
	case *ast.StarExpr:
		return fs.typeOf(x.X)
	case *ast.IndexExpr:
		return fs.elem(fs.typeOf(x.X))
	case *ast.SliceExpr:
		return fs.typeOf(x.X)
	case *ast.SelectorExpr:
		decl, ok := fs.file.scope.types[typeName(fs.typeOf(x.X))].(*ast.StructType)
		if !ok {
			return nil
		}
		for _, field := range decl.Fields.List {
			for _, n := range field.Names {
				if n.Name == x.Sel.Name {
					return field.Type
				}
			}
		}
	case *ast.CallExpr:
		id, ok := x.Fun.(*ast.Ident)
		if !ok || len(x.Args) == 0 {
			return nil
		}
		switch {
		case id.Name == "make" || id.Name == "new":
			return x.Args[0]
		case id.Name == "append":
			return fs.typeOf(x.Args[0])
		case fs.file.scope.types[id.Name] != nil:
			return id // a conversion to a type this package declares
		}
	}
	return nil
}

// isImport reports whether name, in this body, is the name of an import
// rather than a local that shadows it.
func (fs *fnScope) isImport(name string) bool {
	_, imported := fs.file.imports[name]
	return imported && fs.types[name] == nil && fs.names[name] == 0
}

// kind is what a hash contributed to an expression's value.
func (fs *fnScope) kind(e ast.Expr) taint {
	switch x := e.(type) {
	case *ast.Ident:
		k := fs.names[x.Name]
		if fs.file.scope.magic[x.Name] {
			k |= hashWeight
		}
		return k
	case *ast.BasicLit:
		if isMagic(x) {
			return hashWeight
		}
	case *ast.ParenExpr:
		return fs.kind(x.X)
	case *ast.StarExpr:
		return fs.kind(x.X)
	case *ast.UnaryExpr:
		return fs.kind(x.X)
	case *ast.IndexExpr:
		return fs.kind(x.X)
	case *ast.SliceExpr:
		return fs.kind(x.X)
	case *ast.TypeAssertExpr:
		return fs.kind(x.X)
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && fs.isImport(id.Name) {
			if fs.tr.exportedMagic[fs.file.imports[id.Name]][x.Sel.Name] {
				return hashWeight
			}
			return 0
		}
		if typ := typeName(fs.typeOf(x.X)); typ != "" {
			return fs.file.scope.fields[typ][x.Sel.Name]
		}
	case *ast.BinaryExpr:
		switch x.Op {
		case token.ADD, token.SUB, token.MUL, token.QUO, token.XOR, token.OR, token.SHL,
			token.SHR, token.AND_NOT:
			return fs.kind(x.X) | fs.kind(x.Y)
		}
		// % and & WRAP a hash into a BUCKET — a remainder, or a mask's low
		// bits, starts again at zero, so a bucket's order is not the
		// hash's — and a comparison makes a bool: neither is a value a
		// placement could rank by. / is not among them: a quotient SCALES
		// a hash and keeps its order, which is how weighted rendezvous maps
		// one into the unit interval, and a quotient by a large constant
		// is the same coarse order as a shift.
	case *ast.CompositeLit:
		// A slice or map literal of hash-derived values is a collection
		// of them; a struct literal's taint is its fields'.
		if fs.elem(x.Type) == nil {
			return 0
		}
		var k taint
		for _, e := range x.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				e = kv.Value
			}
			k |= fs.kind(e)
		}
		return k
	case *ast.CallExpr:
		return fs.call(x)
	}
	return 0
}

func (fs *fnScope) args(c *ast.CallExpr) taint {
	var k taint
	for _, a := range c.Args {
		k |= fs.kind(a)
	}
	return k
}

// callee is what a call calls, through parentheses and type arguments
// (cmp.Compare[uint64]).
func callee(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		default:
			return e
		}
	}
}

// call is what a call's result carries.
func (fs *fnScope) call(c *ast.CallExpr) taint {
	switch fn := callee(c.Fun).(type) {
	case *ast.Ident:
		if k, ok := fs.closures[fn.Name]; ok {
			return k
		}
		if fs.types[fn.Name] != nil || fs.names[fn.Name] != 0 {
			// A function value handed in: what it returns is its
			// body's business, and the body is not here.
			return 0
		}
		sc := fs.file.scope
		switch {
		case numericTypes[fn.Name]:
			if fs.args(c) != 0 {
				return hashWeight
			}
			return 0
		case fn.Name == "string":
			return fs.args(c) & hashDigest
		case fn.Name == "append", fn.Name == "min", fn.Name == "max":
			return fs.args(c)
		case sc.types[fn.Name] != nil:
			return fs.args(c) // a conversion to a type this package declares
		}
		return sc.funcs[fn.Name]
	case *ast.ArrayType: // []byte(x)
		return fs.args(c) & hashDigest
	case *ast.SelectorExpr:
		if id, ok := fn.X.(*ast.Ident); ok && fs.isImport(id.Name) {
			path := fs.file.imports[id.Name]
			switch {
			case hashPackages[path]:
				return hashCall(fn.Sel.Name)
			case path == "math/bits":
				if fs.args(c) != 0 {
					return hashWeight
				}
				return 0
			case path == "math":
				return mathCall(fn.Sel.Name, fs.args(c))
			}
			return fs.tr.exportedFuncs[path][fn.Sel.Name]
		}
		// encoding/binary's byte orders read an integer back out of a
		// digest: binary.BigEndian.Uint64(sum[:8]).
		if order, ok := fn.X.(*ast.SelectorExpr); ok {
			if id, ok := order.X.(*ast.Ident); ok && fs.isImport(id.Name) {
				if fs.file.imports[id.Name] != "encoding/binary" {
					return 0
				}
				switch fn.Sel.Name {
				case "Uint64", "Uint32", "Uint16":
					if fs.args(c) != 0 {
						return hashWeight
					}
				}
				return 0
			}
		}
		if typ := typeName(fs.typeOf(fn.X)); typ != "" {
			if k := fs.file.scope.methods[typ][fn.Sel.Name]; k != 0 {
				return k
			}
		}
		switch fn.Sel.Name {
		case "Sum64", "Sum32":
			if len(c.Args) == 0 {
				return hashWeight
			}
		case "Sum":
			if fs.kind(fn.X) != 0 {
				return hashDigest
			}
		}
	}
	return 0
}

// hashCall is what calling one function of a hash package yields: a weight
// where it answers an integer, a digest — or a hasher, which only a Sum turns
// into a value — otherwise.
func hashCall(name string) taint {
	switch {
	case strings.HasPrefix(name, "Checksum"), strings.HasPrefix(name, "Update"),
		name == "Sum64", name == "Sum32", name == "Sum64String", name == "String",
		name == "Bytes", name == "Comparable", name == "Hash", name == "HashString":
		return hashWeight
	}
	return hashDigest
}

// mathCall is what calling one function of package math yields over arguments
// carrying k: a WEIGHT wherever a hash reached one — a logarithm, a root or a
// float's bits of a hash are still a value only the hash decided, and weighted
// rendezvous ranks by -weight / math.Log(u) — bar Mod and Remainder, which
// wrap a value exactly as % does and so make a bucket.
func mathCall(name string, k taint) taint {
	if k == 0 || name == "Mod" || name == "Remainder" {
		return 0
	}
	return hashWeight
}

// orders is the construct n orders hash-derived values with, or "".
func (fs *fnScope) orders(n ast.Node) string {
	switch x := n.(type) {
	case *ast.BinaryExpr:
		switch x.Op {
		case token.LSS, token.GTR, token.LEQ, token.GEQ:
			if fs.kind(x.X) != 0 && fs.kind(x.Y) != 0 {
				return x.Op.String()
			}
		}
	case *ast.CallExpr:
		switch fn := callee(x.Fun).(type) {
		case *ast.Ident:
			if fn.Name != "min" && fn.Name != "max" {
				return ""
			}
			if _, local := fs.closures[fn.Name]; local || fs.types[fn.Name] != nil {
				return ""
			}
			tainted := 0
			for _, a := range x.Args {
				if fs.kind(a) != 0 {
					tainted++
				}
			}
			if tainted >= 2 {
				return fn.Name
			}
		case *ast.SelectorExpr:
			id, ok := fn.X.(*ast.Ident)
			if !ok || !fs.isImport(id.Name) {
				return ""
			}
			path := fs.file.imports[id.Name]
			// A collection of DIGESTS too: sorted raw, they are a ring's
			// points hashed with SHA-256 however the comparator reads them.
			if collectionSinks[path][fn.Sel.Name] && len(x.Args) > 0 &&
				fs.kind(x.Args[0]) != 0 {
				return path + "." + fn.Sel.Name
			}
			if orderingSinks[path][fn.Sel.Name] && len(x.Args) == 2 &&
				fs.kind(x.Args[0]) != 0 && fs.kind(x.Args[1]) != 0 {
				return path + "." + fn.Sel.Name
			}
		}
	}
	return ""
}

// hashOrderNeverRead are the fixture's files the walk must not read at all.
var hashOrderNeverRead = []string{"internal/fixtures/testdata/t.go", "internal/nested/v.go"}

// hashOrderFixtureHits is the fixture's verdict, as "<package dir>.<declaration>:
// <construct>".
var hashOrderFixtureHits = []string{
	"internal/helper.order: >",
	"cmd/argmax.pick: >",
	"internal/maphashed.order: cmp.Compare",
	"internal/positional.order: cmp.Compare",
	"internal/keyed.order: >",
	"internal/digestint.better: >",
	"internal/digestbytes.better: bytes.Compare",
	"internal/handrolled.better: >",
	"internal/ring.ring: sort.Slice",
	"internal/ring.ring: <",
	"internal/ring.ring: >=",
	"internal/consumer.better: >",
	"internal/closure.order: >",
	"internal/method.router.better: >",
	"internal/rangefield.pick: >",
	"internal/slicesmax.pick: slices.Max",
	"internal/crcring.ring: slices.Sort",
	"internal/crcring.ring: slices.BinarySearch",
	"internal/oracle.TestWinner: <",
	"internal/crossfile.best: >",
	"internal/tworesults.better: >",
	"internal/magicuse.better: >",
	"internal/sortfunc.points: slices.SortFunc",
	"internal/builtinmax.aWins: max",
	"internal/funcvar.less: <",
	"internal/funcvar.better: >",
	"internal/literal.top: slices.Sort",
	"internal/literal.compare: cmp.Compare",
	"internal/conversion.order: sort.Sort",
	"internal/sinks.stable: slices.SortStableFunc",
	"internal/sinks.sorted: slices.IsSorted",
	"internal/sinks.sortedFunc: slices.IsSortedFunc",
	"internal/sinks.maxFunc: slices.MaxFunc",
	"internal/sinks.lowest: slices.Min",
	"internal/sinks.minFunc: slices.MinFunc",
	"internal/sinks.stableSort: sort.Stable",
	"internal/sinks.less: cmp.Less",
	"internal/sinks.lowestOf: min",
	"internal/sinks.searchFunc: slices.BinarySearchFunc",
	"internal/sinks.ints: sort.Ints",
	"internal/sinks.sliceStable: sort.SliceStable",
	"internal/sinks.sliceStable: <",
	"internal/sinks.intsSorted: sort.IntsAreSorted",
	"internal/sinks.searchInts: sort.SearchInts",
	"internal/sinks.floats: sort.Float64s",
	"internal/sinks.floatsSorted: sort.Float64sAreSorted",
	"internal/sinks.searchFloats: sort.SearchFloat64s",
	"internal/sinks.strs: sort.Strings",
	"internal/sinks.strsSorted: sort.StringsAreSorted",
	"internal/sinks.searchStrs: sort.SearchStrings",
	"internal/sinks.digests: strings.Compare",
	"internal/weighted.better: >",
	"internal/weighted.heavier: >",
	"internal/weighted.heaviest: sort.Float64s",
	"internal/band.before: <",
	"internal/canonical.canonical: slices.SortFunc",
	"internal/canonical.byIndex: sort.Slice",
	"internal/canonical.byIndex: bytes.Compare",
	"internal/canonical.ring: slices.SortFunc",
	"internal/canonical.ring: slices.BinarySearchFunc",
	"internal/digestfield.order: bytes.Compare",
}

// hashOrderFixture is a tree whose verdict is hashOrderFixtureHits: every
// shape a second rendezvous order or hash ring takes, the ways this tree
// legitimately uses a hash, and what the walk must never read.
var hashOrderFixture = map[string]string{
	// ── Each of these orders by hash and must be reported. ──

	// THE ROUTER'S OWN SHAPE before this package: a comparator calling a
	// helper that returns Sum64.
	"internal/helper/p.go": `package helper

import (
	"hash/fnv"
	"slices"
)

func weight(k, n string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(k + n))
	return h.Sum64()
}

func order(k string, nodes []string) {
	slices.SortFunc(nodes, func(a, b string) int {
		if wa, wb := weight(k, a), weight(k, b); wa > wb {
			return -1
		}
		return 1
	})
}
`,
	// An argmax over a CRC, in a command.
	"cmd/argmax/main.go": `package main

import "hash/crc32"

func pick(k string, nodes []string) string {
	var best string
	var top uint32
	for _, n := range nodes {
		if w := crc32.ChecksumIEEE([]byte(k + n)); w > top {
			best, top = n, w
		}
	}
	return best
}

func main() {}
`,
	"internal/maphashed/p.go": `package maphashed

import (
	"cmp"
	"hash/maphash"
	"slices"
)

var seed = maphash.MakeSeed()

func order(k string, nodes []string) {
	ws := map[string]uint64{}
	for _, n := range nodes {
		ws[n] = maphash.String(seed, k+n)
	}
	slices.SortFunc(nodes, func(a, b string) int { return cmp.Compare(ws[b], ws[a]) })
}
`,
	// A weight stored by a POSITIONAL literal and ranked by its field.
	"internal/positional/p.go": `package positional

import (
	"cmp"
	"hash/fnv"
	"slices"
)

type cand struct {
	node string
	w    uint64
}

func order(k string, nodes []string) []cand {
	var cands []cand
	for _, n := range nodes {
		h := fnv.New64a()
		_, _ = h.Write([]byte(k + n))
		cands = append(cands, cand{n, h.Sum64()})
	}
	slices.SortFunc(cands, func(a, b cand) int { return cmp.Compare(b.w, a.w) })
	return cands
}
`,
	"internal/keyed/p.go": `package keyed

import (
	"hash/fnv"
	"slices"
)

type scored struct {
	n string
	w uint64
}

func order(k string, nodes []string) []scored {
	out := make([]scored, 0, len(nodes))
	for _, n := range nodes {
		h := fnv.New64a()
		_, _ = h.Write([]byte(k + n))
		out = append(out, scored{w: h.Sum64(), n: n})
	}
	slices.SortFunc(out, func(a, b scored) int {
		if a.w > b.w {
			return -1
		}
		return 1
	})
	return out
}
`,
	// A digest read back as an integer.
	"internal/digestint/p.go": `package digestint

import (
	"crypto/sha256"
	"encoding/binary"
)

func better(k, a, b string) bool {
	sa := sha256.Sum256([]byte(k + a))
	sb := sha256.Sum256([]byte(k + b))
	return binary.BigEndian.Uint64(sa[:8]) > binary.BigEndian.Uint64(sb[:8])
}
`,
	"internal/digestbytes/p.go": `package digestbytes

import (
	"bytes"
	"crypto/sha256"
)

func better(k, a, b string) bool {
	sa := sha256.Sum256([]byte(k + a))
	sb := sha256.Sum256([]byte(k + b))
	return bytes.Compare(sa[:], sb[:]) > 0
}
`,
	// FNV by hand, its constants in decimal, importing no hash package.
	"internal/handrolled/p.go": `package handrolled

const prime = 1099511628211

func score(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

func better(k, a, b string) bool { return score(k+a) > score(k+b) }
`,
	// A hash ring: sorted points and a search for the key's successor.
	"internal/ring/p.go": `package ring

import (
	"hash/fnv"
	"sort"
)

func point(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func ring(nodes []string, key string) string {
	owner := map[uint64]string{}
	var points []uint64
	for _, n := range nodes {
		points = append(points, point(n))
		owner[point(n)] = n
	}
	sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })
	k := point(key)
	i := sort.Search(len(points), func(i int) bool { return points[i] >= k })
	return owner[points[i%len(points)]]
}
`,
	// An exported producer in one package, ranked in another.
	"internal/producer/p.go": `package producer

import "hash/fnv"

func Score(k, n string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(k + n))
	return h.Sum64()
}
`,
	"internal/consumer/p.go": `package consumer

import "github.com/crewlet/crewlet/internal/producer"

func better(k, a, b string) bool { return producer.Score(k, a) > producer.Score(k, b) }
`,
	"internal/closure/p.go": `package closure

import (
	"hash/fnv"
	"slices"
)

func order(k string, nodes []string) {
	weight := func(n string) uint64 {
		h := fnv.New64a()
		_, _ = h.Write([]byte(k + n))
		return h.Sum64()
	}
	slices.SortFunc(nodes, func(a, b string) int {
		if weight(a) > weight(b) {
			return -1
		}
		return 1
	})
}
`,
	// A method producer, ranked in ANOTHER file that imports no hash.
	"internal/method/weight.go": `package method

import "hash/fnv"

type router struct{ self string }

func (r *router) weight(n string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(r.self + n))
	return h.Sum64()
}
`,
	"internal/method/better.go": `package method

func (r *router) better(a, b string) bool { return r.weight(a) > r.weight(b) }
`,
	"internal/rangefield/p.go": `package rangefield

import "hash/fnv"

type cand struct {
	node string
	w    uint64
}

func pick(k string, nodes []string) string {
	cands := make([]cand, 0, len(nodes))
	for _, n := range nodes {
		h := fnv.New64a()
		_, _ = h.Write([]byte(k + n))
		cands = append(cands, cand{node: n, w: h.Sum64()})
	}
	var best cand
	for _, c := range cands {
		if c.w > best.w {
			best = c
		}
	}
	return best.node
}
`,
	"internal/slicesmax/p.go": `package slicesmax

import (
	"hash/fnv"
	"slices"
)

func pick(k string, nodes []string) string {
	owner := map[uint64]string{}
	var ws []uint64
	for _, n := range nodes {
		h := fnv.New64a()
		_, _ = h.Write([]byte(k + n))
		ws = append(ws, h.Sum64())
		owner[h.Sum64()] = n
	}
	return owner[slices.Max(ws)]
}
`,
	"internal/crcring/p.go": `package crcring

import (
	"hash/crc32"
	"slices"
)

func ring(nodes []string, key string) string {
	owner := map[uint32]string{}
	var points []uint32
	for _, n := range nodes {
		p := crc32.ChecksumIEEE([]byte(n))
		points = append(points, p)
		owner[p] = n
	}
	slices.Sort(points)
	i, _ := slices.BinarySearch(points, crc32.ChecksumIEEE([]byte(key)))
	return owner[points[i%len(points)]]
}
`,
	// A TEST in an external test package ranking by the rendezvous weight
	// itself: a second implementation of the order, used as an oracle.
	"internal/oracle/p_test.go": `package oracle_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/rendezvous"
)

func TestWinner(t *testing.T) {
	if rendezvous.Weight("k", "a") < rendezvous.Weight("k", "b") {
		t.Log("b")
	}
}
`,
	// A weight stored in a field in one file and ranked in another that
	// imports no hash and calls no producer.
	"internal/crossfile/score.go": `package crossfile

import "hash/fnv"

type cand struct {
	node string
	w    uint64
}

func scored(k string, nodes []string) []cand {
	var out []cand
	for _, n := range nodes {
		h := fnv.New64a()
		_, _ = h.Write([]byte(k + n))
		out = append(out, cand{node: n, w: h.Sum64()})
	}
	return out
}
`,
	"internal/crossfile/best.go": `package crossfile

func best(cs []cand) cand {
	var top cand
	for _, c := range cs {
		if c.w > top.w {
			top = c
		}
	}
	return top
}
`,
	"internal/tworesults/p.go": `package tworesults

import "hash/fnv"

func weight(k, n string) (uint64, error) {
	h := fnv.New64a()
	if _, err := h.Write([]byte(k + n)); err != nil {
		return 0, err
	}
	return h.Sum64(), nil
}

func better(k, a, b string) bool {
	wa, _ := weight(k, a)
	wb, _ := weight(k, b)
	return wa > wb
}
`,
	// A magic constant exported by one package, hashed with in another.
	"internal/magicconst/p.go": `package magicconst

// Prime is FNV-64's prime.
const Prime = 1099511628211
`,
	"internal/magicuse/p.go": `package magicuse

import "github.com/crewlet/crewlet/internal/magicconst"

func score(s string) uint64 {
	var h uint64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= magicconst.Prime
	}
	return h
}

func better(k, a, b string) bool { return score(k+a) > score(k+b) }
`,
	// Raw weights sorted by a comparator over its own parameters.
	"internal/sortfunc/p.go": `package sortfunc

import (
	"cmp"
	"hash/fnv"
	"slices"
)

func points(nodes []string) []uint64 {
	var ws []uint64
	for _, n := range nodes {
		h := fnv.New64a()
		_, _ = h.Write([]byte(n))
		ws = append(ws, h.Sum64())
	}
	slices.SortFunc(ws, func(a, b uint64) int { return cmp.Compare(a, b) })
	return ws
}
`,
	"internal/builtinmax/p.go": `package builtinmax

import "hash/fnv"

func weight(k, n string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(k + n))
	return h.Sum64()
}

func aWins(k, a, b string) bool { return max(weight(k, a), weight(k, b)) == weight(k, a) }
`,
	// A producer and a comparator held in package-level variables.
	"internal/funcvar/p.go": `package funcvar

import "hash/fnv"

var weight = func(k, n string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(k + n))
	return h.Sum64()
}

var less = func(k, a, b string) bool { return weight(k, a) < weight(k, b) }

func better(k, a, b string) bool { return weight(k, a) > weight(k, b) }
`,
	// A slice literal of weights, and a compare spelled with a type
	// argument.
	"internal/literal/p.go": `package literal

import (
	"cmp"
	"hash/crc64"
	"slices"
)

var table = crc64.MakeTable(crc64.ISO)

func weight(k, n string) uint64 { return crc64.Checksum([]byte(k+n), table) }

func top(k, a, b string) uint64 {
	ws := []uint64{weight(k, a), weight(k, b)}
	slices.Sort(ws)
	return ws[len(ws)-1]
}

func compare(k, a, b string) int { return cmp.Compare[uint64](weight(k, a), weight(k, b)) }
`,
	// Weights sorted through a conversion to a sort.Interface.
	"internal/conversion/p.go": `package conversion

import (
	"hash/adler32"
	"sort"
)

type byWeight []uint32

func (b byWeight) Len() int           { return len(b) }
func (b byWeight) Less(i, j int) bool { return b[i] < b[j] }
func (b byWeight) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }

func order(nodes []string) []uint32 {
	var ws []uint32
	for _, n := range nodes {
		ws = append(ws, adler32.Checksum([]byte(n)))
	}
	sort.Sort(byWeight(ws))
	return ws
}
`,

	// Every other sink, one declaration each, so no row of the sink tables
	// is a claim nothing exercises.
	"internal/sinks/p.go": `package sinks

import (
	"cmp"
	"crypto/sha256"
	"hash/fnv"
	"slices"
	"sort"
	"strings"
)

func w(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func ws(nodes []string) []uint64 {
	out := make([]uint64, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, w(n))
	}
	return out
}

type byW []uint64

func (b byW) Len() int           { return len(b) }
func (b byW) Less(i, j int) bool { return b[i] < b[j] }
func (b byW) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }

func stable(nodes []string)              { slices.SortStableFunc(ws(nodes), cmp.Compare[uint64]) }
func sorted(nodes []string) bool         { return slices.IsSorted(ws(nodes)) }
func sortedFunc(nodes []string) bool     { return slices.IsSortedFunc(ws(nodes), cmp.Compare[uint64]) }
func maxFunc(nodes []string) uint64      { return slices.MaxFunc(ws(nodes), cmp.Compare[uint64]) }
func lowest(nodes []string) uint64       { return slices.Min(ws(nodes)) }
func minFunc(nodes []string) uint64      { return slices.MinFunc(ws(nodes), cmp.Compare[uint64]) }
func stableSort(nodes []string)          { sort.Stable(byW(ws(nodes))) }
func less(a, b string) bool              { return cmp.Less(w(a), w(b)) }
func lowestOf(a, b string) uint64        { return min(w(a), w(b)) }

func searchFunc(nodes []string, k string) (int, bool) {
	return slices.BinarySearchFunc(ws(nodes), w(k), cmp.Compare[uint64])
}

func ints(nodes []string) {
	var xs []int
	for _, n := range nodes {
		xs = append(xs, int(w(n)))
	}
	sort.Ints(xs)
}

func sliceStable(nodes []string) {
	p := ws(nodes)
	sort.SliceStable(p, func(i, j int) bool { return p[i] < p[j] })
}

func intWs(nodes []string) []int {
	var xs []int
	for _, n := range nodes {
		xs = append(xs, int(w(n)))
	}
	return xs
}

// floatWs are weights read as floats, and digestWs raw digests held as
// strings: a collection of either is ordered by hash.
func floatWs(nodes []string) []float64 {
	var xs []float64
	for _, n := range nodes {
		xs = append(xs, float64(w(n)))
	}
	return xs
}

func digestWs(nodes []string) []string {
	var xs []string
	for _, n := range nodes {
		sum := sha256.Sum256([]byte(n))
		xs = append(xs, string(sum[:]))
	}
	return xs
}

func intsSorted(nodes []string) bool             { return sort.IntsAreSorted(intWs(nodes)) }
func searchInts(nodes []string, k string) int     { return sort.SearchInts(intWs(nodes), int(w(k))) }
func floats(nodes []string)                       { sort.Float64s(floatWs(nodes)) }
func floatsSorted(nodes []string) bool            { return sort.Float64sAreSorted(floatWs(nodes)) }
func searchFloats(nodes []string, u float64) int  { return sort.SearchFloat64s(floatWs(nodes), u) }
func strs(nodes []string)                         { sort.Strings(digestWs(nodes)) }
func strsSorted(nodes []string) bool              { return sort.StringsAreSorted(digestWs(nodes)) }
func searchStrs(nodes []string, sum string) int   { return sort.SearchStrings(digestWs(nodes), sum) }

func digests(a, b string) int {
	sa, sb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return strings.Compare(string(sa[:]), string(sb[:]))
}
`,
	// WEIGHTED RENDEZVOUS, the best-known variant: a weight SCALED into the
	// unit interval by a division, which keeps its order, ranked as it is and
	// through the -weight / ln(u) score — written as one expression, and
	// divided in place.
	"internal/weighted/p.go": `package weighted

import (
	"hash/fnv"
	"math"
	"sort"
)

func w(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func unit(k, n string) float64 { return float64(w(k+n)) / float64(math.MaxUint64) }

func better(k, a, b string) bool { return unit(k, a) > unit(k, b) }

func score(k, n string, weight float64) float64 { return -weight / math.Log(unit(k, n)) }

func heavier(k, a, b string, wa, wb float64) bool { return score(k, a, wa) > score(k, b, wb) }

func scaled(k, n string, weight float64) float64 {
	s := weight
	s /= -math.Log(unit(k, n))
	return s
}

func heaviest(k string, nodes []string, weights []float64) []float64 {
	out := make([]float64, 0, len(nodes))
	for i, n := range nodes {
		out = append(out, scaled(k, n, weights[i]))
	}
	sort.Float64s(out)
	return out
}
`,
	// A COARSE ORDER is still an order: the top three bits of two hashes
	// compared, by a division rather than a shift.
	"internal/band/p.go": `package band

import (
	"hash/fnv"
	"math"
)

func point(id string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return h.Sum64()
}

func before(a, b string) bool {
	return point(a)/(math.MaxUint64/8) < point(b)/(math.MaxUint64/8)
}
`,
	// DIGESTS IN THEIR CANONICAL BYTE ORDER are the first half of a hash ring
	// hashed with SHA-256, and cannot be told from one — so the order is
	// reported however the comparator reaches the digests: its own
	// parameters, the collection by index, and the ring the sorted digests
	// become once a key's successor is searched for.
	"internal/canonical/p.go": `package canonical

import (
	"bytes"
	"crypto/sha256"
	"slices"
	"sort"
)

func canonical(items [][]byte) [][32]byte {
	var sums [][32]byte
	for _, it := range items {
		sums = append(sums, sha256.Sum256(it))
	}
	slices.SortFunc(sums, func(a, b [32]byte) int {
		for i := range a {
			if a[i] != b[i] {
				return int(a[i]) - int(b[i])
			}
		}
		return 0
	})
	return sums
}

func byIndex(items [][]byte) [][32]byte {
	var sums [][32]byte
	for _, it := range items {
		sums = append(sums, sha256.Sum256(it))
	}
	sort.Slice(sums, func(i, j int) bool { return bytes.Compare(sums[i][:], sums[j][:]) < 0 })
	return sums
}

func ring(nodes []string, key string) int {
	var points [][32]byte
	for _, n := range nodes {
		points = append(points, sha256.Sum256([]byte(n)))
	}
	byBytes := func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) }
	slices.SortFunc(points, byBytes)
	i, _ := slices.BinarySearchFunc(points, sha256.Sum256([]byte(key)), byBytes)
	return i % len(points)
}
`,
	// A DIGEST stored in a struct field and ranked by it: rendezvous over
	// SHA-256.
	"internal/digestfield/p.go": `package digestfield

import (
	"bytes"
	"crypto/sha256"
	"slices"
)

type cand struct {
	node string
	sum  [32]byte
}

func order(k string, nodes []string) []cand {
	cands := make([]cand, 0, len(nodes))
	for _, n := range nodes {
		cands = append(cands, cand{node: n, sum: sha256.Sum256([]byte(k + n))})
	}
	slices.SortFunc(cands, func(a, b cand) int { return bytes.Compare(b.sum[:], a.sum[:]) })
	return cands
}
`,

	// ── Each of these uses a hash and orders nothing by it. ──

	// BUCKETS: search.ShardOf's shape, and buckets compared and sorted
	// two-sided — a hash WRAPPED by %, by & and by math.Mod — which a hash
	// must not survive.
	"internal/bucket/p.go": `package bucket

import (
	"cmp"
	"hash/fnv"
	"math"
	"slices"
)

func point(id string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return h.Sum64()
}

func shard(id string) int { return int(point(id) % 64) }

func inRange(id string, from, to int) bool {
	s := shard(id)
	return s >= from && s < to
}

type doc struct {
	id    string
	shard int
}

func scanOrder(ids []string) []doc {
	docs := make([]doc, 0, len(ids))
	for _, id := range ids {
		docs = append(docs, doc{id: id, shard: shard(id)})
	}
	slices.SortFunc(docs, func(a, b doc) int { return cmp.Compare(a.shard, b.shard) })
	return docs
}

func stripeBefore(a, b string) bool { return point(a)&0xff < point(b)&0xff }

func cellBefore(a, b string) bool {
	return math.Mod(float64(point(a)), 64) < math.Mod(float64(point(b)), 64)
}
`,
	// EQUALITY, and digests encoded into NAMES that are then sorted and
	// compared: an encoded digest is a name.
	"internal/equality/p.go": `package equality

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"slices"
)

func same(a, b []byte) bool {
	sa, sb := sha256.Sum256(a), sha256.Sum256(b)
	return bytes.Equal(sa[:], sb[:]) && sa == sb
}

func names(xs []string) []string {
	var out []string
	for _, x := range xs {
		sum := sha256.Sum256([]byte(x))
		out = append(out, x+"-"+hex.EncodeToString(sum[:6]))
	}
	slices.Sort(out)
	return out
}

func key(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func before(a, b []byte) bool { return key(a) < key(b) }
`,
	// A SEED: search.IVFSeed's shape.
	"internal/seed/p.go": `package seed

import (
	"hash/fnv"
	"math/rand/v2"
)

func seed(log string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(log))
	return h.Sum64()
}

func sample(log string, n int) int {
	r := rand.New(rand.NewPCG(seed(log), 1))
	return r.IntN(n)
}
`,
	// A digest field beside an unhashed one the sort is by.
	"internal/bysize/p.go": `package bysize

import (
	"cmp"
	"crypto/sha256"
	"slices"
)

type object struct {
	hash [32]byte
	size int64
}

func put(b []byte) object { return object{hash: sha256.Sum256(b), size: int64(len(b))} }

func bySize(xs [][]byte) []object {
	var out []object
	for _, x := range xs {
		out = append(out, put(x))
	}
	slices.SortFunc(out, func(a, b object) int { return cmp.Compare(a.size, b.size) })
	return out
}
`,
	// A ONE-SIDED threshold, and a bucket compared with a constant:
	// notify's phrase pick.
	"internal/threshold/p.go": `package threshold

import "hash/fnv"

func sampled(id string, limit uint64) bool {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return h.Sum64() < limit
}

func pickPhrase(id string, pool []string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	idx := h.Sum64() % uint64(len(pool))
	if idx > 3 {
		return pool[0]
	}
	return pool[idx]
}
`,
	// A MAC, verified.
	"internal/mac/p.go": `package mac

import (
	"crypto/hmac"
	"crypto/sha256"
)

func verified(key, body, sum []byte) bool {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(body)
	return hmac.Equal(h.Sum(nil), sum)
}
`,
	// THE ROUTER'S SHAPE NOW: the order taken from rendezvous, then
	// reclassed by facts that are not hashes.
	"internal/caller/p.go": `package caller

import (
	"slices"

	"github.com/crewlet/crewlet/internal/rendezvous"
)

func ask(self, sticky string, nodes []string) []string {
	order := slices.DeleteFunc(rendezvous.Order(self, nodes), func(n string) bool { return n == self })
	slices.SortStableFunc(order, func(a, b string) int {
		if (a == sticky) != (b == sticky) {
			if a == sticky {
				return -1
			}
			return 1
		}
		return 0
	})
	return order
}
`,

	// ── The one implementation, and what is never read. ──

	// Recognised once, in its source; its own test ranks by the weight
	// too, and neither counts against the tree.
	"internal/rendezvous/r.go": `package rendezvous

import (
	"cmp"
	"hash/fnv"
	"slices"
)

func Weight(k, n string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(k + n))
	return h.Sum64()
}

func Order(k string, nodes []string) []string {
	out := slices.Clone(nodes)
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(Weight(k, b), Weight(k, a)) })
	return out
}
`,
	"internal/rendezvous/r_test.go": `package rendezvous

func lessForATest(k, a, b string) bool { return Weight(k, a) < Weight(k, b) }
`,
	// testdata is not compiled.
	"internal/fixtures/testdata/t.go": `package t

import "hash/fnv"

func w(s string) uint64 { h := fnv.New64a(); _, _ = h.Write([]byte(s)); return h.Sum64() }

func better(a, b string) bool { return w(a) > w(b) }
`,
	// A nested checkout is another tree.
	"internal/nested/.git": "gitdir: x\n",
	"internal/nested/v.go": `package nested

import "hash/fnv"

func w(s string) uint64 { h := fnv.New64a(); _, _ = h.Write([]byte(s)); return h.Sum64() }

func better(a, b string) bool { return w(a) > w(b) }
`,
	// Not Go past its imports, which import no hash: a walk that parsed
	// it whole would fail on it.
	"internal/notgo/r.go": "package notgo\n\nimport \"strings\"\n\nthis is not Go\n",
}
