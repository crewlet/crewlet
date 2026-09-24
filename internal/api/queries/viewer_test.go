// Who the caller is, and what that entitles them to read.

package queries_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// viewerCompany binds one operator id to one seat, and leaves a second seat
// bound to nobody — which is what makes "somebody else's handle" a real case
// rather than a typo.
const viewerCompany = `
name: Acme
roles:
  - name: Ana Diaz
    handle: ana
    kind: human
    contact:
      crewlet_operator_id: ops-1
  - name: Bo Lang
    handle: bo
    kind: human
    contact:
      slack_user_id: U0COLLEAGUE
`

func viewerSources(t *testing.T, work *stubWork) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(viewerCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return queries.Sources{
		Company: func() *config.Company { return cfg },
		Work:    work,
	}
}

// answer runs one question and insists it succeeded, so a case about a VALUE
// never quietly becomes a case about a refusal.
func answerMap(t *testing.T, got any, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	out, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("answered %T, want a map", got)
	}
	return out
}

// THE CREDENTIAL RESOLVES TO A PERSON. Two steps that have both existed since
// the engine shipped — a token maps to an operator id, a seat names one in its
// contact block — and nothing walked them, so every personal surface in the
// dashboard was guessing.
func TestTheViewerIsTheSeatTheTokenIsBoundTo(t *testing.T) {
	t.Parallel()
	answered, err := askAsOperator(t, viewerSources(t, &stubWork{}), "viewer", nil)
	got := answerMap(t, answered, err)
	if got["handle"] != "ana" {
		t.Errorf("handle = %v, want the seat binding ops-1", got["handle"])
	}
	if got["name"] != "Ana Diaz" {
		t.Errorf("name = %v, want the seat's own", got["name"])
	}
	if got["kind"] != "human" {
		t.Errorf("kind = %v, want human", got["kind"])
	}
	if got["operator"] != true {
		t.Errorf("operator = %v; a presented token is what the guarded rows turn on", got["operator"])
	}
}

// AN UNBOUND TOKEN IS AN ORDINARY STATE, not an error. The remedy is a line of
// company configuration, and a screen that renders an error cannot say so —
// which is why the operator id is answered even when no seat claims it.
func TestAnUnboundTokenAnswersItsOperatorIdAndNoSeat(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, viewerSources(t, &stubWork{}))
	answered, err := r.Answer(t.Context(), "viewer", nil, "ops-nobody")
	got := answerMap(t, answered, err)
	if got["operator_id"] != "ops-nobody" {
		t.Errorf("operator_id = %v, want the id the token carries", got["operator_id"])
	}
	if got["handle"] != "" {
		t.Errorf("handle = %v, want none: no seat names ops-nobody", got["handle"])
	}
}

// AND AN ANONYMOUS READER IS A THIRD STATE. No token at all is neither an
// unbound one nor a bound one, and the three take three different sentences on
// screen — so the answer has to keep them apart.
func TestAnAnonymousReaderIsNeitherBoundNorAnOperator(t *testing.T) {
	t.Parallel()
	answered, err := askNative(t, viewerSources(t, &stubWork{}), "viewer", nil)
	got := answerMap(t, answered, err)
	if got["operator_id"] != "" || got["operator"] != false {
		t.Errorf("anonymous answered %v, want no id and no operator", got)
	}
	if got["handle"] != "" {
		t.Errorf("handle = %v, want none", got["handle"])
	}
}

// A DISABLED GUARD IS NEVER A PERSON. With `api.auth.disabled` every caller
// is stamped with the reserved id, so a seat bound to that id would hand
// whoever reaches the engine that person's dashboard — their inbox, their
// queue, their name on every write. The literal is refused where the company
// is read, naming the field; a `${VAR}` cannot be, because its value lives in
// an environment validation may not see, so the resolution drops it instead.
//
// Not parallel: the reference resolves against the process environment, which
// is what every consumer of the binding reads.
func TestADisabledGuardIsNeverAPerson(t *testing.T) {
	b := config.DefaultBootstrap()
	b.API.Auth.Disabled = true
	caller, ok := auth.New(&b).Operator("")
	if !ok || caller != org.ReservedOperatorID {
		t.Fatalf("a disabled guard answered %q/%v, want the reserved id", caller, ok)
	}

	literal := strings.Replace(viewerCompany, "crewlet_operator_id: ops-1",
		"crewlet_operator_id: "+caller, 1)
	_, err := config.ParseCompany([]byte(literal))
	if !errors.Is(err, org.ErrReservedOperatorID) {
		t.Fatalf("a seat bound to %q parsed with %v, want it refused", caller, err)
	}
	if !strings.Contains(err.Error(), "crewlet_operator_id") {
		t.Errorf("the refusal does not name the field: %v", err)
	}

	t.Setenv("CREWLET_TEST_BOUND_OPERATOR", "Anonymous")
	cfg, err := config.ParseCompany([]byte(strings.Replace(viewerCompany,
		"crewlet_operator_id: ops-1",
		"crewlet_operator_id: ${CREWLET_TEST_BOUND_OPERATOR}", 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{
		Company: func() *config.Company { return cfg },
		Work:    &stubWork{},
	})
	answered, err := r.Answer(t.Context(), "viewer", nil, caller)
	got := answerMap(t, answered, err)
	if got["handle"] != "" {
		t.Errorf("the disabled guard's caller is seat %v; a reference resolving "+
			"to the reserved id must bind nobody", got["handle"])
	}
	if got["operator_id"] != caller {
		t.Errorf("operator_id = %v, want the reserved id the guard stamped",
			got["operator_id"])
	}
}

// THE VIEWER NAMES WHAT IT MAY ACT ON, and only a person may act on anything.
// The act transport admits a token bound to a seat and nobody else
// (ADR-0024), so the list a screen enables its controls from is that
// transport's own for a bound token and EMPTY for everybody else — an array
// either way, because "may do nothing" is a value a screen tests, not an
// absence it has to guess the meaning of.
func TestTheViewerNamesWhatItMayAct(t *testing.T) {
	t.Parallel()
	served := []string{"create_work_item", "mark_inbox"}
	sources := viewerSources(t, &stubWork{})
	sources.OperatorActs = func() []string { return served }
	r := queries.NewRegistry()
	queries.Register(r, sources)
	for _, tc := range []struct {
		name, operatorID string
		want             []string
	}{
		{"a bound token", "ops-1", served},
		{"an unbound token", "ops-nobody", []string{}},
		{"an anonymous reader", "", []string{}},
		{"a disabled guard's caller", org.ReservedOperatorID, []string{}},
	} {
		answered, err := r.Answer(t.Context(), "viewer", nil, tc.operatorID)
		got := answerMap(t, answered, err)
		acts, ok := got["acts"].([]string)
		if !ok {
			t.Errorf("%s: acts is %#v, want an array", tc.name, got["acts"])
			continue
		}
		if !slices.Equal(acts, tc.want) {
			t.Errorf("%s: acts = %v, want %v", tc.name, acts, tc.want)
		}
	}
	// AND A NODE WITH NOTHING TO SERVE ANSWERS AN EMPTY LIST to a bound
	// person too, rather than a null a screen would read as "not loaded".
	bare := queries.NewRegistry()
	queries.Register(bare, viewerSources(t, &stubWork{}))
	answered, err := bare.Answer(t.Context(), "viewer", nil, "ops-1")
	if acts, ok := answerMap(t, answered, err)["acts"].([]string); !ok || len(acts) != 0 {
		t.Errorf("a bound person on a node serving no acts got %#v", acts)
	}
}

// THE PERSONAL QUESTIONS ARE SCOPED, NOT OPERATOR-GATED.
//
// `work_my_work` was registered operator-only and demanded a handle, which
// made the one screen a human teammate would live on both unreachable to them
// and, for an operator, a stranger's day. The rule that replaces it is the
// smallest one that is safe: your own seat without a credential, anybody
// else's with one.
func TestAPersonalQuestionDefaultsToTheCallersOwnSeat(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAsOperator(t, viewerSources(t, work), "work_my_work", nil); err != nil {
		t.Fatalf("work_my_work with no handle: %v", err)
	}
	if work.myWorkQuery.Who.Handle != "ana" {
		t.Errorf("read %q's day, want the caller's own seat", work.myWorkQuery.Who.Handle)
	}
}

// NAMING SOMEBODY ELSE NEEDS THE CREDENTIAL. Without this the scope rule is
// decoration: any reader could name any handle.
func TestReadingAnotherPersonsDayNeedsAnOperatorCredential(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	_, err := askNative(t, viewerSources(t, work), "work_my_work", map[string]any{"handle": "bo"})
	if !errors.Is(err, queries.ErrUnauthorized) {
		t.Fatalf("anonymous read of bo's day = %v, want unauthorized", err)
	}
	if work.myWorkQuery.Who.Handle != "" {
		t.Errorf("the reader was called with %q anyway", work.myWorkQuery.Who.Handle)
	}
	// And WITH one it is allowed: an operator reading a report's day is a
	// real thing to do, and the refusal above must not be "handles other
	// than your own are refused".
	if _, err := askAsOperator(t, viewerSources(t, work), "work_my_work",
		map[string]any{"handle": "bo"}); err != nil {
		t.Fatalf("an operator reading bo's day: %v", err)
	}
	if work.myWorkQuery.Who.Handle != "bo" {
		t.Errorf("read %q's day, want the handle the operator named", work.myWorkQuery.Who.Handle)
	}
}

// A CALLER NOBODY IS BOUND TO IS REFUSED FOR PARAMETERS, NOT FOR AUTHORITY.
// Nobody was denied anything: there is no person to answer about, and telling
// somebody to present a different credential is the wrong remedy for a company
// that has not bound theirs.
func TestAnUnbindableCallerIsRefusedForWantOfAHandle(t *testing.T) {
	t.Parallel()
	_, err := askNative(t, viewerSources(t, &stubWork{}), "work_my_work", nil)
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("no token and no handle = %v, want bad_params", err)
	}
	if errors.Is(err, queries.ErrUnauthorized) {
		t.Error("refused as unauthorized; the remedy is configuration, not a credential")
	}
}

// THE INBOX IS ONE PERSON'S, on the same rule.
func TestTheInboxIsScopedTheSameWay(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAsOperator(t, viewerSources(t, work), "work_inbox", nil); err != nil {
		t.Fatalf("work_inbox with no handle: %v", err)
	}
	if work.inboxQuery.Who.Handle != "ana" {
		t.Errorf("read %q's inbox, want the caller's own", work.inboxQuery.Who.Handle)
	}
	if _, err := askNative(t, viewerSources(t, work), "work_inbox",
		map[string]any{"handle": "bo"}); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("anonymous read of bo's inbox = %v, want unauthorized", err)
	}
}

// EVERY INBOX FILTER REACHES THE READER. A filter the surface accepts and
// drops is a page showing more than the person asked for, silently — the same
// failure the board's own parameter sweep exists to catch.
func TestEveryInboxFilterReachesTheReader(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	_, err := askAsOperator(t, viewerSources(t, work), "work_inbox", map[string]any{
		"unread":          true,
		"primary_only":    true,
		"include_snoozed": true,
		"reasons":         "mention,asked",
		"limit":           7,
		"cursor":          "c-9",
	})
	if err != nil {
		t.Fatalf("work_inbox: %v", err)
	}
	q := work.inboxQuery
	if !q.Unread || !q.PrimaryOnly || !q.IncludeSnoozed {
		t.Errorf("the three flags reached the reader as %+v", q)
	}
	if q.Limit != 7 || q.Cursor != "c-9" {
		t.Errorf("limit/cursor reached the reader as %d/%q", q.Limit, q.Cursor)
	}
	want := []tracker.Reason{tracker.ReasonMention, tracker.ReasonAsked}
	if len(q.Reasons) != len(want) || q.Reasons[0] != want[0] || q.Reasons[1] != want[1] {
		t.Errorf("reasons reached the reader as %v, want %v", q.Reasons, want)
	}
}

// AN UNKNOWN REASON IS REFUSED NAMING THE SET. Carried through, it would
// narrow to nothing and read as an empty inbox — which is the answer a person
// acts on by assuming nobody has written to them.
func TestAnUnknownInboxReasonIsRefusedNamingTheSet(t *testing.T) {
	t.Parallel()
	_, err := askAsOperator(t, viewerSources(t, &stubWork{}), "work_inbox",
		map[string]any{"reasons": "shouted_at"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("an unknown reason = %v, want bad_params", err)
	}
	if !strings.Contains(err.Error(), "mention") {
		t.Errorf("the refusal %q does not name what would have worked", err)
	}
}

// `since` IS A WHOLE LOG POSITION, and a bare sequence number is refused.
//
// The comparison behind it is on the PACKED form — `(generation << 40) | seq`
// — so a sequence with no generation sorts below every position on a stream
// that has been reanchored, and the resume meant to skip what somebody read
// re-delivers all of it. Refusing is the only honest answer: there is no
// generation to guess.
func TestTheInboxResumeTakesTheWholePosition(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAsOperator(t, viewerSources(t, work), "work_inbox",
		map[string]any{"since": "CREWLET_TRACKER_LOG@2:41"}); err != nil {
		t.Fatalf("a whole position: %v", err)
	}
	if work.inboxQuery.Since.Generation != 2 || work.inboxQuery.Since.Seq != 41 {
		t.Errorf("the position reached the reader as %s", work.inboxQuery.Since)
	}
	if _, err := askAsOperator(t, viewerSources(t, work), "work_inbox",
		map[string]any{"since": "41"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a bare sequence = %v, want bad_params", err)
	}
}
