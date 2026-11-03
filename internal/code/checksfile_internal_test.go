package code

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A DEFINITION'S LIST PAST ITS BOUND IS JUDGED BY ITS COUNT ALONE: a queued
// check's record carries its definition, and every writer and every reader of
// one holds it to the rules a file is held to (CheckDef.Validate) — so a list
// longer than its bound is ONE fault, at the list, the count rule's own, and no
// item of it is judged or compared with another, however many it holds; one
// within its bound has each item judged, and an item repeated named against
// its first.
//
// Mutation: judge a list's items past its count, and each list here draws a
// fault an item beside its count's — red.
func TestADefinitionPastItsListsIsJudgedByItsCount(t *testing.T) {
	t.Parallel()
	// ELEVEN THOUSAND ITEMS, about what one list of a 64 KiB file holds, and
	// every one of them a fault were it judged.
	const many = 11_000
	def := CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 60,
		Paths: make([]string, many), Env: make([]string, many),
		Checkout: CheckCheckout{Cone: make([]string, many)}}
	for i := range many {
		def.Paths[i], def.Env[i], def.Checkout.Cone[i] = fmt.Sprint("/", i), fmt.Sprint(i, "x"),
			fmt.Sprint("../", i)
	}
	faults := defFaults(def)
	want := []struct {
		path string
		list checkList
	}{{"paths", pathsList}, {"checkout", coneList}, {"env", envList}}
	if len(faults) != len(want) {
		t.Fatalf("a definition of three lists of %d bad items drew %d faults, want one a list", many, len(faults))
	}
	for i, w := range want {
		detail, _ := w.list.over(many)
		if f := faults[i]; f.path.String() != w.path || !errors.Is(f.kind, config.ErrOutOfRange) ||
			f.detail != detail {
			t.Errorf("fault %d is %s: %v: %s; want %s: %s", i, f.path, f.kind, f.detail, w.path, detail)
		}
	}

	within := CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 60,
		Env: []string{"A", "B", "A", "1x", "B"}}
	var got []string
	for _, f := range defFaults(within) {
		got = append(got, fmt.Sprintf("%s: %v: %s", f.path, f.kind, f.detail))
	}
	wantWithin := []string{
		`env[2]: conflicting settings: "A" is env[0] too`,
		fmt.Sprintf(`env[3]: value not in the allowed set: "1x" is not a variable name: a letter or an `+
			`underscore, then letters, digits and underscores, at most %d bytes — the key of an entry in the `+
			`company's code.checks.env`, MaxCheckEnvNameBytes),
		`env[4]: conflicting settings: "B" is env[1] too`,
	}
	if !slices.Equal(got, wantWithin) {
		t.Errorf("a list within its bound drew\n%q\nwant\n%q", got, wantWithin)
	}
}
