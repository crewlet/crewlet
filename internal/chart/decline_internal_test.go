package chart

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// EVERY WORD A DECLINE IS COUNTED UNDER IS ONE THE CATALOGUE DECLARES, and every
// verb an edge can state is counted under one of them.
//
// The `op` attribute of `crewlet.chart.apply.declined` is a closed set a panel
// is built from, and the words were spelled at each call site: one create was
// counted as `create` where its address was held and as `create_unit` or
// `create_seat` where its parent was missing, and a set_kind under a word the
// catalogue never named — so a panel over the documented values missed part of
// every kind of decline, silently.
func TestEveryDeclineIsCountedUnderAWordTheCatalogueDeclares(t *testing.T) {
	t.Parallel()
	var shows string
	for _, inst := range metrics.Catalogue() {
		if inst.Name == metrics.ChartApplyDeclined {
			shows = inst.Shows
		}
	}
	if shows == "" {
		t.Fatalf("the catalogue declares no %s", metrics.ChartApplyDeclined)
	}
	for _, op := range declineOps {
		if !strings.Contains(shows, "`"+string(op)+"`") {
			t.Errorf("a decline is counted under %q, which the catalogue's "+
				"entry for %s does not declare", op, metrics.ChartApplyDeclined)
		}
	}
	verbs := append([]OperationKind{""}, OperationKinds...)
	for _, verb := range verbs {
		if verb == OpRemoveObject {
			// A removal is its own record and never an edge.
			continue
		}
		if got := edgeOp(Edge{Op: verb}); !slices.Contains(declineOps, got) {
			t.Errorf("an edge stating %q is counted under %q, which is not "+
				"one of %v", verb, got, declineOps)
		}
	}
	for _, create := range []OperationKind{OpCreateUnit, OpCreateSeat} {
		if got := edgeOp(Edge{Op: create}); got != declinedCreate {
			t.Errorf("a %s is counted under %q, want %q — one kind of decline "+
				"under one word, whichever object it would have made",
				create, got, declinedCreate)
		}
	}
}
