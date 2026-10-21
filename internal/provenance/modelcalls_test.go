package provenance

import (
	"bufio"
	"go/ast"
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
)

// llmPackage is the import path of the model contract, whose Request every
// model call is built as.
const llmPackage = "github.com/crewlet/crewlet/internal/providers/llm"

// modelCall says, for one function that builds an [llm.Request], what the call
// reads of somebody else's words and what holds it: the tests that certify its
// fences (each `<package dir>.<TestName>`, and each must exist), or — for a
// call that reads nobody's words — the reason it reads none.
type modelCall struct {
	tests []string
	none  string
}

// modelCalls is EVERY function in the tree that builds a model request, keyed
// `<file>:<function>`. A request built anywhere else fails the gate below
// until somebody decides what it reads (ADR-0040): every model call that reads
// text somebody else wrote reads it fenced under a token of its own call (or,
// for the seat itself, the turn's), beside the standing sentence, and what it
// writes back into a seat's prompt is defused. The list was a set of reads
// nobody had walked — the knowledge answer, the extension judge and four
// post-turn workers all read foreign text bare until it existed.
var modelCalls = map[string]modelCall{
	"internal/agent/toolloop/toolloop.go:Run": {tests: []string{
		// The seat's own conversation: every foreign word in it arrived
		// fenced under the turn's token, by a wake or by a read.
		"internal/engine.TestEveryWakeSurfaceFencesWhatSomebodyWrote",
		"internal/agent/builtin.TestEveryUntrustedReadIsFenced",
		"internal/agent/prefetch.TestEveryBlockDefusesWhatItReads",
		// And the Dashboard Assistant's, the loop's other caller: every
		// answer it reads, and the screen it was asked from, arrive fenced
		// under its turn's token.
		"internal/assistant.TestEveryAssistantToolResultIsFenced",
		"internal/assistant/catalog.TestEveryAssistantToolResultIsFenced",
		"internal/assistant.TestThePageContextIsFencedAndARemovedPageIsAbsent",
	}},
	"internal/agent/prefetch/knowledge.go:auxRequest": {tests: []string{
		"internal/agent/prefetch.TestTheTurnStartJudgementsReadSomebodyElsesWordsFenced",
		"internal/agent/prefetch.TestAPastTurnsAskIsFencedWhereverItIsRead",
		"internal/agent/prefetch.TestEveryBlockDefusesWhatItReads",
	}},
	"internal/agent/builtin/answerknowledge.go:CallForTurn": {tests: []string{
		"internal/agent/builtin.TestAnAnswerReadsItsSourcesAsTheirWritersWords",
	}},
	"internal/agent/extension/llmjudge.go:Decide": {tests: []string{
		"internal/agent/extension.TestTheJudgeReadsForeignWordsUnderItsOwnTokenAndItsReasonIsDefused",
		"internal/agent/runner.TestTheJudgeIsToldTheTurnsTokenAndItsReasonReachesTheSeatDefused",
	}},
	"internal/compact/compact.go:complete": {tests: []string{
		"internal/compact.TestARewriteReadsItsTextFencedUnderATokenOfItsOwn",
	}},
	"internal/learning/persist.go:Decide": {tests: []string{
		"internal/learning.TestEveryPostTurnWorkerReadsWhatItsTurnWasToldFenced",
		"internal/learning.TestEachWorkerCallFencesUnderATokenOfItsOwn",
	}},
	"internal/learning/profiler.go:patch": {tests: []string{
		"internal/learning.TestEveryPostTurnWorkerReadsWhatItsTurnWasToldFenced",
	}},
	"internal/learning/synthesize.go:Reflect": {tests: []string{
		"internal/learning.TestEveryPostTurnWorkerReadsWhatItsTurnWasToldFenced",
	}},
	"internal/learning/refine.go:Reflect": {tests: []string{
		"internal/learning.TestEveryPostTurnWorkerReadsWhatItsTurnWasToldFenced",
	}},
	"internal/learning/cluster.go:draftFromCluster": {tests: []string{
		"internal/learning.TestTheBackgroundPassesDefuseWhatTheyRead",
	}},
	"internal/learning/promote.go:promoteUnit": {tests: []string{
		"internal/learning.TestTheBackgroundPassesDefuseWhatTheyRead",
	}},
	"internal/engine/learning.go:auxSummarizer": {tests: []string{
		// A transport: its system and user prompts are the compaction
		// pass's, built in internal/learning.
		"internal/learning.TestTheBackgroundPassesDefuseWhatTheyRead",
	}},
	"internal/providers/llm/cliagent/doctor.go:probeRequest": {
		none: "every probe the doctor sends (the smoke test, the shell probe and " +
			"the web probe) asks a fixed prompt the engine wrote"},
	"internal/assistant/conversations.go:nameConversation": {
		none: "it titles a person's own conversation from their question and the " +
			"assistant's answer to it, which the assistant wrote for them after reading " +
			"everybody else's words fenced; and the title reaches that person's list of " +
			"conversations, never a model's prompt"},
}

// EVERY MODEL CALL IS ACCOUNTED FOR (ADR-0040). The gate walks every non-test
// Go file in the tree for a composite literal of llm.Request and requires the
// function that builds it to be in [modelCalls] — with the tests that hold its
// fences, each of which must exist, or the reason it reads nobody's words —
// and every entry there to still build one. It is the walk, not the list,
// that finds a new call: a model call added anywhere is refused until
// somebody decides what it reads and names what holds it.
func TestEveryModelCallIsAccountedFor(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	found, parsed := modelCallSites(t, root)
	if parsed < 500 {
		t.Fatalf("parsed %d Go files — the walk certified nothing", parsed)
	}
	for _, site := range found {
		call, ok := modelCalls[site]
		if !ok {
			t.Errorf("%s builds a model request nothing accounts for: decide what it "+
				"reads of somebody else's words, fence it (ADR-0040), and add it to "+
				"modelCalls with the test that holds it, or the reason it reads none", site)
			continue
		}
		if (len(call.tests) == 0) == (call.none == "") {
			t.Errorf("%s must name the tests that hold its fences or why it reads "+
				"nobody's words, and not both", site)
		}
		for _, test := range call.tests {
			if !testExists(t, root, test) {
				t.Errorf("%s is held by %s, which does not exist", site, test)
			}
		}
	}
	for site := range modelCalls {
		if !slices.Contains(found, site) {
			t.Errorf("%s no longer builds a model request: drop its entry or fix its name", site)
		}
	}
}

// modelCallSites is every `<file>:<function>` that builds an llm.Request, in
// the order found, and how many files the walk parsed.
func modelCallSites(t *testing.T, root string) ([]string, int) {
	t.Helper()
	var (
		sites  []string
		parsed int
	)
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "node_modules" ||
				name == "dashboard" || name == "static" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		parsed++
		alias := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == llmPackage {
				alias = "llm"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// The llm package's own files name the type bare.
		own := strings.HasPrefix(rel, "internal/providers/llm/") && !strings.Contains(
			strings.TrimPrefix(rel, "internal/providers/llm/"), "/")
		if alias == "" && !own {
			return nil
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			builds := false
			ast.Inspect(fn, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				switch typ := lit.Type.(type) {
				case *ast.SelectorExpr:
					if x, ok := typ.X.(*ast.Ident); ok && x.Name == alias && typ.Sel.Name == "Request" {
						builds = true
					}
				case *ast.Ident:
					if own && typ.Name == "Request" {
						builds = true
					}
				}
				return true
			})
			if builds {
				sites = append(sites, rel+":"+fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	return sites, parsed
}

// testExists reports whether `<package dir>.<TestName>` is declared, at the
// start of a line, in a test file of that directory.
func testExists(t *testing.T, root, qualified string) bool {
	t.Helper()
	dot := strings.LastIndexByte(qualified, '.')
	if dot < 0 {
		return false
	}
	dir, name := qualified[:dot], qualified[dot+1:]
	files, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(dir), "*_test.go"))
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 0, 1<<16), 1<<22)
		for scan.Scan() {
			if strings.HasPrefix(scan.Text(), "func "+name+"(") {
				_ = f.Close()
				return true
			}
		}
		_ = f.Close()
	}
	return false
}
