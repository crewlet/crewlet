package provenance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// provenancePackage is this package's import path, which every door decides a
// trust through.
const provenancePackage = "github.com/crewlet/crewlet/internal/provenance"

// surface is whose site the text a door labels is on.
type surface string

const (
	// hosted is a surface the engine hosts — its own tracker, knowledge
	// base, chat, artifacts, project files and agent-to-agent channels —
	// whose text is trusted by [TrustForHosted] and nothing else.
	hosted surface = "hosted"
	// vendor is a site the company shares with the world, whose text is
	// trusted by [TrustFor], where nobody named is `external`.
	vendor surface = "vendor"
	// either is a door that serves both and decides by what the text says
	// about where it came from: it must reach both rules.
	either surface = "either"
	// adopted is what an administrator adopted as the company's own, the
	// adopted brand's voice: the one text on a surface the engine hosts that
	// is not read by [TrustForHosted], but at [TrustCompany] and nothing
	// else, which no set of writers is ever inferred to be.
	adopted surface = "adopted"
	// scripted is what a page's script reported on behalf of whoever viewed
	// the page, such as how a page action it applied went, which the engine
	// cannot verify: read at [TrustPage] and nothing else, which no set of
	// writers is ever inferred to be either.
	scripted surface = "scripted"
)

// trustDoor is one place a trust is decided for text a model reads: whose
// surface that text is on, and the tests that hold the door to its rule (each
// `<package dir>.<TestName>`, and each must exist).
type trustDoor struct {
	surface surface
	tests   []string
}

// trustDoors is EVERY function (or package-level value) in the tree outside
// this package that decides a trust — calls one of [TrustFor],
// [TrustForPartial], [LabelFor], [EnvelopeFor], [TrustForHosted] or
// [LabelForHosted], writes a [Label] literal with a trust in it, or uses one
// of the trust levels as a value — keyed `<file>:<name>`. A door anywhere else
// fails the gate below until somebody says whose surface its text is on.
//
// It exists because the same page reached a seat by two doors and was trusted
// two ways: a wake whose record named nobody read `external` (lane b's rule for
// a vendor's payload) while the same page as a search hit read `colleague`.
// There is one rule for a surface the engine hosts, and every door to one
// takes it.
var trustDoors = map[string]trustDoor{
	"internal/notify/bodyfence.go:Label": {either, []string{
		// Every wake surface, an unnamed writer read by who hosts it.
		"internal/engine.TestEveryWakeSurfaceReadsAnUnnamedWriterByWhoHostsIt",
		"internal/engine.TestEveryWakeSurfaceFencesWhatSomebodyWrote",
		"internal/notify.TestAHostedSurfaceVouchesForAWriterItsRecordDoesNotName",
	}},
	"internal/agent/prefetch/thread.go:threadLabel": {either, []string{
		"internal/agent/prefetch.TestAThreadsUnnamedSenderIsReadByWhoHostsIt",
	}},
	"internal/agent/builtin/fence.go:label": {hosted, []string{
		"internal/agent/builtin.TestEveryUntrustedReadIsFenced",
		"internal/agent/builtin.TestAReadNamingNoWriterIsAColleagues",
	}},
	"internal/agent/builtin/answerknowledge.go:workDescription": {hosted, []string{
		"internal/agent/builtin.TestAnAnswerReadsItsSourcesAsTheirWritersWords",
	}},
	"internal/events/types/a2a.go:colleagueLabel": {hosted, []string{
		"internal/engine.TestEveryWakeTypeFencesSomebodyElsesWords",
	}},
	"internal/doclet/search.go:HitLabel": {hosted, []string{
		"internal/doclet.TestANativeHitIsAColleaguesPageWithNobodyNamed",
	}},
	"internal/confluence/search.go:HitLabel": {vendor, []string{
		"internal/confluence.TestASeatWithItsOwnCredentialSearchesUnscoped",
	}},
	"internal/agent/builtin/artifacts_brand.go:CallForTurn": {adopted, []string{
		"internal/agent/builtin.TestTheBrandVoiceArrivesFencedAsTheCompanys",
	}},
	// THE DASHBOARD ASSISTANT reads as the person it acts for and is nobody
	// of its own (ADR-0050), so no text is its own, and a route's answer names
	// none of its writers: the engine's own records are a colleague's by the
	// hosted rule, whichever of its doors they come through.
	"internal/assistant/catalog/tools.go:all": {hosted, []string{
		"internal/assistant/catalog.TestEveryAssistantToolResultIsFenced",
		"internal/assistant/catalog.TestAnAnswerIsFencedAtTheTrustItsTextEarns",
	}},
	"internal/assistant/catalog/api.go:read": {hosted, []string{
		"internal/assistant/catalog.TestAGenericReadIsFencedAtTheTrustItsRouteEarns",
	}},
	"internal/assistant/catalog/api.go:trustOf": {vendor, []string{
		// The routes whose answers carry a vendor's chat and what quoted it,
		// and any answer carrying an executor's question (Executor); every
		// other route's answer is knowledgeTrust's to decide.
		"internal/assistant/catalog.TestAGenericReadIsFencedAtTheTrustItsRouteEarns",
		"internal/agent/builtin.TestACodingRunsAskIsReadAtOneTrustThroughEveryDoor",
	}},
	"internal/assistant/catalog/reads.go:knowledgeTrust": {either, []string{
		// Doclet's pages by the hosted rule and a connected knowledge base's
		// as a vendor's, as each backend labels its own hits.
		"internal/assistant/catalog.TestAnAnswerIsFencedAtTheTrustItsTextEarns",
	}},
	"internal/assistant/catalog/reads.go:outsideWhere": {either, []string{
		// The engine's own rows, unless one carries an executor's question,
		// read by the one rule for a coding CLI's words (Executor).
		"internal/assistant/catalog.TestAnAnswerIsFencedAtTheTrustItsTextEarns",
		"internal/assistant.TestAProposalAfterOutsideTextIsTainted",
		"internal/agent/builtin.TestACodingRunsAskIsReadAtOneTrustThroughEveryDoor",
	}},
	// AN EXECUTOR'S ASK, a coding CLI's words, through every door it
	// reaches a model by: answer_executor's receipt, and the answer of the
	// assistant's write that sends it.
	"internal/agent/builtin/answerexecutor.go:CallForTurn": {vendor, []string{
		"internal/agent/builtin.TestEveryUntrustedReadIsFenced",
		"internal/agent/builtin.TestACodingRunsAskIsReadAtOneTrustThroughEveryDoor",
	}},
	"internal/assistant/catalog/writes.go:answersRun": {vendor, []string{
		"internal/agent/builtin.TestACodingRunsAskIsReadAtOneTrustThroughEveryDoor",
		"internal/assistant.TestARunAnswersReceiptIsReadAtTheRunsTrust",
	}},
	"internal/assistant/prompt.go:pageFence": {hosted, []string{
		"internal/assistant.TestThePageContextIsFencedAndARemovedPageIsAbsent",
		"internal/assistant.TestTheOpenDraftReachesTheModelFencedAndRedacted",
	}},
	"internal/assistant/resume.go:answerTrust": {hosted, []string{
		// The engine's own records, unless the write's catalogue tool says
		// its answer carries an executor's words (writes.go:answersRun).
		"internal/assistant.TestAnApprovalResumesTheParkedTurn",
		"internal/assistant.TestARunAnswersReceiptIsReadAtTheRunsTrust",
	}},
	"internal/assistant/resume.go:pageReport": {scripted, []string{
		"internal/assistant.TestAResumedTurnRunsOnTheScreenTheDecisionWasMadeFrom",
	}},
	"internal/assistant/deep.go:deepReport": {vendor, []string{
		// A deep-work run's report: a coding CLI's words after it read the
		// open web, which nobody in the company wrote (ADR-0054).
		"internal/assistant.TestADeepRunReachesCrewletOnlyThroughTheTurnsSurface",
	}},
}

// The rules each family is: the hosted rule's two functions, and the vendor
// rule's — whose levels a door writes as a value only where it is that rule —
// and the two levels no writer earns, each read only by the one door that
// knows its text is that: an adoption, and a page script's report.
var (
	hostedRule = []string{"TrustForHosted", "LabelForHosted"}
	// The vendor rule's, and the one rule for a coding CLI's words, which
	// are a vendor's model's whoever's seat the run works for (Executor).
	vendorRule   = []string{"TrustFor", "TrustForPartial", "LabelFor", "EnvelopeFor", "Executor"}
	trustLevel   = []string{"TrustOwn", "TrustColleague", "TrustExternal"}
	adoptedLevel = []string{"TrustCompany"}
	pageLevel    = []string{"TrustPage"}
)

// EVERY DOOR THAT DECIDES A TRUST IS ACCOUNTED FOR, AND A DOOR TO A SURFACE
// THE ENGINE HOSTS TAKES THE ONE RULE (ADR-0040). The walk finds every site in
// the tree that decides how far a model may rely on text — a call of a rule, a
// label written with a trust, a level used as a value (a comparison decides
// nothing and is not one) — and requires each to be in [trustDoors]: a hosted
// door reaches [TrustForHosted] or [LabelForHosted] and no vendor rule or level
// literal, a vendor door the reverse, a door serving both reaches both, the
// door to what an administrator adopted writes [TrustCompany] and nothing
// else, the door to what a page's script reported writes [TrustPage] and
// nothing else, and no other door writes either; and each names tests that
// exist. It is the walk, not the table, that finds a new door, and every level
// is one the walk looks for.
func TestEveryTrustDecisionIsAccountedFor(t *testing.T) {
	t.Parallel()
	if got, want := len(trustLevel)+len(adoptedLevel)+len(pageLevel), len(Trusts); got != want {
		t.Fatalf("the walk looks for %d trust levels and the package declares %d, so a "+
			"door could write the new one as a value unseen: add its name to trustLevel, "+
			"or to a family of its own if no writer can earn it", got, want)
	}
	root := sourcetree.Root(t)
	found, parsed := trustDecisionSites(t, root)
	if parsed < 500 {
		t.Fatalf("parsed %d Go files — the walk certified nothing", parsed)
	}
	for site, uses := range found {
		door, ok := trustDoors[site]
		if !ok {
			t.Errorf("%s decides a trust (%v) nothing accounts for: say whose surface its "+
				"text is on — a surface the engine hosts takes TrustForHosted (ADR-0040) — "+
				"and add it to trustDoors with the tests that hold it", site, uses)
			continue
		}
		hostedUses, vendorUses, adoptedUses, pageUses := 0, 0, 0, 0
		for _, use := range uses {
			switch {
			case slices.Contains(hostedRule, use):
				hostedUses++
			case slices.Contains(adoptedLevel, use):
				adoptedUses++
			case slices.Contains(pageLevel, use):
				pageUses++
			default:
				vendorUses++
			}
		}
		if adoptedUses > 0 && door.surface != adopted {
			t.Errorf("%s decides by %v: TrustCompany is what an administrator adopted for "+
				"everybody, never what a surface's writers earn, and only the read of an "+
				"adoption takes it", site, uses)
		}
		if pageUses > 0 && door.surface != scripted {
			t.Errorf("%s decides by %v: TrustPage is what a page's script reported, which the "+
				"engine cannot verify, never what a surface's writers earn, and only the read "+
				"of such a report takes it", site, uses)
		}
		if pageUses > 0 && door.surface != scripted {
			t.Errorf("%s decides by %v: TrustPage is what a page's script reported, which the "+
				"engine cannot verify, never what a surface's writers earn, and only the read "+
				"of such a report takes it", site, uses)
		}
		switch door.surface {
		case hosted:
			if vendorUses > 0 || hostedUses == 0 {
				t.Errorf("%s is a door to a surface the engine hosts and decides by %v: it "+
					"takes TrustForHosted or LabelForHosted and nothing else", site, uses)
			}
		case vendor:
			if hostedUses > 0 || vendorUses == 0 {
				t.Errorf("%s is a door to a vendor's surface and decides by %v: the hosted "+
					"rule vouches for writers a vendor's site never can", site, uses)
			}
		case either:
			if hostedUses == 0 || vendorUses == 0 {
				t.Errorf("%s serves both kinds of surface and decides by %v: it must reach "+
					"the hosted rule and the vendor's", site, uses)
			}
		case adopted:
			if hostedUses > 0 || vendorUses > 0 || adoptedUses == 0 {
				t.Errorf("%s reads what an administrator adopted and decides by %v: it "+
					"takes TrustCompany and nothing else", site, uses)
			}
		case scripted:
			if hostedUses > 0 || vendorUses > 0 || adoptedUses > 0 || pageUses == 0 {
				t.Errorf("%s reads what a page's script reported and decides by %v: it "+
					"takes TrustPage and nothing else", site, uses)
			}
		default:
			t.Errorf("%s names no surface", site)
		}
		if len(door.tests) == 0 {
			t.Errorf("%s names no test that holds it", site)
		}
		for _, test := range door.tests {
			if !testExists(t, root, test) {
				t.Errorf("%s is held by %s, which does not exist", site, test)
			}
		}
	}
	for site := range trustDoors {
		if _, ok := found[site]; !ok {
			t.Errorf("%s no longer decides a trust: drop its entry or fix its name", site)
		}
	}
}

// trustDecisionSites is every `<file>:<name>` outside this package that
// decides a trust, with what it decides by, and how many files were parsed.
func trustDecisionSites(t *testing.T, root string) (map[string][]string, int) {
	t.Helper()
	found := map[string][]string{}
	parsed := 0
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
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		parsed++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/provenance/") {
			return nil
		}
		alias := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == provenancePackage {
				alias = "provenance"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
			}
		}
		if alias == "" {
			return nil
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if uses := trustUses(decl, alias); len(uses) > 0 {
					found[rel+":"+decl.Name.Name] = uses
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					if uses := trustUses(value, alias); len(uses) > 0 {
						for _, name := range value.Names {
							found[rel+":"+name.Name] = uses
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
	return found, parsed
}

// trustUses is every way node decides a trust through the package imported as
// alias: the rules it calls, `Label` for a label literal carrying a trust, and
// the levels it uses as a value. A level that is an operand of a comparison
// decides nothing and is left out.
func trustUses(node ast.Node, alias string) []string {
	compared := map[ast.Expr]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if bin, ok := n.(*ast.BinaryExpr); ok && (bin.Op == token.EQL || bin.Op == token.NEQ) {
			compared[bin.X], compared[bin.Y] = true, true
		}
		return true
	})
	var uses []string
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if name, ok := selectorOf(n.Fun, alias); ok &&
				(slices.Contains(hostedRule, name) || slices.Contains(vendorRule, name)) {
				uses = append(uses, name)
			}
		case *ast.CompositeLit:
			if name, ok := selectorOf(n.Type, alias); ok && name == "Label" {
				for _, elt := range n.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Trust" {
							uses = append(uses, "Label")
						}
					}
				}
			}
		case *ast.SelectorExpr:
			if name, ok := selectorOf(n, alias); ok && !compared[n] &&
				(slices.Contains(trustLevel, name) || slices.Contains(adoptedLevel, name) ||
					slices.Contains(pageLevel, name)) {
				uses = append(uses, name)
			}
		}
		return true
	})
	return uses
}

// selectorOf is the name expr selects from the package imported as alias.
func selectorOf(expr ast.Expr, alias string) (string, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != alias {
		return "", false
	}
	return sel.Sel.Name, true
}
