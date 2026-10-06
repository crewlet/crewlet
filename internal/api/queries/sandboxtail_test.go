package queries_test

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
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
func TestASandboxTailNeedsTheTurnAndTheJob(t *testing.T) {
	t.Parallel()
	tails := &recordingTails{answer: sandbox.TailAnswer{
		Outcome: sandbox.TailOwnerSilent, TurnID: "t1", LaunchID: "l1", Node: "n2",
	}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{SandboxTail: tails})

	for _, params := range []map[string]any{
		{"turn_id": "t1"}, {"launch_id": "l1"}, {"turn_id": " ", "launch_id": "l1"},
	} {
		if _, err := r.Answer(t.Context(), "sandbox_tail", params, "operator"); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("sandbox_tail %v = %v; want a bad-params refusal", params, err)
		}
	}
	got, err := r.Answer(t.Context(), "sandbox_tail", map[string]any{"turn_id": "t1", "launch_id": "l1"}, "operator")
	if err != nil {
		t.Fatalf("sandbox_tail: %v", err)
	}
	answer, ok := got.(sandbox.TailAnswer)
	if !ok || answer.Outcome != sandbox.TailOwnerSilent || answer.Node != "n2" {
		t.Errorf("answer = %#v; want the reader's own, the silent owner named", got)
	}
	if len(tails.asked) != 1 || tails.asked[0].TurnID != "t1" || tails.asked[0].LaunchID != "l1" {
		t.Errorf("the reader was asked %v; want exactly (t1, l1)", tails.asked)
	}
}

// NO CURSOR IS NOT A CURSOR AT NOTHING. An asker that never said it reads
// cursors — the dashboard an older node serves, a REST caller written against
// this route before cursors — replaces what it shows with each answer, so it
// must be answered the window it always was; one that says it reads cursors and
// holds nothing yet is answered a reset. The two used to be indistinguishable
// on the wire, which is the whole reason the flag is explicit.
//
// Mutation: read a cursor from `after` alone, and a REST caller passing an
// offset for some other reason is sent deltas it replaces its screen with.
func TestACursorIsAskedForExplicitly(t *testing.T) {
	t.Parallel()
	tails := &recordingTails{answer: sandbox.TailAnswer{Outcome: sandbox.TailRunning}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{SandboxTail: tails})
	ask := func(params map[string]any) sandbox.TailQuery {
		t.Helper()
		if _, err := r.Answer(t.Context(), "sandbox_tail", params, "operator"); err != nil {
			t.Fatalf("sandbox_tail %v: %v", params, err)
		}
		return tails.asked[len(tails.asked)-1]
	}

	if q := ask(map[string]any{"turn_id": "t1", "launch_id": "l1", "after": "40"}); q.Cursor != nil {
		t.Errorf("a request with no cursor flag was read as a cursor: %+v", q.Cursor)
	}
	if q := ask(map[string]any{"turn_id": "t1", "launch_id": "l1", "cursor": true}); q.Cursor == nil ||
		*q.Cursor != (sandbox.TailCursor{}) {
		t.Errorf("a cursor holding nothing = %+v; want the empty cursor", q.Cursor)
	}
	// A socket frame's numbers are float64s; a whole one is the offset.
	if q := ask(map[string]any{"turn_id": "t1", "launch_id": "l1", "cursor": true, "after": float64(4096)}); q.Cursor == nil ||
		q.Cursor.Offset != 4096 {
		t.Errorf("a socket frame's whole offset = %+v; want 4096", q.Cursor)
	}
	// Over REST every value is a string, the flag included.
	rest := queries.FromQuery(url.Values{
		"turn_id": {"t1"}, "launch_id": {"l1"}, "cursor": {"true"},
		"epoch": {"transcript@0"}, "after": {"4096"}, "digest": {"ab12"},
	})
	if _, err := r.Answer(t.Context(), "sandbox_tail", rest.Values(), "operator"); err != nil {
		t.Fatalf("sandbox_tail over REST: %v", err)
	}
	want := sandbox.TailCursor{Epoch: "transcript@0", Offset: 4096, Digest: "ab12"}
	if got := tails.asked[len(tails.asked)-1].Cursor; got == nil || *got != want {
		t.Errorf("a REST cursor = %+v; want %+v", got, want)
	}

	// A socket frame's number is a float64, and one with a fraction names no
	// byte: read as the whole number below it, it was a different request
	// answered as though it were the one asked.
	for _, after := range []any{"-1", "the end", float64(-3), 12.5, "12.5", 1e300, true} {
		_, err := r.Answer(t.Context(), "sandbox_tail",
			map[string]any{"turn_id": "t1", "launch_id": "l1", "cursor": true, "after": after}, "operator")
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
