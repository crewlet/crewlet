package engine

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
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
	"maintenanceDutyTTL": {maintenanceDutyTTL, true},
	"retentionDutyTTL":   {retentionDutyTTL, true},
	"embedDutyTTL":       {embedDutyTTL, true},
	"integrationDutyTTL": {integrationDutyTTL, true},
	"learningDutyTTL":    {learningDutyTTL, true},
	// THE IDENTITY DUTIES' LEASE is three claims, and a claim is at most
	// an hour apart whatever the interval — so the probe's longest
	// operator setting, a day, still takes a lease of three hours.
	"identityDutyTTL(interval)": {identityDutyTTL(config.DeactivationProbeCeiling), true},
	"setup.LeaseTTL":            {setup.LeaseTTL, true},
	"companyKeyHoldTTL":         {companyKeyHoldTTL, true},
	"schedule.DutyTTL(tick)":    {schedule.DutyTTL(schedule.MaxTick), true},
	"waiterDutyTTL(interval)":   {waiterDutyTTL(coord.MaxDutyTTL / dutyTTLTicks), false},
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
	root := filepath.Join("..", "..")
	helpers := map[string]bool{
		filepath.Join("internal", "engine", "duty.go"):   true,
		filepath.Join("internal", "schedule", "duty.go"): true,
	}
	var sites []string
	for _, dir := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calleeName(call.Fun)
				switch name {
				case "ClaimNamedDuty", "HoldNamedDuty", "ClaimDuty":
					if !helpers[rel] {
						t.Errorf("%s calls schedule.%s directly; claim a duty through "+
							"Engine.workerDuty or Engine.workerHold so its TTL is checked here",
							fset.Position(call.Pos()), name)
					}
				case "workerDuty", "workerHold":
					if len(call.Args) != 2 {
						return true
					}
					expr := exprText(fset, call.Args[1])
					sites = append(sites, expr)
					if _, listed := dutyTTLs[expr]; !listed {
						t.Errorf("%s claims a duty with TTL %q, which dutyTTLs does not list; add it "+
							"with the longest value it can take", fset.Position(call.Pos()), expr)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
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
