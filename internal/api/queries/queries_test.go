package queries_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
)

func answersWith(v any) queries.Answer {
	return func(context.Context, queries.Params) (any, error) { return v, nil }
}

// --- the registry -------------------------------------------------------- //

func TestARegisteredQuestionIsAnswered(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", auth.ReachOpen, answersWith("rows"))

	got, err := r.Answer(t.Context(), "events", nil, nobody)
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
	_, err := r.Answer(t.Context(), "nope", nil, asAdmin("ops"))
	if !errors.Is(err, queries.ErrUnknown) {
		t.Errorf("err = %v, want ErrUnknown", err)
	}
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v, want it to name the question nothing answered", err)
	}
}

// A QUESTION SERVES EXACTLY THE CALLERS ITS REACH COVERS, and refuses the rest
// in two different words: `unauthorized` to a caller with no key, who should
// sign in, and `forbidden` to one whose key was accepted and reaches less, for
// whom signing in again would change nothing.
func TestAQuestionServesExactlyTheCallersItsReachCovers(t *testing.T) {
	t.Parallel()
	callers := map[string]auth.Principal{
		"nobody, anonymous none":   {Reach: auth.ReachOpen},
		"nobody, anonymous public": nobody,
		"a member":                 asMember("ada"),
		"an admin":                 asAdmin("founder"),
	}
	r := queries.NewRegistry()
	for _, reach := range auth.Reaches {
		r.Register(string(reach), reach, answersWith("served"))
	}
	for name, caller := range callers {
		for _, reach := range auth.Reaches {
			got, err := r.Answer(t.Context(), string(reach), nil, caller)
			switch {
			case caller.Reach.Covers(reach):
				if err != nil || got != "served" {
					t.Errorf("%s asking a %s question = %v %v, want it served", name, reach, got, err)
				}
			case caller.Authenticated():
				if !errors.Is(err, queries.ErrForbidden) {
					t.Errorf("%s asking a %s question = %v, want forbidden", name, reach, err)
				}
			default:
				if !errors.Is(err, queries.ErrUnauthorized) {
					t.Errorf("%s asking a %s question = %v, want unauthorized", name, reach, err)
				}
			}
		}
	}
}

// REACHOF IS WHAT ANSWER ENFORCES. The REST route a question is served on is
// mounted at it, so the route and the registry cannot disagree about who may
// ask — and a name nothing answers declares no reach at all.
func TestReachOfIsWhatAnswerEnforces(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", auth.ReachAdmin, answersWith(nil))
	r.Register("work_items", auth.ReachMember, answersWith(nil))

	for what, want := range map[string]auth.Reach{
		"events": auth.ReachAdmin, "work_items": auth.ReachMember, "nope": "",
	} {
		if got := r.ReachOf(what); got != want {
			t.Errorf("%s: reach = %q, want %q", what, got, want)
		}
	}
	if _, err := r.Answer(t.Context(), "events", nil, asMember("ada")); !errors.Is(err, queries.ErrForbidden) {
		t.Errorf("events: ReachOf says admin but Answer let a member through: %v", err)
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
	r.Register("events", auth.ReachAdmin, answersWith(nil))
	r.Register("events", auth.ReachAdmin, answersWith(nil))
}

func TestRegisteringNothingUsefulPanics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		run    func(*queries.Registry)
		reason string
	}{
		{"no name", func(r *queries.Registry) { r.Register("", auth.ReachAdmin, answersWith(nil)) }, "an unnamed question"},
		{"no answer", func(r *queries.Registry) { r.Register("events", auth.ReachAdmin, nil) }, "a question with no answer"},
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

// A QUESTION THAT CANNOT SAY WHICH SIDE OF THE WITHHOLDING RULE IT IS ON IS
// REFUSED AT REGISTRATION (ADR-0031), the zero reach included: served to
// whoever asked because nobody classified it is how a transcript reaches a
// public screen. And the counterfactual — every reach this build knows is
// accepted — so a registry that refused everything fails here too.
func TestARegistrationWithoutAReachPanics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		run    func(*queries.Registry)
		reason string
	}{
		{"no reach", func(r *queries.Registry) { r.Register("events", "", answersWith(nil)) }, "a question with no reach"},
		{"an unknown reach", func(r *queries.Registry) { r.Register("events", "owner", answersWith(nil)) },
			"a question with a reach this build does not know"},
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
	r := queries.NewRegistry()
	for _, reach := range auth.Reaches {
		r.Register(string(reach), reach, answersWith(nil))
	}
	if got := len(r.Names()); got != len(auth.Reaches) {
		t.Errorf("registered %d of the %d reaches this build knows", got, len(auth.Reaches))
	}
}

func TestTheRegistryListsWhatItAnswers(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", auth.ReachAdmin, answersWith(nil))
	r.Register("config", auth.ReachAdmin, answersWith(nil))
	r.Register("agent", auth.ReachAdmin, answersWith(nil))

	if got := r.Names(); !slices.Equal(got, []string{"agent", "config", "events"}) {
		t.Errorf("names = %v, want them sorted", got)
	}
}

func TestAFailingAnswerReachesTheCaller(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("the store fell over")
	r := queries.NewRegistry()
	r.Register("events", auth.ReachAdmin, func(context.Context, queries.Params) (any, error) {
		return nil, sentinel
	})
	if _, err := r.Answer(t.Context(), "events", nil, asAdmin("ops")); !errors.Is(err, sentinel) {
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

// KEYS IS SORTED, AND SOMETHING DEPENDS ON IT.
//
// The tracker's custom-field filters are `f.<slug>` — a prefix whose slugs a
// company declares — so the only way to find them is to enumerate, and a
// parsed query carries them in the order they were enumerated. Two callers
// passing the same filters must produce the same query, and a map's iteration
// order is deliberately random, so the guarantee has to live here: a caller
// that re-sorted would be a second place the property could be true, and the
// first time the two disagreed nobody would know which one ran.
func TestKeysAreSortedBecauseACallerDependsOnIt(t *testing.T) {
	t.Parallel()
	p := queries.FromMap(map[string]any{
		"f.severity": "s1", "status": "todo", "f.area": "platform",
		"container": "workspace", "f.owner": "ana",
	})
	got := p.Keys()
	if !slices.IsSorted(got) {
		t.Fatalf("Keys returned %v, which is not sorted — a parsed query would "+
			"differ between two identical requests", got)
	}
	if len(got) != 5 {
		t.Fatalf("Keys returned %d names for five parameters: %v", len(got), got)
	}
	// And it is the same answer every time, which one sorted call proves
	// and one map iteration does not.
	for range 16 {
		if again := p.Keys(); !slices.Equal(again, got) {
			t.Fatalf("Keys returned %v and then %v", got, again)
		}
	}
}

// A REFUSAL'S SENTENCE IS THE AUTHOR'S, WITHOUT THE CLASS NAME.
//
// Both transports carry this beside the `bad_params` code, so it is text a
// person reads: the sentinel's own words are a Go package naming the class,
// and they are taken out wherever a refusal wrapped them — at the front, at
// the end, or under a wrapping that named the question — while the rest,
// a wrapped cause included, is kept word for word.
func TestARefusalsDetailIsTheAuthorsSentence(t *testing.T) {
	t.Parallel()
	cause := errors.New("tokens: a spend window is 1 to 90 company days")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"sentinel first", fmt.Errorf("%w: days=91 is too many", queries.ErrBadParams), "days=91 is too many"},
		{"sentinel last", fmt.Errorf("name one, not both: %w", queries.ErrBadParams), "name one, not both"},
		{"wrapped by the question", fmt.Errorf("tokens: %w", fmt.Errorf("%w: %w: days=91", queries.ErrBadParams, cause)),
			"tokens: tokens: a spend window is 1 to 90 company days: days=91"},
		{"nothing", nil, ""},
	} {
		if got := queries.RefusalDetail(tc.err); got != tc.want {
			t.Errorf("%s: RefusalDetail = %q, want %q", tc.name, got, tc.want)
		}
	}
}
