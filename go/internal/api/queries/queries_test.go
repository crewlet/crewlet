package queries_test

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
)

func answersWith(v any) queries.Answer {
	return func(context.Context, queries.Params) (any, error) { return v, nil }
}

// --- the registry -------------------------------------------------------- //

func TestARegisteredQuestionIsAnswered(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", answersWith("rows"))

	got, err := r.Answer(t.Context(), "events", nil, "")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got != "rows" {
		t.Errorf("answer = %v", got)
	}
}

func TestAnUnregisteredQuestionIsUnknown(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	_, err := r.Answer(t.Context(), "nope", nil, "")
	if !errors.Is(err, queries.ErrUnknown) {
		t.Errorf("err = %v, want ErrUnknown", err)
	}
	// And it names the question: a client switching on the code still
	// needs a log line saying which name nothing answered.
	if err == nil || !slices.Contains([]string{"nope"}, "nope") {
		t.Error("unreachable")
	}
}

func TestAnOperatorQuestionRefusesAnAnonymousCaller(t *testing.T) {
	t.Parallel()
	// The config surface is all of them: reading it exposes the whole
	// company document, including every ${VAR} reference by name.
	r := queries.NewRegistry()
	r.RegisterOperator("config", answersWith("the company"))

	if _, err := r.Answer(t.Context(), "config", nil, ""); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
	got, err := r.Answer(t.Context(), "config", nil, "founder")
	if err != nil {
		t.Fatalf("an operator was refused: %v", err)
	}
	if got != "the company" {
		t.Errorf("answer = %v", got)
	}
}

func TestAPublicQuestionServesAnAnonymousCaller(t *testing.T) {
	t.Parallel()
	// The counterfactual: requiring an operator everywhere would satisfy
	// the case above and close the dashboard to the read posture that is
	// the default.
	r := queries.NewRegistry()
	r.Register("events", answersWith("rows"))
	if _, err := r.Answer(t.Context(), "events", nil, ""); err != nil {
		t.Errorf("a public question refused an anonymous caller: %v", err)
	}
}

func TestRequiresOperatorMatchesWhatAnswerEnforces(t *testing.T) {
	t.Parallel()
	// A REST route makes the same decision before it calls the answer, so
	// the two must not be able to disagree.
	r := queries.NewRegistry()
	r.Register("events", answersWith(nil))
	r.RegisterOperator("config", answersWith(nil))

	for what, want := range map[string]bool{"events": false, "config": true, "nope": false} {
		if got := r.RequiresOperator(what); got != want {
			t.Errorf("%s: requires operator = %v, want %v", what, got, want)
		}
		if !want {
			continue
		}
		if _, err := r.Answer(t.Context(), what, nil, ""); !errors.Is(err, queries.ErrUnauthorized) {
			t.Errorf("%s: RequiresOperator says yes but Answer let it through", what)
		}
	}
}

func TestRegisteringAQuestionTwicePanics(t *testing.T) {
	t.Parallel()
	// Two answers to one question is exactly the divergence this package
	// exists to prevent, and a wiring mistake resolving to whichever ran
	// last would be invisible.
	defer func() {
		if recover() == nil {
			t.Error("registering a question twice was accepted")
		}
	}()
	r := queries.NewRegistry()
	r.Register("events", answersWith(nil))
	r.Register("events", answersWith(nil))
}

func TestRegisteringNothingUsefulPanics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		run    func(*queries.Registry)
		reason string
	}{
		{"no name", func(r *queries.Registry) { r.Register("", answersWith(nil)) }, "an unnamed question"},
		{"no answer", func(r *queries.Registry) { r.Register("events", nil) }, "a question with no answer"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s was accepted", tc.reason)
				}
			}()
			tc.run(queries.NewRegistry())
		}()
	}
}

func TestTheRegistryListsWhatItAnswers(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", answersWith(nil))
	r.RegisterOperator("config", answersWith(nil))
	r.Register("agent", answersWith(nil))

	if got := r.Names(); !slices.Equal(got, []string{"agent", "config", "events"}) {
		t.Errorf("names = %v, want them sorted", got)
	}
}

func TestAFailingAnswerReachesTheCaller(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("the store fell over")
	r := queries.NewRegistry()
	r.Register("events", func(context.Context, queries.Params) (any, error) {
		return nil, sentinel
	})
	if _, err := r.Answer(t.Context(), "events", nil, ""); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the answer's own", err)
	}
}

// --- params -------------------------------------------------------------- //

func TestBothTransportsReadTheSameParameters(t *testing.T) {
	t.Parallel()
	// The reason Params exists: without it each question needs two
	// readers, which is where a filter honoured on one path and ignored on
	// the other comes from.
	fromSocket := queries.FromMap(map[string]any{
		"role": "Lead", "limit": float64(25), "failed": true,
	})
	fromREST := queries.FromQuery(url.Values{
		"role": {"Lead"}, "limit": {"25"}, "failed": {"true"},
	})

	for name, p := range map[string]queries.Params{"socket": fromSocket, "rest": fromREST} {
		if got := p.String("role"); got != "Lead" {
			t.Errorf("%s: role = %q", name, got)
		}
		if got := p.Int("limit", 0); got != 25 {
			t.Errorf("%s: limit = %d", name, got)
		}
		if got := p.Bool("failed", false); !got {
			t.Errorf("%s: failed = %v", name, got)
		}
	}
}

func TestANumberSentAsANumberReadsAsAString(t *testing.T) {
	t.Parallel()
	// A socket frame's JSON has one number type, so an id sent as a number
	// arrives as a float rather than as the string the question wants.
	p := queries.FromMap(map[string]any{"id": float64(42), "flag": true})
	if got := p.String("id"); got != "42" {
		t.Errorf("id = %q, want 42", got)
	}
	if got := p.String("flag"); got != "true" {
		t.Errorf("flag = %q", got)
	}
}

func TestARepeatedQueryKeyTakesTheFirst(t *testing.T) {
	t.Parallel()
	// A query string can carry a key twice and a JSON object cannot, so
	// taking the last would make the two transports disagree about a
	// request only one of them can express.
	p := queries.FromQuery(url.Values{"role": {"First", "Second"}})
	if got := p.String("role"); got != "First" {
		t.Errorf("role = %q, want the first", got)
	}
}

func TestAMalformedFilterFallsBackRatherThanFailing(t *testing.T) {
	t.Parallel()
	// These are a dashboard's own filters. Refusing the whole question
	// over one malformed one would blank a screen to report a typo.
	p := queries.FromMap(map[string]any{"limit": "not-a-number", "failed": "maybe"})
	if got := p.Int("limit", 50); got != 50 {
		t.Errorf("limit = %d, want the fallback", got)
	}
	if got := p.Bool("failed", true); !got {
		t.Errorf("failed = %v, want the fallback", got)
	}
}

func TestAnAbsentFilterIsNotAnEmptyOne(t *testing.T) {
	t.Parallel()
	// A filter set to "" asks for rows with no value; a filter absent asks
	// for all of them.
	p := queries.FromMap(map[string]any{"actor": ""})
	if !p.Has("actor") {
		t.Error("an explicitly empty filter reads as absent")
	}
	if p.Has("role") {
		t.Error("an absent filter reads as present")
	}
}

func TestAnEmptyParamsIsUsable(t *testing.T) {
	t.Parallel()
	// A socket frame may carry no params at all.
	var p queries.Params
	if got := p.String("role"); got != "" {
		t.Errorf("role = %q", got)
	}
	if got := p.Int("limit", 7); got != 7 {
		t.Errorf("limit = %d", got)
	}
	if p.Has("anything") {
		t.Error("a nil params reported a key")
	}
}

func TestALimitIsClampedAtBothEnds(t *testing.T) {
	t.Parallel()
	// Zero or negative is a request that returns nothing, which is never
	// what a dashboard means by leaving a limit off; unbounded lets one
	// query pull the whole event log through a process every tab shares.
	for _, tc := range []struct{ requested, want int }{
		{0, 50}, {-1, 50}, {10, 10}, {50, 50}, {500, 200}, {1 << 20, 200},
	} {
		if got := queries.Clamp(tc.requested, 50, 200); got != tc.want {
			t.Errorf("Clamp(%d) = %d, want %d", tc.requested, got, tc.want)
		}
	}
}
