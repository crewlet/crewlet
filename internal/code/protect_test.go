package code_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A BRANCH'S MERGE GATE THROUGH A PROTECT REQUEST: the change the deciding node
// resolves against the rule its snapshot holds, and who may make it — a seat
// tightens a gate field by field and only a person loosens one, the path
// owners included where the gate makes them gating — through the whole path, a
// real broker, the shipped applier and the store's own reads back.

// tightGate is a gate holding a merge to something in every field a seat
// could lower.
func tightGate() code.MergeGate {
	return code.MergeGate{
		MergeBy: code.MergeByMaintainers, Approvals: code.ApprovalsOne,
		ApprovalsUnless: []code.ChangeClass{code.ChangeClassCommentOnly},
		HumanPaths: []code.HumanPath{
			{Glob: "deploy/**", Unless: []code.ChangeClass{code.ChangeClassCommentOnly}},
		},
		Checks:        []string{"lint", "unit"},
		Landing:       code.Landing{Depth: 4, Batch: 4},
		OwnersApprove: true, DeleteSource: code.DeleteSeats,
	}
}

// aLoosening is one change that loosens tightGate in one field: the field,
// the change, and the phrase a refusal names what it opens by.
type aLoosening struct {
	field  string
	change code.GateChange
	opens  string
}

// gateLoosenings is a change loosening tightGate in each of its fields — the
// human paths twice, a path dropped and a path's unless widened — and the gate
// removed, in MergeGate's order.
func gateLoosenings() []aLoosening {
	both := []code.ChangeClass{code.ChangeClassCommentOnly, code.ChangeClassWhitespaceOnly}
	return []aLoosening{
		{"merge_by", code.GateChange{MergeBy: ptr(code.MergeByWriters)},
			"lowers merge_by from maintainers to writers"},
		{"approvals", code.GateChange{Approvals: ptr(code.ApprovalsNone)}, "lowers approvals from one to none"},
		{"approvals_unless", code.GateChange{ApprovalsUnless: ptr(both)},
			"adds whitespace_only to approvals_unless"},
		{"human_paths", code.GateChange{HumanPaths: ptr([]code.HumanPath{})}, "drops deploy/** from human_paths"},
		{"human_paths' unless", code.GateChange{HumanPaths: ptr([]code.HumanPath{{Glob: "deploy/**",
			Unless: both}})}, "adds whitespace_only to the unless of deploy/** in human_paths"},
		{"checks", code.GateChange{Checks: ptr([]string{"lint"})}, "drops unit from checks"},
		{"owners_approve", code.GateChange{OwnersApprove: ptr(false)}, "turns owners_approve off"},
		{"agents_edit_checks", code.GateChange{AgentsEditChecks: ptr(true)}, "turns agents_edit_checks on"},
		{"the gate", code.GateChange{Remove: true}, "removes its merge gate"},
	}
}

// gateOf is the gate of pattern's rule as a protect request's answer names it
// whole (code.LifecycleResult.Rule), failing the case where the answer names
// no rule for the pattern; nil for a rule with none.
func gateOf(t *testing.T, res code.LifecycleResult, pattern string) *code.MergeGate {
	t.Helper()
	if res.Rule == nil || res.Rule.Pattern != pattern {
		t.Fatalf("the answer names the rule %+v, want %s's", res.Rule, pattern)
	}
	return res.Rule.Gate
}

// gateAmong is pattern's gate among gates — a repository's detail asked for
// them (code.RepoDetail.Gates) — and nil where none names the pattern.
func gateAmong(gates []code.RuleGate, pattern string) *code.MergeGate {
	for i, g := range gates {
		if g.Pattern == pattern {
			return &gates[i].Gate
		}
	}
	return nil
}

// sameGate fails the case where got is not want: none where want is nil.
func sameGate(t *testing.T, stage string, got, want *code.MergeGate) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("%s: the gate reads %+v, want none", stage, *got)
	case want == nil:
	case got == nil:
		t.Fatalf("%s: the rule holds no gate, want %+v", stage, *want)
	case !got.Equal(*want):
		t.Fatalf("%s: the gate reads %+v, want %+v", stage, *got, *want)
	}
}

// A GATE CHANGE IS RESOLVED INSIDE THE DECIDE: a protect request names the
// gate fields it changes, and the deciding node puts them in the gate the
// pattern's rule carries in its own snapshot — so two requests changing two
// fields of one gate, in either order, leave both changes, where a caller that
// sent the gate whole as it had read it would undo the other's. A rule with no
// gate takes the defaults beside the fields named; a request naming no gate
// field keeps the gate. And a change whose gate, with what it kept, weighs
// past what a record may carry is refused ErrInvalid naming protect.gate, for
// whoever asks, and nothing is written — which only the decide can judge,
// since only it reads what the change kept.
//
// Mutation: resolve a change against the default gate rather than the rule's
// — the second request's change drops the first's, red.
func TestAGateChangeIsResolvedInsideTheDecide(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	const main = "refs/heads/main"
	want := code.MergeGate{MergeBy: code.MergeByPersons, Approvals: code.ApprovalsNone, Checks: []string{"unit"},
		Landing: code.Landing{Depth: config.CodeDefaultQueueDepth,
			Batch: config.CodeDefaultQueueBatch}, DeleteSource: code.DeleteSeats}
	for i, order := range [][2]code.GateChange{
		{{Checks: ptr([]string{"unit"})}, {MergeBy: ptr(code.MergeByPersons)}},
		{{MergeBy: ptr(code.MergeByPersons)}, {Checks: ptr([]string{"unit"})}},
	} {
		id := n.made(lcPerson, fmt.Sprintf("resolved-%d", i), unitOwned("eng"))
		n.lands(lcGate(agent("cto"), id, main, persons, order[0]))
		got := n.lands(lcGate(lcPerson, id, main, persons, order[1]))
		sameGate(t, fmt.Sprintf("after %+v then %+v", order[0], order[1]), gateOf(t, got, main), &want)
		got = n.lands(lcProtect(lcPerson, id, main,
			&code.Protection{PushRule: code.PushRule{DirectPush: code.DirectNone}}))
		sameGate(t, "after a request naming no gate field", gateOf(t, got, main), &want)
	}

	// PAST WHAT A RECORD MAY CARRY with what the change keeps: a gate of
	// human paths JSON spells long (an & is six bytes) lands under the bound,
	// and a change requiring thirty-two checks beside them is refused — the
	// change alone a record could carry.
	id := n.made(lcPerson, "heavy", unitOwned("eng"))
	paths := make([]code.HumanPath, 12)
	for i := range paths {
		paths[i] = code.HumanPath{Glob: fmt.Sprintf("%02d/%s", i, strings.Repeat("&", 100))}
	}
	n.lands(lcGate(lcPerson, id, main, persons, code.GateChange{HumanPaths: &paths}))
	checks := make([]string, code.MaxRequiredChecks)
	for i := range checks {
		checks[i] = fmt.Sprintf("c%02d-%s", i, strings.Repeat("x", 60))
	}
	end := n.end()
	for _, actor := range []code.Principal{lcPerson, agent("cto")} {
		_, err := n.store.Lifecycle(t.Context(), lcGate(actor, id, main, persons,
			code.GateChange{Checks: &checks}))
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "protect.gate:") ||
			!strings.Contains(err.Error(), "bytes") {
			t.Errorf("%s requiring %d long checks beside long human paths = %v, want ErrInvalid naming "+
				"protect.gate and its weight", actor.Seat, len(checks), err)
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the refused gate wrote %d records", got-end)
	}
}

// AN AGENT CANNOT LOOSEN A GATE (ADR-0058, field by field): the unit's lead at
// full_access, which maintains the repository, asks a protect request that
// loosens main's merge gate in one field — each in turn: merge_by or approvals
// lowered, a class added to approvals_unless, a human path dropped or its
// unless widened, a required check dropped, owners_approve turned off,
// agents_edit_checks turned on, or the gate removed. Each is refused not_permitted, the sentence
// naming the pattern and what the change opens by its field, and nothing is
// written: the gate a person set stands. A change that tightens one field and
// opens another is refused for the one it opens, and names that one alone.
//
// Mutation: treat agents_edit_checks turned on as neutral in LoosenedBy — the
// lead turns it on, red.
func TestAnAgentCannotLoosenAGate(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	cto := agent("cto")
	const main = "refs/heads/main"
	id := n.made(lcPerson, "gated", unitOwned("eng"))
	tight := tightGate()
	n.lands(lcGate(lcPerson, id, main, persons, gateTo(tight)))
	end := n.end()
	rows := append(gateLoosenings(), aLoosening{"checks, beside merge_by raised",
		code.GateChange{MergeBy: ptr(code.MergeByPersons), Checks: ptr([]string{"unit"})}, "drops lint from checks"})
	for _, tc := range rows {
		res := n.life(lcGate(cto, id, main, persons, tc.change))
		switch {
		case res.Refusal != code.LifecycleNotPermitted:
			t.Errorf("the lead's change of %s: %+v, want not_permitted", tc.field, res)
			continue
		case !strings.Contains(res.Detail, "only a person may remove or loosen the protection on "+main) ||
			!strings.Contains(res.Detail, tc.opens):
			t.Errorf("the lead's change of %s refused %q, want the sentence naming %s and that it %s", tc.field,
				res.Detail, main, tc.opens)
		case !errors.Is(res.Err(), code.ErrNotPermitted):
			t.Errorf("the lead's change of %s as an error is %v, want ErrNotPermitted", tc.field, res.Err())
		}
		if tc.change.MergeBy != nil && *tc.change.MergeBy == code.MergeByPersons &&
			strings.Contains(res.Detail, "merge_by") {
			t.Errorf("the lead's change of %s names merge_by, which it raises: %q", tc.field, res.Detail)
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the lead's refused loosenings wrote %d records", got-end)
	}
	sameGate(t, "after the lead's refused loosenings", gateAmong(n.gatesOf("gated"), main), &tight)
}

// AN AGENT MAY TIGHTEN A GATE: the unit's lead adds a gate to a rule that had
// none — the defaults, and the one field it names — and tightens a person's
// loose gate field by field: merge_by and approvals raised, a class taken from
// approvals_unless, a human path's unless narrowed and a human path added, a
// check required more, owners_approve turned on and agents_edit_checks off —
// each landing — and moves the
// neutral fields, landing.depth, landing.batch and delete_source, either way.
// A request naming no gate field tightens direct_push and keeps the gate, which
// ends as every change left it.
//
// Mutation: judge the gate whole again in loosenings, any field changed a
// loosening — the lead's first tightening is refused, red.
func TestAnAgentMayTightenAGate(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	cto := agent("cto")
	const main, release = "refs/heads/main", "refs/heads/release/*"
	id := n.made(lcPerson, "tightened", unitOwned("eng"))
	both := []code.ChangeClass{code.ChangeClassCommentOnly, code.ChangeClassWhitespaceOnly}
	loose := code.MergeGate{MergeBy: code.MergeByWriters, Approvals: code.ApprovalsNone, ApprovalsUnless: both,
		HumanPaths: []code.HumanPath{{Glob: "deploy/**", Unless: both}}, Checks: []string{"lint"},
		Landing: code.Landing{Depth: 4, Batch: 4}, DeleteSource: code.DeleteSeats, AgentsEditChecks: true}
	n.lands(lcGate(lcPerson, id, main, persons, gateTo(loose)))

	// A GATE ADDED where the rule had none: the defaults, and the one field
	// the lead names.
	n.lands(lcProtect(cto, id, release, persons))
	got := n.lands(lcGate(cto, id, release, persons, code.GateChange{Checks: ptr([]string{"unit"})}))
	sameGate(t, "a gate the lead added", gateOf(t, got, release), &code.MergeGate{
		MergeBy: code.MergeByWriters, Approvals: code.ApprovalsNone, Checks: []string{"unit"},
		Landing: code.Landing{Depth: config.CodeDefaultQueueDepth,
			Batch: config.CodeDefaultQueueBatch}, DeleteSource: code.DeleteSeats})

	for _, change := range []code.GateChange{
		{MergeBy: ptr(code.MergeByPersons)},
		{Approvals: ptr(code.ApprovalsOne)},
		{ApprovalsUnless: ptr([]code.ChangeClass{code.ChangeClassCommentOnly})},
		{HumanPaths: ptr([]code.HumanPath{{Glob: ".crewlet/**"},
			{Glob: "deploy/**", Unless: []code.ChangeClass{code.ChangeClassCommentOnly}}})},
		{Checks: ptr([]string{"lint", "unit"})},
		{OwnersApprove: ptr(true)},
		{AgentsEditChecks: ptr(false)},
		// NEUTRAL, either way.
		{Landing: &code.LandingChange{Depth: ptr(1), Batch: ptr(code.MaxQueueBatch)}},
		{DeleteSource: ptr(code.DeleteNone)},
		{Landing: &code.LandingChange{Depth: ptr(code.MaxQueueDepth), Batch: ptr(1)}},
		{DeleteSource: ptr(code.DeleteSeats)},
	} {
		if res := n.life(lcGate(cto, id, main, persons, change)); res.Refusal != "" ||
			res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
			t.Fatalf("the lead's change %+v: %+v, want it recorded and applied", change, res)
		}
	}
	got = n.lands(lcProtect(cto, id, main, &code.Protection{PushRule: code.PushRule{DirectPush: code.DirectNone}}))
	sameGate(t, "tightened by the lead", gateOf(t, got, main), &code.MergeGate{
		MergeBy: code.MergeByPersons, Approvals: code.ApprovalsOne,
		ApprovalsUnless: []code.ChangeClass{code.ChangeClassCommentOnly},
		HumanPaths: []code.HumanPath{{Glob: ".crewlet/**"},
			{Glob: "deploy/**", Unless: []code.ChangeClass{code.ChangeClassCommentOnly}}},
		Checks:        []string{"lint", "unit"},
		Landing:       code.Landing{Depth: code.MaxQueueDepth, Batch: 1},
		OwnersApprove: true, DeleteSource: code.DeleteSeats,
	})
}

// A PERSON MAY LOOSEN A GATE: every change the lead is refused lands for a
// person, the gate ending as the person left it, and a person removes it.
//
// Mutation: judge a person's protect request as a seat's — drop the person's
// return from lifecycleRefusal — the person's first loosening is refused, red.
func TestAPersonMayLoosenAGate(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	const main = "refs/heads/main"
	id := n.made(lcPerson, "loosened", unitOwned("eng"))
	n.lands(lcGate(lcPerson, id, main, persons, gateTo(tightGate())))
	var removal code.GateChange
	for _, tc := range gateLoosenings() {
		if tc.change.Remove {
			removal = tc.change
			continue
		}
		if res := n.life(lcGate(lcPerson, id, main, persons, tc.change)); res.Refusal != "" ||
			res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
			t.Fatalf("a person's change of %s: %+v, want it recorded and applied", tc.field, res)
		}
	}
	both := []code.ChangeClass{code.ChangeClassCommentOnly, code.ChangeClassWhitespaceOnly}
	sameGate(t, "loosened by a person", gateAmong(n.gatesOf("loosened"), main), &code.MergeGate{
		MergeBy: code.MergeByWriters, Approvals: code.ApprovalsNone, ApprovalsUnless: both,
		HumanPaths: []code.HumanPath{{Glob: "deploy/**", Unless: both}}, Checks: []string{"lint"},
		Landing: code.Landing{Depth: 4, Batch: 4}, DeleteSource: code.DeleteSeats, AgentsEditChecks: true,
	})
	got := n.lands(lcGate(lcPerson, id, main, persons, removal))
	sameGate(t, "removed by a person", gateOf(t, got, main), nil)
}

// AN AGENT CANNOT REMOVE A PATH OWNER UNDER owners_approve: where a rule's
// merge gate sets it, a repository's path owners are whom a merge waits for, so
// a seat that maintains the repository — the unit's lead — is refused a
// settings change taking a glob from one, an owner whole, or a glob beside one
// it gives, not_permitted with the sentence naming path_owners, what it takes
// and the rule; nothing is written. A glob no owner holds, taken, takes
// nothing and is answered from the row; giving a path owner globs only asks a
// merge for more, and lands. On a repository whose gate sets no
// owners_approve the same removals land, the path owners a review's alone; and
// a person takes them under owners_approve.
//
// Mutation: judge the path owners neutral — drop the settings arm of
// lifecycleRefusal's loosening branch — the lead's removals land, red.
func TestAnAgentCannotRemoveAPathOwnerUnderOwnersApprove(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	cto := agent("cto")
	const main = "refs/heads/main"
	approved := n.made(lcPerson, "approved", unitOwned("eng"))
	plain := n.made(lcPerson, "plain", unitOwned("eng"))
	for _, id := range []string{approved, plain} {
		n.lands(lcOwners(lcPerson, id, []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "deploy/**", "infra/**"),
			pathOwner(code.OwnerSeat, "dev", "Makefile")}, nil))
	}
	n.lands(lcGate(lcPerson, approved, main, persons, code.GateChange{OwnersApprove: ptr(true)}))
	n.lands(lcGate(lcPerson, plain, main, persons, code.GateChange{Checks: ptr([]string{"unit"})}))

	end := n.end()
	for _, tc := range []struct {
		what        string
		add, remove []code.PathOwner
		takes       string
	}{
		{"a glob", nil, []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "infra/**")}, "infra/** from unit plat"},
		{"an owner whole", nil, []code.PathOwner{pathOwner(code.OwnerSeat, "dev", "Makefile")},
			"Makefile from seat dev"},
		{"a glob beside one it gives", []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "web/**")},
			[]code.PathOwner{pathOwner(code.OwnerUnit, "plat", "deploy/**")}, "deploy/** from unit plat"},
	} {
		res := n.life(lcOwners(cto, approved, tc.add, tc.remove))
		switch {
		case res.Refusal != code.LifecycleNotPermitted:
			t.Errorf("the lead taking %s: %+v, want not_permitted", tc.what, res)
		case !strings.Contains(res.Detail, "path_owners") || !strings.Contains(res.Detail, tc.takes) ||
			!strings.Contains(res.Detail, main):
			t.Errorf("the lead taking %s refused %q, want the sentence naming path_owners, %s and %s", tc.what,
				res.Detail, tc.takes, main)
		case !errors.Is(res.Err(), code.ErrNotPermitted):
			t.Errorf("the lead taking %s as an error is %v, want ErrNotPermitted", tc.what, res.Err())
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the lead's refused removals wrote %d records", got-end)
	}
	if res := n.life(lcOwners(cto, approved, nil, []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "web/**")})); res.Refusal != "" || res.Outcome != statelog.OutcomeApplied || !res.Position.IsZero() {
		t.Errorf("the lead taking a glob no owner holds: %+v, want it answered from the row", res)
	}
	n.lands(lcOwners(cto, approved, []code.PathOwner{pathOwner(code.OwnerUnit, "sales", "sales/**")}, nil))
	n.lands(lcOwners(cto, plain, nil, []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "infra/**"),
		pathOwner(code.OwnerSeat, "dev", "Makefile")}))
	n.lands(lcOwners(lcPerson, approved, nil, []code.PathOwner{pathOwner(code.OwnerUnit, "plat", "infra/**"),
		pathOwner(code.OwnerSeat, "dev", "Makefile")}))
	for name, want := range map[string][]code.PathOwner{
		"approved": {pathOwner(code.OwnerUnit, "plat", "deploy/**"), pathOwner(code.OwnerUnit, "sales", "sales/**")},
		"plain":    {pathOwner(code.OwnerUnit, "plat", "deploy/**")},
	} {
		if got := n.ownersOf(name); !reflect.DeepEqual(got, want) {
			t.Errorf("%s's path owners are %+v, want %+v", name, got, want)
		}
	}
}

// A PROTECT REQUEST'S ANSWER NAMES ITS RULE WHOLE: the repository's row never
// carries a merge gate (code.RepoRef.Protections are code.PushRules), so the
// answer names the protected pattern's rule beside it, its gate as the
// deciding node resolved it (code.LifecycleResult.Rule) — read back with the
// row where the request's record applied, read back where a repeat of the
// operation collapsed onto the record before it, and as the decide held it
// where the request needed no record. A rule the request removed, and every
// other kind's answer, names none.
//
// Mutations: read the row back without the pattern's rule — the applied
// answer names no rule, red; answer a request that needed no record with no
// rule — the repeated gate names none, red.
func TestAProtectAnswerNamesItsRuleWhole(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	const main = "refs/heads/main"
	id := n.made(lcPerson, "answered", unitOwned("eng"))
	gate := tightGate()
	whole := func(stage string, res code.LifecycleResult, direct code.DirectPush) {
		t.Helper()
		sameGate(t, stage, gateOf(t, res, main), &gate)
		if res.Rule.DirectPush != direct {
			t.Fatalf("%s: the answer's rule pushes %s, want %s", stage, res.Rule.DirectPush, direct)
		}
	}

	set := lcGate(lcPerson, id, main, persons, gateTo(gate))
	whole("applied", n.lands(set), code.DirectPersons)
	if again := n.life(set); again.Outcome != statelog.OutcomeApplied || again.Refusal != "" {
		t.Fatalf("the same operation again: %+v, want it applied", again)
	} else {
		whole("repeated", again, code.DirectPersons)
	}
	same := n.life(lcGate(lcPerson, id, main, persons, gateTo(gate)))
	if same.Outcome != statelog.OutcomeApplied || !same.Position.IsZero() {
		t.Fatalf("the gate sent again as it stands: %+v, want it answered from the row", same)
	}
	whole("needing no record", same, code.DirectPersons)
	tightened := n.lands(lcProtect(lcPerson, id, main,
		&code.Protection{PushRule: code.PushRule{DirectPush: code.DirectNone}}))
	whole("a field it names", tightened, code.DirectNone)

	if removed := n.lands(lcProtect(lcPerson, id, main, nil)); removed.Rule != nil {
		t.Errorf("the rule's removal answers the rule %+v, want none", *removed.Rule)
	}
	described := "answered, and described"
	if changed := n.lands(lcSettings(lcPerson, id, code.RepoChange{Description: &described})); changed.Rule != nil {
		t.Errorf("a settings change answers the rule %+v, want none", *changed.Rule)
	}
}
