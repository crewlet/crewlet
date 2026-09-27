package chart_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// A SEAT'S `manages:` LIST IS STRUCTURE (record version 3).
//
// It is who a seat's manager is, and a rename's cascade — a structural record
// on the tree's subject — moves the entries naming the renamed object. While
// the list rode the seat's CONTENT record it had two writers on two subjects,
// and only one of them could see the other: a content write arbitrates on the
// seat's own subject, which a rename never moves.

// seatNamed writes one seat's prose, with the rig's own party.
func (r *writeRig) seatNamed(opID, handle, name string) {
	r.t.Helper()
	if _, err := r.writer.WriteSeat(r.t.Context(), opID,
		chart.SeatContent{Handle: handle, Name: name}); err != nil {
		r.t.Fatalf("write the content of %s: %v", handle, err)
	}
}

// view is the organisation this rig's rows derive.
func (r *writeRig) view() *org.Organization {
	r.t.Helper()
	rows, err := r.reader().Read(r.t.Context(), session())
	if err != nil {
		r.t.Fatalf("read the chart: %v", err)
	}
	return org.FromRows(rows, org.Settings{Name: "Acme"}).Org
}

// managerOf is the handle of whoever the organisation says manages handle, or
// empty for nobody.
func managerOf(o *org.Organization, handle string) string {
	role := o.Role(handle)
	if role == nil {
		return ""
	}
	if boss := o.Manager(role); boss != nil {
		return boss.Handle()
	}
	return ""
}

// A LEAD'S GOAL EDIT DECIDED BEFORE A RENAME APPLIED LEAVES THE ENTRY THE RENAME
// MOVED.
//
// The race the finding names, run for real: `report` manages `bob`, an
// administrator renames `bob` to `robert`, and `report`'s lead — on a node that
// has not applied the rename yet — corrects a goal. The seat's own subject has
// not moved, so the broker accepts the lead's record after the rename's.
//
// While the content record restated the list, its apply wrote `report → bob`
// back over the `report → robert` the cascade had just written, and a later
// hire on the retired `bob` then made `report` the newcomer's manager — a lead
// relation nobody gave. The content record carries no list now, so the entry
// stays on the address the seat answers to, and the newcomer is nobody's
// report.
//
// Mutation: have a version-3 seat content apply replace the list again
// ([chart.Applier] `applySeat`), or write content at version 2, and the entry
// is cleared; drop the replay's move and nothing here changes — that is the
// batch case below.
func TestALeadsGoalEditDecidedBeforeARenameLeavesTheRenamedEntry(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	// BOB WAS HIRED AS `rob`, so `bob` is an address and never his identity:
	// the one kind a creation may later take.
	r.batch("op-hire",
		op(chart.OpCreateSeat, chart.KindSeat, "report", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "rob", ""))
	r.applySeatRekey("op-to-bob", "bob", "rob")
	r.batch("op-list", managesOp("report", "bob"))
	r.seatNamed("op-report", "report", "Report")
	r.seatNamed("op-bob", "bob", "Bob")
	r.drain()

	// THE RENAME IS PUBLISHED AND NOT APPLIED HERE, which is the node the
	// lead's request reaches.
	if _, err := r.writer.Unwaited().WriteBatch(t.Context(), "op-rename",
		chart.Batch{Operations: []chart.Operation{
			renameOp(chart.KindSeat, "bob", "robert")}}); err != nil {
		t.Fatalf("publish the rename: %v", err)
	}
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = 'report'`); !slices.Equal(
		got, []string{"bob"}) {
		t.Fatalf("the rename applied before the lead decided: report manages %v", got)
	}

	// THE LEAD SENDS BACK THE SEAT AS THEY READ IT, with a new goal, and
	// holds no grant: a prose edit is theirs.
	lead := r.writer.As("mira", chart.AuthorHuman, nil, chart.Provenance{}).Unwaited()
	if _, err := lead.WriteSeat(t.Context(), "op-goal", chart.SeatContent{
		Handle: "report", Name: "Report", Goal: "ship the quarter",
	}); err != nil {
		t.Fatalf("the lead's goal edit: %v", err)
	}
	r.drain()

	if got := r.mustSeat("report").Goal; got != "ship the quarter" {
		t.Errorf("report's goal is %q, want the lead's edit", got)
	}
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = 'report'`); !slices.Equal(
		got, []string{"robert"}) {
		t.Errorf("report manages %v after the lead's edit, want [robert] — the "+
			"content record wrote the list it was decided on back over the "+
			"rename's cascade", got)
	}

	// AND A HIRE ON THE RETIRED ADDRESS IS NOBODY'S REPORT.
	r.batch("op-newcomer", op(chart.OpCreateSeat, chart.KindSeat, "bob", ""))
	r.seatNamed("op-newcomer-content", "bob", "Another Bob")
	r.drain()
	o := r.view()
	if got := managerOf(o, "robert"); got != "report" {
		t.Errorf("robert's manager is %q, want report", got)
	}
	if got := managerOf(o, "bob"); got == "report" {
		t.Error("the newcomer on the retired address is report's — a lead " +
			"relation nobody gave, reached through an entry the rename had moved")
	}
}

// A `manages:` LIST TAKES THE COMPANY'S GRANT, AS ALL STRUCTURE DOES.
//
// It is who a seat's manager is, so a lead who could write it could add the
// founder to a report's list and become the founder's ancestor, with every
// owner-or-lead read and every further edit that brings. A set_manages is a
// structural record, and the domain refuses it below `config:write` whoever the
// door admitted — naming the grant, and publishing nothing.
func TestAManagesListIsStructureALeadCannotWrite(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire",
		op(chart.OpCreateSeat, chart.KindSeat, "report", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "founder", ""))
	r.drain()

	lead := r.writer.As("mira", chart.AuthorHuman, nil, chart.Provenance{})
	_, err := lead.WriteBatch(t.Context(), "op-lead", chart.Batch{
		Operations: []chart.Operation{managesOp("report", "founder")}})
	var refusal *chart.GrantRefusal
	if !errors.As(err, &refusal) || !errors.Is(err, chart.ErrRefused) {
		t.Fatalf("a lead's set_manages: err = %v, want a grant refusal", err)
	}
	if refusal.Class != chart.ClassStructure ||
		!slices.Equal(refusal.Grants, []iam.Grant{iam.GrantConfigWrite}) {
		t.Errorf("refused as %s needing %v, want structure needing [config:write]",
			refusal.Class, refusal.Grants)
	}
	r.drain()
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = 'report'`); len(got) != 0 {
		t.Fatalf("the refused list reached the rows: %v", got)
	}

	admin := r.writer.As("ops", chart.AuthorHuman,
		[]iam.Grant{iam.GrantConfigWrite}, chart.Provenance{})
	r.whileDraining(func() {
		if _, err := admin.WriteBatch(t.Context(), "op-admin", chart.Batch{
			Operations: []chart.Operation{managesOp("report", "Founder")}}); err != nil {
			t.Errorf("the company's grant setting the list: %v", err)
		}
	})
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = 'report'`); !slices.Equal(
		got, []string{"founder"}) {
		t.Errorf("report manages %v, want [founder]", got)
	}
	// AND AN EMPTY LIST IS A SEAT THAT MANAGES NOBODY.
	r.whileDraining(func() {
		if _, err := admin.WriteBatch(t.Context(), "op-clear", chart.Batch{
			Operations: []chart.Operation{managesOp("report")}}); err != nil {
			t.Errorf("clear the list: %v", err)
		}
	})
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = 'report'`); len(got) != 0 {
		t.Errorf("report manages %v after a set_manages with no entries, want nobody", got)
	}
}

// A BATCH THAT RENAMES WHAT A LIST NAMES PUBLISHES THE LIST THE RENAME LEAVES.
//
// Every seat edge states the seat's whole list, and the apply writes it after
// the renames' cascade — so an edge carrying the address a rename in the same
// batch retired would write it straight back, the reversion version 3 ends,
// reached from inside one batch. The replay moves the entries exactly as the
// cascade will: a list set before the rename, and one the rows already hold for
// a seat the batch moves, each land on the new address, and a list set after
// the rename is the one it states.
//
// Mutation: drop the replay's move ([working.moveManages]) and the first two
// cases publish and store `bob`.
func TestABatchThatRenamesWhatAListNamesPublishesTheListTheRenameLeaves(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		batch []chart.Operation
		// edge is the report edge the batch must publish.
		edge []string
	}{
		{"a list set before the rename", []chart.Operation{
			managesOp("report", "bob", "ceo"),
			renameOp(chart.KindSeat, "bob", "robert"),
		}, []string{"ceo", "robert"}},
		{"a stored list, its seat moved after the rename", []chart.Operation{
			renameOp(chart.KindSeat, "bob", "robert"),
			op(chart.OpMove, chart.KindSeat, "report", "eng"),
		}, []string{"robert"}},
		{"a list set after the rename", []chart.Operation{
			renameOp(chart.KindSeat, "bob", "robert"),
			managesOp("report", "robert"),
		}, []string{"robert"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newWriteRig(t)
			r.batch("op-hire",
				op(chart.OpCreateUnit, chart.KindUnit, "eng", ""),
				op(chart.OpCreateSeat, chart.KindSeat, "report", ""),
				op(chart.OpCreateSeat, chart.KindSeat, "bob", ""),
				op(chart.OpCreateSeat, chart.KindSeat, "ceo", ""),
				managesOp("report", "bob"))
			r.drain()

			var edges []chart.Edge
			if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
				var err error
				edges, _, err = chart.Batch{Operations: tc.batch}.Validate(
					t.Context(), tx, nil)
				return err
			}); err != nil {
				t.Fatalf("validate the batch: %v", err)
			}
			var report *chart.Edge
			for i := range edges {
				if edges[i].Object == (chart.ObjectRef{Kind: chart.KindSeat, ID: "report"}) {
					report = &edges[i]
				}
			}
			if report == nil || !slices.Equal(report.Manages, tc.edge) {
				t.Fatalf("the batch published report's edge as %+v, want the "+
					"list %v", report, tc.edge)
			}

			r.batch("op-batch", tc.batch...)
			r.drain()
			if got := r.column(`SELECT target FROM chart_manages
				WHERE manager = 'report' ORDER BY target`); !slices.Equal(got, tc.edge) {
				t.Errorf("report manages %v, want %v", got, tc.edge)
			}
		})
	}
}

// A UNIT RENAMED ONTO A KEY A SEAT ANSWERS TO KEEPS ITS ENTRIES ON THE KEY IT
// RETIRED — in the replay as at the apply.
//
// A seat reading of a spelling wins, so an entry moved onto that key would name
// the seat. The cascade leaves such entries on the unit's retired key, which
// still reaches it; a replay that moved them would publish an edge handing the
// seat's manager a person instead of a team.
func TestTheReplayLeavesAnEntryWhereTheCascadeLeavesIt(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire",
		op(chart.OpCreateUnit, chart.KindUnit, "ops", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "omar", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "vee", ""),
		managesOp("vee", "ops"))
	r.drain()
	r.batch("op-batch",
		renameOp(chart.KindUnit, "ops", "omar"),
		op(chart.OpMove, chart.KindSeat, "vee", "omar"))
	r.drain()
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = 'vee'`); !slices.Equal(
		got, []string{"ops"}) {
		t.Errorf("vee manages %v, want [ops] — `omar` is a seat's handle, so an "+
			"entry moved onto it would name him rather than the team", got)
	}
}

// A SET_MANAGES IS HELD TO THE LIST'S RULES AND TO ITS OWN SHAPE.
//
// Every entry is an address — a seat's handle or a unit's key — because one
// that could never be an address names nothing any chart can hold, and the
// company file refuses the same entry; the list is bounded at MaxManages,
// counted as it lands; only a seat manages anybody; and no other operation may
// carry a list, because nothing would read it. Each is refused whole, before
// anything is published, and the list that lands is folded and de-duplicated.
func TestASetManagesIsHeldToTheListsRules(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-report", chart.KindSeat, "report", ""))
	h.must(create("op-eng", chart.KindUnit, "eng", ""))

	long := make([]string, 0, chart.MaxManages+1)
	for i := range chart.MaxManages + 1 {
		long = append(long, strings.Repeat("s", 1+i%8)+string(rune('a'+i/8)))
	}
	for _, tc := range []struct {
		name string
		op   chart.Operation
		rule string
	}{
		{"an entry no address can be", managesOp("report", "platform/core"),
			chart.RuleBadKey},
		{"a wildcard", managesOp("report", "*"), chart.RuleBadKey},
		{"past the cap", managesOp("report", long...), chart.RuleTooManyManaged},
		{"on a unit", chart.Operation{Kind: chart.OpSetManages,
			Object:  chart.ObjectRef{Kind: chart.KindUnit, ID: "eng"},
			Manages: []string{"report"}}, chart.RuleUnknownKind},
		{"on a seat nobody created", managesOp("ghost", "report"),
			chart.RuleNoSuchObject},
		{"a list on a move", chart.Operation{Kind: chart.OpMove,
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "report"},
			Parent: "eng", Manages: []string{"eng"}}, chart.RuleUnusedField},
	} {
		_, _, err := h.validate(chart.Batch{Operations: []chart.Operation{tc.op}})
		if ref := refusal(t, err); ref.Rule != tc.rule {
			t.Errorf("%s: rule = %q (%s), want %q", tc.name, ref.Rule, ref.Detail,
				tc.rule)
		}
	}

	// THE CAP COUNTS WHAT LANDS: the same address in two spellings is one row.
	folded := make([]string, 0, 2*chart.MaxManages)
	for i := range chart.MaxManages {
		entry := "seat-" + strings.Repeat("x", i%5) + string(rune('a'+i/5))
		folded = append(folded, entry, strings.ToUpper(entry))
	}
	edges, _, err := h.validate(chart.Batch{Operations: []chart.Operation{
		managesOp("report", folded...)}})
	if err != nil {
		t.Fatalf("a list of %d addresses in two spellings each: %v",
			chart.MaxManages, err)
	}
	if len(edges) != 1 || len(edges[0].Manages) != chart.MaxManages ||
		!slices.IsSorted(edges[0].Manages) {
		t.Errorf("the edge carries %d entries, want %d folded and sorted",
			len(edges[0].Manages), chart.MaxManages)
	}
}

// AN IMPORT STATES EVERY SEAT'S LIST, WHOLE, AND ITS WRITER HOLDS IT TO THE
// LIST'S RULES.
//
// A company file's `manages:` is structure like the rest of its chart, so the
// import that places a seat states the list too, as the apply stores it — and
// the content records that follow carry none. A unit's edge may carry no list,
// because a unit manages nobody.
func TestAnImportStatesEachSeatsManagesList(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	seat := func(handle string, manages ...string) chart.Edge {
		return chart.Edge{Object: chart.ObjectRef{Kind: chart.KindSeat, ID: handle},
			Kind: chart.SeatAgent, Manages: manages}
	}
	r.whileDraining(func() {
		if _, err := r.writer.WriteImport(t.Context(), "op-import", "rev-1",
			[]chart.Edge{seat("ceo", "CTO", "cto", "eng"), seat("cto")}); err != nil {
			t.Errorf("import: %v", err)
		}
	})
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = 'ceo'
		ORDER BY target`); !slices.Equal(got, []string{"cto", "eng"}) {
		t.Errorf("ceo manages %v, want [cto eng] — folded and de-duplicated", got)
	}

	for _, tc := range []struct {
		name string
		edge chart.Edge
	}{
		{"a unit's list", chart.Edge{
			Object:  chart.ObjectRef{Kind: chart.KindUnit, ID: "eng"},
			Manages: []string{"cto"}}},
		{"an entry no address can be", seat("cto", "a/b")},
	} {
		_, err := r.writer.WriteImport(t.Context(), "op-"+tc.name, "rev-2",
			[]chart.Edge{tc.edge})
		if !errors.Is(err, chart.ErrRefused) {
			t.Errorf("%s: err = %v, want a refusal", tc.name, err)
		}
	}

	// AND A RE-IMPORT OF A LATER REVISION REPLACES THE LIST WHOLE.
	r.whileDraining(func() {
		if _, err := r.writer.WriteImport(t.Context(), "op-import-3", "rev-3",
			[]chart.Edge{seat("ceo", "cto"), seat("cto")}); err != nil {
			t.Errorf("import the next revision: %v", err)
		}
	})
	if got := r.column(`SELECT target FROM chart_manages WHERE manager = 'ceo'`); !slices.Equal(
		got, []string{"cto"}) {
		t.Errorf("ceo manages %v after the next revision, want [cto]", got)
	}
}

// THE PERMANENT READERS: a record below version 3 is applied as it was meant.
//
// A version-1 or version-2 content record replaced the seat's list with the one
// it carried — and still does, which is the race above written down: decided
// before a rename applied, it writes the retired address back. An edge below
// version 3 carries no list and leaves the stored one alone, whatever it holds;
// a version-3 content record carries none and leaves it alone too, even one a
// writer stuffed a list into; and a set_manages below version 3 is a verb no
// writer at that version declared, so its record fails rather than applying
// under a word nobody meant.
func TestARecordBelowVersionThreeIsAppliedAsItWasMeant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.must(create("op-report", chart.KindSeat, "report", ""))
	h.must(create("op-bob", chart.KindSeat, "bob", ""))
	h.must(setManages("op-list", "report", "", "bob"))
	listed := func() []string {
		return h.column(`SELECT target FROM chart_manages WHERE manager = 'report'
			ORDER BY target`)
	}

	h.must(place("op-rename", renameEdge(chart.KindSeat, "bob", "robert", "", "")))
	h.must(v2(seatRecord("op-v2", "report", "", func(p *chart.SeatPayload) {
		p.Manages = []string{"bob"}
	})))
	if got := listed(); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("a version-2 content record left report managing %v, want "+
			"[bob] — what version 2 meant", got)
	}

	h.must(setManages("op-relist", "report", "", "robert"))
	h.must(seatRecord("op-v3", "report", "", func(p *chart.SeatPayload) {
		p.Manages = []string{"somebody"}
	}))
	if got := listed(); !slices.Equal(got, []string{"robert"}) {
		t.Errorf("a version-3 content record moved the list to %v", got)
	}

	h.must(v2(place("op-v2-move", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "report"},
		Op:     chart.OpMove, Kind: chart.SeatAgent})))
	if got := listed(); !slices.Equal(got, []string{"robert"}) {
		t.Errorf("a version-2 edge with no list cleared it to %v", got)
	}
	if _, _, err := h.apply(v2(setManages("op-v2-list", "report", "", "ceo"))); err == nil {
		t.Error("a set_manages at version 2 applied — no writer at that " +
			"version declared the verb")
	}
	if got := listed(); !slices.Equal(got, []string{"robert"}) {
		t.Errorf("a refused version-2 set_manages moved the list to %v", got)
	}
}
