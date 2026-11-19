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

// dutyTTLs is every TTL expression a duty claim site passes, keyed by its
// source text, with the LONGEST value that expression can take.
//
// Two entries are bounds: schedule.MaxTick refuses the scheduler's tick, and
// Engine.waiterDuty refuses an interval past the ceiling. The waiter's bound
// IS the ceiling, so it does not size it: that would make the ceiling justify
// itself. Every other entry is a constant, whose reason stays at its definition.
var dutyTTLs = map[string]struct {
	longest time.Duration
	// sizesCeiling is false for a bound derived from coord.MaxDutyTTL.
	sizesCeiling bool
}{
	"maintenanceDutyTTL":   {maintenanceDutyTTL, true},
	"retentionDutyTTL":     {retentionDutyTTL, true},
	"embedDutyTTL":         {embedDutyTTL, true},
	"collectorDutyTTL":     {collectorDutyTTL, true},
	"codeRetentionDutyTTL": {codeRetentionDutyTTL, true},
	"trashReaperDutyTTL":   {trashReaperDutyTTL, true},
	"chatPruneDutyTTL":     {chatPruneDutyTTL, true},
	"delegatedSweepTTL":    {delegatedSweepTTL, true},
	"flowDutyTTL":          {flowDutyTTL, true},
	"integrationDutyTTL":   {integrationDutyTTL, true},
	"atlasCollectDutyTTL":  {atlasCollectDutyTTL, true},
	"atlasWakeDutyTTL":     {atlasWakeDutyTTL, true},
	"learningDutyTTL":      {learningDutyTTL, true},
	// THE IDENTITY DUTIES' one claim site passes the helper's parameter, which
	// only the roster resolves (identityduties_internal_test.go).
	"identityDutyTTL(interval)": {longestIdentityDutyTTL(), true},
	"setup.LeaseTTL":            {setup.LeaseTTL, true},
	"companyKeyHoldTTL":         {companyKeyHoldTTL, true},
	"schedule.DutyTTL(tick)":    {schedule.DutyTTL(schedule.MaxTick), true},
	"waiterDutyTTL(interval)":   {waiterDutyTTL(coord.MaxDutyTTL / dutyTTLTicks), false},
}

// TestEveryDutyTTLFitsTheDutyCeiling ties coord.MaxDutyTTL to the duties that
// justify it, in both directions: every duty TTL fits under it, since every
// backend refuses one above and a refused duty never runs; and the longest
// EQUALS it, so the ceiling cannot drift above the duty it is sized for or
// stay put when that duty shrinks.
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

// TestEveryDutyClaimSiteIsInTheTTLTable keeps dutyTTLs honest: a duty claimed
// with a TTL expression the table does not list fails here, so a new duty
// cannot skip the ceiling check by not being written down. Every duty goes
// through workerDuty or workerHold, and nothing else calls the schedule
// package's claim functions. On the syntax tree, since grep cannot tell a call
// from a comment naming it.
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
	// A renamed helper would otherwise leave this scanning nothing, green.
	for expr := range dutyTTLs {
		if !slices.Contains(sites, expr) {
			t.Errorf("dutyTTLs lists %q but no duty helper's call passes it; "+
				"the table is stale or the scan matched nothing", expr)
		}
	}
}

// claimFunctions are the schedule package's claim functions, which nothing
// but the duty helpers may call; dutyHelpers are those helpers, whose TTL
// argument the table must list.
var (
	claimFunctions = []string{"ClaimNamedDuty", "HoldNamedDuty", "ClaimDuty"}
	dutyHelpers    = []string{"workerDuty", "workerHold", "liveDuty"}
)

// dutyHelperFiles are the two files allowed to call claimFunctions.
var dutyHelperFiles = []string{"internal/engine/duty.go", "internal/schedule/duty.go"}

// dutySite is one call the duty gate judges: a helper's TTL expression or the
// claim function called directly. A helper's call also carries the helper and
// its duty-name expression, which dutydocs_internal_test.go resolves.
type dutySite struct {
	where, expr string
	helper      string
	name        string
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
// ONLY A FILE THAT NAMES ONE OF THE FUNCTIONS IS PARSED: an identifier has no
// escapes, so a file whose bytes spell none holds no such call
// (sourcetree.Identifiers), and parsing every file is slow under race.
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
					out.direct = append(out.direct, dutySite{where: fset.Position(call.Pos()).String(), expr: name})
				}
			case slices.Contains(dutyHelpers, name):
				if len(call.Args) == 2 {
					out.sites = append(out.sites, dutySite{
						where:  fset.Position(call.Pos()).String(),
						expr:   exprText(fset, call.Args[1]),
						helper: name,
						name:   exprText(fset, call.Args[0]),
					})
				}
			}
			return true
		})
	}
	return out
}

// THE DUTY GATE, ON A TREE WHOSE VERDICT IS KNOWN: a helper's call and a
// direct claim outside the helpers' files, a claim inside one, and an
// unparsable file naming no claim, which the prefilter must never open.
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
	if len(claims.sites) != 1 || claims.sites[0].expr != "plantedTTL" ||
		claims.sites[0].helper != "workerDuty" || claims.sites[0].name != `"thing"` {
		t.Errorf("helper calls = %+v, want workerDuty's one, naming \"thing\" and "+
			"passing plantedTTL", claims.sites)
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
