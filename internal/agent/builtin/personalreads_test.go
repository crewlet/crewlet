package builtin_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A PERSON'S OWN RECORD IS READ BY THEM AND BY THEIR LEADS, AND BY NOBODY ELSE.
//
// get_person, work_inbox and a named list_work_views viewer each take the
// person to read as an argument, and none of them checked it: once a member's
// key reached the operator catalogue (ADR-0031), any teammate could page
// through the founder's inbox, queue and private views by handle — the records
// the API's personal questions already hold to their owner and the leads whose
// line they are in. The rule is the same here, and an ADMIN's key adds nothing
// to it: the role is reach over the machine, never over somebody else's
// record.
//
// EVERY REFUSAL READS NOTHING, which the fake's recorded query proves: a gate
// that refused after asking would have handed the record to the log.
func TestAPersonalRecordIsReadByItsOwnerAndTheirLeads(t *testing.T) {
	t.Parallel()
	// THE FOUNDER LEADS RUI. Omar is a teammate who leads nobody, holding a
	// member's key; the CI key is an unlinked admin's.
	parties := func(handle string) tracker.Party {
		switch handle {
		case "rui-santos":
			return tracker.Party{Handle: handle, OperatorID: "rui"}
		case "omar":
			return tracker.Party{Handle: handle, OperatorID: "omar-key"}
		}
		return boundParties(handle)
	}
	leads := func(_ context.Context, actor, handle string) bool {
		return actor == "jane-founder" && handle == "rui-santos"
	}
	type actorFn = func(context.Context, *turnctx.Turn) (builtin.Actor, error)
	omar := func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{Handle: "omar-key", Kind: tracker.AuthorOperator,
			OperatorID: "omar-key", Seat: "omar"}, nil
	}
	reads := map[string]struct {
		tool string
		args func(handle string) map[string]any
		// asked is who the read was answered about, or nothing when it
		// never reached the tracker.
		asked func(trk *fakeTracker) []string
	}{
		"get_person": {
			tool: tracker.GetPersonTool,
			args: func(h string) map[string]any { return map[string]any{"handle": h} },
			asked: func(trk *fakeTracker) []string {
				return trk.personQuery.Who.Handles()
			},
		},
		"work_inbox": {
			tool: tracker.WorkInboxTool,
			args: func(h string) map[string]any { return map[string]any{"handle": h} },
			asked: func(trk *fakeTracker) []string {
				return trk.inboxQuery.Who.Handles()
			},
		},
		"list_work_views": {
			tool: tracker.ListWorkViewsTool,
			args: func(h string) map[string]any {
				return map[string]any{"container": "project:ENG", "viewer": h}
			},
			asked: func(trk *fakeTracker) []string {
				return trk.viewQuery.Viewer.Handles()
			},
		},
	}
	for _, c := range []struct {
		name   string
		actor  actorFn
		handle string
		// want is who the read is answered about, or nil for a refusal.
		want []string
	}{
		{"their own, by the seat", boundOperator, "jane-founder",
			[]string{"jane-founder", "founder"}},
		{"their own, by the key's own id", boundOperator, "founder",
			[]string{"founder"}},
		{"a report's, by their lead", boundOperator, "rui-santos",
			[]string{"rui-santos", "rui"}},
		{"a teammate's, by a member who leads nobody", omar, "rui-santos", nil},
		{"the founder's, by a member who leads nobody", omar, "jane-founder", nil},
		{"anybody's, by an admin who leads nobody", unboundOperator, "rui-santos", nil},
		{"their own, by an unlinked admin key", unboundOperator, "ci",
			[]string{"ci"}},
	} {
		for name, read := range reads {
			t.Run(c.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				trk := newFakeTracker()
				reg := ledPersonSurface(t, trk, &personSpy{}, c.actor, parties, leads)
				got := callPlain(t, reg, read.tool, read.args(c.handle))
				if c.want == nil {
					if !got.Failed || got.Refusal != tools.RefusalForbidden {
						t.Fatalf("%s read %s's record: %+v", c.name, c.handle, got)
					}
					if who := "Only " + c.handle + " or one of their leads"; !strings.Contains(got.Output, who) {
						t.Errorf("the refusal does not name who may (%q): %q", who, got.Output)
					}
					if asked := read.asked(trk); len(asked) != 0 {
						t.Errorf("the refused read reached the tracker about %v", asked)
					}
					return
				}
				if got.Failed {
					t.Fatalf("%s was refused: %q", c.name, got.Output)
				}
				if asked := read.asked(trk); !slices.Equal(asked, c.want) {
					t.Errorf("answered about %v, want %v", asked, c.want)
				}
			})
		}
	}
}

// AND THE SHARED STRIP IS EVERYBODY'S. A view listing that names no viewer
// asks for nobody's record — no pins, no personal views — so it is served to
// a member who leads nobody without a lead lookup, and the tracker is asked
// for the unnamed party it documents as the shared strip.
func TestTheSharedViewStripAsksNothingOfTheCaller(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	asked := false
	leads := func(context.Context, string, string) bool {
		asked = true
		return false
	}
	reg := ledPersonSurface(t, trk, &personSpy{}, unboundOperator, boundParties, leads)
	if got := callPlain(t, reg, tracker.ListWorkViewsTool,
		map[string]any{"container": "project:ENG"}); got.Failed {
		t.Fatalf("the shared strip was refused: %q", got.Output)
	}
	if who := trk.viewQuery.Viewer.Handles(); len(who) != 0 {
		t.Errorf("the shared strip was asked for as %v", who)
	}
	if asked {
		t.Error("the shared strip asked the chart who leads whom")
	}
}
