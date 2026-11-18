package gitlet_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A BRANCH'S MERGE GATE THROUGH A PROTECT REQUEST, over a real broker, the
// shipped applier and the store's own reads: the deciding node resolves the
// change against its snapshot's rule, a seat only tightens a gate field by
// field, and only a person loosens one (path owners too, under owners_approve).

// tightGate is a gate holding a merge to something in every field a seat
// could lower.
func tightGate() gitlet.MergeGate {
	return gitlet.MergeGate{
		MergeBy: gitlet.MergeByMaintainers, Approvals: gitlet.ApprovalsOne,
		ApprovalsUnless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly},
		HumanPaths: []gitlet.HumanPath{
			{Glob: "deploy/**", Unless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly}},
		},
		Checks:        []string{"lint", "unit"},
		Landing:       gitlet.Landing{Depth: 4, Batch: 4},
		OwnersApprove: true, DeleteSource: gitlet.DeleteSeats,
	}
}

// aLoosening is one change that loosens tightGate in one field: the field,
// the change, and the phrase a refusal names what it opens by.
type aLoosening struct {
	field  string
	change gitlet.GateChange
	opens  string
}

// gateLoosenings is a change loosening tightGate in each of its fields — the
// human paths twice, a path dropped and a path's unless widened — and the gate
// removed, in MergeGate's order.
func gateLoosenings() []aLoosening {
	both := []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly, gitlet.ChangeClassWhitespaceOnly}
	return []aLoosening{
		{"merge_by", gitlet.GateChange{MergeBy: ptr(gitlet.MergeByWriters)},
			"lowers merge_by from maintainers to writers"},
		{"approvals", gitlet.GateChange{Approvals: ptr(gitlet.ApprovalsNone)}, "lowers approvals from one to none"},
		{"approvals_unless", gitlet.GateChange{ApprovalsUnless: &gitlet.ListChange[gitlet.ChangeClass]{
			Add: []gitlet.ChangeClass{gitlet.ChangeClassWhitespaceOnly}}}, `adds "whitespace_only" to approvals_unless`},
		{"human_paths", gitlet.GateChange{HumanPaths: &gitlet.HumanPathsChange{Remove: []string{"deploy/**"}}},
			`drops "deploy/**" from human_paths`},
		{"human_paths' unless", gitlet.GateChange{HumanPaths: &gitlet.HumanPathsChange{Put: []gitlet.HumanPath{
			{Glob: "deploy/**", Unless: both}}}}, `adds "whitespace_only" to the unless of deploy/** in human_paths`},
		{"checks", gitlet.GateChange{Checks: &gitlet.ListChange[string]{Remove: []string{"unit"}}},
			`drops "unit" from checks`},
		{"owners_approve", gitlet.GateChange{OwnersApprove: ptr(false)}, "turns owners_approve off"},
		{"agents_edit_checks", gitlet.GateChange{AgentsEditChecks: ptr(true)}, "turns agents_edit_checks on"},
		{"the gate", gitlet.GateChange{Remove: true}, "removes its merge gate"},
	}
}

// gateOf is the gate of pattern's rule as a protect request's answer names it
// whole (gitlet.LifecycleResult.Rule), failing the case where the answer names
// no rule for the pattern; nil for a rule with none.
func gateOf(t *testing.T, res gitlet.LifecycleResult, pattern string) *gitlet.MergeGate {
	t.Helper()
	if res.Rule == nil || res.Rule.Pattern != pattern {
		t.Fatalf("the answer names the rule %+v, want %s's", res.Rule, pattern)
	}
	return res.Rule.Gate
}

// gateAmong is pattern's gate among gates — a repository's detail asked for
// them (gitlet.RepoDetail.Gates) — and nil where none names the pattern.
func gateAmong(gates []gitlet.RuleGate, pattern string) *gitlet.MergeGate {
	for i, g := range gates {
		if g.Pattern == pattern {
			return &gates[i].Gate
		}
	}
	return nil
}

// sameGate fails the case where got is not want: none where want is nil.
func sameGate(t *testing.T, stage string, got, want *gitlet.MergeGate) {
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

// A GATE CHANGE IS RESOLVED INSIDE THE DECIDE, against the gate the deciding
// node's snapshot holds: two requests changing two fields, in either order,
// keep both, where a gate sent whole as read would undo the other's. A rule
// with no gate takes the defaults beside the named fields; naming no gate field
// keeps the gate. A change whose resulting gate weighs past what a record may
// carry is refused ErrInvalid naming protect.gate, for anyone, writing nothing:
// only the decide sees what the change kept.
//
// Mutation: resolve against the default gate rather than the rule's — the
// second request's change drops the first's, red.
func TestAGateChangeIsResolvedInsideTheDecide(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	const main = "refs/heads/main"
	want := gitlet.MergeGate{MergeBy: gitlet.MergeByPersons, Approvals: gitlet.ApprovalsNone, Checks: []string{"unit"},
		Landing: gitlet.Landing{Depth: config.CodeDefaultQueueDepth,
			Batch: config.CodeDefaultQueueBatch}, DeleteSource: gitlet.DeleteSeats}
	for i, order := range [][2]gitlet.GateChange{
		{addChecks("unit"), {MergeBy: ptr(gitlet.MergeByPersons)}},
		{{MergeBy: ptr(gitlet.MergeByPersons)}, addChecks("unit")},
	} {
		id := n.made(lcPerson, fmt.Sprintf("resolved-%d", i), unitOwned("eng"))
		n.lands(lcGate(agent("cto"), id, main, persons, order[0]))
		got := n.lands(lcGate(lcPerson, id, main, persons, order[1]))
		sameGate(t, fmt.Sprintf("after %+v then %+v", order[0], order[1]), gateOf(t, got, main), &want)
		got = n.lands(lcProtect(lcPerson, id, main,
			&gitlet.Protection{PushRule: gitlet.PushRule{DirectPush: gitlet.DirectNone}}))
		sameGate(t, "after a request naming no gate field", gateOf(t, got, main), &want)
	}

	// PAST WHAT A RECORD MAY CARRY only with what the change keeps: human paths
	// JSON spells long (an & is six bytes) land under the bound; thirty-two long
	// checks beside them are refused, though the change alone would fit.
	id := n.made(lcPerson, "heavy", unitOwned("eng"))
	paths := make([]gitlet.HumanPath, 12)
	for i := range paths {
		paths[i] = gitlet.HumanPath{Glob: fmt.Sprintf("%02d/%s", i, strings.Repeat("&", 100))}
	}
	n.lands(lcGate(lcPerson, id, main, persons, gitlet.GateChange{HumanPaths: &gitlet.HumanPathsChange{Put: paths}}))
	checks := make([]string, gitlet.MaxRequiredChecks)
	for i := range checks {
		checks[i] = fmt.Sprintf("c%02d-%s", i, strings.Repeat("x", 60))
	}
	end := n.end()
	for _, actor := range []gitlet.Principal{lcPerson, agent("cto")} {
		_, err := n.store.Lifecycle(t.Context(), lcGate(actor, id, main, persons, addChecks(checks...)))
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "protect.gate:") ||
			!strings.Contains(err.Error(), "bytes") {
			t.Errorf("%s requiring %d long checks beside long human paths = %v, want ErrInvalid naming "+
				"protect.gate and its weight", actor.Seat, len(checks), err)
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the refused gate wrote %d records", got-end)
	}
}

// AN AGENT CANNOT LOOSEN A GATE (ADR-0088), field by field: the unit's lead,
// which maintains the repository, is refused not_permitted for each of
// gateLoosenings, the sentence naming the pattern and what the change opens,
// and nothing is written. A change tightening one field and opening another is
// refused naming only the one it opens.
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
		gitlet.GateChange{MergeBy: ptr(gitlet.MergeByPersons), Checks: &gitlet.ListChange[string]{
			Remove: []string{"lint"}}}, `drops "lint" from checks`})
	for _, tc := range rows {
		res := n.life(lcGate(cto, id, main, persons, tc.change))
		switch {
		case res.Refusal != gitlet.LifecycleNotPermitted:
			t.Errorf("the lead's change of %s: %+v, want not_permitted", tc.field, res)
			continue
		case !strings.Contains(res.Detail, "only a person may remove or loosen the protection on "+main) ||
			!strings.Contains(res.Detail, tc.opens):
			t.Errorf("the lead's change of %s refused %q, want the sentence naming %s and that it %s", tc.field,
				res.Detail, main, tc.opens)
		case !errors.Is(res.Err(), gitlet.ErrNotPermitted):
			t.Errorf("the lead's change of %s as an error is %v, want ErrNotPermitted", tc.field, res.Err())
		}
		if tc.change.MergeBy != nil && *tc.change.MergeBy == gitlet.MergeByPersons &&
			strings.Contains(res.Detail, "merge_by") {
			t.Errorf("the lead's change of %s names merge_by, which it raises: %q", tc.field, res.Detail)
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the lead's refused loosenings wrote %d records", got-end)
	}
	sameGate(t, "after the lead's refused loosenings", gateAmong(n.gatesOf("gated"), main), &tight)
}

// AN AGENT MAY TIGHTEN A GATE: the unit's lead adds a gate where the rule had
// none (the defaults plus the field named), tightens a person's loose gate in
// every field, and moves the neutral fields (landing.depth, landing.batch,
// delete_source) either way, each landing; a request naming no gate field
// tightens direct_push and keeps the gate.
//
// Mutation: judge the gate whole in loosenings, any changed field a loosening
// — the lead's first tightening is refused, red.
func TestAnAgentMayTightenAGate(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	cto := agent("cto")
	const main, release = "refs/heads/main", "refs/heads/release/*"
	id := n.made(lcPerson, "tightened", unitOwned("eng"))
	both := []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly, gitlet.ChangeClassWhitespaceOnly}
	loose := gitlet.MergeGate{MergeBy: gitlet.MergeByWriters, Approvals: gitlet.ApprovalsNone, ApprovalsUnless: both,
		HumanPaths: []gitlet.HumanPath{{Glob: "deploy/**", Unless: both}}, Checks: []string{"lint"},
		Landing: gitlet.Landing{Depth: 4, Batch: 4}, DeleteSource: gitlet.DeleteSeats, AgentsEditChecks: true}
	n.lands(lcGate(lcPerson, id, main, persons, gateTo(loose)))

	// A GATE ADDED where the rule had none: the defaults plus the field named.
	n.lands(lcProtect(cto, id, release, persons))
	got := n.lands(lcGate(cto, id, release, persons, addChecks("unit")))
	sameGate(t, "a gate the lead added", gateOf(t, got, release), &gitlet.MergeGate{
		MergeBy: gitlet.MergeByWriters, Approvals: gitlet.ApprovalsNone, Checks: []string{"unit"},
		Landing: gitlet.Landing{Depth: config.CodeDefaultQueueDepth,
			Batch: config.CodeDefaultQueueBatch}, DeleteSource: gitlet.DeleteSeats})

	for _, change := range []gitlet.GateChange{
		{MergeBy: ptr(gitlet.MergeByPersons)},
		{Approvals: ptr(gitlet.ApprovalsOne)},
		{ApprovalsUnless: &gitlet.ListChange[gitlet.ChangeClass]{
			Remove: []gitlet.ChangeClass{gitlet.ChangeClassWhitespaceOnly}}},
		{HumanPaths: &gitlet.HumanPathsChange{Put: []gitlet.HumanPath{{Glob: ".crewlet/**"},
			{Glob: "deploy/**", Unless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly}}}}},
		addChecks("unit"),
		{OwnersApprove: ptr(true)},
		{AgentsEditChecks: ptr(false)},
		// NEUTRAL, either way.
		{Landing: &gitlet.LandingChange{Depth: ptr(1), Batch: ptr(gitlet.MaxQueueBatch)}},
		{DeleteSource: ptr(gitlet.DeleteNone)},
		{Landing: &gitlet.LandingChange{Depth: ptr(gitlet.MaxQueueDepth), Batch: ptr(1)}},
		{DeleteSource: ptr(gitlet.DeleteSeats)},
	} {
		if res := n.life(lcGate(cto, id, main, persons, change)); res.Refusal != "" ||
			res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
			t.Fatalf("the lead's change %+v: %+v, want it recorded and applied", change, res)
		}
	}
	got = n.lands(lcProtect(cto, id, main, &gitlet.Protection{PushRule: gitlet.PushRule{DirectPush: gitlet.DirectNone}}))
	sameGate(t, "tightened by the lead", gateOf(t, got, main), &gitlet.MergeGate{
		MergeBy: gitlet.MergeByPersons, Approvals: gitlet.ApprovalsOne,
		ApprovalsUnless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly},
		HumanPaths: []gitlet.HumanPath{{Glob: ".crewlet/**"},
			{Glob: "deploy/**", Unless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly}}},
		Checks:        []string{"lint", "unit"},
		Landing:       gitlet.Landing{Depth: gitlet.MaxQueueDepth, Batch: 1},
		OwnersApprove: true, DeleteSource: gitlet.DeleteSeats,
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
	var removal gitlet.GateChange
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
	both := []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly, gitlet.ChangeClassWhitespaceOnly}
	sameGate(t, "loosened by a person", gateAmong(n.gatesOf("loosened"), main), &gitlet.MergeGate{
		MergeBy: gitlet.MergeByWriters, Approvals: gitlet.ApprovalsNone, ApprovalsUnless: both,
		HumanPaths: []gitlet.HumanPath{{Glob: "deploy/**", Unless: both}}, Checks: []string{"lint"},
		Landing: gitlet.Landing{Depth: 4, Batch: 4}, DeleteSource: gitlet.DeleteSeats, AgentsEditChecks: true,
	})
	got := n.lands(lcGate(lcPerson, id, main, persons, removal))
	sameGate(t, "removed by a person", gateOf(t, got, main), nil)
}

// AN AGENT CANNOT REMOVE A PATH OWNER UNDER owners_approve, where a merge
// waits for the path owners: the unit's lead is refused not_permitted taking a
// glob, an owner whole, or a glob beside one it gives, the sentence naming
// path_owners, what it takes and the rule, and nothing is written. Taking a
// glob no owner holds is answered from the row; giving globs only asks more of
// a merge, and lands. Without owners_approve the same removals land; a person
// takes them under it.
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
		n.lands(lcOwners(lcPerson, id, []gitlet.PathOwner{pathOwner(gitlet.OwnerUnit, "plat", "deploy/**", "infra/**"),
			pathOwner(gitlet.OwnerSeat, "dev", "Makefile")}, nil))
	}
	n.lands(lcGate(lcPerson, approved, main, persons, gitlet.GateChange{OwnersApprove: ptr(true)}))
	n.lands(lcGate(lcPerson, plain, main, persons, addChecks("unit")))

	end := n.end()
	for _, tc := range []struct {
		what        string
		add, remove []gitlet.PathOwner
		takes       string
	}{
		{"a glob", nil, []gitlet.PathOwner{pathOwner(gitlet.OwnerUnit, "plat", "infra/**")}, "infra/** from unit plat"},
		{"an owner whole", nil, []gitlet.PathOwner{pathOwner(gitlet.OwnerSeat, "dev", "Makefile")},
			"Makefile from seat dev"},
		{"a glob beside one it gives", []gitlet.PathOwner{pathOwner(gitlet.OwnerUnit, "plat", "web/**")},
			[]gitlet.PathOwner{pathOwner(gitlet.OwnerUnit, "plat", "deploy/**")}, "deploy/** from unit plat"},
	} {
		res := n.life(lcOwners(cto, approved, tc.add, tc.remove))
		switch {
		case res.Refusal != gitlet.LifecycleNotPermitted:
			t.Errorf("the lead taking %s: %+v, want not_permitted", tc.what, res)
		case !strings.Contains(res.Detail, "path_owners") || !strings.Contains(res.Detail, tc.takes) ||
			!strings.Contains(res.Detail, main):
			t.Errorf("the lead taking %s refused %q, want the sentence naming path_owners, %s and %s", tc.what,
				res.Detail, tc.takes, main)
		case !errors.Is(res.Err(), gitlet.ErrNotPermitted):
			t.Errorf("the lead taking %s as an error is %v, want ErrNotPermitted", tc.what, res.Err())
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the lead's refused removals wrote %d records", got-end)
	}
	if res := n.life(lcOwners(cto, approved, nil, []gitlet.PathOwner{pathOwner(gitlet.OwnerUnit, "plat", "web/**")})); res.Refusal != "" || res.Outcome != statelog.OutcomeApplied || !res.Position.IsZero() {
		t.Errorf("the lead taking a glob no owner holds: %+v, want it answered from the row", res)
	}
	n.lands(lcOwners(cto, approved, []gitlet.PathOwner{pathOwner(gitlet.OwnerUnit, "sales", "sales/**")}, nil))
	n.lands(lcOwners(cto, plain, nil, []gitlet.PathOwner{pathOwner(gitlet.OwnerUnit, "plat", "infra/**"),
		pathOwner(gitlet.OwnerSeat, "dev", "Makefile")}))
	n.lands(lcOwners(lcPerson, approved, nil, []gitlet.PathOwner{pathOwner(gitlet.OwnerUnit, "plat", "infra/**"),
		pathOwner(gitlet.OwnerSeat, "dev", "Makefile")}))
	for name, want := range map[string][]gitlet.PathOwner{
		"approved": {pathOwner(gitlet.OwnerUnit, "plat", "deploy/**"), pathOwner(gitlet.OwnerUnit, "sales", "sales/**")},
		"plain":    {pathOwner(gitlet.OwnerUnit, "plat", "deploy/**")},
	} {
		if got := n.ownersOf(name); !reflect.DeepEqual(got, want) {
			t.Errorf("%s's path owners are %+v, want %+v", name, got, want)
		}
	}
}

// A PROTECT REQUEST'S ANSWER NAMES ITS RULE WHOLE (gitlet.LifecycleResult.Rule),
// since the row carries no merge gate (RepoRef.Protections are PushRule
// values): read back with the row when the record applied or a repeat
// collapsed onto the one before, and as the decide held it when no record was
// needed. A removed rule, and every other kind's answer, names none.
//
// Mutations: read the row back without the pattern's rule — the applied
// answer names none, red; answer a request needing no record with no rule —
// the repeated gate names none, red.
func TestAProtectAnswerNamesItsRuleWhole(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	const main = "refs/heads/main"
	id := n.made(lcPerson, "answered", unitOwned("eng"))
	gate := tightGate()
	whole := func(stage string, res gitlet.LifecycleResult, direct gitlet.DirectPush) {
		t.Helper()
		sameGate(t, stage, gateOf(t, res, main), &gate)
		if res.Rule.DirectPush != direct {
			t.Fatalf("%s: the answer's rule pushes %s, want %s", stage, res.Rule.DirectPush, direct)
		}
	}

	set := lcGate(lcPerson, id, main, persons, gateTo(gate))
	whole("applied", n.lands(set), gitlet.DirectPersons)
	if again := n.life(set); again.Outcome != statelog.OutcomeApplied || again.Refusal != "" {
		t.Fatalf("the same operation again: %+v, want it applied", again)
	} else {
		whole("repeated", again, gitlet.DirectPersons)
	}
	same := n.life(lcGate(lcPerson, id, main, persons, gateTo(gate)))
	if same.Outcome != statelog.OutcomeApplied || !same.Position.IsZero() {
		t.Fatalf("the gate sent again as it stands: %+v, want it answered from the row", same)
	}
	whole("needing no record", same, gitlet.DirectPersons)
	tightened := n.lands(lcProtect(lcPerson, id, main,
		&gitlet.Protection{PushRule: gitlet.PushRule{DirectPush: gitlet.DirectNone}}))
	whole("a field it names", tightened, gitlet.DirectNone)

	if removed := n.lands(lcProtect(lcPerson, id, main, nil)); removed.Rule != nil {
		t.Errorf("the rule's removal answers the rule %+v, want none", *removed.Rule)
	}
	described := "answered, and described"
	if changed := n.lands(lcSettings(lcPerson, id, gitlet.RepoChange{Description: &described})); changed.Rule != nil {
		t.Errorf("a settings change answers the rule %+v, want none", *changed.Rule)
	}
}

// A PROTECT CHANGE KEEPS THE PUSH CONTROLS IT DOES NOT NAME, as the deciding
// node's snapshot holds them: a gate-only change leaves a branch frozen at
// direct_push none with allow_force on, for a person and for the unit's lead,
// whose change tightens rather than being refused a direct_push loosening it
// never named. Naming one control changes that one alone. A new pattern takes
// each unnamed control's least (persons, off, off) and no gate.
//
// Mutation: start every resolved rule from its least, keeping only the held
// gate — the person's gate-only change sends main back to direct pushes, red.
func TestAProtectChangeKeepsTheControlsItDoesNotName(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	cto := agent("cto")
	const main, release = "refs/heads/main", "refs/heads/release/*"
	id := n.made(lcPerson, "frozen", unitOwned("eng"))
	frozen := gitlet.PushRule{Pattern: main, DirectPush: gitlet.DirectNone, AllowForce: true}
	n.lands(lcProtect(lcPerson, id, main, &gitlet.Protection{PushRule: frozen}))
	pushOf := func(stage string, res gitlet.LifecycleResult, want gitlet.PushRule) {
		t.Helper()
		if res.Rule == nil || res.Rule.PushRule != want {
			t.Fatalf("%s: the rule reads %+v, want the push rule %+v", stage, res.Rule, want)
		}
	}

	pushOf("a person's gate-only change", n.lands(lcGate(lcPerson, id, main, nil, addChecks("unit"))), frozen)
	pushOf("the lead's gate-only change", n.lands(lcGate(cto, id, main, nil, addChecks("lint"))), frozen)
	if gate := gateAmong(n.gatesOf("frozen"), main); gate == nil || strings.Join(gate.Checks, " ") != "lint unit" {
		t.Fatalf("after two gate-only changes the gate reads %+v, want both checks", gate)
	}
	off := frozen
	off.AllowForce = false
	pushOf("the lead's allow_force alone", n.lands(lcControls(cto, id, main,
		gitlet.ProtectChange{AllowForce: ptr(false)})), off)

	pushOf("a new rule naming nothing", n.lands(lcControls(cto, id, release, gitlet.ProtectChange{})),
		gitlet.PushRule{Pattern: release, DirectPush: gitlet.DirectPersons})
	if gate := gateAmong(n.gatesOf("frozen"), release); gate != nil {
		t.Fatalf("a new rule naming no gate field holds the gate %+v, want none", *gate)
	}
}

// lcControls is a protect request for pattern on the repository id changing
// what change names, and nothing else.
func lcControls(actor gitlet.Principal, id, pattern string, change gitlet.ProtectChange) gitlet.LifecycleRequest {
	req := lcOn(gitlet.LifecycleProtect, actor, id)
	change.Pattern = pattern
	req.Protect = &change
	return req
}

// A GATE'S LIST CHANGE KEEPS WHAT ANOTHER CHANGE ADDED: the deciding node
// applies its adds and removes to the list its snapshot holds, so two requests
// each adding a check, in either order, keep both, where lists sent whole as
// read would drop one; the lead is refused neither, as neither drops anything.
// Human paths, keyed by glob, hold the same; a person removing one check and
// exempting one class keeps the rest.
//
// Mutation: replace a list with what its change adds — the second request
// drops the first's check, red.
func TestAGateListChangeKeepsWhatAnotherChangeAdded(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	cto := agent("cto")
	const main = "refs/heads/main"
	for i, actor := range []gitlet.Principal{lcPerson, cto} {
		for j, order := range [][2]string{{"e2e", "fuzz"}, {"fuzz", "e2e"}} {
			name := fmt.Sprintf("listed-%d-%d", i, j)
			id := n.made(lcPerson, name, unitOwned("eng"))
			n.lands(lcGate(lcPerson, id, main, persons, gateTo(tightGate())))
			n.lands(lcGate(actor, id, main, nil, addChecks(order[0])))
			n.lands(lcGate(actor, id, main, nil, addChecks(order[1])))
			n.lands(lcGate(actor, id, main, nil, gitlet.GateChange{HumanPaths: &gitlet.HumanPathsChange{
				Put: []gitlet.HumanPath{{Glob: "web/**"}}}}))
			n.lands(lcGate(actor, id, main, nil, gitlet.GateChange{HumanPaths: &gitlet.HumanPathsChange{
				Put: []gitlet.HumanPath{{Glob: ".crewlet/**"}}}}))
			want := tightGate()
			want.Checks = []string{"e2e", "fuzz", "lint", "unit"}
			want.HumanPaths = []gitlet.HumanPath{{Glob: ".crewlet/**"}, want.HumanPaths[0], {Glob: "web/**"}}
			sameGate(t, fmt.Sprintf("%s adding %s then %s", actor.Kind, order[0], order[1]),
				gateAmong(n.gatesOf(name), main), &want)
		}
	}

	id := n.made(lcPerson, "trimmed", unitOwned("eng"))
	n.lands(lcGate(lcPerson, id, main, persons, gateTo(tightGate())))
	n.lands(lcGate(lcPerson, id, main, nil, gitlet.GateChange{
		Checks: &gitlet.ListChange[string]{Remove: []string{"lint"}},
		ApprovalsUnless: &gitlet.ListChange[gitlet.ChangeClass]{
			Add: []gitlet.ChangeClass{gitlet.ChangeClassWhitespaceOnly}}}))
	want := tightGate()
	want.Checks = []string{"unit"}
	want.ApprovalsUnless = []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly, gitlet.ChangeClassWhitespaceOnly}
	sameGate(t, "a person removing lint and exempting whitespace", gateAmong(n.gatesOf("trimmed"), main), &want)
}
