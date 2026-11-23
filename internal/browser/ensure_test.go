package browser

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// declared is every constant of typ this package's ensure.go declares, by
// its value, read from the source: a value the list leaves out is one the
// list never decided about.
func declared(t *testing.T, typ string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "ensure.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if ident, ok := spec.Type.(*ast.Ident); !ok || ident.Name != typ {
			return true
		}
		for _, v := range spec.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("a %s constant is %T, and this case reads string literals", typ, v)
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, s)
		}
		return true
	})
	if len(values) == 0 {
		t.Fatalf("ensure.go declares no %s constant, so this case would pass on nothing", typ)
	}
	return values
}

// EVERY REASON TAKES ITS OWN CAUSES AND NO OTHER: every declared reason and
// cause is on the list, each cause under a reason that takes it, and a string
// off it is neither; the box's reasons in the order ensure checks them, then
// the engine's in the order a launch meets them; Printed true for exactly the
// box's, each taking causes; host_network's causes the kernel, each route in
// the check's order, then unchecked; Takes admitting a reason's own causes,
// and no cause exactly for an engine reason with nothing more to tell; and
// every closed type's zero value invalid, so an unset field is none.
//
// Mutations, each red: declare a cause no reason takes; swap two reasons, or
// two of host_network's causes; put host_network before bridge_deferred, or
// ensure_timeout before helper_unwritten; let Takes admit no cause anywhere.
func TestEveryReasonTakesItsOwnCausesAndNoOther(t *testing.T) {
	t.Parallel()
	for _, c := range declared(t, "EnsureCause") {
		if !EnsureCause(c).Valid() {
			t.Errorf("the cause %q is declared and no reason takes it", c)
		}
	}
	for _, r := range declared(t, "EnsureReason") {
		if !EnsureReason(r).Valid() {
			t.Errorf("the reason %q is declared and off the list", r)
		}
	}
	box := []EnsureReason{ReasonNodeMissing, ReasonNodeTooOld, ReasonFolderUnusable, ReasonPlaywrightMissing,
		ReasonChromiumMissing, ReasonChromiumSandboxUnavailable, ReasonChromiumStartFailed, ReasonSupervisorFailed}
	engine := []EnsureReason{ReasonCellNotContainer, ReasonWebDenied, ReasonBridgeDeferred, ReasonHostNetwork,
		ReasonHelperUnwritten, ReasonEnsureTimeout, ReasonEnsureUnreadable}
	if got, want := EnsureReasons(), slices.Concat(box, engine); !slices.Equal(got, want) {
		t.Errorf("EnsureReasons is %q, want %q: the box's in the order ensure checks them, then the engine's in "+
			"the order a launch meets them", got, want)
	}
	hostNetwork := []EnsureCause{CauseAnotherKernel, CauseLoopback, CauseGateway, CauseHostAlias, CauseRootlessGateway,
		CauseProxy, CauseUnchecked}
	if got := ReasonHostNetwork.Causes(); !slices.Equal(got, hostNetwork) {
		t.Errorf("host_network takes %q, want %q: the kernel, each route in the order the check names the first "+
			"that arrived, then a check that could not tell", got, hostNetwork)
	}
	causeless := []EnsureReason{ReasonCellNotContainer, ReasonWebDenied, ReasonBridgeDeferred, ReasonEnsureTimeout}
	for _, r := range EnsureReasons() {
		if r.Printed() != slices.Contains(box, r) {
			t.Errorf("%s.Printed() is %t, and a box prints exactly %q", r, r.Printed(), box)
		}
		if r.Printed() && len(r.Causes()) == 0 {
			t.Errorf("%s is a box's and takes no cause, and a box's line always names one", r)
		}
		if r.Takes("") != slices.Contains(causeless, r) {
			t.Errorf("%s.Takes(\"\") is %t, and exactly %q are recorded with no cause", r, r.Takes(""), causeless)
		}
		for _, c := range r.Causes() {
			if !r.Takes(c) {
				t.Errorf("%s does not take its own cause %s", r, c)
			}
		}
	}
	for _, pair := range []struct {
		r EnsureReason
		c EnsureCause
	}{{ReasonHelperUnwritten, CauseNoLine}, {ReasonCellNotContainer, CauseRefused}, {ReasonChromiumMissing, CauseRoot},
		{ReasonHostNetwork, CauseTimeout}, {ReasonEnsureUnreadable, CauseUnchecked}} {
		if pair.r.Takes(pair.c) {
			t.Errorf("%s takes %s, another reason's cause", pair.r, pair.c)
		}
	}
	const off = "out_of_coffee"
	if EnsureReason(off).Valid() || EnsureCause(off).Valid() || EnsureReason(off).Printed() ||
		EnsureReason(off).Causes() != nil || EnsureReason(off).Takes("") || EnsureReason(off).Takes(CauseNotFound) {
		t.Errorf("%q, off the list, is read as a reason or a cause", off)
	}
	for name, valid := range map[string]bool{
		"EnsureVerdict":   EnsureVerdict("").Valid(),
		"EnsureReason":    EnsureReason("").Valid(),
		"EnsureCause":     EnsureCause("").Valid(),
		"ChromiumBuild":   ChromiumBuild("").Valid(),
		"ChromiumSandbox": ChromiumSandbox("").Valid(),
		"SupervisorStart": SupervisorStart("").Valid(),
	} {
		if valid {
			t.Errorf("%s's zero value is valid, and an unset field would read as a value", name)
		}
	}
}
