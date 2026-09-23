// Package clientsource finds a declaration the dashboard makes, so a gate can
// hold it against the engine's own.
//
// # Why the engine reads the client at all
//
// Several lists exist twice by necessity. The dashboard is a separate build in
// a separate language that cannot import a Go identifier, so where it must
// know a set the engine owns — the event categories a filter chip offers, the
// addressable config collections a screen lists, the field names a history row
// is keyed on — it carries its own copy. Every one of those copies has drifted
// at least once, and each drift is SILENT in the direction that matters: a
// category the engine never assigns is a chip that filters a log down to
// nothing, a config kind spelled with the wrong separator is a refusal on
// every read, and a field name the history bag does not use is an attribution
// line that simply never appears.
//
// So the copy is checked. The engine side is where the check belongs, because
// the engine is the side that owns the value.
//
// # Keyed on the declaration, never on a path
//
// Every reader walks the tree and finds the declaration by its NAME, rather
// than reading a file somebody named. A gate that hard-codes where a constant
// lives breaks when a screen moves — and breaks LOUDLY but WRONGLY, reporting a
// drift between two lists neither of which changed. Keyed on the declaration,
// a move and a rename of the file are both invisible, and the two failures a
// reader can report are the two that matter: nothing declares it, which is a
// gate certifying nothing, and TWO declarations exist, which is two copies that
// can drift from each other as well as from the engine — wherever the second
// one is, including beside the first in the same file.
//
// # Keyed on the syntax, never on the layout
//
// The readers used to be regular expressions over the source, and every one of
// them was a claim about how prettier happens to lay a declaration out: a list
// ending at `\n];`, an object's keys at exactly two spaces of indent, a union
// ending at the first `;` once its comments were blanked, an interface closed
// by a `}` at column zero, a call whose string argument sits on the same line
// as its paren. Each of those held until a formatter, a longer entry or a
// comment moved something, and a regex that stops matching does not fail —
// it matches LESS, and a gate over a shorter list reports a pass it did not
// earn. A bracket inside a string closed the list early; a quoted word inside
// a comment became a member; a key that had to be quoted because it holds a
// dot (`"turn.guard_breach":`) was not read at all.
//
// So the source is SCANNED ([lex]) — strings, comments, template literals,
// regular expressions and JSX prose each recognised for what they are — and
// every reader works on tokens: [Literal] returns the balanced bracket a
// `const` is initialised with, [Scalar] the one number or string one is,
// [Union] the string members of a type alias, [Interface] the members of an
// interface and whether each is optional, and [Calls] the string each call to
// a named function is handed first. The same
// declaration laid out on one line or forty, with or without a trailing comma,
// in single quotes or double, with comments between its members, reads the
// same. What is not a literal — a spread, a member computed from something
// else — is REFUSED by name rather than skipped, because a reader that
// returns part of a set is the failure this package exists to prevent.
//
// # One registry, both ways
//
// [Contract] is the table of every declaration a gate reads: its name, the
// reader it is read with, and the one gate that owns it. A reader REFUSES a
// name the table does not carry, so a gate cannot hold a declaration nobody
// registered; and `contract_test.go` walks the table the other way — every row
// resolves to exactly one declaration in the real tree, and names a gate that
// exists and reads it. A declaration with two gates, or a gate reading a
// declaration under the wrong reader, fails there rather than drifting.
//
// # One home: `dashboard/src/contract/`
//
// Every declaration in the table lives in [ContractDir], and that directory
// holds nothing else. A copy of an engine-owned set kept beside the screen
// that draws it is one a reader of that screen edits without knowing a Go gate
// holds it, so the dashboard keeps them in PURE modules of their own — data
// and types, importing nothing but each other, which the dashboard's own
// suite enforces — and `contract_test.go` holds the directory both ways: every
// row is declared there, and every name the directory exports is a row. A
// reader still walks the WHOLE tree, so a second copy declared in a screen is
// two declarations rather than one the gate cannot see.
//
// # One reader, for the same reason as everything else here
//
// This walk was written three times before it was written here — the category
// gate, the rooms sweep and a path-reading gate each carried their own — and
// three implementations of "find the one declaration that matches" is three
// chances for one to start skipping a directory, counting files instead of
// matches, or reading a layout the others no longer rely on. A gate that reads
// less than it thinks reports a pass it did not earn.
package clientsource

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Tree is the dashboard's SOURCE, relative to a package directory under
// `internal/`. Not the built bundle: that is minified and carries no
// declaration to find, and a gate pointed at it silently matched nothing for
// the whole of the React rewrite while reporting a pass.
//
// Callers one level deeper (`internal/api/...`) join another `..` themselves.
const Tree = "../../dashboard/src"

// Body is the inside of the one array or object literal a `const` is
// initialised with: its tokens between the outermost brackets, scanned in the
// file they came from.
type Body struct {
	name   string
	array  bool // `[…]` rather than `{…}`
	source string
	toks   []token
}

// String is the body's source text, for a failure message.
func (b Body) String() string { return b.source }

// Member is one member an interface declares.
type Member struct {
	Name string
	// Optional is a member declared with `?`: the TypeScript interface says
	// every value MAY omit it, where a required one says every value carries
	// it.
	Optional bool
}

// Literal returns the array or object literal the ONE `const <name>` under
// `tree` is initialised with.
//
// The head is `const <name>`, exported or not, with any type annotation after
// it; the body is the first balanced `[…]` or `{…}` after the `=`, reached
// through nothing but a constructor's name — so `[…] as const`,
// `new Set([…])` and `{…} satisfies T` are all their bracket. EVERY
// declaration in every file is counted, at any depth: a `const CATEGORIES`
// at block scope inside a component is legal TypeScript, and with the
// module-scope copy still above it a gate validated the first while the screen
// rendered the second.
//
// A SPREAD in the literal is refused, because the members it brings are
// declared somewhere this body does not show, and a gate over what IS shown
// would certify part of a set.
func Literal(tree, name string) (Body, error) {
	if err := registered(name, ReadLiteral); err != nil {
		return Body{}, err
	}
	found, err := declaration(tree, headConst, name)
	if err != nil {
		return Body{}, err
	}
	toks, at := found.toks, found.at
	j := at + 1
	if j < len(toks) && toks[j].is(":") {
		if j, err = skipType(toks, j+1, found, "="); err != nil {
			return Body{}, err
		}
	}
	if j >= len(toks) || !toks[j].is("=") {
		return Body{}, found.errorf("const %s has no initialiser to read", name)
	}
	j++
	for j < len(toks) && !toks[j].is("[", "{") {
		switch t := toks[j]; {
		case t.kind == kIdent, t.is(".", "("):
			j++
		case t.is("<"):
			if j, err = skipAngles(toks, j, found); err != nil {
				return Body{}, err
			}
		default:
			return Body{}, found.errorf("const %s is initialised with %q, where "+
				"an array or object literal was expected", name, t.text())
		}
	}
	// `foo[0]` is an index, not a literal: the bracket must follow the `=`
	// or open a constructor's arguments.
	if j >= len(toks) || !toks[j-1].is("=", "(") {
		return Body{}, found.errorf("const %s is not initialised with an "+
			"array or object literal", name)
	}
	end, err := match(toks, j, found)
	if err != nil {
		return Body{}, err
	}
	body := Body{
		name:   name,
		array:  toks[j].is("["),
		source: found.src[toks[j].start:toks[end].end],
		toks:   toks[j+1 : end],
	}
	if spread := spreadIn(body.toks); spread >= 0 {
		return Body{}, found.errorfAt(body.toks[spread], "const %s spreads "+
			"members in from elsewhere, so no reader of this literal can see "+
			"all of it — declare the members here", name)
	}
	return body, nil
}

// Union returns the string members of the ONE `type <name> = …` under `tree`.
//
// EVERY MEMBER MUST BE A STRING LITERAL, and anything else is an error naming
// it: a member that refers to another type, or a suffix that turns the union
// into something else, is a member no gate can compare, and skipping it would
// report the union as smaller than it is. Comments between members are
// comments, whatever they quote and wherever their semicolons fall.
func Union(tree, name string) ([]string, error) {
	if err := registered(name, ReadUnion); err != nil {
		return nil, err
	}
	found, err := declaration(tree, headType, name)
	if err != nil {
		return nil, err
	}
	toks := found.toks
	j := found.at + 1
	if j < len(toks) && toks[j].is("<") {
		if j, err = skipAngles(toks, j, found); err != nil {
			return nil, err
		}
	}
	if j >= len(toks) || !toks[j].is("=") {
		return nil, found.errorf("type %s has no `=`", name)
	}
	j++
	if j < len(toks) && toks[j].is("|") {
		j++
	}
	var members []string
	for {
		if j >= len(toks) {
			return nil, found.errorf("type %s ends before its next member", name)
		}
		value, ok := toks[j].literal()
		if !ok || toks[j].kind == kTemplate && !complete(toks[j].text()) {
			return nil, found.errorfAt(toks[j], "type %s has the member %q, "+
				"which is not a string literal — a gate can compare only what "+
				"the union spells out", name, toks[j].text())
		}
		members = append(members, value)
		j++
		if j < len(toks) && toks[j].is("|") {
			j++
			continue
		}
		break
	}
	// The union must END here: at a semicolon, the end of the file or of the
	// block it is in, or a line break that begins the next statement.
	if j < len(toks) && !toks[j].is(";", "}") && !toks[j].nl {
		return nil, found.errorfAt(toks[j], "type %s continues past its "+
			"string members with %q, so it is not a union of strings", name,
			toks[j].text())
	}
	return members, nil
}

// Scalar returns the one value the ONE `const <name>` under `tree` is
// initialised with: a string's value, or a number's source text, a leading
// minus included. The gate parses the number, so a numeric separator or a
// radix prefix reads as the value it spells rather than as a mismatch.
//
// ONE LITERAL AND NOTHING ELSE, bar a type annotation before the `=` and an
// `as const` after the value. `200 * 2`, `LIMIT` and `limits.feed` are each a
// value somewhere else, and reading the first token of an expression would
// hand a gate the one part of the arithmetic it can see.
func Scalar(tree, name string) (string, error) {
	if err := registered(name, ReadScalar); err != nil {
		return "", err
	}
	found, err := declaration(tree, headConst, name)
	if err != nil {
		return "", err
	}
	toks := found.toks
	j := found.at + 1
	if j < len(toks) && toks[j].is(":") {
		if j, err = skipType(toks, j+1, found, "="); err != nil {
			return "", err
		}
	}
	if j >= len(toks) || !toks[j].is("=") {
		return "", found.errorf("const %s has no initialiser to read", name)
	}
	j++
	negative := j < len(toks) && toks[j].is("-")
	if negative {
		j++
	}
	if j >= len(toks) {
		return "", found.errorf("const %s ends before its value", name)
	}
	var value string
	switch t := toks[j]; {
	case t.kind == kNumber:
		value = t.text()
		if negative {
			value = "-" + value
		}
	case !negative && (t.kind == kString || t.kind == kTemplate && complete(t.text())):
		value, _ = t.literal()
	default:
		return "", found.errorfAt(t, "const %s is initialised with %q, where one "+
			"number or string was expected", name, t.text())
	}
	j++
	if j+1 < len(toks) && toks[j].word("as") && toks[j+1].word("const") {
		j += 2
	}
	// The statement must END here: at a semicolon, the end of the file or of
	// the block it is in, or a line break that begins the next statement. A
	// line break alone does not end one — `400\n  * 2` is one expression, and
	// so is a value followed on the next line by `(` or a template — so the
	// next token has to be one that can only start a statement of its own.
	if j < len(toks) && !toks[j].is(";", "}") && !startsStatement(toks[j]) {
		return "", found.errorfAt(toks[j], "const %s continues past its value "+
			"with %q, so it is an expression rather than one literal", name,
			toks[j].text())
	}
	return value, nil
}

// startsStatement is a token that, after a line break, begins the next
// statement rather than continuing the expression before it.
func startsStatement(t token) bool {
	if !t.nl {
		return false
	}
	switch t.kind {
	case kIdent:
		return !t.word("as") && !t.word("satisfies") && !t.word("in") &&
			!t.word("instanceof")
	case kString, kNumber:
		return true
	}
	return false
}

// Interface returns the members of the ONE `interface <name>` under `tree`,
// in the order they are declared, with what it extends read first.
//
// THE MEMBERS, NOT THE LINES THEY ARE ON: a member ends at its `;` or `,`, or
// at the line break TypeScript itself would end it at, and a type spanning
// several lines — a long union, a nested object — is one member however it is
// wrapped. An index signature (`[key: string]: unknown`) names no member and
// is not one. What the interface EXTENDS is read the same way and comes first,
// a member it redeclares taking the redeclaration's optionality; an `extends`
// with type arguments is refused, because which members a `Pick<…>` or an
// `Omit<…>` leaves is a type computation, not something a declaration shows.
func Interface(tree, name string) ([]Member, error) {
	if err := registered(name, ReadInterface); err != nil {
		return nil, err
	}
	return interfaceMembers(tree, name, nil)
}

func interfaceMembers(tree, name string, seen []string) ([]Member, error) {
	if slices.Contains(seen, name) {
		return nil, fmt.Errorf("clientsource: interface %s extends itself through %s",
			name, strings.Join(seen, " → "))
	}
	seen = append(seen, name)
	found, err := declaration(tree, headInterface, name)
	if err != nil {
		return nil, err
	}
	toks := found.toks
	j := found.at + 1
	if j < len(toks) && toks[j].is("<") {
		if j, err = skipAngles(toks, j, found); err != nil {
			return nil, err
		}
	}
	var members []Member
	if j < len(toks) && toks[j].word("extends") {
		for j++; j < len(toks) && !toks[j].is("{"); j++ {
			t := toks[j]
			switch {
			case t.is(","):
			case t.kind == kIdent:
				if j+1 < len(toks) && toks[j+1].is("<", ".") {
					return nil, found.errorfAt(t, "interface %s extends %s with "+
						"type arguments or a qualified name, and which members "+
						"that leaves is not something a declaration shows", name,
						t.text())
				}
				inherited, inheritErr := interfaceMembers(tree, t.text(), seen)
				if inheritErr != nil {
					return nil, fmt.Errorf("clientsource: interface %s extends %s: %w",
						name, t.text(), inheritErr)
				}
				members = merge(members, inherited)
			default:
				return nil, found.errorfAt(t, "interface %s extends %q, which "+
					"is not an interface name", name, t.text())
			}
		}
	}
	if j >= len(toks) || !toks[j].is("{") {
		return nil, found.errorf("interface %s has no body", name)
	}
	end, err := match(toks, j, found)
	if err != nil {
		return nil, err
	}
	own, err := typeMembers(toks[j+1:end], found, name)
	if err != nil {
		return nil, err
	}
	return merge(members, own), nil
}

// merge appends `more` to `members`, a redeclared member replacing the one
// before it in place.
func merge(members, more []Member) []Member {
	for _, m := range more {
		if i := slices.IndexFunc(members, func(have Member) bool {
			return have.Name == m.Name
		}); i >= 0 {
			members[i] = m
			continue
		}
		members = append(members, m)
	}
	return members
}

// typeMembers reads the members of an object type's body.
func typeMembers(toks []token, found decl, name string) ([]Member, error) {
	var members []Member
	for i := 0; i < len(toks); {
		t := toks[i]
		if t.is(";", ",") {
			i++
			continue
		}
		start := i
		// `readonly name: T`, `get name(): T`, `set name(v: T)` — but
		// `readonly: boolean` is a member named readonly.
		if (t.word("readonly") || t.word("get") || t.word("set")) &&
			i+1 < len(toks) && startsName(toks[i+1]) {
			i++
			t = toks[i]
		}
		switch {
		case t.is("["):
			// An index signature, or a computed key: no member to name.
			end, err := match(toks, i, found)
			if err != nil {
				return nil, err
			}
			i = end + 1
		case t.is("(", "<"), t.word("new") && i+1 < len(toks) && toks[i+1].is("(", "<"):
			// A call or construct signature: callable, not a member.
		case t.kind == kIdent, t.kind == kString, t.kind == kNumber:
			m := Member{Name: t.text()}
			if t.kind == kString {
				m.Name = t.value
			}
			i++
			if i < len(toks) && toks[i].is("?") {
				m.Optional = true
				i++
			}
			members = append(members, m)
		default:
			return nil, found.errorfAt(t, "interface %s has %q where a member "+
				"was expected", name, t.text())
		}
		var err error
		if i, err = memberEnd(toks, i, start, found); err != nil {
			return nil, err
		}
	}
	return members, nil
}

// memberEnd is the index just past the member that began at `start` and
// whose type starts at i.
//
// A `;` or a `,` at depth zero ends it, and so does a LINE BREAK where
// TypeScript's own parser would end it: before a token that cannot continue a
// type, after one that cannot end one. So `a:\n | "x"\n | "y"` is one member
// and `a: string\n b?: number` is two, whichever way a formatter wraps them.
// Never at the member's own first token, which a line break precedes as a
// matter of course — a call signature's `(` is where the member starts, not
// where the one before it ended.
func memberEnd(toks []token, i, start int, found decl) (int, error) {
	depth := 0
	for k := i; k < len(toks); k++ {
		t := toks[k]
		if depth == 0 {
			if t.is(";", ",") {
				return k + 1, nil
			}
			if t.nl && k > start && !continuesAfter(toks[k-1]) && !continuesBefore(t) {
				return k, nil
			}
		}
		switch {
		case t.is("(", "[", "{", "<"):
			depth++
		case t.is(")", "]", "}", ">"):
			depth--
			if depth < 0 {
				return 0, found.errorfAt(t, "a %q closes nothing", t.text())
			}
		}
	}
	return len(toks), nil
}

// continuesAfter is a token a type cannot end on.
func continuesAfter(t token) bool {
	if t.kind == kIdent {
		switch t.text() {
		case "extends", "keyof", "typeof", "infer", "is", "in", "as", "readonly",
			"new", "unique", "asserts", "abstract":
			return true
		}
		return false
	}
	return t.is("|", "&", ":", "?", "=>", ".", "?.", ",", "(", "[", "{", "<", "=", "...")
}

// continuesBefore is a token a type can continue with at the start of a line.
func continuesBefore(t token) bool {
	return t.word("extends") || t.word("is") || t.word("as") ||
		t.is("|", "&", "?", ":", ".", "?.", "=>", ")", "]", "}", ">", ",", ";", "=")
}

func startsName(t token) bool {
	return t.kind == kIdent || t.kind == kString || t.kind == kNumber || t.is("[")
}

// Calls returns, for every call under `tree` to one of `callees`, the string
// it is handed as its first argument, mapped to the files (relative to the
// tree, slash-separated, sorted) that make it.
//
// A call is the callee's name — bare, or as a method (`socket.query(`) —
// optionally with type arguments (`useQuery<"x">(`) or an optional chain
// (`f?.(`), then a `(` whose first argument is a string literal on its own:
// followed by `,` or `)`, however many lines it sits across. A call handed
// anything else first — a variable, an expression, a string with something
// added to it — names no kind this package can read, and is not in the
// answer. That is what a hook's own definition looks like
// (`useQuery(what, …)`), and it is why a screen passing a kind through a
// variable is invisible here: the dashboard's own `app/source.test.ts` is
// where that is refused. It reads the calls this does — `useQuery` and
// `query` by spelling, bare or as a method, whatever they are bound to — and
// the literal this does, a quoted string or a template with nothing
// substituted into it, so a call it passes is one this reads.
func Calls(tree string, callees ...string) (map[string][]string, error) {
	files, err := scan(tree)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, f := range files {
		toks := f.toks
		for i, t := range toks {
			if t.kind != kIdent || !slices.Contains(callees, t.text()) {
				continue
			}
			j := i + 1
			if j < len(toks) && toks[j].is("<") {
				// Type arguments — or a comparison (`query < limit`), which
				// is not a call and is not an error either.
				var angled error
				if j, angled = skipAngles(toks, j, decl{file: f}); angled != nil {
					continue
				}
			}
			if j < len(toks) && toks[j].is("?.") {
				j++
			}
			if j+2 >= len(toks) || !toks[j].is("(") || !toks[j+2].is(",", ")") {
				continue
			}
			arg, ok := toks[j+1].literal()
			if !ok || toks[j+1].kind == kTemplate && !complete(toks[j+1].text()) {
				continue
			}
			if !slices.Contains(out[arg], f.rel) {
				out[arg] = append(out[arg], f.rel)
			}
		}
	}
	for _, files := range out {
		slices.Sort(files)
	}
	return out, nil
}

// Strings is every string a literal's body holds, in order.
//
// WHATEVER IS BETWEEN THE QUOTES, never a shape that looks right. A pattern
// that matched only well-formed values silently skipped the malformed one —
// so a config kind spelled `mcp_servers` was not extracted at all, and the
// gate passed on a screen that could never read that collection. A string
// inside a comment is not in the body, because a comment is not.
func Strings(body Body) []string {
	var out []string
	for _, t := range body.toks {
		if value, ok := t.literal(); ok {
			out = append(out, value)
		}
	}
	return out
}

// Field is the value of every `name: "…"` property in a literal's body, at
// any depth — for a list of objects rather than a list of strings. The key may
// be bare or quoted; the value is read only when it is a string.
func Field(body Body, name string) []string {
	toks := body.toks
	var out []string
	for i := 0; i+2 < len(toks); i++ {
		key := toks[i]
		keyName, quoted := key.literal()
		switch {
		case key.kind == kIdent && key.text() == name:
		case quoted && key.kind == kString && keyName == name:
		default:
			continue
		}
		// A KEY sits where a property starts; `cond ? kind : "x"` is not one.
		if i > 0 && !toks[i-1].is("{", ",") {
			continue
		}
		if !toks[i+1].is(":") {
			continue
		}
		if value, ok := toks[i+2].literal(); ok {
			out = append(out, value)
		}
	}
	return out
}

// Keys is the keys of an object literal's own properties, in order — the
// top level only, bare or quoted alike.
//
// QUOTED KEYS ARE KEYS. A key holding a dot cannot be written bare
// (`"turn.guard_breach": …`), and a reader that took only the bare form would
// stop covering exactly the entries most likely to need it. A computed key
// (`[name]: …`) is refused, because what it names is a value somewhere else.
func Keys(body Body) ([]string, error) {
	if body.array {
		return nil, fmt.Errorf("clientsource: %s is an array, and has no keys", body.name)
	}
	toks := body.toks
	var out []string
	for i := 0; i < len(toks); {
		t := toks[i]
		if t.is(",") {
			i++
			continue
		}
		// `get x()`, `set x(v)`, `async x()`, `*x()`: the name follows.
		if (t.word("get") || t.word("set") || t.word("async")) &&
			i+1 < len(toks) && startsName(toks[i+1]) && !toks[i+1].is("[") {
			i++
			t = toks[i]
		}
		if t.is("*") && i+1 < len(toks) {
			i++
			t = toks[i]
		}
		switch {
		case t.kind == kIdent || t.kind == kNumber:
			out = append(out, t.text())
		case t.kind == kString:
			out = append(out, t.value)
		default:
			return nil, fmt.Errorf("clientsource: %s has a property keyed by %q, "+
				"which names no key a reader can see", body.name, t.text())
		}
		// On to the next property at depth zero.
		depth := 0
		for i++; i < len(toks); i++ {
			if depth == 0 && toks[i].is(",") {
				break
			}
			switch {
			case toks[i].is("(", "[", "{"):
				depth++
			case toks[i].is(")", "]", "}"):
				depth--
			}
		}
	}
	return out, nil
}

// file is one scanned source file.
type file struct {
	rel  string // relative to the tree, slash-separated
	src  string
	toks []token
}

// decl is one declaration: the file it is in, and where its name token is.
type decl struct {
	file
	at int
}

func (f decl) errorf(format string, args ...any) error {
	return f.errorfAt(f.toks[f.at], format, args...)
}

func (f decl) errorfAt(t token, format string, args ...any) error {
	line := strings.Count(f.src[:t.start], "\n") + 1
	return fmt.Errorf("clientsource: %s:%d: %s", f.rel, line, fmt.Sprintf(format, args...))
}

// head is the keyword a kind of declaration starts with.
type head string

const (
	headConst     head = "const"
	headType      head = "type"
	headInterface head = "interface"
)

// declaration is the ONE declaration of `name` as `kind` under `tree`.
//
// The error says which of the two failures happened, because they have
// different remedies: none means the declaration was renamed or removed and
// the gate now certifies nothing; two means a second copy exists and must go.
func declaration(tree string, kind head, name string) (decl, error) {
	files, err := scan(tree)
	if err != nil {
		return decl{}, err
	}
	var hits []decl
	for _, f := range files {
		for _, at := range heads(f.toks, kind, name) {
			hits = append(hits, decl{file: f, at: at})
		}
	}
	if len(hits) != 1 {
		var where []string
		for _, h := range hits {
			where = append(where, fmt.Sprintf("%s:%d", h.rel,
				strings.Count(h.src[:h.toks[h.at].start], "\n")+1))
		}
		return decl{}, fmt.Errorf("clientsource: %d declarations of %s %s under "+
			"%s %v, want exactly one — none is a gate certifying nothing, and two "+
			"are two copies that can drift from each other as well as from the "+
			"engine", len(hits), kind, name, tree, where)
	}
	return hits[0], nil
}

// heads is the index of the name token of every `kind` declaration of `name`
// in one file's tokens.
//
// A `type` is a declaration only when an `=` or its type parameters follow
// the name, which is what tells `type QueryErrorCode = …` from the
// `import { type QueryErrorCode }` a screen writes to use it.
func heads(toks []token, kind head, name string) []int {
	var out []int
	for i := 0; i+1 < len(toks); i++ {
		if !toks[i].word(string(kind)) || !toks[i+1].word(name) {
			continue
		}
		if i > 0 && toks[i-1].is(".", "?.") {
			continue
		}
		next := token{}
		if i+2 < len(toks) {
			next = toks[i+2]
		}
		switch kind {
		case headConst:
		case headType:
			if !next.is("=", "<") {
				continue
			}
		case headInterface:
			if !next.is("{", "<") && !next.word("extends") {
				continue
			}
		}
		out = append(out, i+1)
	}
	return out
}

// scan reads and lexes every source file under `tree`.
//
// THE SUITES ARE EXCLUDED, or a case that quotes a constant to assert its
// shape would count as a second declaration of it, and a call a test makes
// would count as a screen reading a query. A file that does not scan FAILS the
// read rather than being skipped: a gate that reads less than it thinks
// reports a pass it did not earn.
func scan(tree string) ([]file, error) {
	var files []file
	err := filepath.WalkDir(tree, func(path string, entry os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			return nil
		case !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx"):
			return nil
		case strings.Contains(entry.Name(), ".test."):
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(tree, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		toks, err := lexed(rel, string(source), strings.HasSuffix(path, ".tsx"))
		if err != nil {
			return err
		}
		files = append(files, file{rel: rel, src: string(source), toks: toks})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("clientsource: the dashboard tree at %s could not be "+
			"read, so this gate certifies nothing: %w", tree, err)
	}
	return files, nil
}

// skipType is the index of the first `stop` at depth zero in a type starting
// at j — angle brackets counting as brackets, which in a type they are.
func skipType(toks []token, j int, found decl, stop string) (int, error) {
	depth := 0
	for ; j < len(toks); j++ {
		t := toks[j]
		if depth == 0 && t.is(stop) {
			return j, nil
		}
		switch {
		case t.is("(", "[", "{", "<"):
			depth++
		case t.is(")", "]", "}", ">"):
			depth--
			if depth < 0 {
				return 0, found.errorfAt(t, "a %q closes nothing", t.text())
			}
		}
	}
	return 0, found.errorf("the type after %s never reaches its %q",
		found.toks[found.at].text(), stop)
}

// skipAngles is the index just past the `<…>` group opening at j.
func skipAngles(toks []token, j int, found decl) (int, error) {
	depth := 0
	for k := j; k < len(toks); k++ {
		switch {
		case toks[k].is("<"):
			depth++
		case toks[k].is(">"):
			depth--
			if depth == 0 {
				return k + 1, nil
			}
		}
	}
	return 0, found.errorfAt(toks[j], "a `<` is never closed")
}

// match is the index of the bracket closing the one at j, counting (), [] and
// {} alike.
func match(toks []token, j int, found decl) (int, error) {
	depth := 0
	for k := j; k < len(toks); k++ {
		switch {
		case toks[k].is("(", "[", "{"):
			depth++
		case toks[k].is(")", "]", "}"):
			depth--
			if depth == 0 {
				return k, nil
			}
		}
	}
	return 0, found.errorfAt(toks[j], "a %q is never closed", toks[j].text())
}

// spreadIn is the index of the first `...` in a literal's body that spreads
// into the literal itself or into an array or object inside it — never into a
// call's arguments or a function's parameters, which are not members. -1 when
// there is none.
func spreadIn(toks []token) int {
	var open []bool // for each open bracket: is it `(`?
	for i, t := range toks {
		switch {
		case t.is("(", "[", "{"):
			open = append(open, t.is("("))
		case t.is(")", "]", "}"):
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
		case t.is("..."):
			if len(open) == 0 || !open[len(open)-1] {
				return i
			}
		}
	}
	return -1
}

// lexed is [lex] for one file of the tree, remembered for the life of the
// process.
//
// KEYED ON THE CONTENT, never on the path. Every reader scans the whole tree,
// a gate reads several declarations and the contract reads all of them, so
// without this a package's gates lexed the same few megabytes once per
// declaration — ten seconds under the race detector for the contract alone.
// A path-keyed cache would answer a file's OLD tokens to a test that rewrote
// it; a content-keyed one cannot be stale, because different content is a
// different key.
func lexed(rel, src string, jsx bool) ([]token, error) {
	key := lexKey{rel: rel, src: src, jsx: jsx}
	if hit, ok := lexCache.Load(key); ok {
		result := hit.(lexResult)
		return result.toks, result.err
	}
	toks, err := lex(rel, src, jsx)
	lexCache.Store(key, lexResult{toks: toks, err: err})
	return toks, err
}

type lexKey struct {
	rel, src string
	jsx      bool
}

type lexResult struct {
	toks []token
	err  error
}

var lexCache sync.Map // lexKey → lexResult

// exports is every name the source files under `tree` export, mapped to the
// files (relative to the tree, slash-separated) that export it.
//
// A DECLARATION PER NAME, and everything else refused: a re-export
// (`export { x } from …`, `export * from …`), a default export, and a
// destructuring or multi-name `const` each export a name this walk would have
// to resolve somewhere else to see — and a walk that returned the names it
// could see would report a module as exporting less than it does, which is
// the one failure the contract's "every export is a row" direction cannot
// afford. `export` as an object key or a member name is not a statement.
func exports(tree string) (map[string][]string, error) {
	files, err := scan(tree)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, f := range files {
		toks := f.toks
		for i, t := range toks {
			if !t.word("export") || i > 0 && toks[i-1].is(".", "?.") {
				continue
			}
			found := decl{file: f, at: i}
			name, err := exported(toks, i+1, found)
			if err != nil {
				return nil, err
			}
			if name != "" {
				out[name] = append(out[name], f.rel)
			}
		}
	}
	return out, nil
}

// exported is the name an `export` whose next token is at j declares, or ""
// when the `export` is not a statement at all.
func exported(toks []token, j int, found decl) (string, error) {
	refuse := func(what string) (string, error) {
		return "", found.errorf("this exports through %s, which a walk of "+
			"the module's exports cannot see — declare the name here, one per "+
			"statement", what)
	}
	for j < len(toks) && (toks[j].word("declare") || toks[j].word("abstract") ||
		toks[j].word("async")) {
		j++
	}
	if j >= len(toks) {
		return refuse("nothing")
	}
	t := toks[j]
	switch {
	case t.is(":", "(", ",", ")", "}", "?", "?."):
		// A key or a member named `export`, not a statement.
		return "", nil
	case t.word("const"), t.word("let"), t.word("var"):
		if j+1 >= len(toks) || toks[j+1].kind != kIdent {
			return refuse("a destructuring pattern")
		}
		if err := oneDeclarator(toks, j+2, found); err != nil {
			return "", err
		}
		return toks[j+1].text(), nil
	case t.word("function"):
		j++
		if j < len(toks) && toks[j].is("*") {
			j++
		}
	case t.word("type"):
		j++
		if j < len(toks) && toks[j].is("{", "*") {
			return refuse("a type re-export")
		}
	case t.word("class"), t.word("interface"), t.word("enum"), t.word("namespace"),
		t.word("module"):
		j++
	case t.word("default"):
		return refuse("a default export")
	case t.is("{"):
		return refuse("an export list")
	case t.is("*"):
		return refuse("a star re-export")
	default:
		return refuse(fmt.Sprintf("%q", t.text()))
	}
	if j >= len(toks) || toks[j].kind != kIdent {
		return refuse("an unnamed declaration")
	}
	return toks[j].text(), nil
}

// oneDeclarator refuses a `const` statement that declares a second name after
// the one whose annotation or initialiser starts at j.
func oneDeclarator(toks []token, j int, found decl) error {
	if j < len(toks) && toks[j].is(":") {
		end, err := skipType(toks, j+1, found, "=")
		if err != nil {
			return err
		}
		j = end
	}
	depth := 0
	for ; j < len(toks); j++ {
		t := toks[j]
		if depth == 0 && (t.is(";", "}") || startsStatement(t)) {
			return nil
		}
		switch {
		case t.is("(", "[", "{"):
			depth++
		case t.is(")", "]", "}"):
			depth--
		case t.is("<") && j > 0 && toks[j-1].kind == kIdent:
			// Type arguments (`new Set<string>(…)`), whose commas are not
			// declarators — or a comparison, which has none to skip.
			if end, err := skipAngles(toks, j, found); err == nil {
				j = end - 1
			}
		case depth == 0 && t.is(","):
			return found.errorfAt(t, "a second name in one exported "+
				"statement, which a walk of the module's exports would miss — "+
				"declare one name per statement")
		}
	}
	return nil
}
