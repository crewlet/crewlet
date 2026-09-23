package queries_test

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/iam"
)

func answersWith(v any) queries.Answer {
	return func(context.Context, queries.Params) (any, error) { return v, nil }
}

// --- the registry -------------------------------------------------------- //

func TestARegisteredQuestionIsAnswered(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", iam.GrantStateRead, answersWith("rows"))

	got, err := r.Answer(everyGrant(t), "events", nil)
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
	_, err := r.Answer(everyGrant(t), "nope", nil)
	if !errors.Is(err, queries.ErrUnknown) {
		t.Errorf("err = %v, want ErrUnknown", err)
	}
	// And it names the question: a client switching on the code still
	// needs a log line saying which name nothing answered.
	if err == nil || !slices.Contains([]string{"nope"}, "nope") {
		t.Error("unreachable")
	}
}

// A QUESTION IS ANSWERED TO THE GRANT IT DECLARES, and to nothing narrower.
//
// This used to be a bool — is there a credential, yes or no — and under that a
// token that could read the board could also read the company document and
// every ${VAR} reference in it by name. What the grant buys is the reader that
// replaced `allow_anonymous_read`: a credential holding `state:read` and
// nothing else.
func TestAQuestionIsAnsweredOnlyToTheGrantItDeclares(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("config", iam.GrantConfigRead, answersWith("the company"))
	r.Register("events", iam.GrantAuditRead, answersWith("rows"))

	narrow := asking(t, iam.GrantStateRead)
	if _, err := r.Answer(narrow, "config", nil); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("a state:read credential read the company document: %v", err)
	}
	if _, err := r.Answer(narrow, "events", nil); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("a state:read credential read the transcripts: %v", err)
	}
	// The counterfactual: the grant it DOES declare is answered, or the
	// assertions above would pass on a registry that refused everything.
	got, err := r.Answer(asking(t, iam.GrantConfigRead), "config", nil)
	if err != nil {
		t.Fatalf("the declared grant was refused: %v", err)
	}
	if got != "the company" {
		t.Errorf("answer = %v", got)
	}
}

// A CALLER WITH NO PRINCIPAL AT ALL IS REFUSED, which is the direction
// [iam.From] takes everywhere: silence is not permission. There is no question
// left that answers to nobody — the read posture that made one possible is
// gone.
//
// AND IT IS REFUSED AS A FAULT RATHER THAN AS AN AUTHORIZATION FAILURE. A
// context no resolver ever touched is a route nobody wired through the guard,
// which is a bug in this build and not a caller presenting the wrong thing.
// Answered `unauthorized`, the one symptom of that bug is a client being told
// its credential is bad — so the operator goes and rotates a perfectly good
// token, and the routing hole stays open.
func TestAQuestionIsRefusedToACallerNobodyResolved(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", iam.GrantStateRead, answersWith("rows"))
	// DELIBERATELY t.Context(): a context no resolver ever touched, which
	// is what a handler nobody wired through the guard hands down.
	data, err := r.Answer(t.Context(), "events", nil)
	if err == nil {
		t.Fatalf("a context nobody resolved was answered: %v", data)
	}
	if errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("a wiring bug was reported as the caller's credential: %v", err)
	}
	// NOR AS A RETRY, which is the other half: this never clears by
	// waiting, and a Retry-After sends a client round a loop that can only
	// end when somebody edits the routing.
	if errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("a wiring bug was reported as transient: %v", err)
	}
	// And it says which of the two it is, or the log carries a refusal
	// nobody can act on.
	if !errors.Is(err, iam.ErrUnresolved) {
		t.Errorf("the refusal does not carry iam.ErrUnresolved: %v", err)
	}
}

// NOBODY ASKING AND SOMEBODY LACKING THE GRANT ARE TWO REFUSALS.
//
// They ask a client for opposite things — "present a credential" and "the one
// you presented does not carry this" — and a narrow reader meets the second
// the moment they open a screen outside their grants, which is the ordinary
// case rather than the exceptional one. Folded together, the surface tells
// that reader to go and get a new credential, which is how a company learns to
// rotate working tokens over a permissions message.
//
// NEITHER IS REACHABLE THROUGH THE WIRED GUARD, which answers an anonymous
// request a layer up, and every socket authenticates at its handshake. That is
// exactly why it is asserted here: this package is reached by two transports
// and the middleware in front of one of them is not this package's to keep, so
// a refusal that is correct only because something upstream answers first is
// one edit from being wrong.
func TestNobodyAskingIsADifferentRefusalFromLackingTheGrant(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", iam.GrantAuditRead, answersWith("rows"))

	_, anonymous := r.Answer(iam.WithAnonymous(t.Context()), "events", nil)
	if !errors.Is(anonymous, queries.ErrUnauthenticated) {
		t.Errorf("a caller presenting nothing is not ErrUnauthenticated: %v", anonymous)
	}
	if errors.Is(anonymous, queries.ErrUnauthorized) {
		t.Errorf("a caller presenting nothing was refused as one whose grants "+
			"fall short: %v", anonymous)
	}

	_, narrow := r.Answer(asking(t, iam.GrantStateRead), "events", nil)
	if !errors.Is(narrow, queries.ErrUnauthorized) {
		t.Errorf("a principal lacking the grant is not ErrUnauthorized: %v", narrow)
	}
	if errors.Is(narrow, queries.ErrUnauthenticated) {
		t.Errorf("a principal who presented a perfectly good credential was told "+
			"to present one: %v", narrow)
	}
}

// BUT AN IDENTITY ESTATE THIS NODE CANNOT READ IS A RETRY, not a refusal about
// the caller — and this is the arm the walk in internal/api/auth exists for.
//
// The principal is the same zero value both times, so a registry that dropped
// [iam.From]'s resolution would answer `unauthorized` here: every credential in
// the company reported invalid, at once, for as long as the estate is
// unreachable. That is the failure that teaches a company to reset working
// passwords during an outage.
func TestAQuestionIsRetriedRatherThanRefusedWhenIdentityIsUnreadable(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", iam.GrantStateRead, answersWith("rows"))

	down := errors.New("the identity estate is behind")
	ctx := iam.WithUnresolved(t.Context(), down)
	data, err := r.Answer(ctx, "events", nil)
	if err == nil {
		t.Fatalf("a question was answered while identity was unreadable: %v", data)
	}
	if !errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("an unreadable identity estate is not reported as transient: %v", err)
	}
	if errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("an outage was reported as the caller's credential: %v", err)
	}
	// THE REASON SURVIVES, because "the store timed out" and "this session
	// decodes to nothing" send an operator to opposite places, and the
	// registry is not what chooses between them.
	if !errors.Is(err, down) {
		t.Errorf("the resolver's own reason was dropped: %v", err)
	}
}

// AND THE DECLARED GRANT IS READABLE WITHOUT RUNNING THE ANSWER, which is what
// a gate asserting the posture needs. An unregistered name answers the zero
// grant, which is invalid — so a caller asking about one is told it is not a
// question rather than handed a plausible answer about nothing.
func TestTheDeclaredGrantIsWhatAnswerEnforces(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", iam.GrantAuditRead, answersWith(nil))
	r.Register("config", iam.GrantConfigRead, answersWith(nil))

	for what, want := range map[string]iam.Grant{
		"events": iam.GrantAuditRead,
		"config": iam.GrantConfigRead,
		"nope":   "",
	} {
		if got := r.Needs(what); got != want {
			t.Errorf("%s needs %q, want %q", what, got, want)
		}
		if want == "" {
			continue
		}
		// Holding every OTHER grant is still a refusal, which is what
		// makes the declaration the thing being enforced rather than
		// "is there anybody".
		others := make([]iam.Grant, 0, len(iam.AllGrants))
		for _, g := range iam.AllGrants {
			if g != want {
				others = append(others, g)
			}
		}
		if _, err := r.Answer(asking(t, others...), what, nil); !errors.Is(err, queries.ErrUnauthorized) {
			t.Errorf("%s answered a caller holding every grant but %s: %v", what, want, err)
		}
	}
}

// A QUESTION REGISTERED WITH NO GRANT PANICS, at wiring time, on every build.
//
// It would answer to any credential at all — the exact shape of an ungated
// surface that looks deliberate — and a company finding that out on the first
// request has already served it.
func TestAQuestionWithNoGrantPanicsAtRegistration(t *testing.T) {
	t.Parallel()
	for name, grant := range map[string]iam.Grant{
		"the zero grant":                   "",
		"a grant this build does not know": "secrets:exfiltrate",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("a question was registered with no usable grant")
				}
			}()
			queries.NewRegistry().Register("events", grant, answersWith(nil))
		})
	}
}

// asking is a context carrying a principal with the given grants.
func asking(t *testing.T, grants ...iam.Grant) context.Context {
	t.Helper()
	return iam.WithPrincipal(t.Context(), iam.Principal{
		ID: uuid.New(), Login: "token:test", Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: grants,
	})
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
	r.Register("events", iam.GrantStateRead, answersWith(nil))
	r.Register("events", iam.GrantStateRead, answersWith(nil))
}

func TestRegisteringNothingUsefulPanics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		run    func(*queries.Registry)
		reason string
	}{
		{"no name", func(r *queries.Registry) {
			r.Register("", iam.GrantStateRead, answersWith(nil))
		}, "an unnamed question"},
		{"no answer", func(r *queries.Registry) {
			r.Register("events", iam.GrantStateRead, nil)
		}, "a question with no answer"},
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
	r.Register("events", iam.GrantStateRead, answersWith(nil))
	r.Register("config", iam.GrantConfigRead, answersWith(nil))
	r.Register("agent", iam.GrantStateRead, answersWith(nil))

	if got := r.Names(); !slices.Equal(got, []string{"agent", "config", "events"}) {
		t.Errorf("names = %v, want them sorted", got)
	}
}

func TestAFailingAnswerReachesTheCaller(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("the store fell over")
	r := queries.NewRegistry()
	r.Register("events", iam.GrantStateRead, func(context.Context, queries.Params) (any, error) {
		return nil, sentinel
	})
	if _, err := r.Answer(asking(t, iam.GrantStateRead), "events", nil); !errors.Is(err, sentinel) {
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
