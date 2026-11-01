package toolname_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/toolname/toolnametest"
)

// toolNames is every tool name the engine offers a model, the set the walk
// below holds every literal to: every tool-shaped verb of the authority table
// (a verb with no dot, which internal/agent/builtin's walks hold equal to the
// tools its registry and the operator catalogue register, in both directions)
// and every name this package declares, read off its own source so a name is
// covered the day it is added here. Among those are the tools a phase surface
// offers of its own (the submissions, the discovery pair, `delegate` and the
// executor's `ask_and_wait`), which no row names. ONE SET, read in
// [toolnametest.Names], which the runner's suite holds every such tool to as
// well, so a surface that offers a tool neither source names fails there
// rather than the walk passing over it unread.
func toolNames(t *testing.T) []string {
	t.Helper()
	return toolnametest.Names(t)
}

// spelledElsewhere is every literal that spells a tool name ON PURPOSE, keyed
// on the file and the name, each with why a constant would be wrong there.
//
// Each is the word in ANOTHER VOCABULARY — a wire value, a question's name, an
// authority verb, the Dashboard Assistant's name for one of its own tools —
// that a rename of the tool must NOT move. An entry nothing fires is
// reported, so an exemption outlives neither its literal nor its reason.
var spelledElsewhere = map[string]map[string]string{
	// THE ESTATE'S OPERATION NAMES are wire values a node sends another,
	// pinned byte for byte by the envelope gate: a fleet mid-upgrade runs two
	// builds, and a tool renamed in one must not rename the operation the
	// other serves.
	"internal/estate/ops.go": {
		"create_channel": "an estate operation's wire name",
		"mark_inbox":     "an estate operation's wire name",
		"my_work":        "an estate operation's wire name",
		"write_project":  "an estate operation's wire name",
	},
	// EVENT PAYLOAD VALUES: the event envelope evolves additive-only, and a
	// row written before a tool's rename keeps the word it was written with.
	"internal/events/types/auxiliary.go": {
		"answer_knowledge": "an auxiliary purpose on the event wire",
	},
	"internal/events/types/knowledge.go": {
		"get_page": "a knowledge read's channel on the event wire",
	},
	// THE INBOX QUESTION is the query registry's own name for the question
	// the tool also answers — the socket protocol's and the dashboard
	// contract's word, which a tool's rename must not move. The assistant's
	// exposure table names it too, as the question its inbox read is built
	// on, and the registry's own walk holds that row to a question it
	// answers.
	"internal/api/queries/answers.go": {
		"work_inbox": "the inbox question's name in the query registry",
	},
	"internal/api/rest.go": {
		"work_inbox":        "the inbox question's name in the query registry",
		"code_repo_readers": "a code question's name in the query registry",
		"code_log":          "a code question's name in the query registry",
		"code_blame":        "a code question's name in the query registry",
		"code_diff":         "a code question's name in the query registry",
		"code_compare":      "a code question's name in the query registry",
	},
	// THE CODE HOST'S QUESTIONS share their words with the code tools that
	// answer a seat the same reads: the dashboard's Code screens ask them by
	// these names (its contract's domains and protocol types), so a tool's
	// rename must not move the question.
	"internal/api/queries/code.go": {
		"code_repo_readers": "a code question's name in the query registry",
		"code_log":          "a code question's name in the query registry",
		"code_blame":        "a code question's name in the query registry",
		"code_diff":         "a code question's name in the query registry",
		"code_compare":      "a code question's name in the query registry",
	},
	"internal/api/queries/codegit.go": {
		"code_log":     "a code question's name in the query registry",
		"code_blame":   "a code question's name in the query registry",
		"code_diff":    "a code question's name in the query registry",
		"code_compare": "a code question's name in the query registry",
	},
	"internal/assistant/catalog/exposure.go": {
		"work_inbox": "the inbox question's name in the query registry",
	},
	// THE DASHBOARD ASSISTANT'S TOOL NAMES are its own words, recorded on
	// every step and proposal a conversation keeps and named by the eval
	// suites operators write, so a curated tool that shares its word with
	// an engine tool keeps it when the engine's tool is renamed. The
	// catalogue declares each such word once, in this one file, and reads
	// every engine tool's name it needs off the authority verb.
	"internal/assistant/catalog/toolnames.go": {
		"get_artifact":     "a Dashboard Assistant tool's own name",
		"get_page":         "a Dashboard Assistant tool's own name",
		"list_artifacts":   "a Dashboard Assistant tool's own name",
		"list_projects":    "a Dashboard Assistant tool's own name",
		"mark_inbox":       "a Dashboard Assistant tool's own name",
		"search_knowledge": "a Dashboard Assistant tool's own name",
		"share_artifact":   "a Dashboard Assistant tool's own name",
		"steer_turn":       "a Dashboard Assistant tool's own name",
		"unshare_artifact": "a Dashboard Assistant tool's own name",
	},
}

// authorityTable is the file the authority table's verbs are declared in.
//
// A VERB A TOOL SERVES IS NAMED AFTER THE TOOL, so the table spells every tool
// name, and it is the one other place a name may be spelled. For the
// artifacts' tools it is the name's DECLARATION — internal/agent/builtin reads
// each artifacts tool's name off its row — except for the two a format's
// reference names, which this package declares and their rows read. For every
// other tool it is a copy of the owner's constant, and it stays one on
// purpose: internal/authz cannot read the tracker's, the chat's or the pages'
// constants without importing the store and the broker into a package that
// decides over values, and it need not, because internal/agent/builtin's
// TestEveryRegisteredToolHasAnAuthorityRule and
// TestEveryToolShapedActionIsARegisteredTool hold the table to the names this
// build registers in both directions — a rename that left a row behind fails
// the build there. Anything ELSE the file spells is a copy like any other.
const authorityTable = "internal/authz/rules.go"

// NO TOOL NAME IS SPELLED TWICE. Every string literal in the module's non-test
// Go source is read, and one that carries a tool name as a word of its own is
// either that name's ONE declaration — the whole value of a constant — or a
// copy, which fails naming the file and line: a prompt, a description, a
// refusal or a deny list that spells the name instead of reading its
// constant is a copy a rename leaves behind, compiling, telling a model to
// call a tool that no longer exists or exempting one that does not. And every
// name is declared exactly once, so there is one constant to read.
//
// EVERY TOOL A MODEL IS OFFERED, not only the ones this package declares: the
// walk used to read only this package's names, and the tools' own
// descriptions, schemas and results spelled some two hundred names their
// owners — the tracker, the chat, the pages, the builtins — declared as
// constants, each a copy nothing compared. And the set fails closed: a tool a
// phase surface offers beyond the seat's registry, which no authority row
// names, is held to it by the runner's suite, which is how `ask_and_wait`,
// declared beside its implementation alone, came to be read here at all.
//
// Test files are not read — a test pins the literal on purpose, so that a
// rename is a decision its suite notices — and neither are test-support
// packages (a package whose name ends in "test"), which spell what they fake.
//
// Mutation: put "activate_tool" back in skills' exempt list, write
// "call get_work_item" into a tool's description, or spell "`ask_and_wait`"
// in the run's brief (internal/engine's askBrief), and this is red; declare a
// second constant equal to "post_message" and it names both.
func TestNoToolNameIsSpelledTwice(t *testing.T) {
	t.Parallel()
	names := toolNames(t)
	if len(names) < 50 {
		t.Fatalf("read %d tool names, so the walk asserts nothing", len(names))
	}
	root := sourcetree.Root(t)
	declared := map[string][]string{}
	tabled := map[string]bool{}
	fired := map[string]map[string]bool{}
	files, literals := 0, 0
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if strings.HasSuffix(parsed.Name.Name, "test") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files++
		exempt := spelledElsewhere[rel]
		declarations := constantLiterals(parsed)
		tags := structTags(parsed)
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Errorf("%s: unquoting %s: %v", fset.Position(lit.Pos()), lit.Value, err)
				return true
			}
			literals++
			for _, name := range names {
				if !spells(text, name) {
					continue
				}
				line := fset.Position(lit.Pos()).Line
				switch {
				case tags[lit]:
					t.Errorf("%s:%d: a struct tag spells the tool name %q, "+
						"and a tag cannot read a constant — derive the text "+
						"from the constant where the tag is read (a config "+
						"type's fieldDescriptions, for the schema)",
						rel, line, name)
				case rel == authorityTable && declarations[lit] && text == name:
					tabled[name] = true
				case exempt[name] != "":
					if fired[rel] == nil {
						fired[rel] = map[string]bool{}
					}
					fired[rel][name] = true
				case declarations[lit] && text == name:
					declared[name] = append(declared[name], rel+":"+strconv.Itoa(line))
				default:
					t.Errorf("%s:%d spells the tool name %q: read it from the "+
						"constant that declares it, so a rename reaches it",
						rel, line, name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// THE CONTROLS: a walk rooted in the wrong place, or one that read no
	// literal, passes every assertion above and protects nothing.
	if files < 500 || literals < 5000 {
		t.Fatalf("read %d files and %d string literals; the walk is not "+
			"reading the module", files, literals)
	}
	for _, name := range names {
		switch at := declared[name]; len(at) {
		case 0:
			if !tabled[name] {
				t.Errorf("no constant declares the tool name %q, so a reader "+
					"has nothing to read it from", name)
			}
		case 1:
		default:
			t.Errorf("the tool name %q is declared %d times (%v): one owner "+
				"declares it and every other reader reads that constant",
				name, len(at), at)
		}
	}
	for file, exempt := range spelledElsewhere {
		for name := range exempt {
			if !fired[file][name] {
				t.Errorf("%s is exempt for %q and no longer spells it: the "+
					"exemption goes with the literal", file, name)
			}
		}
	}
}

// structTags is every struct tag in a file. A tag is no literal a constant
// could replace — the language takes a literal there — so a tag may not spell
// a tool name at all: a config field's `desc` tag named `query_episodes`, and
// a rename would have left the published schema naming a tool that no longer
// exists. Whoever reads the tag derives that text from the constant instead
// (internal/config's fieldDescriptions).
func structTags(file *ast.File) map[*ast.BasicLit]bool {
	out := map[*ast.BasicLit]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if field, ok := n.(*ast.Field); ok && field.Tag != nil {
			out[field.Tag] = true
		}
		return true
	})
	return out
}

// constantLiterals is every string literal in a file that is the WHOLE value
// of a constant — the only place a tool name may be spelled.
func constantLiterals(file *ast.File) map[*ast.BasicLit]bool {
	out := map[*ast.BasicLit]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for _, value := range spec.(*ast.ValueSpec).Values {
				if lit, ok := value.(*ast.BasicLit); ok {
					out[lit] = true
				}
			}
		}
	}
	return out
}

// spells reports whether text carries name as a word of its own: a name
// inside a longer identifier (`activate_tools`, `xactivate_tool`) is another
// word, and a check that matched it would refuse names nobody shares.
//
// A NAME WITH NO UNDERSCORE IS ALSO AN ENGLISH WORD ("delegate"), so it counts
// only where the text marks it as code: the whole literal, in backticks, or
// called with a parenthesis. "Do not delegate further" is prose about the act,
// and a walk that refused it would make every sentence about delegating spell
// the tool's name. A snake_case name is never prose, so any word of it counts.
func spells(text, name string) bool {
	if !strings.Contains(name, "_") {
		return text == name || strings.Contains(text, "`"+name+"`") ||
			strings.Contains(text, "`"+name+"(") || strings.Contains(text, name+"(")
	}
	for from := 0; ; {
		at := strings.Index(text[from:], name)
		if at < 0 {
			return false
		}
		at += from
		end := at + len(name)
		if (at == 0 || !wordByte(text[at-1])) && (end == len(text) || !wordByte(text[end])) {
			return true
		}
		from = at + 1
	}
}

func wordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// A NAME IS MATCHED AS A WORD, so the walk refuses a copy and not a longer
// name that happens to contain one. The control the walk's verdicts rest on.
func TestANameIsMatchedAsAWordOfItsOwn(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]bool{
		"activate_tool":                true,
		"call `activate_tool(name=…)`": true,
		"activate_tools":               false,
		"preactivate_tool":             false,
		"x activate_tool":              true,
		"":                             false,
	} {
		if got := spells(text, toolname.Activate); got != want {
			t.Errorf("spells(%q) = %v, want %v", text, got, want)
		}
	}
	// AND A NAME THAT IS ALSO A WORD COUNTS ONLY AS CODE.
	for text, want := range map[string]bool{
		"delegate":                    true,
		"call `delegate` with tasks":  true,
		"`delegate(tasks=...)`":       true,
		"Do not delegate further.":    false,
		"delegate: two tasks share":   false,
		"one delegate call may carry": false,
	} {
		if got := spells(text, toolname.Delegate); got != want {
			t.Errorf("spells(%q) = %v, want %v", text, got, want)
		}
	}
}

// THIS PACKAGE IMPORTS NOTHING, and that is what lets every package that must
// name a tool read it here: the prompts import only the org model and the
// provenance fence, the skill guard only the prompts, the logger,
// internal/words and the tool layer's argument leaves, and a name kept
// anywhere that imports more is a name one of them cannot reach. The suite
// may import what it likes; the source the build compiles may not. Mutation:
// add any import to toolname.go and this is red.
func TestThePackageImportsNothing(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
	}
	if len(pkg.GoFiles) == 0 {
		t.Fatal("the package has no source the build compiles, so this asserts nothing")
	}
	if len(pkg.Imports) > 0 {
		t.Errorf("the package imports %v: a leaf every caller reaches imports nothing",
			pkg.Imports)
	}
}

// DISCOVERY IS A FRESH SLICE EACH CALL: one caller sorting or appending to
// what it was handed must not change another's list.
func TestDiscoveryHandsEachCallerItsOwnSlice(t *testing.T) {
	t.Parallel()
	first := toolname.Discovery()
	first[0] = "changed"
	if second := toolname.Discovery(); slices.Contains(second, "changed") {
		t.Errorf("a change to one caller's slice reached the next: %v", second)
	}
}
