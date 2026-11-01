package queries_test

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// recordingTails answers every request with the outcome it is told to, and
// remembers what it was asked.
type recordingTails struct {
	asked  []sandbox.TailQuery
	answer sandbox.TailAnswer
}

func (r *recordingTails) Tail(_ context.Context, q sandbox.TailQuery) (sandbox.TailAnswer, error) {
	r.asked = append(r.asked, q)
	return r.answer, nil
}

// A TAIL NAMES ITS JOB, not only its turn: a turn may launch more than one job,
// and a request naming only the turn would show whichever the run's record
// holds now — a different job from the span a person opened, the moment a
// second launch replaces the first.
func TestAnExecutorTailNeedsTheTurnAndTheJob(t *testing.T) {
	t.Parallel()
	tails := &recordingTails{answer: sandbox.TailAnswer{
		Outcome: sandbox.TailOwnerSilent, TurnID: "t1", LaunchID: "l1", Node: "n2",
	}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{ExecutorTail: tails})

	for _, params := range []map[string]any{
		{"turn_id": "t1"}, {"launch_id": "l1"}, {"turn_id": " ", "launch_id": "l1"},
	} {
		if _, err := r.Answer(everyGrant(t), "executor_tail", params); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("executor_tail %v = %v; want a bad-params refusal", params, err)
		}
	}
	got, err := r.Answer(everyGrant(t), "executor_tail", map[string]any{"turn_id": "t1", "launch_id": "l1"})
	if err != nil {
		t.Fatalf("executor_tail: %v", err)
	}
	answer, ok := got.(sandbox.TailAnswer)
	if !ok || answer.Outcome != sandbox.TailOwnerSilent || answer.Node != "n2" {
		t.Errorf("answer = %#v; want the reader's own, the silent owner named", got)
	}
	if len(tails.asked) != 1 || tails.asked[0].TurnID != "t1" || tails.asked[0].LaunchID != "l1" {
		t.Errorf("the reader was asked %v; want exactly (t1, l1)", tails.asked)
	}
}

// WHAT A CODING AGENT PRINTS IS ITS TRANSCRIPT, so the tail is read by
// whoever reads the run's SEAT's trail — an admin, or the seat's lead — and
// nobody else: everybody sees that a run is running, and only they see what
// it says (ADR-0033).
//
// The seat is the one the run's record names, read before the tail is asked
// for; a run no record names any more is `not_running`, answered without
// asking the tail reader, whose own later read of the record could find one
// this decision never saw; and a record that cannot be read is unavailable to
// a lead, never a refusal. A caller who leads nobody — a person with no
// reports, an unbound token — is refused BEFORE the record is read, so a run
// store that cannot answer never tells them to come back to a refusal.
//
// Mutation: drop [queries.Sources.mayTail]'s seat decision and the stranger
// is shown the run's output; decide on the caller instead of the run's seat
// and the lead is refused; drop the decision taken before the record is read
// and the callers who lead nobody are answered unavailable.
func TestAnExecutorTailIsReadByItsSeatsTrail(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{TurnID: "t1", LaunchID: "l1", AgentHandle: "swe",
		Status: sandbox.StatusRunning}
	params := map[string]any{"turn_id": "t1", "launch_id": "l1"}
	running := sandbox.TailAnswer{Outcome: sandbox.TailRunning, TurnID: "t1", LaunchID: "l1"}
	for _, c := range []struct {
		name    string
		caller  context.Context
		runs    queries.PendingRuns
		chart   authz.Chart
		asked   bool
		outcome sandbox.TailOutcome
		refused bool
		err     error
	}{
		{"an admin, with no run record to read", everyGrant(t), nil, flatChart{},
			true, sandbox.TailRunning, false, nil},
		{"the seat's lead, holding no permission", asSeat(t, "ana"), fixedRuns{run},
			leadsChart{lead: "ana", report: "swe"}, true, sandbox.TailRunning, false, nil},
		{"a caller holding every app and leading nobody", asSeat(t, "ana", iam.Apps...),
			fixedRuns{run}, flatChart{}, false, "", true, nil},
		{"a lead, on a node with no run record to read", asSeat(t, "ana"), nil,
			leadsChart{lead: "ana", report: "swe"}, false, "", true, nil},
		{"a lead, on a run no record names", asSeat(t, "ana"), fixedRuns{},
			leadsChart{lead: "ana", report: "swe"}, false, sandbox.TailNotRunning, false, nil},
		{"a lead, on a record that cannot be read", asSeat(t, "ana"),
			unreachableRuns{}, leadsChart{lead: "ana", report: "swe"}, false, "", false,
			queries.ErrUnavailable},
		{"somebody leading nobody, on a record that cannot be read", asSeat(t, "ana"),
			unreachableRuns{}, flatChart{}, false, "", true, nil},
		{"an unbound token, on a record that cannot be read",
			asGrants(t, "ci", iam.Apps...), unreachableRuns{},
			leadsChart{lead: "ana", report: "swe"}, false, "", true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tails := &recordingTails{answer: running}
			r := queries.NewRegistry()
			queries.Register(r, queries.Sources{ExecutorTail: tails, Sandbox: c.runs,
				Chart: c.chart})
			got, err := r.Answer(c.caller, "executor_tail", params)
			switch {
			case c.err != nil:
				if !errors.Is(err, c.err) {
					t.Fatalf("answered %v, want %v", err, c.err)
				}
			case c.refused:
				var refusal *queries.Refusal
				if !errors.As(err, &refusal) || !slices.Equal(refusal.Grants,
					[]iam.Grant{iam.GrantAdmin}) {
					t.Fatalf("answered %v, want a refusal naming [admin]", err)
				}
			default:
				answer, ok := got.(sandbox.TailAnswer)
				if err != nil || !ok || answer.Outcome != c.outcome {
					t.Fatalf("answered %#v, %v; want outcome %s", got, err, c.outcome)
				}
			}
			if asked := len(tails.asked) > 0; asked != c.asked {
				t.Errorf("the tail reader was asked: %v, want %v", asked, c.asked)
			}
		})
	}
}

// EVERY REQUEST IS A CURSOR: what the asker holds, read off `epoch`, `after`
// and `digest`, and a request naming none of them holds nothing yet — the zero
// cursor, answered with a reset.
//
// Mutation: drop `after` from the cursor, and a REST caller holding 4096 bytes
// is sent everything again.
func TestATailIsAskedByCursor(t *testing.T) {
	t.Parallel()
	tails := &recordingTails{answer: sandbox.TailAnswer{Outcome: sandbox.TailRunning}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{ExecutorTail: tails})
	ask := func(params map[string]any) sandbox.TailQuery {
		t.Helper()
		if _, err := r.Answer(everyGrant(t), "executor_tail", params); err != nil {
			t.Fatalf("executor_tail %v: %v", params, err)
		}
		return tails.asked[len(tails.asked)-1]
	}

	if q := ask(map[string]any{"turn_id": "t1", "launch_id": "l1"}); q.Cursor != (sandbox.TailCursor{}) {
		t.Errorf("a request holding nothing = %+v; want the empty cursor", q.Cursor)
	}
	// A socket frame's numbers are float64s; a whole one is the offset.
	if q := ask(map[string]any{"turn_id": "t1", "launch_id": "l1", "after": float64(4096)}); q.Cursor.Offset != 4096 {
		t.Errorf("a socket frame's whole offset = %+v; want 4096", q.Cursor)
	}
	// Over REST every value is a string.
	rest := queries.FromQuery(url.Values{
		"turn_id": {"t1"}, "launch_id": {"l1"},
		"epoch": {"transcript@0"}, "after": {"4096"}, "digest": {"ab12"},
	})
	if _, err := r.Answer(everyGrant(t), "executor_tail", rest.Values()); err != nil {
		t.Fatalf("executor_tail over REST: %v", err)
	}
	want := sandbox.TailCursor{Epoch: "transcript@0", Offset: 4096, Digest: "ab12"}
	if got := tails.asked[len(tails.asked)-1].Cursor; got != want {
		t.Errorf("a REST cursor = %+v; want %+v", got, want)
	}

	// A socket frame's number is a float64, and one with a fraction names no
	// byte: read as the whole number below it, it was a different request
	// answered as though it were the one asked.
	for _, after := range []any{"-1", "the end", float64(-3), 12.5, "12.5", 1e300, true} {
		_, err := r.Answer(everyGrant(t), "executor_tail",
			map[string]any{"turn_id": "t1", "launch_id": "l1", "after": after})
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("after=%v = %v; want a bad-params refusal naming the offset", after, err)
		}
	}
}

// A WHOLE NUMBER IS READ EXACTLY OR NOT AT ALL: a socket frame's number with a
// fraction, or one past int64 (whose conversion Go leaves to the platform), is
// no whole number — never the nearest one.
func TestAWholeNumberIsReadExactlyOrNotAtAll(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		value any
		want  int64
		whole bool
	}{
		"a frame's whole number": {value: float64(4096), want: 4096, whole: true},
		"a query string's":       {value: "4096", want: 4096, whole: true},
		"the least int64":        {value: float64(-(1 << 63)), want: -(1 << 63), whole: true},
		"a fraction":             {value: 12.5},
		"the first past int64":   {value: float64(1 << 63)},
		"a string's fraction":    {value: "12.5"},
		"padded":                 {value: " 12"},
		"not a number":           {value: true},
	} {
		got, whole := queries.FromMap(map[string]any{"n": c.value}).WholeInt("n")
		if whole != c.whole || got != c.want {
			t.Errorf("%s: WholeInt(%v) = %d, %v; want %d, %v", name, c.value, got, whole, c.want, c.whole)
		}
	}
	if _, whole := queries.FromMap(nil).WholeInt("n"); whole {
		t.Error("an absent key read as a whole number")
	}
}
