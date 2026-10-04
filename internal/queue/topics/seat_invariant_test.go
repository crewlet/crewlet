package topics_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// separators is what a seat token must never contribute to a subject: the
// token separator, and the two NATS wildcards. A token carrying any of them
// stops being a name and becomes syntax.
const separators = ".*>"

// THIS FILE WAS ABOUT THE HANDLE, and it is re-pointed rather than deleted.
//
// [topics.AgentInbox] used to interpolate a seat's HANDLE into a subject, and
// what made that safe lived entirely on the other side of the tree:
// org.ValidHandle enforces ^[a-z0-9][a-z0-9-]*$, which happens to exclude
// every character the subject grammar treats as syntax. The subject carries
// the seat's ID now (ADR-0013), so that particular dependency is gone — and
// the invariant it protected is not. It simply moved: what has to be a safe,
// single, canonical token is whatever [org.Organization.AgentIDFor] derives.
//
// The dependency is still load-bearing and still written down nowhere else.
// Neither package states it: org derives a uuid without knowing a subject is
// built from it, and topics interpolates and now PARSES a token without
// knowing where it came from.
//
// The consequences, unchanged in kind from the handle's:
//
//   - A dot splits the subject into extra tokens, so the seat's inbox no
//     longer matches crewlet.agent.*.inbox and stops being covered by
//     anything scoped to one seat.
//   - A `*` or a `>` turns the seat's OWN inbox into a wildcard PATTERN. A
//     node subscribing that seat would attach to a subject matching its
//     peers' inboxes — it receives their mail, and they stop receiving it.
//   - And one the handle never had: the token is PARSED back by
//     [topics.SeatFromInbox], which a diagnostic names a seat from and a
//     retirement sweep DELETES what it names. A derivation that produced a
//     non-canonical spelling would mint a subject its own inverse refuses,
//     so the sweep would see an unregistered mailbox for a seat that has one.

// TestEverySeatIDTheOrgDerivesYieldsASafeSubject walks the handles the org
// accepts and mints, derives each seat's id the way the engine does, and holds
// every name built from it against the subject grammar.
//
// Over the HANDLES rather than over arbitrary uuids, because the derivation is
// what is under test: a change that made AgentIDFor return something other
// than a uuid — a handle, a composite, a base64 digest — is exactly the change
// this has to catch, and a case that fed it a uuid to begin with could not.
func TestEverySeatIDTheOrgDerivesYieldsASafeSubject(t *testing.T) {
	t.Parallel()

	// Every handle the org ACCEPTS, exhaustive over characters rather than
	// a sample, in all three positions a character-class regex can
	// distinguish.
	accepted := 0
	for r := rune(0); r < 0x300; r++ {
		for _, candidate := range []string{
			string(r),             // the whole handle
			"a" + string(r),       // last position
			string(r) + "a",       // first position
			"a" + string(r) + "b", // interior
		} {
			if !org.ValidHandle(candidate) {
				continue
			}
			accepted++
			requireSafeSubject(t, "acme", candidate)
		}
	}
	if accepted == 0 {
		t.Fatal("org.ValidHandle accepted none of the candidates — either the pattern " +
			"changed shape or this loop stopped generating handles, and either way " +
			"the invariant went uncertified")
	}
	t.Logf("checked %d single-character handle placements org.ValidHandle accepts", accepted)

	// ...and the handles the org actually MINTS, which is the other way a
	// seat reaches this package: an operator who never wrote one gets
	// Slugify's output from the role name.
	minted := 0
	for _, name := range []string{
		"CEO", "QA Lead", "Head of Platform / Infra", "Émile Zola",
		"release.control", "a*b", "a>b", "ops — on call", "  padded  ",
		"7-Eleven Liaison", "Ünïcødé Person",
	} {
		handle := org.Slugify(name)
		if handle == "" {
			continue
		}
		if !org.ValidHandle(handle) {
			t.Errorf("org.Slugify(%q) = %q, which org.ValidHandle rejects — a seat "+
				"the org can name but cannot address", name, handle)
			continue
		}
		minted++
		requireSafeSubject(t, "acme", handle)
	}
	if minted == 0 {
		t.Fatal("org.Slugify produced no usable handles; this half certified nothing")
	}

	// AND THE COMPANY NAME IS AN INPUT TOO, so a company named in prose
	// full of subject syntax must not reach a subject either.
	for _, company := range []string{"Acme", "acme.corp", "a*b", "a>b", "Ünïcødé GmbH", "   "} {
		requireSafeSubject(t, company, "ceo")
	}
}

// requireSafeSubject derives the seat's id the way the engine does and asserts
// the properties every name built from it must have: the expected token count,
// no subject syntax smuggled in, and a token its own inverse reads back.
func requireSafeSubject(t *testing.T, company, handle string) {
	t.Helper()

	o := &org.Organization{Name: company, Roles: []*org.Role{
		{Name: "Seat", DeclaredHandle: handle}}}
	id, ok := o.AgentIDFor(o.Roles[0])
	if !ok {
		// A company or a handle too unnamed to derive from. Nothing is
		// built, which is the documented answer and not a failure.
		return
	}
	token := id.String()

	for _, tc := range []struct {
		what    string
		subject string
		want    []string
	}{
		{"AgentInbox", topics.AgentInbox(id), []string{"crewlet", "agent", token, "inbox"}},
		{"AgentControl", topics.AgentControl(id), []string{"crewlet", "agent", token, "control"}},
	} {
		got := strings.Split(tc.subject, ".")
		if len(got) != len(tc.want) {
			t.Errorf("%s for company %q seat %q = %q splits into %d tokens %q, want %d — "+
				"a seat id that carries a separator stops being one token, so the seat "+
				"is no longer covered by anything scoped to one seat",
				tc.what, company, handle, tc.subject, len(got), got, len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s for company %q seat %q = %q, token %d is %q, want %q",
					tc.what, company, handle, tc.subject, i, got[i], tc.want[i])
			}
		}
		if i := strings.IndexAny(tc.subject, "*>"); i >= 0 {
			t.Errorf("%s for company %q seat %q = %q contains the wildcard %q — this "+
				"seat's own inbox is now a PATTERN over its peers' inboxes, so it "+
				"receives their mail and they stop receiving it",
				tc.what, company, handle, tc.subject, tc.subject[i:i+1])
		}
	}

	// THE INVERSE READS IT BACK. A sweep deletes what it names, so a
	// derivation minting a token the inverse refuses would leave every
	// mailbox looking unregistered.
	if back, ok := topics.SeatFromInbox(topics.AgentInbox(id)); !ok || back != id {
		t.Errorf("SeatFromInbox(AgentInbox(%s)) = (%s, %v) for company %q seat %q, "+
			"want the same id — a subject its own inverse refuses is a mailbox "+
			"nothing can identify", id, back, ok, company, handle)
	}

	// Groups are not subjects, but they are wire names on the same
	// characters, and a wildcard in a durable subscription name is
	// rejected outright by JetStream — a seat that cannot be subscribed
	// at all.
	for _, tc := range []struct{ what, group string }{
		{"AgentInboxGroup", topics.AgentInboxGroup(id)},
		{"AgentControlGroup", topics.AgentControlGroup(id)},
	} {
		if i := strings.IndexAny(tc.group, separators); i >= 0 {
			t.Errorf("%s for company %q seat %q = %q contains %q",
				tc.what, company, handle, tc.group, tc.group[i:i+1])
		}
	}
}

// TestASubjectIsBuiltFromTheIDAndNotTheHandle shows the invariant from the
// other side: the exact strings that would break routing if a seat's handle
// ever reached a subject again, and the fact that nothing but a canonical
// uuid is read back out of one.
//
// Without this half, the test above could pass because the derivation had
// been narrowed to accept nothing at all.
func TestASubjectIsBuiltFromTheIDAndNotTheHandle(t *testing.T) {
	t.Parallel()

	// The collision, demonstrated rather than asserted in the abstract.
	// These calls are legal — AgentInbox has no opinion about its argument
	// beyond its type — and the results are why the type is a uuid.
	victim := topics.AgentInbox(alice)
	for _, tc := range []struct{ name, token string }{
		{"a dot, the namespacing someone will eventually propose", "platform.infra"},
		{"the one-token wildcard", "*"},
		{"the tail wildcard", ">"},
		{"a space", "qa lead"},
		{"uppercase, which two producers would case-fold differently", "Alice"},
	} {
		hand := topics.AgentInboxPrefix + tc.token + topics.AgentInboxSuffix
		if _, ok := topics.SeatFromInbox(hand); ok {
			t.Errorf("%s: SeatFromInbox(%q) named a seat. The inverse is what a "+
				"diagnostic reads and what a retirement sweep DELETES, so a "+
				"hand-built subject under this prefix must never resolve to one",
				tc.name, hand)
		}
	}
	if wildcard := topics.AgentInboxPrefix + "*" + topics.AgentInboxSuffix; !topics.Match(wildcard, victim) {
		t.Errorf("expected %q to match %q; the demonstration this test rests on no "+
			"longer holds, so re-derive it before trusting the rule", wildcard, victim)
	}
	if tail := topics.AgentInboxPrefix + ">" + topics.AgentInboxSuffix; topics.Match(tail, victim) {
		// `>` is only a wildcard as the FINAL token, so a token of ">"
		// does not swallow a peer's inbox — it produces a subject that
		// matches nothing but itself. Recorded so the next reader does
		// not assume both wildcards fail the same way.
		t.Errorf("%q matched %q; `>` is documented as final-token-only", tail, victim)
	}
}

// TestDistinctSeatsNeverShareAName is the collision property stated
// positively: distinct seats get distinct subjects and distinct groups, and
// no seat's subject matches another's.
//
// Over seats the ORG derives, including the near misses a shared company name
// or a shared handle makes possible, since those are where an accidental
// sharing would actually come from.
func TestDistinctSeatsNeverShareAName(t *testing.T) {
	t.Parallel()

	type seat struct{ company, handle string }
	seats := []seat{
		{"acme", "alice"}, {"acme", "alice-2"}, {"acme", "a"}, {"acme", "ab"},
		{"acme", "a-b"}, {"acme", "0"}, {"acme", "release"},
		{"acme", "release-control"}, {"acme", "control"}, {"acme", "inbox"},
		// Two companies sharing a handle: the derivation is namespaced by
		// the company name precisely so these cannot collide.
		{"acme-two", "alice"}, {"acme", "alice"},
	}
	ids := map[uuid.UUID]seat{}
	for _, s := range seats {
		id, ok := org.DeriveAgentID(s.company, s.handle)
		if !ok {
			t.Fatalf("%q/%q derives no id; fix the corpus", s.company, s.handle)
		}
		if prev, dup := ids[id]; dup && prev != s {
			t.Errorf("%q/%q and %q/%q derive one id %s — one seat would take the "+
				"other's mail", prev.company, prev.handle, s.company, s.handle, id)
		}
		ids[id] = s
	}

	seen := map[string]string{}
	claim := func(t *testing.T, name, owner string) {
		t.Helper()
		if prev, dup := seen[name]; dup {
			t.Errorf("%q is minted for both %s and %s", name, prev, owner)
			return
		}
		seen[name] = owner
	}
	for id, s := range ids {
		claim(t, topics.AgentInbox(id), fmt.Sprintf("inbox of %q/%q", s.company, s.handle))
		claim(t, topics.AgentControl(id), fmt.Sprintf("control of %q/%q", s.company, s.handle))
	}

	for a := range ids {
		for b := range ids {
			if a == b {
				continue
			}
			if topics.Match(topics.AgentInbox(a), topics.AgentInbox(b)) {
				t.Errorf("seat %s's inbox subject matches seat %s's — one would take "+
					"the other's mail", a, b)
			}
			if topics.Match(topics.AgentInbox(a), topics.AgentControl(b)) {
				t.Errorf("seat %s's inbox subject matches seat %s's control subject; a "+
					"sandbox completion would be requeued behind the wait it exists to end",
					a, b)
			}
		}
	}
	if len(seen) != 2*len(ids) {
		t.Fatalf("minted %d distinct names for %d seats; the corpus or the walk is wrong",
			len(seen), 2*len(ids))
	}
}

// topicsImport is this package's import path, which the walk below resolves a
// file's local name for.
const topicsImport = "github.com/crewlet/crewlet/internal/queue/topics"

// seatGrammar is the pieces a SEAT's mailbox names are composed of: the inbox
// and control subjects, and their two consumer groups.
var seatGrammar = map[string]bool{
	"AgentInboxPrefix":        true,
	"AgentInboxSuffix":        true,
	"AgentControlSuffix":      true,
	"AgentGroupPrefix":        true,
	"AgentControlGroupSuffix": true,
}

// suffixReaders are the strings functions that READ a name against a piece of
// the grammar rather than build one — asking whether a subject is an inbox at
// all is not composing somebody's.
var suffixReaders = map[string]bool{
	"HasPrefix": true, "HasSuffix": true, "CutPrefix": true,
	"CutSuffix": true, "TrimPrefix": true, "TrimSuffix": true,
}

// TestNoPackageBuildsASeatsMailboxFromAString fails the build when a package
// outside this one composes a seat's inbox or control subject, or either of
// their consumer groups, out of the grammar's pieces — `AgentInboxPrefix + x +
// AgentInboxSuffix`, a Sprintf over them, a piece held in a variable to be
// joined later.
//
// THE TYPE ALREADY REFUSES A HANDLE: [topics.AgentInbox] takes a uuid, so a
// call handing it a handle does not compile. This is the other door.
// The grammar's pieces are exported — the stream topology and the mailbox
// sweep need the all-seats wildcard — and a site handed a handle where the
// type wants an id could reach past the constructor and concatenate the
// subject itself. That compiles, routes by an address no consumer is named
// by, and fails silently: the publish lands on a subject nobody is attached to, and
// [TestNoPackageBuildsASubjectByHand] cannot see it, because no literal in it
// names a subject.
//
// TWO USES ARE READINGS AND STAY LEGAL: the inbox prefix followed by `>` and
// nothing else — the wildcard over every seat's inbox the stream is created
// over and the sweep lists — and a piece handed to a strings function that
// asks whether a name has it. Every other reference to a piece is a name being
// built, and the constructor is where names are built.
func TestNoPackageBuildsASeatsMailboxFromAString(t *testing.T) {
	t.Parallel()

	// THE CONTROLS, because a guard asserting an absence passes the same
	// way when the tree is clean and when the matcher has gone inert.
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"a handle between the inbox pieces": {`h := "alice"; _ = topics.AgentInboxPrefix + h + topics.AgentInboxSuffix`, 2},
		"a control subject":                 {`_ = topics.AgentInboxPrefix + "alice" + topics.AgentControlSuffix`, 2},
		"a group by Sprintf":                {`_ = fmt.Sprintf("%s%s", topics.AgentGroupPrefix, "alice")`, 1},
		"a piece held for later":            {`p := topics.AgentInboxPrefix; _ = p`, 1},
		"a control group":                   {`_ = topics.AgentGroupPrefix + "alice" + topics.AgentControlGroupSuffix`, 2},
		"the wildcard, extended":            {`_ = topics.AgentInboxPrefix + ">" + "x"`, 1},
		"the wildcard over every inbox":     {`_ = []string{topics.AgentInboxPrefix + ">"}`, 0},
		"a parenthesised wildcard":          {`_ = (topics.AgentInboxPrefix + ">")`, 0},
		"asking whether it is an inbox":     {`_ = strings.HasSuffix("t", topics.AgentInboxSuffix)`, 0},
		"the constructor":                   {`_ = topics.AgentInbox(uuid.Nil)`, 0},
	} {
		src := "package p\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n\n\t\"github.com/google/uuid\"\n\n\t\"" +
			topicsImport + "\"\n)\n\nvar _ = fmt.Sprint\nvar _ = strings.HasSuffix\nvar _ = uuid.Nil\n\nfunc f() {\n\t" + tc.src + "\n}\n"
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name+".go", src, 0)
		if err != nil {
			t.Fatalf("control %q does not parse: %v", name, err)
		}
		got, _ := seatGrammarBuilds(fset, file)
		if len(got) != tc.want {
			t.Errorf("control %q: the walk flagged %d references %v, want %d", name, len(got), got, tc.want)
		}
	}
	// AN ALIASED IMPORT IS THE SAME PACKAGE, and a walk keyed on the word
	// "topics" would read it as somebody else's.
	aliased := "package p\n\nimport q \"" + topicsImport + "\"\n\nfunc f(h string) string { return q.AgentInboxPrefix + h + q.AgentInboxSuffix }\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "aliased.go", aliased, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := seatGrammarBuilds(fset, file); len(got) != 2 {
		t.Errorf("an aliased import built an inbox by hand and the walk flagged %v", got)
	}

	root := sourcetree.Root(t)
	topicsDir := filepath.Join(root, "internal", "queue", "topics")
	var files, readings int
	var builds []string
	for _, tree := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, tree), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// The grammar's own package composes its names; test
				// code and the *test support packages build malformed
				// ones on purpose — the boundary
				// [TestNoPackageBuildsASubjectByHand] draws, and for
				// its reason.
				if path == topicsDir || filepath.Base(path) == "testdata" || isSupportPackage(filepath.Base(path)) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			files++
			found, read := seatGrammarBuilds(fset, file)
			readings += read
			for _, at := range found {
				builds = append(builds, shortPos(root, at))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", tree, err)
		}
	}
	if files == 0 {
		t.Fatal("parsed no source files — this guard certified nothing")
	}
	// THE LIVENESS HALF: the stream topology and the mailbox sweep read the
	// all-inboxes wildcard, so a walk that saw no legal reading either lost
	// its reach or stopped resolving the import.
	if readings == 0 {
		t.Fatal("saw no legal reading of the seat grammar anywhere; the walk stopped " +
			"recognising references to this package and would pass any tree")
	}
	for _, at := range builds {
		t.Errorf("%s: builds a seat's mailbox name out of the grammar's pieces.\n"+
			"\tcall topics.AgentInbox / AgentInboxGroup / AgentControl / "+
			"AgentControlGroup with the seat's id — a name composed from "+
			"anything else is a mailbox nobody is attached to", at)
	}
	t.Logf("scanned %d files: %d legal readings of the seat grammar, %d builds",
		files, readings, len(builds))
}

// seatGrammarBuilds reports the positions in one file where a piece of the
// seat grammar is used to BUILD a name, and how many references were legal
// readings ([TestNoPackageBuildsASeatsMailboxFromAString] says which).
func seatGrammarBuilds(fset *token.FileSet, file *ast.File) ([]string, int) {
	local, strs := "", "strings"
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		switch path {
		case topicsImport:
			local = "topics"
			if spec.Name != nil {
				local = spec.Name.Name
			}
		case "strings":
			if spec.Name != nil {
				strs = spec.Name.Name
			}
		}
	}
	if local == "" || local == "_" {
		return nil, 0
	}
	var builds []string
	var readings int
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if sel, ok := n.(*ast.SelectorExpr); ok && seatGrammar[sel.Sel.Name] {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
				if seatGrammarReading(sel, stack, strs) {
					readings++
				} else {
					builds = append(builds, fset.Position(sel.Pos()).String())
				}
			}
		}
		stack = append(stack, n)
		return true
	})
	return builds, readings
}

// seatGrammarReading reports whether one reference to a piece of the seat
// grammar, under the nodes that enclose it, is one of the two legal readings.
func seatGrammarReading(sel *ast.SelectorExpr, stack []ast.Node, strs string) bool {
	if len(stack) == 0 {
		return false
	}
	parent := stack[len(stack)-1]
	switch p := parent.(type) {
	case *ast.BinaryExpr:
		// The all-inboxes wildcard: the prefix, `>`, and nothing more —
		// a concatenation that carried on past the wildcard is building
		// something.
		lit, isLit := p.Y.(*ast.BasicLit)
		if sel.Sel.Name != "AgentInboxPrefix" || p.Op != token.ADD || p.X != sel ||
			!isLit || lit.Kind != token.STRING || lit.Value != `">"` {
			return false
		}
		if len(stack) > 1 {
			if outer, ok := stack[len(stack)-2].(*ast.BinaryExpr); ok && outer.Op == token.ADD {
				return false
			}
		}
		return true
	case *ast.CallExpr:
		fn, ok := p.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := fn.X.(*ast.Ident)
		return ok && pkg.Name == strs && suffixReaders[fn.Sel.Name]
	}
	return false
}
