package queries_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The merged questions over a node whose native halves are wired but not all
// kept: the shape the engine's live sources take on a company that runs Jira,
// Confluence or no tracker or knowledge base at all.

// errNotOurs is what the engine's live source answers for a half this company
// does not keep in the engine (internal/api's `nativeAbsent`).
var errNotOurs = fmt.Errorf("%w: this company does not run this half", queries.ErrUnknown)

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

// absentPages is [absentWork] for the knowledge base.
type absentPages struct {
	queries.PageReader
	err error
}

func (a absentPages) Kept() error { return a.err }

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
	stub := &stubWork{decisions: worklet.DecisionsAnswer{
		Asks:  []worklet.AskRow{{Comment: "c-1", AskedAt: askedAt}},
		Total: worklet.ClaimTotal{Total: 1},
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

// THE FEED KEEPS A KIND ONLY WHERE ITS HALF IS THE ENGINE'S OWN, and answers
// the rest. A company on Jira still sees its schedules' runs and its pages, a
// company on Confluence its work and its schedules' runs; a kind named in
// `kinds=` whose half is not kept is refused by name, as a page kind on a
// node with no pages always was; and a half this node cannot place yet makes
// the feed unavailable rather than short.
//
// Mutation: keep the tracker's kinds or the page kind whenever its source is
// wired, as `feedKept` did, and the absent half is read (a panic here, the
// whole feed refused in production).
func TestTheFeedKeepsOnlyTheKindsWhoseHalfIsKept(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	base := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	ran := base.Add(-30 * time.Minute)
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO usage_schedule_runs
				(day, node, scope_type, scope_id, name, fired_at, target,
				 outcome, trace_id, turn_id, version)
			VALUES (?, 'n1', 'role', 'pm', 'standup', ?, 'pm', 'fired', '', '', 1)`,
			ran.Format("2006-01-02"), store.EncodeTime(ran))
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A STUB PER CASE: each records what it was asked, and the cases run in
	// parallel, so one stub shared between them was written by two at once.
	work := func() *stubWork {
		return &stubWork{feed: worklet.FeedPage{Rows: []worklet.FeedRow{
			{Task: "a", Kind: worklet.FeedCompleted, At: base,
				Cursor: worklet.FeedCursor{At: base, Seq: 9}.String()},
		}, Complete: true}}
	}
	wiki := func() *stubPages {
		return &stubPages{changes: []doclet.PageChange{
			{ID: "h-1", PageID: "p", Kind: doclet.ChangeSaved, At: base.Add(-time.Hour), LogSeq: 4},
		}}
	}

	rowKinds := func(t *testing.T, s queries.Sources, kinds string) ([]string, error) {
		t.Helper()
		got, err := askNative(t, s, "company_feed", map[string]any{"kinds": kinds})
		if err != nil {
			return nil, err
		}
		var out []string
		for _, row := range got.(map[string]any)["rows"].([]queries.FeedEntry) {
			switch {
			case row.Work != nil:
				out = append(out, "work")
			case row.Page != nil:
				out = append(out, "page")
			case row.Schedule != nil:
				out = append(out, "schedule")
			}
		}
		return out, nil
	}

	for _, tc := range []struct {
		name    string
		sources queries.Sources
		kinds   string
		want    []string
		wantErr error
	}{
		{"both halves kept", queries.Sources{Work: keptWork{work()}, Pages: wiki(),
			Usage: usage.Reader{Estate: db.Replicated()}}, "", []string{"work", "schedule", "page"}, nil},
		{"the company runs Jira", queries.Sources{Work: absentWork{err: errNotOurs},
			Pages: wiki(), Usage: usage.Reader{Estate: db.Replicated()}}, "", []string{"schedule", "page"}, nil},
		{"the company runs Confluence", queries.Sources{Work: keptWork{work()},
			Pages: absentPages{err: errNotOurs}, Usage: usage.Reader{Estate: db.Replicated()}}, "",
			[]string{"work", "schedule"}, nil},
		{"neither half is the engine's", queries.Sources{Work: absentWork{err: errNotOurs},
			Pages: absentPages{err: errNotOurs}, Usage: usage.Reader{Estate: db.Replicated()}}, "",
			[]string{"schedule"}, nil},
		{"a tracker kind named on Jira", queries.Sources{Work: absentWork{err: errNotOurs},
			Pages: wiki(), Usage: usage.Reader{Estate: db.Replicated()}}, "completed,schedule", nil, queries.ErrBadParams},
		{"the page kind named on Confluence", queries.Sources{Work: keptWork{work()},
			Pages: absentPages{err: errNotOurs}, Usage: usage.Reader{Estate: db.Replicated()}}, "page", nil,
			queries.ErrBadParams},
		{"a half this node cannot place yet", queries.Sources{Work: absentWork{err: errNotUpYet},
			Pages: wiki(), Usage: usage.Reader{Estate: db.Replicated()}}, "", nil, queries.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := rowKinds(t, tc.sources, tc.kinds)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("company_feed = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("company_feed: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("the feed's rows are %v, want %v newest first", got, tc.want)
			}
		})
	}
}
