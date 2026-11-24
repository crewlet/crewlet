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

// modulePath is the module every import path below is under.
const modulePath = "github.com/crewlet/crewlet"

// modelInputs are the types a model is handed its prompt in, by the import
// path of the package that declares them: a provider's request
// (internal/providers/llm), and every request a harness's run is launched or
// continued from. Every seat's executor is such a run (ADR-0066).
//
// AND WHAT A RUN IS HANDED TO REMEMBER: a continued session is a model's
// context before it reads its brief, so the type naming the restored session
// (sandbox.CarriedSession) is an input, and so is the continuation telling it
// what it lacks (SessionContinuation, ExactTelling).
var modelInputs = map[string][]string{
	modulePath + "/internal/providers/llm": {"Request"},
	modulePath + "/internal/agent/runner": {"AgentRunRequest", "Continuation", "SessionContinuation",
		"ExactTelling"},
	modulePath + "/internal/sandbox": {"LaunchRequest", "Continuation", "RunRequest", "SessionContinuation",
		"ExactTelling", "CarriedSession"},
}

// modelCall says, for one function that builds a model's input (one of
// [modelInputs]), what the call reads of somebody else's words and what holds
// it: the tests that certify its fences (each `<package dir>.<TestName>`, and
// each must exist), or — for a call that reads nobody's words — the reason it
// reads none.
type modelCall struct {
	tests []string
	none  string
}

// modelCalls is EVERY function in the tree that builds a model's input, a
// provider request or an executor's launch, keyed `<file>:<function>` (or
// `<file>:<variable>` for one a package-level variable holds). An input built
// anywhere else fails the gate below until somebody decides what it reads
// (ADR-0040): every model call that reads text somebody else wrote reads it
// fenced under a token of its own call (or, for the seat itself, the turn's),
// beside the standing sentence, and what it writes back into a seat's prompt
// is defused. The list was a set of reads nobody had walked — the knowledge
// answer, the extension judge and four post-turn workers all read foreign
// text bare until it existed.
var modelCalls = map[string]modelCall{
	"internal/agent/toolloop/toolloop.go:Run": {tests: []string{
		// The engine's own model loop: a seat's reviewer, its onboarding
		// pass and the workers it delegates to, never its executor, which
		// is its harness's run (runner.executeAsAgentRun, below). Every
		// foreign word in the conversation arrived fenced under the turn's
		// token, by a wake or by a read.
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
	// THE SEAT'S EXECUTOR, its harness's run (ADR-0066): launched with the
	// seat's system prompt and the turn's brief. Every foreign word in either
	// arrived fenced under the token of the segment that built it (the trigger,
	// the prefetched blocks, what the run's tools read over the bridge), the
	// system says what a fence is, and a reply to the run's question is a chat
	// participant's words, fenced as the trigger labelled them.
	//
	// AND HOW THE CONVERSATION'S EARLIER TURNS ENDED, which every brief carries
	// as its dispatch rendered it: every account somebody else wrote in it
	// (what a turn was asked, its reviewer's and executor's words, a stop's
	// reason, the engine's accounts that quote a box) fenced under that
	// segment's token, and every name and engine word defused.
	"internal/agent/runner/agentrun.go:executeAsAgentRun": {tests: []string{
		"internal/engine.TestEveryWakeSurfaceFencesWhatSomebodyWrote",
		"internal/engine.TestTheTurnsAskIsFencedAsItsTriggerIs",
		"internal/agent/prefetch.TestEveryBlockDefusesWhatItReads",
		"internal/agent/builtin.TestEveryUntrustedReadIsFenced",
		"internal/agent/runner.TestALaunchedRunIsToldWhatAFenceIs",
		"internal/agent/runner.TestAFreshRunsEarlierAnswersAreFencedUnderItsToken",
		"internal/agent/prompts.TestAConversationsEndingsAreFencedUnderTheTurnsToken",
	}},
	// A RELAUNCH CONTINUES THE TURN'S SESSION (continued.go), holding what
	// earlier segments read, fenced under THEIR tokens: the mix holds because
	// no foreign word carries an unescaped marker of any token
	// (provenance.Neutralise, ADR-0040). What the continuation adds is fenced
	// or defused under the relaunching segment's token.
	//
	// TWO THINGS RIDE UNFENCED, defused, ON PURPOSE: an answer by turn and a
	// note a person sends the turn. Each is an instruction from somebody the
	// engine let direct it (ADR-0068, authz.ActionExecutorAnswer; ADR-0067),
	// sealed on the run's record with who gave it (ADR-0031); a fence would
	// tell the run not to follow the one instruction it waited for.
	"internal/agent/runner/agentrun.go:continueAgentRun": {tests: []string{
		"internal/agent/runner.TestARunReadsAReplyFencedAndAnAnswerByTurnAsItsAnswerersOwn",
		"internal/agent/runner.TestAFreshRunsToldCallsAreDefusedUnderItsToken",
		"internal/agent/runner.TestAToldAgainAnswerIsFencedUnderTheRelaunchsToken",
		"internal/agent/runner.TestAFreshRunsEarlierAnswersAreFencedUnderItsToken",
		"internal/engine.TestAChatReplyToAParkedRunIsRecordedAsWhatThePersonTyped",
		"internal/engine.TestEveryWakeSurfaceFencesWhatSomebodyWrote",
		"internal/agent/prefetch.TestEveryBlockDefusesWhatItReads",
		"internal/agent/builtin.TestEveryUntrustedReadIsFenced",
		"internal/agent/runner.TestALaunchedRunIsToldWhatAFenceIs",
		"internal/agent/prompts.TestAConversationsEndingsAreFencedUnderTheTurnsToken",
	}},
	// TRANSPORTS of the runner's launch: the system and the brief it built,
	// which its entries above hold, the continuation it composed, and the
	// engine's own words beside them (how to ask a person, from the chart's
	// names; the box the run works in, from the company's configuration).
	"internal/engine/agentrun.go:launch": {tests: []string{
		"internal/agent/runner.TestALaunchedRunIsToldWhatAFenceIs",
		"internal/agent/runner.TestARunReadsAReplyFencedAndAnAnswerByTurnAsItsAnswerersOwn",
		"internal/engine.TestALaunchedRunIsToldHowToGetAPersonsAnswer",
	}},
	"internal/engine/agentrun.go:sandboxContinuation": {tests: []string{
		"internal/agent/runner.TestARunReadsAReplyFencedAndAnAnswerByTurnAsItsAnswerersOwn",
	}},
	// THE CONTINUATION OF A KEPT SESSION, composed by the runner and carried
	// by the engine to the launch: the engine's own words and the seat's, the
	// notes the session has not read — of a session cut off, every note the
	// turn has read, under the engine's header — as their senders' (each
	// defused, as a note in any brief is), the calls made after the session
	// was saved defused under the turn's token, and the engine's record of the
	// session's box and branch defused; and, of a session cut off, the leads its
	// job's brief opened with told again, a chat reply fenced under the
	// relaunching segment's token and every other word defused.
	// keptSession fences and defuses each part before the relaunch's last
	// budget check; led only puts the lead in front of them once that check has
	// passed, so it is led's request this holds.
	"internal/agent/runner/continued.go:led": {tests: []string{
		"internal/agent/runner.TestASentBackSessionIsToldWhatItLacks",
		"internal/agent/runner.TestAToldAgainAnswerIsFencedUnderTheRelaunchsToken",
		"internal/agent/runner.TestACarriedOnSessionIsToldWhatCameAfterItWasSaved",
		"internal/agent/runner.TestAnAnsweredOrARefusedRunsSessionIsContinuedToldWhatCarriesItOn",
		"internal/agent/runner.TestASessionContinuedInANewBoxIsToldWhatThatBoxHolds",
		"internal/agent/runner.TestALaunchedRunIsToldWhatAFenceIs",
	}},
	// AND BESIDE IT THE ONE TOLD BY A READING OF WHAT A CUT-OFF SESSION HOLDS,
	// composed by the same keptSession before the budget check, its calls
	// defused under the turn's token and the subagents it names, the box's
	// words, defused and cut; led only puts the lead in front of them once the
	// check has passed, and attaches them where their own first request sends
	// the session whole.
	"internal/agent/runner/firstrequest.go:led": {tests: []string{
		"internal/agent/runner.TestACutOffKeptSessionIsToldOnlyTheCallsItsReadingDoesNotHold",
		"internal/agent/runner.TestASubagentNamedInABriefIsDefusedAndCut",
		"internal/agent/runner.TestALaunchedRunIsToldWhatAFenceIs",
	}},
	// THE CONTINUATION OF THE SESSION A CONVERSATION'S LAST TURN LEFT: the
	// engine's own lead — the turn boundary and the new box, the base's turn
	// and home defused under the launching segment's token, in a schedule's
	// words for a schedule's fire — then the launch's own fresh brief whole,
	// whose fences its own entries hold, the turn's earlier answers among it.
	"internal/agent/runner/continued.go:baseSession": {tests: []string{
		"internal/agent/runner.TestAConversationsNextTurnIsToldWhereItBegins",
		"internal/agent/runner.TestAFreshRunsToldCallsAreDefusedUnderItsToken",
		"internal/agent/runner.TestAFreshRunsEarlierAnswersAreFencedUnderItsToken",
		"internal/agent/runner.TestAConversationsNextTurnIsToldWhatItsNewBoxHolds",
		"internal/agent/runner.TestAScheduledFireIsToldWhereItBegins",
		"internal/agent/runner.TestABaseContinuationArmsAnEmptyGuard",
		"internal/agent/runner.TestALaunchedRunIsToldWhatAFenceIs",
	}},
	// AND ITS TRANSPORT to the sandbox layer: the two briefs the runner
	// composed, each where the box it was written for gets it, and beside a
	// kept capture's the two told by a reading of what it holds.
	"internal/engine/executorrestore.go:sandboxSession": {tests: []string{
		"internal/engine.TestARelaunchContinuesItsSessionInTheBoxItGetsWithTheBriefForIt",
		"internal/engine.TestTheBriefToldByAReadingReachesTheSandboxBesideTheOtherTwo",
		"internal/engine.TestTheFrozenBaseOutlivesASiblingsEnd",
		"internal/agent/runner.TestASentBackSessionIsToldWhatItLacks",
	}},
	// THE RESTORE CARRIER, built by the one function that chooses it: which
	// session object a run is handed to remember. The turn's own — the kept
	// capture of the job the relaunch follows, of the seat whose run it is —
	// read off the turn's own row and opened against that row's associated
	// data, so no other seat's, turn's or job's session opens into the run.
	"internal/sandbox/restore.go:continueKept": {tests: []string{
		"internal/sandbox.TestARestoredSessionIsTheSeatsAndTheTurnsOwn",
		"internal/sandbox.TestARelaunchContinuesOnlyTheSessionOfTheJobItFollows",
		"internal/sandbox.TestTheBriefMatchesWhatTheRunHas",
		"internal/sandbox.TestABriefToldByAReadingIsHandedOnlyToABoxThatAnswersItsRelease",
		"internal/executordomain.TestAStoredSessionOpensOnlyForItsOwnRow",
	}},
	// AND OF THE SESSION A CONVERSATION'S LAST TURN LEFT, the base the turn's
	// first launch froze: that turn's own object, of the launching seat, read
	// off that turn's own row only while the turn's row holds it and it still
	// names the object the read froze, opened against that row's associated
	// data — never another conversation's, and never one a read made since.
	"internal/sandbox/restore.go:continueBase": {tests: []string{
		"internal/sandbox.TestATurnsFirstLaunchContinuesItsConversationsHead",
		"internal/sandbox.TestARelaunchOnAnotherNodeAfterASiblingEndedContinuesTheFrozenBase",
		"internal/sandbox.TestARelaunchContinuesTheBaseOnlyWhileTheTurnHasKeptNone",
		"internal/sandbox.TestABaseIsContinuedOnlyInABoxOfItsOwn",
		"internal/executordomain.TestAStoredSessionOpensOnlyForItsOwnRow",
	}},
	"internal/sandbox/launch.go:Launch": {tests: []string{
		"internal/agent/runner.TestALaunchedRunIsToldWhatAFenceIs",
		"internal/agent/runner.TestARunReadsAReplyFencedAndAnAnswerByTurnAsItsAnswerersOwn",
	}},
	"internal/engine/providerpark.go:providerProbe": {
		none: "the probe asks a fixed prompt the engine wrote, and reads nothing of " +
			"its answer but whether one came"},
	// A web search: the run is handed the query alone, with no tool but the
	// vendor's search, and what it answers is the addresses its search tool
	// returned, never the run's prose.
	"internal/providers/llm/harness/search.go:Search": {tests: []string{
		"internal/providers/llm/harness.TestTheSearchRunHasNoToolButTheVendorsSearch",
		"internal/providers/llm/harness.TestASearchResultsURLIsTakenFromTheToolResultNotTheProse",
	}},
	"internal/providers/llm/harness/doctor.go:probeRequest": {
		none: "every probe the doctor sends (the smoke test, the shell probe and " +
			"the web probe) asks a fixed prompt the engine wrote"},
	"internal/sandbox/executor/detached.go:releaseAsk": {
		none: "it builds an empty request only to take the launch's pins for a " +
			"box's `--version`, which runs no model and hands it no words"},
	// The summary of a conversation's earlier messages: the cards they showed
	// read fenced under the call's token, and the summary defused.
	"internal/assistant/summary.go:summaryRequest": {tests: []string{
		"internal/assistant.TestASummaryReadsWhatItsMessagesShowedFencedUnderATokenOfItsOwn",
		"internal/assistant.TestALongConversationIsSummarizedForItsNextTurn",
	}},
	"internal/assistant/conversations.go:nameConversation": {
		none: "it titles a person's own conversation from their question and the " +
			"assistant's answer to it, which the assistant wrote for them after reading " +
			"everybody else's words fenced; and the title reaches that person's list of " +
			"conversations, never a model's prompt"},
}

// EVERY MODEL CALL IS ACCOUNTED FOR (ADR-0040). The gate walks every non-test
// Go file in the tree for a composite literal of a model's input
// ([modelInputs]) and requires the function that builds it, or the variable
// that holds it, to be in [modelCalls] (with the tests that hold its fences,
// each of which must exist, or the reason it reads nobody's words), and every
// entry there to still build one. The walk, not the list, finds a new call: it
// is refused until somebody decides what it reads and names what holds it.
//
// Mutation: launch an executor from a function nothing lists, or hold a
// provider request in a package-level variable nothing lists, and this is
// red.
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

// modelCallSites is every `<file>:<function>` (or `<file>:<variable>`) that
// builds a model's input ([modelInputs]), in the order found, and how many
// files the walk parsed.
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// THE INPUT TYPES THIS FILE CAN NAME: each input package's under the
		// name the file imports it by, and its own package's bare.
		named := map[string][]string{}
		if own, ok := modelInputs[modulePath+"/"+filepath.ToSlash(filepath.Dir(rel))]; ok {
			named[""] = own
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			inputs, ok := modelInputs[p]
			if !ok {
				continue
			}
			alias := p[strings.LastIndexByte(p, '/')+1:]
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			named[alias] = inputs
		}
		if len(named) == 0 {
			return nil
		}
		builds := func(n ast.Node) bool {
			found := false
			ast.Inspect(n, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				switch typ := lit.Type.(type) {
				case *ast.SelectorExpr:
					if x, ok := typ.X.(*ast.Ident); ok && x.Name != "" &&
						slices.Contains(named[x.Name], typ.Sel.Name) {
						found = true
					}
				case *ast.Ident:
					if slices.Contains(named[""], typ.Name) {
						found = true
					}
				}
				return true
			})
			return found
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if builds(decl) {
					sites = append(sites, rel+":"+decl.Name.Name)
				}
			case *ast.GenDecl:
				// A PACKAGE-LEVEL VARIABLE holds an input as surely as a
				// function builds one: the provider probe's request is one.
				for _, spec := range decl.Specs {
					values, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range values.Names {
						if i < len(values.Values) && builds(values.Values[i]) {
							sites = append(sites, rel+":"+name.Name)
						}
					}
				}
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
