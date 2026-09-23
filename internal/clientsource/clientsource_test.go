package clientsource_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// tree writes a dashboard-shaped directory: each entry is a path under it and
// the source at that path.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, source := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// literalStrings is the strings of the one CATEGORIES literal in a tree
// holding `source` as one screen.
func literalStrings(t *testing.T, path, source string) []string {
	t.Helper()
	body, err := clientsource.Literal(tree(t, map[string]string{path: source}), "CATEGORIES")
	if err != nil {
		t.Fatalf("Literal: %v", err)
	}
	return clientsource.Strings(body)
}

// A LITERAL IS THE SAME WHATEVER THE LAYOUT.
//
// Every reader this replaced was a regular expression shaped like prettier's
// output — a list ending at `] as const`, a key at two spaces of indent — and
// a formatter change, a longer entry or a comment was enough to make one match
// less and report a pass over a shorter list. The same declaration on one line
// or many, exported or not, annotated or not, quoted either way, with or
// without a trailing comma and a semicolon, built through a constructor, and
// with comments between its members, reads as the same two strings.
func TestALiteralIsTheSameWhateverTheLayout(t *testing.T) {
	t.Parallel()
	layouts := map[string]string{
		"one line":     `const CATEGORIES = ["task", "system"] as const;`,
		"one per line": "export const CATEGORIES = [\n  \"task\",\n  \"system\",\n] as const;\n",
		"single quotes, no semicolon": "const CATEGORIES = ['task', 'system']\n" +
			"export const OTHER = 1\n",
		"annotated": "export const CATEGORIES: readonly (\"task\" | \"x\")[] = [\"task\", \"system\"];",
		"a constructor": "export const CATEGORIES: ReadonlySet<string> = new Set<string>([\n" +
			"  \"task\", \"system\"]);",
		"commented": "const CATEGORIES = [\n  // the work tracker's own rows\n  \"task\", /* and */\n" +
			"  \"system\" // the engine's\n] as const",
		"a template":        "const CATEGORIES = [`task`, \"sys\\u0074em\"] satisfies string[];",
		"no space anywhere": `const CATEGORIES:string[]=["task","system"]`,
	}
	for name, source := range layouts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := literalStrings(t, "routes/activity/Activity.tsx", source)
			if !slices.Equal(got, []string{"task", "system"}) {
				t.Errorf("strings = %q, want [task system]", got)
			}
		})
	}
}

// A BRACKET IN A STRING OR A COMMENT DOES NOT CLOSE THE LITERAL, AND A QUOTE
// IN A COMMENT DOES NOT OPEN A MEMBER.
//
// A pattern ending at the first `]` stops inside a string holding one and
// reads a shorter list; a pattern pulling every quoted run out of the body
// reads a word a comment quotes as a member. Both are the silent narrowing —
// or widening — a gate over the list cannot see.
func TestABracketInAStringOrCommentDoesNotCloseTheLiteral(t *testing.T) {
	t.Parallel()
	got := literalStrings(t, "routes/activity/Activity.tsx",
		"const CATEGORIES = [\n"+
			"  \"a]\", /* ] as const; */ \"b\",\n"+
			"  // \"chore\" left with the retired types ]\n"+
			"  'c\\'s]', `d]`,\n"+
			"] as const;\n"+
			"const NEXT = [\"not\", \"in\", \"it\"];\n")
	if want := []string{"a]", "b", "c's]", "d]"}; !slices.Equal(got, want) {
		t.Errorf("strings = %q, want %q", got, want)
	}
}

// JSX PROSE AND A REGULAR EXPRESSION ARE NOT CODE.
//
// Both are real in this tree and both undo a quote-counting scan: a sentence
// of JSX text says "the company's sealed credentials", and a regular
// expression's class holds a brace or a quote (`/^\}/`). Read as code, the
// apostrophe opens a string that runs to the next one and the brace
// unbalances everything after it — so a declaration after either is missed,
// or a phrase inside one is taken for a declaration. The same goes for the
// escapes a TAGGED template may carry and a string may not (`String.raw` over
// `\users`), which are text there rather than a file that fails to scan.
func TestJSXProseAndARegularExpressionAreNotCode(t *testing.T) {
	t.Parallel()
	source := `import { type ReactNode } from "react";

const tidy = (s: string) => s.replace(/^\}/, "").replace(/["'{[]+/g, "-") / 2;
const PATH = String.raw` + "`C:\\users\\x{[\\d+\\]`" + `;

export function Screen<T extends string>(props: { lens: T }): ReactNode {
  const pick = <K,>(value: K) => value;
  const both = <A extends string>(value: A) => value;
  const wrapped = <B extends
    string>(value: B) => value;
  if (props.lens.length < 3 && tidy("x") > 1) return null;
  return (
    <section aria-label="the company's log" data-x='it"s'>
      <Segmented<Lens> value={pick("a")} render={() => <b>it's {"}"}</b>} />
      <>
        The company's sealed credentials — const CATEGORIES = ["prose"] as const;
        {/* a comment's apostrophe: don't */}
        {items.map((item) => (
          <Row key={item.id} note={` + "`${item.name}'s`" + `} />
        ))}
      </>
    </section>
  );
}

const CATEGORIES = ["task", "system"] as const;
`
	got := literalStrings(t, "routes/activity/Activity.tsx", source)
	if !slices.Equal(got, []string{"task", "system"}) {
		t.Errorf("strings = %q, want the one declaration after the markup", got)
	}
}

// TWO DECLARATIONS IN ONE FILE ARE TWO.
//
// The walk once matched once per FILE and appended one entry for each, so it
// counted files, and a second declaration beside the first — a `const
// CATEGORIES = [...] as const` at block scope inside a component, which is
// legal TypeScript — passed the gate unseen. The gate then validated the
// module-scope list while the screen rendered the other one, which is exactly
// the silent drift this package exists to report.
func TestASecondDeclarationInTheSameFileIsTwo(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/activity/Activity.tsx": "const CATEGORIES = [\"task\", \"system\"] as const;\n" +
			"export function Activity() {\n" +
			"  const CATEGORIES = [\"task\", \"chore\"] as const;\n" +
			"  return <ul>{CATEGORIES.map((c) => <li key={c}>{c}</li>)}</ul>;\n" +
			"}\n",
	})
	_, err := clientsource.Literal(root, "CATEGORIES")
	if err == nil {
		t.Fatal("two declarations in one file passed the gate")
	}
	// NAMING HOW MANY AND WHERE, because the remedy differs: none means the
	// gate certifies nothing, and more than one means a copy has to go.
	for _, want := range []string{"2 declarations", "Activity.tsx:1", "Activity.tsx:3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}

// AND TWO FILES ARE TOO — the failure that was always reported, kept so the
// count is over declarations in both arrangements rather than over one.
func TestASecondDeclarationInAnotherFileIsTwo(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/activity/Activity.tsx": "const CATEGORIES = [\"task\"] as const;\n",
		"routes/inbox/Inbox.tsx":       "export const CATEGORIES = [\"system\"];\n",
	})
	if _, err := clientsource.Literal(root, "CATEGORIES"); err == nil {
		t.Fatal("two files declaring it passed the gate")
	}
}

// NOTHING DECLARING IT IS AN ERROR, NOT AN EMPTY LIST.
//
// A gate comparing against an empty list certifies nothing and, in its
// subset direction, passes. So each reader refuses — including when the name
// is only USED, never declared, which is what an import of a type is.
func TestNoDeclarationIsAnErrorNotAnEmptyList(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/activity/Activity.tsx": "import { type QueryErrorCode, type IntegrationRow } " +
			"from \"../protocol/types\";\n" +
			"const KINDS = [\"task\"] as const;\nconsole.log(CATEGORIES);\n" +
			"let x: QueryErrorCode = \"closed\"; x = CATEGORIES.type;\n",
	})
	// NAMING THAT NONE WAS FOUND, which is a different remedy from two.
	if _, err := clientsource.Literal(root, "CATEGORIES"); err == nil ||
		!strings.Contains(err.Error(), "0 declarations") {
		t.Errorf("err = %v, want a tree declaring no CATEGORIES refused as having none", err)
	}
	if _, err := clientsource.Union(root, "QueryErrorCode"); err == nil {
		t.Error("a tree only importing QueryErrorCode read as a union")
	}
	if _, err := clientsource.Interface(root, "IntegrationRow"); err == nil {
		t.Error("a tree only importing IntegrationRow read as an interface")
	}
}

// THE SUITES ARE NOT DECLARATIONS.
//
// A case that quotes a constant to assert its shape would otherwise count as
// a second copy of it and fail every gate, and a call a test makes would
// count as a screen reading a query.
func TestTheSuitesAreNotDeclarations(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/activity/Activity.tsx":      "const CATEGORIES = [\"task\", \"system\"] as const;\n",
		"routes/activity/Activity.test.tsx": "const CATEGORIES = [\"task\"] as const;\nuseQuery(\"events\");\n",
		"lib/range.ts":                      "export const SPANS = [1, 2];\n",
	})
	body, err := clientsource.Literal(root, "CATEGORIES")
	if err != nil {
		t.Fatalf("Literal: %v", err)
	}
	if got := clientsource.Strings(body); !slices.Equal(got, []string{"task", "system"}) {
		t.Errorf("strings = %q, want the screen's own two", got)
	}
	calls, err := clientsource.Calls(root, "useQuery")
	if err != nil {
		t.Fatalf("Calls: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("calls = %v, want none: the only one is in a suite", calls)
	}
}

// AN OPTIONAL MEMBER IS OPTIONAL, AND A MEMBER IS A MEMBER HOWEVER IT WRAPS.
//
// The two halves of an interface are different contracts — a required member
// is required of every value, an optional one of none — so a gate needs to
// know which is which. And the reader this replaced took a member to be a line
// at two spaces of indent inside a `}` at column zero, so a type wrapped over
// several lines, a nested object and a one-line interface each read as
// something else.
func TestAnOptionalMemberIsOptional(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"protocol/types.ts": `export interface IntegrationRow {
  key: string;
  /** a doc comment: label?: not a member; */
  label?: string
  "quoted-name"?: number,
  readonly configured: boolean;
  readonly: boolean
  state:
    | "live"
    | "stale";
  nested: {
    inner?: string;
    deeper: { x: number };
  }
  handler?(event: { a: string }): void;
  generic: Map<string, Array<{ id: string }>>
  [key: string]: unknown;
  (event: string): void
  new (x: number): IntegrationRow
  get size(): number
  untyped
}
export interface Other { key: string }
`,
	})
	got, err := clientsource.Interface(root, "IntegrationRow")
	if err != nil {
		t.Fatalf("Interface: %v", err)
	}
	want := []clientsource.Member{
		{Name: "key"}, {Name: "label", Optional: true}, {Name: "quoted-name", Optional: true},
		{Name: "configured"}, {Name: "readonly"}, {Name: "state"}, {Name: "nested"},
		{Name: "handler", Optional: true}, {Name: "generic"}, {Name: "size"}, {Name: "untyped"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("members = %v, want %v", got, want)
	}
}

// AN INTERFACE'S MEMBERS INCLUDE WHAT IT EXTENDS.
//
// A row type built on a shared base declares half its members elsewhere, and
// a gate over only the own half would certify an answer missing the other.
// What an `extends` with type arguments leaves is a type computation, so that
// is refused rather than guessed at.
func TestAnInterfaceReadsWhatItExtends(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"protocol/types.ts": "interface Bucket { tokens: number; calls?: number }\n" +
			"interface Named { name: string }\n" +
			"export interface IntegrationRow extends Bucket, Named { calls: number; key: string }\n" +
			"export interface IntegrationsAnswer extends Omit<Bucket, \"calls\"> { rows: string[] }\n",
	})
	got, err := clientsource.Interface(root, "IntegrationRow")
	if err != nil {
		t.Fatalf("Interface: %v", err)
	}
	want := []clientsource.Member{{Name: "tokens"}, {Name: "calls"}, {Name: "name"}, {Name: "key"}}
	if !slices.Equal(got, want) {
		t.Errorf("members = %v, want %v — the base's first, a redeclared one taking "+
			"the redeclaration's optionality", got, want)
	}
	if _, err := clientsource.Interface(root, "IntegrationsAnswer"); err == nil {
		t.Error("an interface extending Omit<…> read as though its members were known")
	}
}

// A UNION IGNORES ITS COMMENTS.
//
// The members' doc comments are prose: they quote words ("there is nothing")
// and carry semicolons of their own. The reader this replaced had to blank
// them before looking for the union's end, or a comment could end the union
// early or add a quoted word as a member.
func TestAUnionIgnoresItsComments(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"protocol/types.ts": `/** The codes. */
export type QueryErrorCode =
  | "unknown_query"
  /** The caller's fault; retrying sends "the same bad request" again. */
  | "bad_params" // and not "there is nothing";
  | 'timeout'
export type WorkViewShape = "list" | "board";
`,
	})
	got, err := clientsource.Union(root, "QueryErrorCode")
	if err != nil {
		t.Fatalf("Union: %v", err)
	}
	if want := []string{"unknown_query", "bad_params", "timeout"}; !slices.Equal(got, want) {
		t.Errorf("members = %q, want %q", got, want)
	}
	one, err := clientsource.Union(root, "WorkViewShape")
	if err != nil || !slices.Equal(one, []string{"list", "board"}) {
		t.Errorf("a one-line union = %q, %v", one, err)
	}
}

// A UNION THAT IS NOT ALL STRINGS IS REFUSED, not read as the strings it has.
func TestAUnionThatIsNotAllStringsIsRefused(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]string{
		"a type reference": `export type QueryErrorCode = "closed" | ClientCode;`,
		"an array suffix":  `export type QueryErrorCode = "closed" | "timeout"[];`,
		"a template":       "export type QueryErrorCode = \"closed\" | `x_${string}`;",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := tree(t, map[string]string{"protocol/types.ts": source})
			if got, err := clientsource.Union(root, "QueryErrorCode"); err == nil {
				t.Errorf("read as %q; want a refusal naming the member it cannot compare", got)
			}
		})
	}
}

// A QUOTED KEY IS A KEY, AND A VALUE IS NOT ONE.
//
// A key holding a dot cannot be written bare, so the first event type of that
// shape an object keys on arrives quoted — and a reader taking only the bare
// form, or taking every quoted string, reads it wrongly either way. The
// reader this replaced also needed each key at the start of its own line.
func TestAQuotedKeyIsAKey(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"lib/turnstory.ts": "export const ABSORBED: Record<string, string> = { agent_phase_started: " +
			"\"the phase card it opens\", \"turn.guard_breach\": 'the header: its own', " +
			"'prompt.size': `the ${'prompt'} line`, nested: { inner: \"x\", deeper: [\"y\"] }, " +
			"shorthand, method() { return { notAKey: 1 }; }, get read() { return 2; }, 3: \"n\" };",
	})
	body, err := clientsource.Literal(root, "ABSORBED")
	if err != nil {
		t.Fatalf("Literal: %v", err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	want := []string{"agent_phase_started", "turn.guard_breach", "prompt.size", "nested",
		"shorthand", "method", "read", "3"}
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %q, want %q", keys, want)
	}
}

// FIELD READS A PROPERTY, NOT EVERY `name:` IN SIGHT.
//
// `kind: "x"` inside a conditional expression is a colon after a word, not a
// property; and a property whose value is not a string has no value to read.
func TestFieldReadsAPropertyNotEveryColon(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"lib/work.ts": "export const CHANGES = [\n" +
			"  { kind: \"created\", mark: \"add\" },\n" +
			"  { \"kind\": 'moved', mark: cond ? kind : \"not-a-kind\" },\n" +
			"  { kind: KIND_CONST, mark: \"x\" },\n" +
			"];",
	})
	body, err := clientsource.Literal(root, "CHANGES")
	if err != nil {
		t.Fatalf("Literal: %v", err)
	}
	if got := clientsource.Field(body, "kind"); !slices.Equal(got, []string{"created", "moved"}) {
		t.Errorf("kinds = %q, want [created moved]", got)
	}
}

// A SPREAD IS REFUSED, because the members it brings are declared somewhere
// the literal does not show. A spread into a CALL inside a member is an
// argument, not a member, and is not.
func TestASpreadIsRefused(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]string{
		"into the list":   `const CATEGORIES = [...BASE, "task"] as const;`,
		"into an element": `const CATEGORIES = [{ ...base, value: "task" }];`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := tree(t, map[string]string{"routes/Activity.tsx": source})
			if _, err := clientsource.Literal(root, "CATEGORIES"); err == nil ||
				!strings.Contains(err.Error(), "spreads") {
				t.Errorf("err = %v, want the spread refused", err)
			}
		})
	}
	root := tree(t, map[string]string{
		"routes/Activity.tsx": `const CATEGORIES = [label("task", ...rest), "system"];`,
	})
	if _, err := clientsource.Literal(root, "CATEGORIES"); err != nil {
		t.Errorf("a spread into a call's arguments was refused: %v", err)
	}
}

// A DECLARATION THE CONTRACT DOES NOT NAME IS REFUSED, and so is one read
// with a reader the contract does not give it.
//
// This is the half of "one registry, both ways" the contract's own test
// cannot walk: a gate holding a declaration nobody registered is a comparison
// no table knows about.
func TestAReaderRefusesWhatTheContractDoesNotName(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"lib/x.ts": "export const UNREGISTERED = [\"a\"];\nexport type CATEGORIES = \"a\";\n",
	})
	if _, err := clientsource.Literal(root, "UNREGISTERED"); err == nil ||
		!strings.Contains(err.Error(), "not in the contract") {
		t.Errorf("err = %v, want an unregistered declaration refused", err)
	}
	if _, err := clientsource.Union(root, "CATEGORIES"); err == nil ||
		!strings.Contains(err.Error(), "reads CATEGORIES as a literal") {
		t.Errorf("err = %v, want a declaration read with the wrong reader refused", err)
	}
}

// A FILE THAT DOES NOT SCAN FAILS THE READ, rather than being skipped: a gate
// that reads fewer files than it thinks reports a pass it did not earn.
//
// AND A BRACKET CLOSING THE WRONG KIND OF BRACKET IS A FILE THAT DOES NOT
// SCAN. It is the one symptom a scanner that lost its place is nearly certain
// to show, so it is checked for every bracket rather than only the braces the
// scanner's modes turn on.
func TestAFileThatDoesNotScanFailsTheRead(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]string{
		"a string":      "const x = \"never closed;\n",
		"a comment":     "/* never closed\n",
		"a regex":       "const r = /never closed\n",
		"a brace":       "function f() {\n",
		"a paren":       "const x = f(1, [2]\n",
		"a tag":         "const e = <div>never closed;\n",
		"the wrong one": "const x = [f(1]);\n",
		"nothing":       "const x = 1);\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := tree(t, map[string]string{
				"routes/Activity.tsx": "const CATEGORIES = [\"task\"] as const;\n",
				"routes/Broken.tsx":   source,
			})
			if _, err := clientsource.Literal(root, "CATEGORIES"); err == nil ||
				!strings.Contains(err.Error(), "Broken.tsx:1") {
				t.Errorf("err = %v, want the broken file named", err)
			}
		})
	}
}

// CALLS SEES A MULTI-LINE FIRST ARGUMENT.
//
// A formatter wraps a call whose arguments do not fit, and `useQuery(\n
// "config_diff",` is the same call as the one that fits on a line; so is a
// method call, one with type arguments, and one inside markup or a template's
// substitution. The files are named relative to the tree, once each however
// many times a file makes the call.
func TestCallsSeesAMultiLineFirstArgument(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/admin/Config.tsx": "const one = useQuery(\n  \"config_diff\",\n  { id },\n" +
			"  { enabled: true },\n);\nconst two = useQuery(\"config_diff\");\n" +
			"const three = useQuery<\"stream\">('stream');\n",
		"routes/Activity.tsx": "const page = await socket.query(\n\t// the log\n\t\"events\", params);\n" +
			"export const A = () => <p>it's {useQuery(\"a2a_channels\").data}</p>;\n" +
			"const s = `${query(\"event\")}`;\n",
	})
	got, err := clientsource.Calls(root, "useQuery", "query")
	if err != nil {
		t.Fatalf("Calls: %v", err)
	}
	want := map[string][]string{
		"config_diff":  {"routes/admin/Config.tsx"},
		"stream":       {"routes/admin/Config.tsx"},
		"events":       {"routes/Activity.tsx"},
		"a2a_channels": {"routes/Activity.tsx"},
		"event":        {"routes/Activity.tsx"},
	}
	if len(got) != len(want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	for kind, files := range want {
		if !slices.Equal(got[kind], files) {
			t.Errorf("%s is called from %v, want %v", kind, got[kind], files)
		}
	}
}

// CALLS IGNORES A VARIABLE FIRST ARGUMENT.
//
// A hook's own definition, a generic wrapper and a computed name all hand the
// callee something other than a kind this package can read; none of them is
// in the answer, and nor is a name the callee merely resembles.
func TestCallsIgnoresAVariableFirstArgument(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"lib/useQuery.ts": "export function useQuery<K extends QueryName>(what: K, params?: P) {\n" +
			"  return socket.query(what, params);\n}\n" +
			"const a = useQuery(kind);\nconst b = useQuery(\"a\" + suffix);\n" +
			"const c = useQuery(cond ? \"x\" : \"y\");\nconst d = refetchQuery(\"nope\");\n" +
			"const e = useQueryState(\"nope\");\nconst f = query.name(\"nope\");\n" +
			"const g = useQuery(`t_${x}`);\nconst h = query < limit;\n",
	})
	got, err := clientsource.Calls(root, "useQuery", "query")
	if err != nil {
		t.Fatalf("Calls: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}
