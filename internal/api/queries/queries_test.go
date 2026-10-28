package queries_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

func answersWith(v any) queries.Answer {
	return func(context.Context, queries.Params) (any, error) { return v, nil }
}

// --- the registry -------------------------------------------------------- //

func TestARegisteredQuestionIsAnswered(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", queries.Requires(iam.GrantAdmin), answersWith("rows"))

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
}

// A QUESTION IS ANSWERED TO THE POSTURE IT DECLARES, and to nothing narrower.
//
// This used to be a bool — is there a credential, yes or no — and under that a
// token that could read the board could also read the company document and
// every ${VAR} reference in it by name. Now a question is the overview's
// (everybody), one app's, or admin's, and the declaration is what the registry
// enforces: an app's question is answered to its holder and to an admin, whose
// permission includes every app, and refused with the other app and with none.
func TestAQuestionIsAnsweredOnlyToThePostureItDeclares(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("config", queries.Requires(iam.GrantAdmin), answersWith("the company"))
	r.Register("work_items", queries.Requires(iam.GrantWorklet), answersWith("the board"))
	r.Register("pages", queries.Requires(iam.GrantDoclet), answersWith("the pages"))
	r.Register("budgets", queries.Everybody(), answersWith("the caps"))

	for _, tc := range []struct {
		what    string
		holding []iam.Grant
		admits  bool
	}{
		{"config", []iam.Grant{iam.GrantAdmin}, true},
		{"config", slices.Clone(iam.Apps), false},
		{"config", nil, false},
		{"work_items", []iam.Grant{iam.GrantWorklet}, true},
		{"work_items", []iam.Grant{iam.GrantAdmin}, true},
		{"work_items", []iam.Grant{iam.GrantDoclet}, false},
		{"work_items", nil, false},
		{"pages", []iam.Grant{iam.GrantDoclet}, true},
		{"pages", []iam.Grant{iam.GrantAdmin}, true},
		{"pages", []iam.Grant{iam.GrantWorklet}, false},
		{"budgets", nil, true},
	} {
		_, err := r.Answer(asking(t, tc.holding...), tc.what, nil)
		switch {
		case tc.admits && err != nil:
			t.Errorf("%s holding %v was refused: %v", tc.what, tc.holding, err)
		case !tc.admits && !errors.Is(err, queries.ErrUnauthorized):
			t.Errorf("%s holding %v answered %v, want an authorization refusal",
				tc.what, tc.holding, err)
		}
	}
}

// A REFUSAL OF A QUESTION'S PERMISSION NAMES THE ONE PERMISSION THAT WOULD
// ADMIT, never admin beside an app: admin admits through the app it includes,
// and a screen telling a person who only uses the board to ask for host access
// sends them to ask for the wrong thing.
func TestARefusalNamesThePermissionTheQuestionDeclares(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("work_items", queries.Requires(iam.GrantWorklet), answersWith(nil))
	_, err := r.Answer(asking(t, iam.GrantDoclet), "work_items", nil)
	var refusal *queries.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a *queries.Refusal", err)
	}
	if !slices.Equal(refusal.Grants, []iam.Grant{iam.GrantWorklet}) {
		t.Errorf("the refusal names %v, want exactly [worklet]", refusal.Grants)
	}
}

// A STAGE THAT MAY NOT ACT IS REFUSED BEFORE ANY POSTURE, an everybody question
// included: the overview decides nothing else, so without it a suspended
// person's live session would go on reading the company every other gesture of
// theirs is refused.
func TestAStageThatMayNotActIsRefusedEveryQuestion(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("budgets", queries.Everybody(), answersWith("the caps"))
	r.Register("turns", queries.DecidedBy(authz.ActionSeatTrailRead), answersWith("turns"))
	for _, stage := range []iam.Stage{iam.StageSuspended, iam.StageRetired} {
		ctx := iam.WithPrincipal(t.Context(), iam.Principal{
			ID: uuid.New(), Login: "ana.lee", Kind: iam.KindPerson, Seat: "ana",
			Stage: stage, Grants: []iam.Grant{iam.GrantAdmin},
		})
		for _, what := range []string{"budgets", "turns"} {
			_, err := r.Answer(ctx, what, nil)
			var refusal *queries.Refusal
			if !errors.As(err, &refusal) || refusal.Reason != authz.ReasonStage {
				t.Errorf("%s asked by a %s admin answered %v, want a stage refusal",
					what, stage, err)
			}
		}
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
	r.Register("budgets", queries.Everybody(), answersWith("the caps"))
	// DELIBERATELY t.Context(): a context no resolver ever touched, which
	// is what a handler nobody wired through the guard hands down.
	data, err := r.Answer(t.Context(), "budgets", nil)
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
		t.Errorf("a wiring bug was reported as unavailable: %v", err)
	}
	// And it says which of the two it is, or the log carries a refusal
	// nobody can act on.
	if !errors.Is(err, iam.ErrUnresolved) {
		t.Errorf("the refusal does not carry iam.ErrUnresolved: %v", err)
	}
}

// NOBODY ASKING AND SOMEBODY LACKING THE PERMISSION ARE TWO REFUSALS.
//
// They ask a client for opposite things — "present a credential" and "the one
// you presented does not carry this" — and a narrow reader meets the second
// the moment they open a screen outside their access, which is the ordinary
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
func TestNobodyAskingIsADifferentRefusalFromLackingThePermission(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	r.Register("events", queries.Requires(iam.GrantAdmin), answersWith("rows"))
	r.Register("budgets", queries.Everybody(), answersWith("the caps"))

	for _, what := range []string{"events", "budgets"} {
		_, anonymous := r.Answer(iam.WithAnonymous(t.Context()), what, nil)
		if !errors.Is(anonymous, queries.ErrUnauthenticated) {
			t.Errorf("%s: a caller presenting nothing is not ErrUnauthenticated: %v",
				what, anonymous)
		}
		if errors.Is(anonymous, queries.ErrUnauthorized) {
			t.Errorf("%s: a caller presenting nothing was refused as one whose "+
				"access falls short: %v", what, anonymous)
		}
	}

	_, narrow := r.Answer(asking(t, iam.GrantWorklet), "events", nil)
	if !errors.Is(narrow, queries.ErrUnauthorized) {
		t.Errorf("a principal lacking the permission is not ErrUnauthorized: %v", narrow)
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
	r.Register("budgets", queries.Everybody(), answersWith("the caps"))

	down := errors.New("the identity estate is behind")
	ctx := iam.WithUnresolved(t.Context(), down)
	data, err := r.Answer(ctx, "budgets", nil)
	if err == nil {
		t.Fatalf("a question was answered while identity was unreadable: %v", data)
	}
	if !errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("an unreadable identity estate is not reported as unavailable: %v", err)
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

// EVERY QUESTION DECLARES A POSTURE, it is the one ADR-0033 decided, and it is
// what Answer enforces.
//
// THE TABLE IS STATED HERE IN FULL, in both directions: a question this build
// answers that it does not name fails, and so does a name it holds that this
// build does not answer — so a question whose posture is changed, or one added
// without a decision, is a reviewed edit of this table rather than a quiet
// change to who reads what. Then, for every one, the declaration is held to
// what the registry does: an app's question ADMITS its app and admin, whose
// permission includes every app, and REFUSES the other app and nothing; an
// admin's admits admin alone; and everybody's, or one its answer decides,
// admits a caller holding nothing.
//
// ADMISSION IS ASKED OF A REGISTRY CARRYING THE SAME POSTURES over answers
// that do nothing, because an admitted question runs its answer and the
// fixture's seams are empty values; a REFUSAL is asked of the production
// registry itself, since the registry refuses before any answer runs.
func TestEveryQuestionDeclaresAPosture(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"agent": "everybody", "live_call": "everybody", "tokens": "everybody",
		"token_series": "everybody", "seat_activity": "everybody",
		"viewer": "everybody", "schedule_runs": "everybody",
		"budgets": "everybody", "schedules": "everybody",
		"knowledge": "everybody", "colleague": "everybody",
		"sandbox_runs": "everybody", "company_feed": "everybody",
		"decisions": "everybody",
		// The other half of `decisions`: a person's own asks need no app.
		"answered_decisions": "everybody",

		"events": "admin", "event_series": "admin", "event": "admin",
		"trace": "admin", "turn": "admin", "fleet": "admin",
		"mcp_servers_status": "admin", "backups": "admin",
		"fleet_broker": "admin",
		"integrations": "admin", "retention": "admin", "a2a_channels": "admin",
		"config": "admin", "config_audit": "admin", "config_diff": "admin",
		"config_entities": "admin", "limits": "admin", "sign_in_policy": "admin",

		"work_items": "worklet", "work_comments": "worklet",
		"work_item_turns": "worklet", "work_views": "worklet",
		"work_saved_views": "worklet", "work_projects": "worklet",
		"work_project": "worklet", "work_workload": "worklet",
		"work_activity": "worklet", "work_my_work": "worklet",
		"work_inbox": "worklet", "work_routing": "worklet",
		"work_catalogue": "worklet", "work_person": "worklet",
		"work_flow": "worklet", "work_files": "worklet", "work_search": "worklet",

		"pages": "doclet", "page": "doclet", "containers": "doclet",
		"page_activity": "doclet", "page_revision": "doclet",
		"page_reads": "doclet",

		"work_item":       "decided_by:" + string(authz.ActionWorkRead),
		"turns":           "decided_by:" + string(authz.ActionSeatTrailRead),
		"phases":          "decided_by:" + string(authz.ActionSeatTrailRead),
		"sandbox_tail":    "decided_by:" + string(authz.ActionSeatTrailRead),
		"agent_memory":    "decided_by:" + string(authz.ActionSeatTrailRead),
		"agent_episode":   "decided_by:" + string(authz.ActionSeatTrailRead),
		"memory_overview": "decided_by:" + string(authz.ActionSeatTrailRead),
		"conversations":   "decided_by:" + string(authz.ActionSeatTrailRead),

		// THE ARTIFACTS: the listing and the folder tree are everybody's and
		// filtered row by row and folder by folder;
		// every other question is decided per artifact by the artifacts' one
		// admission, the usage against the plan is the deployment's, and the
		// company trail of every artifact is the company-wide trail's.
		"artifacts":             "everybody",
		"artifact_folders":      "everybody",
		"artifact":              "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_versions":     "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_version":      "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_history":      "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_assets":       "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_seen_by":      "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_access":       "decided_by:" + string(authz.ActionArtifactRead),
		"artifact_usage":        "decided_by:" + string(authz.ActionArtifactUsage),
		"artifact_trail":        "decided_by:" + string(authz.ActionAuditRead),
		"brand":                 "decided_by:" + string(authz.ActionBrandRead),
		"artifact_data":         "decided_by:" + string(authz.ActionArtifactDataRead),
		"artifact_data_doc":     "decided_by:" + string(authz.ActionArtifactDataRead),
		"artifact_data_query":   "decided_by:" + string(authz.ActionArtifactDataQuery),
		"artifact_data_history": "decided_by:" + string(authz.ActionArtifactDataRead),
		"artifact_data_schema":  "decided_by:" + string(authz.ActionArtifactDataRead),
		"artifact_data_state":   "decided_by:" + string(authz.ActionArtifactDataRead),
		"artifact_data_usage":   "decided_by:" + string(authz.ActionArtifactUsage),
	}
	r := queries.NewRegistry()
	queries.Register(r, everySeam(t))
	if got := r.PostureOf("nope"); got.Valid() {
		t.Errorf("an unregistered question has the posture %s", got)
	}
	names := r.Names()
	for what := range want {
		if !slices.Contains(names, what) {
			t.Errorf("%s is in the table and this build does not answer it", what)
		}
	}
	mirror := queries.NewRegistry()
	for _, what := range names {
		posture := r.PostureOf(what)
		if !posture.Valid() {
			t.Errorf("%s is registered with no valid posture", what)
			continue
		}
		if got, ok := want[what]; !ok {
			t.Errorf("%s is answered with the posture %s and the table does not "+
				"name it — decide who reads it, and say so here", what, posture)
		} else if posture.String() != got {
			t.Errorf("%s is %s, want %s", what, posture, got)
		}
		mirror.Register(what, posture, answersWith("answered"))

		g := posture.Grant()
		admitted := [][]iam.Grant{nil}
		var refusedWith [][]iam.Grant
		switch g {
		case "":
		case iam.GrantAdmin:
			admitted = [][]iam.Grant{{iam.GrantAdmin}}
			refusedWith = [][]iam.Grant{nil, slices.Clone(iam.Apps)}
		default:
			admitted = [][]iam.Grant{{g}, {iam.GrantAdmin}}
			refusedWith = [][]iam.Grant{nil, slices.DeleteFunc(slices.Clone(iam.Apps),
				func(app iam.Grant) bool { return app == g })}
		}
		for _, holding := range admitted {
			if _, err := mirror.Answer(asking(t, holding...), what, nil); err != nil {
				t.Errorf("%s is %s and refused a caller holding %v: %v",
					what, posture, holding, err)
			}
		}
		for _, holding := range refusedWith {
			_, err := r.Answer(asking(t, holding...), what, nil)
			var refusal *queries.Refusal
			if !errors.As(err, &refusal) || refusal.Reason != authz.ReasonNoGrant ||
				!slices.Equal(refusal.Grants, []iam.Grant{g}) {
				t.Errorf("%s is %s and answered a caller holding %v with %v, "+
					"want a refusal naming [%s]", what, posture, holding, err, g)
			}
		}
	}
}

// A QUESTION REGISTERED WITH NO POSTURE PANICS, at wiring time, on every
// build — and so does one naming a permission that is not admin or an app, or
// a verb the authority table has no row for.
//
// Any of them would answer to any credential at all — the exact shape of an
// ungated surface that looks deliberate — and a company finding that out on
// the first request has already served it.
func TestAZeroPostureAndAnUnknownVerbPanicAtRegistration(t *testing.T) {
	t.Parallel()
	for name, posture := range map[string]queries.Posture{
		"the zero posture":                          {},
		"a permission this build does not know":     queries.Requires("secrets:exfiltrate"),
		"no permission at all":                      queries.Requires(""),
		"a verb the authority table has no row for": queries.DecidedBy("audit.everything"),
		"no verb at all":                            queries.DecidedBy(""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("a question was registered with %s", name)
				}
			}()
			queries.NewRegistry().Register("events", posture, answersWith(nil))
		})
	}
	// THE COUNTERFACTUAL: every legal posture registers, or the cases above
	// would pass on a registry that panicked on everything.
	r := queries.NewRegistry()
	r.Register("a", queries.Everybody(), answersWith(nil))
	r.Register("b", queries.Requires(iam.GrantAdmin), answersWith(nil))
	for _, app := range iam.Apps {
		r.Register("c"+string(app), queries.Requires(app), answersWith(nil))
	}
	r.Register("d", queries.DecidedBy(authz.ActionSeatTrailRead), answersWith(nil))
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
	r.Register("events", queries.Requires(iam.GrantAdmin), answersWith(nil))
	r.Register("events", queries.Requires(iam.GrantAdmin), answersWith(nil))
}

func TestRegisteringNothingUsefulPanics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		run    func(*queries.Registry)
		reason string
	}{
		{"no name", func(r *queries.Registry) {
			r.Register("", queries.Everybody(), answersWith(nil))
		}, "an unnamed question"},
		{"no answer", func(r *queries.Registry) {
			r.Register("events", queries.Everybody(), nil)
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
	r.Register("events", queries.Requires(iam.GrantAdmin), answersWith(nil))
	r.Register("config", queries.Requires(iam.GrantAdmin), answersWith(nil))
	r.Register("agent", queries.Everybody(), answersWith(nil))

	if got := r.Names(); !slices.Equal(got, []string{"agent", "config", "events"}) {
		t.Errorf("names = %v, want them sorted", got)
	}
}

func TestAFailingAnswerReachesTheCaller(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("the store fell over")
	r := queries.NewRegistry()
	r.Register("events", queries.Everybody(), func(context.Context, queries.Params) (any, error) {
		return nil, sentinel
	})
	if _, err := r.Answer(asking(t), "events", nil); !errors.Is(err, sentinel) {
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

// requiresExactly asserts a question is registered as requiring grant, and
// that a caller holding every OTHER permission but admin is refused it on
// authority — the posture stated once, with the two halves a gate has to
// check: what the registry declares, and that the declaration is what decides.
// Admin is left out of "every other" because it includes every app: a caller
// holding it is admitted to every app's question by design.
func requiresExactly(t *testing.T, r *queries.Registry, what string, grant iam.Grant) {
	t.Helper()
	if got := r.PostureOf(what).Grant(); got != grant {
		t.Fatalf("%s requires %q, want %q", what, got, grant)
	}
	everythingElse := slices.DeleteFunc(slices.Clone(iam.AllGrants),
		func(g iam.Grant) bool { return g == grant || g == iam.GrantAdmin })
	if _, err := r.Answer(asking(t, everythingElse...), what, nil); !errors.Is(err,
		queries.ErrUnauthorized) {
		t.Errorf("%s without %s answered %v, want an authorization refusal",
			what, grant, err)
	}
}
