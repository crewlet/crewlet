package queries_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The merged questions over a node whose native halves are wired but not all
// kept: the shape the engine's live sources take on a company that runs Jira,
// Confluence or no tracker or knowledge base at all.

// errNotOurs is what the engine's live source answers for a half this company
// does not keep in the engine (internal/api's `nativeAbsent`).
var errNotOurs = fmt.Errorf("%w: this company's half is not the engine's own", queries.ErrUnknown)

// errNotUpYet is what it answers on a node that has not met its company yet.
var errNotUpYet = fmt.Errorf("%w: no company yet", queries.ErrUnavailable)

// absentWork is a tracker source that answers [queries.Keeper] with err and
// was never meant to be read: its embedded reader is nil, so a read panics and
// a case proves the half was left alone rather than read and its refusal
// swallowed.
type absentWork struct {
	queries.WorkReader
	err error
}

func (a absentWork) Kept() error { return a.err }

// keptWork is a tracker source that says it is kept and answers from w.
type keptWork struct{ *stubWork }

func (keptWork) Kept() error { return nil }

// WHAT WAITS ON A PERSON IS STILL ANSWERED WHERE THE TRACKER IS NOT THE
// ENGINE'S: their coding runs parked on a question to them are sandbox state,
// which every backend has. Where the tracker is kept its asks are read beside
// them; where it is not, it is left out and never read; and where this node
// cannot say yet, the question is unavailable rather than answered short.
//
// Mutation: read `s.Work` whenever it is wired, as [queries.Sources.decisions]
// did, and the Jira company's case panics on the nil reader (in production,
// refused whole with `unknown_query`).
func TestDecisionsAnswerTheParkedRunsWhereTheTrackerIsNotKept(t *testing.T) {
	t.Parallel()
	askedAt := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	runs := fixedRuns{{TurnID: "parked", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
		AudienceHandles: []string{"ana"}, UpdatedAt: askedAt.Add(-time.Hour)}}
	stub := &stubWork{decisions: tracker.DecisionsAnswer{
		Asks:  []tracker.AskRow{{Comment: "c-1", AskedAt: askedAt}},
		Total: tracker.ClaimTotal{Total: 1},
	}}
	for _, tc := range []struct {
		name    string
		work    queries.WorkReader
		want    []string
		wantErr error
	}{
		{"the tracker is the engine's own", keptWork{stub}, []string{"c-1", "parked"}, nil},
		{"the company runs Jira", absentWork{err: errNotOurs}, []string{"parked"}, nil},
		{"the node has met no company yet", absentWork{err: errNotUpYet}, nil, queries.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := viewerSources(t, stub)
			s.Work, s.Sandbox = tc.work, runs
			got, err := askAsSeat(t, s, "ana", "decisions", nil)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("decisions = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decisions: %v", err)
			}
			answer := got.(map[string]any)
			var order []string
			for _, item := range answer["items"].([]queries.DecisionItem) {
				if item.Ask != nil {
					order = append(order, item.Ask.Comment)
				} else {
					order = append(order, item.Run["turn_id"].(string))
				}
			}
			if !slices.Equal(order, tc.want) {
				t.Errorf("items are %v, want %v", order, tc.want)
			}
			if answer["total"] != len(tc.want) {
				t.Errorf("total = %v, want %d", answer["total"], len(tc.want))
			}
		})
	}
}
