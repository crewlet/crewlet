package builtin

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/toolname"
)

// rowShape is what an authority row asks, as far as a gate names it.
type rowShape struct {
	class authz.Class
	app   iam.Grant
	role  artifacts.Role
}

// gateRows is the row shape each gate stands for: the meaning [Gate]'s
// constants state in words, written once more as data so the walk below can
// hold a declared gate to the row it names.
var gateRows = map[Gate]rowShape{
	GateEverybody:      {class: authz.ClassEverybody},
	GateSelf:           {class: authz.ClassSelf},
	GateWorklet:        {class: authz.ClassApp, app: iam.GrantWorklet},
	GateWorkletAsked:   {class: authz.ClassAppOrAsked, app: iam.GrantWorklet},
	GateProjectLead:    {class: authz.ClassDestructive, app: iam.GrantWorklet},
	GateDoclet:         {class: authz.ClassApp, app: iam.GrantDoclet},
	GateChatlet:        {class: authz.ClassApp, app: iam.GrantChatlet},
	GateArtifacts:      {class: authz.ClassApp, app: iam.GrantArtifacts},
	GateArtifactViewer: {class: authz.ClassShared, role: artifacts.RoleViewer},
	GateArtifactEditor: {class: authz.ClassShared, role: artifacts.RoleEditor, app: iam.GrantArtifacts},
	GateArtifactOwner:  {class: authz.ClassShared, role: artifacts.RoleOwner},
}

// shapeOf is a tool's authority row as a gate would name it, and false for a
// tool no row decides.
//
// A PERSONAL VERB THAT TAKES NO NAME IS AN APP VERB TO ITS CALLER: its object
// is always the caller's own record ([subject.objectFor]), so the owner arm
// of its class always holds and the row's app is all that is left to ask.
// That is `my_work`, filed under [GateWorklet], and the reading is checked by
// deciding it in [TestAnAppGateAdmitsAnAgentHoldingTheAppAndNoOther].
func shapeOf(tool string) (rowShape, bool) {
	action := authz.Action(tool)
	class, ok := authz.ClassOf(action)
	if !ok {
		return rowShape{}, false
	}
	app, _ := authz.AppOf(action)
	role, _ := authz.MinRoleOf(action)
	shape := rowShape{class: class, app: app, role: role}
	if class == authz.ClassOwnRecord || class == authz.ClassOwnOrLead {
		if s, governed := subjectOf[tool]; governed && aboutTheCaller(s) {
			shape.class = authz.ClassApp
		}
	}
	return shape, true
}

// aboutTheCaller reports whether a tool's subject can only ever be its
// caller's own record: a person it names nobody in.
func aboutTheCaller(s subject) bool {
	return s.kind == authz.KindPerson && s.owner == "" && s.personal == "" &&
		s.container == "" && !s.inTool
}

// EVERY BUILT-IN'S GATE IS WHAT ITS AUTHORITY ROW ASKS.
//
// The catalogue DECLARES who may call each tool, and internal/authz DECIDES
// it; a declaration nothing compared with the decision is one a reader trusts
// and nobody keeps. So each filed tool's row (its class, its app and its
// least artifact role) must be exactly the row its gate names. Changing a row
// in internal/authz, or the gate a tool is filed with, turns this red until
// the two say the same thing.
//
// A TOOL NO ROW DECIDES may only be the discovery pair, which the runner
// hands to every phase that discovers, and which nothing gates: filed under
// anything but [GateEverybody], the catalogue would claim a check nobody
// makes.
func TestEveryBuiltinsGateIsWhatItsAuthorityRowAsks(t *testing.T) {
	t.Parallel()
	if len(filed) == 0 {
		t.Fatal("the catalogue files nothing, so this walk certifies nothing")
	}
	for _, f := range filed {
		shape, governed := shapeOf(f.tool)
		if !governed {
			if !slices.Contains(toolname.Discovery(), f.tool) {
				t.Errorf("%s is filed and has no authority row; only the discovery "+
					"pair is offered with no row deciding it", f.tool)
			} else if f.gate != GateEverybody {
				t.Errorf("%s is filed as %q; no row decides it, so nothing is asked "+
					"and its gate is %q", f.tool, f.gate, GateEverybody)
			}
			continue
		}
		if humanOnly, _ := authz.HumanOnlyOf(authz.Action(f.tool)); humanOnly {
			t.Errorf("%s is filed in a catalogue of what agents hold and its row "+
				"bars every agent", f.tool)
		}
		want, named := gateRows[f.gate]
		if !named {
			t.Errorf("%s is filed as %q, which names no row shape", f.tool, f.gate)
			continue
		}
		if shape != want {
			t.Errorf("%s is filed as %q, which names %+v; its authority row asks %+v",
				f.tool, f.gate, want, shape)
		}
	}
}

// AND EVERY GATE NAMES ONE ROW SHAPE AND IS NAMED BY A TOOL.
//
// Both directions of the vocabulary: a gate with no shape here is one the
// walk above cannot hold anybody to, and a gate no tool is filed under is a
// word the dashboard carries for a row no built-in has.
func TestEveryGateNamesARowAToolHas(t *testing.T) {
	t.Parallel()
	for _, g := range Gates {
		if !g.Valid() {
			t.Errorf("gate %q is listed and not valid", g)
		}
		if _, ok := gateRows[g]; !ok {
			t.Errorf("gate %q names no row shape", g)
		}
		if !slices.ContainsFunc(filed, func(f struct {
			tool  string
			group Group
			gate  Gate
		}) bool {
			return f.gate == g
		}) {
			t.Errorf("no built-in is filed as %q", g)
		}
	}
	if len(gateRows) != len(Gates) {
		t.Errorf("%d row shapes for %d gates", len(gateRows), len(Gates))
	}
	seen := map[rowShape]Gate{}
	for g, shape := range gateRows {
		if other, dup := seen[shape]; dup {
			t.Errorf("%q and %q name one row shape %+v", g, other, shape)
		}
		seen[shape] = g
	}
}

// AN APP GATE ADMITS AN AGENT HOLDING THE APP, AND NO OTHER.
//
// What the gate's words promise a reader ("agents with Worklet access"),
// asked of internal/authz itself on the object the call forms: for the
// personal verb filed under an app gate the object is the caller's own
// record, so this is what makes [shapeOf]'s reading of it a fact rather than
// an argument. The control is the same agent holding every OTHER app.
func TestAnAppGateAdmitsAnAgentHoldingTheAppAndNoOther(t *testing.T) {
	t.Parallel()
	apps := map[Gate]iam.Grant{
		GateWorklet: iam.GrantWorklet, GateDoclet: iam.GrantDoclet,
		GateChatlet: iam.GrantChatlet, GateArtifacts: iam.GrantArtifacts,
	}
	agent := func(grants ...iam.Grant) iam.Principal {
		return iam.Principal{Kind: iam.KindSeat, Login: "ana", Seat: "ana",
			Stage: iam.StageActive, Grants: grants}
	}
	decided := 0
	for _, f := range filed {
		app, ok := apps[f.gate]
		if !ok {
			continue
		}
		others := slices.DeleteFunc(slices.Clone(iam.Apps),
			func(g iam.Grant) bool { return g == app })
		for _, c := range []struct {
			caller iam.Principal
			admit  bool
		}{
			{agent(app), true},
			{agent(others...), false},
		} {
			object := authz.Object{}
			if s, governed := subjectOf[f.tool]; governed {
				object = s.objectFor(c.caller, nil)
			}
			d := authz.Decide(t.Context(), c.caller, authz.Action(f.tool), object,
				authz.NoChart{}, time.Now())
			if d.Unknown() {
				t.Fatalf("%s could not be decided: %v", f.tool, d.Err)
			}
			if d.Allowed != c.admit {
				t.Errorf("%s, filed as %q, decided %v (%s) for an agent holding %v",
					f.tool, f.gate, d.Allowed, d.Reason, c.caller.Grants)
			}
		}
		decided++
	}
	if decided == 0 {
		t.Fatal("no tool is filed under an app gate, so this decided nothing")
	}
}
