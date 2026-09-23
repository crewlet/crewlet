package builtin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// trashSpy records what the trash was asked to do.
type trashSpy struct{ removed, restored []string }

func (s *trashSpy) RemoveTask(_ context.Context, _, id, _ string, _ bool,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	s.removed = append(s.removed, id)
	return tracker.WriteResult{Result: statelog.Result{
		Outcome: statelog.OutcomeApplied,
	}}, nil
}

func (s *trashSpy) RestoreTask(_ context.Context, _, id, _ string,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	s.restored = append(s.restored, id)
	return tracker.WriteResult{Result: statelog.Result{
		Outcome: statelog.OutcomeApplied,
	}}, nil
}

// A ROW-DECIDED TOOL ASKS ONCE IT HAS READ THE ROW.
//
// Taking an item out of circulation is its PROJECT's lead's, and which project
// an item is filed under is a stored row rather than an argument — so the
// registration gate cannot form the object and these tools ask themselves.
// Decided at the gate on the empty object it had, every caller but the holder
// of the admin grant was refused as naming no project, and a project's own
// lead could not clear their own board.
//
// Both halves, because the gate stepping aside is only safe if the tool then
// asks: the lead is admitted, and a colleague holding the ordinary write grant
// but leading nothing is refused with the authority's own answer. Delete the
// ask from either tool and the second half goes red.
func TestARowDecidedToolAsksAfterItReads(t *testing.T) {
	t.Parallel()
	lead := iam.WithPrincipal(context.Background(), iam.Principal{
		ID: uuid.New(), Kind: iam.KindPerson, Login: "cto.lead", Seat: "cto",
		Stage:  iam.StageActive,
		Grants: []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite},
	})
	for _, c := range []struct {
		name    string
		chart   authz.Chart
		allowed bool
	}{
		{"the project's lead", chartLeads, true},
		{"a colleague who leads nothing", chartRefuses, false},
	} {
		for _, verb := range []string{
			tracker.RemoveWorkItemTool, tracker.RestoreWorkItemTool,
		} {
			t.Run(c.name+"/"+verb, func(t *testing.T) {
				t.Parallel()
				trk := newFakeTracker()
				if verb == tracker.RestoreWorkItemTool {
					detail := trk.tasks["ENG-1"]
					detail.Task.Removed = &tracker.Tombstone{By: "cto"}
					trk.tasks["ENG-1"] = detail
				}
				spy := &trashSpy{}
				catalogue := builtin.OperatorTools(builtin.OperatorDeps{
					Work: builtin.WorkDeps{
						Reader: trk, Writer: trk.as,
						TrashWriter: func(builtin.Actor) builtin.TrashWriter {
							return spy
						},
						Actor: builtin.PrincipalActor,
					},
					Authorize: builtin.Decide(c.chart),
				})
				var got string
				var failed bool
				var cause error
				for _, tool := range catalogue {
					if tool.Name() != verb {
						continue
					}
					res, err := tool.Call(lead, map[string]any{"item": "ENG-1"})
					if err != nil {
						t.Fatalf("%s: %v", verb, err)
					}
					got, failed, cause = res.Output, res.Failed, res.Cause
				}
				wrote := len(spy.removed)+len(spy.restored) > 0
				switch {
				case c.allowed && (failed || !wrote):
					t.Errorf("the lead was refused: %s", got)
				case !c.allowed && (!failed || wrote):
					t.Errorf("a colleague leading nothing wrote the trash: %s", got)
				case !c.allowed && !errors.Is(cause, builtin.ErrRefused):
					t.Errorf("the refusal carries %v, not the authority's own "+
						"answer", cause)
				}
			})
		}
	}
}
