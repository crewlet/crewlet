package engine

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// dutyTTLs is every TTL expression a duty claim site in this package passes,
// keyed by its source text, with the LONGEST value that expression can take.
//
// Two entries are bounds rather than constants, and each says whose check makes
// the bound true: the scheduler's tick is refused at or above schedule.MaxTick,
// and the waiter's interval is refused by Engine.waiterDuty once its duty would
// exceed the ceiling. The waiter's bound therefore IS the ceiling, so it is
// marked as not sizing it: counting it would make the ceiling justify itself.
//
// Every other entry is a plain CONSTANT, and its longest value is the constant
// itself: the expression takes no argument, so there is nothing for it to
// reach. Why each constant is the duration it is stays at its own definition
// rather than being copied here, where the copy would be free to disagree.
var dutyTTLs = map[string]struct {
	longest time.Duration
	// sizesCeiling is false for a bound derived from coord.MaxDutyTTL.
	sizesCeiling bool
}{
	"maintenanceDutyTTL":      {maintenanceDutyTTL, true},
	"retentionDutyTTL":        {retentionDutyTTL, true},
	"embedDutyTTL":            {embedDutyTTL, true},
	"collectorDutyTTL":        {collectorDutyTTL, true},
	"integrationDutyTTL":      {integrationDutyTTL, true},
	"learningDutyTTL":         {learningDutyTTL, true},
	"setup.LeaseTTL":          {setup.LeaseTTL, true},
	"schedule.DutyTTL(tick)":  {schedule.DutyTTL(schedule.MaxTick), true},
	"waiterDutyTTL(interval)": {waiterDutyTTL(coord.MaxDutyTTL / dutyTTLTicks), false},
}

// TestEveryDutyTTLFitsTheDutyCeiling ties coord.MaxDutyTTL to the duties that
// justify it, in both directions.
//
// Every duty TTL must fit under the ceiling, because every backend refuses one
// above it, and a refused duty never runs: that is exactly how the retention
// sweep, the mailbox retirement, the integration reconcile and the skill
// curator went unrun on every embedded-kv fleet, one warning per tick. And the
// longest must EQUAL the ceiling, so the constant cannot drift above the duty
// it is sized for (an unjustified ceiling) or stay put when that duty shrinks.
func TestEveryDutyTTLFitsTheDutyCeiling(t *testing.T) {
	t.Parallel()
	longest := time.Duration(0)
	for expr, duty := range dutyTTLs {
		if duty.longest > coord.MaxDutyTTL {
			t.Errorf("duty TTL %s can reach %v, beyond coord.MaxDutyTTL (%v): every claim of it is refused",
				expr, duty.longest, coord.MaxDutyTTL)
		}
		if duty.longest <= 0 {
			t.Errorf("duty TTL %s is %v, which mints a lease that has already lapsed", expr, duty.longest)
		}
		if duty.sizesCeiling {
			longest = max(longest, duty.longest)
		}
	}
	if longest != coord.MaxDutyTTL {
		t.Fatalf("the longest duty TTL is %v but coord.MaxDutyTTL is %v; the ceiling must be "+
			"sized from the longest duty, so change one to match the other", longest, coord.MaxDutyTTL)
	}
	if got := waiterDutyTTL(sandbox.DefaultPollInterval); got > coord.MaxDutyTTL {
		t.Fatalf("the default waiter duty TTL %v exceeds the ceiling", got)
	}
}

// TestEveryDutyClaimSiteIsInTheTTLTable is what keeps the table above honest:
// a duty claimed anywhere with a TTL expression the table does not list fails
// here, so a new duty cannot skip the ceiling check by not being written down.
//
// On the syntax tree rather than by grep, because a claim is a call and a
// line-oriented scan cannot tell a call from a comment naming it. Two layers:
// every duty in the engine goes through workerDuty or workerHold, and nothing
// outside the two duty helpers calls the schedule package's claim functions
// directly.
func TestEveryDutyClaimSiteIsInTheTTLTable(t *testing.T) {
	t.Parallel()
	claims := dutyClaims(t, sourcetree.Root(t))
	for _, site := range claims.direct {
		t.Errorf("%s calls schedule.%s directly; claim a duty through "+
			"Engine.workerDuty or Engine.workerHold so its TTL is checked here",
			site.where, site.expr)
	}
	sites := make([]string, 0, len(claims.sites))
	for _, site := range claims.sites {
		sites = append(sites, site.expr)
		if _, listed := dutyTTLs[site.expr]; !listed {
			t.Errorf("%s claims a duty with TTL %q, which dutyTTLs does not list; add it "+
				"with the longest value it can take", site.where, site.expr)
		}
	}
	// A guard asserting an absence must assert it matched its subject: a
	// renamed helper would otherwise leave this scanning nothing, green.
	for expr := range dutyTTLs {
		if !slices.Contains(sites, expr) {
			t.Errorf("dutyTTLs lists %q but no workerDuty or workerHold call passes it; "+
				"the table is stale or the scan matched nothing", expr)
		}
	}
}

// claimFunctions are the schedule package's claim functions, which nothing
// but the two duty helpers may call; dutyHelpers are those helpers, whose TTL
// argument the table must list.
var (
	claimFunctions = []string{"ClaimNamedDuty", "HoldNamedDuty", "ClaimDuty"}
	dutyHelpers    = []string{"workerDuty", "workerHold"}
)

// dutyHelperFiles are the two files allowed to call claimFunctions.
var dutyHelperFiles = []string{"internal/engine/duty.go", "internal/schedule/duty.go"}

// dutySite is one call the duty gate judges: where it is, and what it names —
// a helper's TTL expression, or the claim function called directly.
type dutySite struct {
	where, expr string
}

// dutyCalls is what the duty gate found.
type dutyCalls struct {
	// sites are the helpers' calls, each with its TTL expression.
	sites []dutySite
	// direct are calls of a claim function outside the duty helpers.
	direct []dutySite
}

// dutyClaims reads every non-test Go file under root's internal/ and cmd/
// for the calls the gate above judges.
//
// ONLY A FILE THAT NAMES ONE OF THE FUNCTIONS IS PARSED: each judged call
// spells its callee, and an identifier has no escapes, so a file whose bytes
// spell none of them holds no such call (sourcetree.Identifiers). Parsing all
// of internal/ and cmd/ to find the two dozen that do was ten seconds under
// the race detector.
func dutyClaims(t *testing.T, root string) dutyCalls {
	t.Helper()
	names := sourcetree.MustIdentifiers(append(slices.Clone(claimFunctions), dutyHelpers...)...)
	var out dutyCalls
	for _, f := range moduleFiles(t, root) {
		if !f.under("internal", "cmd") || !names.In(f.body) {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f.path, f.body, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f.rel, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call.Fun)
			switch {
			case slices.Contains(claimFunctions, name):
				if !slices.Contains(dutyHelperFiles, f.rel) {
					out.direct = append(out.direct, dutySite{fset.Position(call.Pos()).String(), name})
				}
			case slices.Contains(dutyHelpers, name):
				if len(call.Args) == 2 {
					out.sites = append(out.sites,
						dutySite{fset.Position(call.Pos()).String(), exprText(fset, call.Args[1])})
				}
			}
			return true
		})
	}
	return out
}

// THE DUTY GATE, ON A TREE WHOSE VERDICT IS KNOWN — through the same read,
// prefilter and matcher. A helper's call and a direct claim are planted
// outside the helpers' files, the same claim inside one, and a file that
// does not parse and names no claim, which the gate must never open.
func TestTheDutyGateFindsEveryClaimInAPlantedTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/engine/thing.go": "package engine\n\n" +
			"func (e *Engine) run() { e.workerDuty(\"thing\", plantedTTL) }\n",
		"cmd/crewlet/claim.go": "package main\n\n" +
			"func f() { schedule.ClaimDuty(ctx, c, \"x\", time.Minute) }\n",
		"internal/schedule/duty.go": "package schedule\n\n" +
			"func g() { ClaimNamedDuty(ctx, c, \"x\", time.Minute) }\n",
		"internal/broken/broken.go": "package broken\n\nthis is not Go\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	claims := dutyClaims(t, root)
	if len(claims.sites) != 1 || claims.sites[0].expr != "plantedTTL" {
		t.Errorf("helper calls = %+v, want the one passing plantedTTL", claims.sites)
	}
	if len(claims.direct) != 1 || claims.direct[0].expr != "ClaimDuty" ||
		!strings.Contains(claims.direct[0].where, "claim.go") {
		t.Errorf("direct claims = %+v, want cmd/crewlet's ClaimDuty alone — the "+
			"helper's own file may call one", claims.direct)
	}
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func exprText(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return ""
	}
	return b.String()
}
