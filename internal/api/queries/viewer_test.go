// Who the caller is, and what that entitles them to read.

package queries_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tracker"
)

// viewerCompany holds two human seats, so "somebody else's handle" is a real
// case rather than a typo.
//
// NOTHING BINDS A CREDENTIAL HERE ANY MORE. Ana's contact block used to name
// the token id `ops-1`, which was the engine's only link between a token and a
// seat; the identity estate holds that binding now,
// and a test says who is calling by building the PRINCIPAL rather than by
// configuring the company.
const viewerCompany = `
name: Acme
roles:
  - name: Ana Diaz
    handle: ana
    kind: human
    contact:
      slack_user_id: U0ANA
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
		Company: companySource(t, cfg),
		Work:    work,
		// THE CHART ANSWERS NO RELATION, which is what makes the
		// cross-person cases about authority rather than about a lead
		// this fixture happens to declare. A case that needs the lead
		// arm states its own chart.
		Chart: flatChart{},
	}
}

// flatChart is a chart that can answer and reports no relation at all —
// deliberately NOT [authz.NoChart], whose every answer is UNKNOWN.
//
// The two are opposite fixtures and the distinction is the whole of what
// internal/authz's three-valued seam buys: "this company has no lead relation
// between these two" is a 403, and "this node could not read its chart" is a
// 503 the caller retries.
type flatChart struct{}

func (flatChart) Leads(context.Context, string, string) (bool, error) {
	return false, nil
}

func (flatChart) LeadsProject(context.Context, string, string) (bool, error) {
	return false, nil
}

func (flatChart) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, nil
}

func (flatChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
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

// THE VIEWER IS THE PRINCIPAL, and nothing is derived.
//
// It used to walk two steps — a token mapped to an operator id, and a seat
// named one in its contact block — which was the engine's only link between a
// credential and a person. The identity estate replaced both: a person is a
// row, a session resolves to it, and the seat they hold is on the chart.
func TestTheViewerIsThePrincipalsOwnSeat(t *testing.T) {
	t.Parallel()
	answered, err := askAsSeat(t, viewerSources(t, &stubWork{}), "ana",
		"viewer", nil)
	got := answerMap(t, answered, err)
	if got["handle"] != "ana" {
		t.Errorf("handle = %v, want the principal's own seat", got["handle"])
	}
	if got["name"] != "Ana Diaz" {
		t.Errorf("name = %v, want the seat's own", got["name"])
	}
	if got["kind"] != "human" {
		t.Errorf("kind = %v, want human", got["kind"])
	}
	if got["login"] != "ana" {
		t.Errorf("login = %v, want the principal's own", got["login"])
	}
}

// AN UNBOUND CREDENTIAL IS AN ORDINARY STATE, not an error. The remedy is a
// binding in the org chart, and a screen that renders an error cannot say so —
// which is why the login is answered even when no seat holds it.
func TestAnUnboundCredentialAnswersItsLoginAndNoSeat(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, viewerSources(t, &stubWork{}))
	answered, err := r.Answer(everyGrant(t), "viewer", nil)
	got := answerMap(t, answered, err)
	if got["login"] == "" {
		t.Errorf("login = %v, want the one the credential carries", got["login"])
	}
	if got["handle"] != "" {
		t.Errorf("handle = %v, want none: this credential holds no seat", got["handle"])
	}
}

// AND AN ANONYMOUS READER IS A THIRD STATE. No credential at all is neither an
// unbound one nor a bound one, and the three take three different sentences on
// screen — so the answer has to keep them apart.
func TestAnAnonymousReaderIsNeitherBoundNorSeated(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, viewerSources(t, &stubWork{}))
	_, err := r.Answer(iam.WithAnonymous(t.Context()), "viewer", nil)
	if !errors.Is(err, queries.ErrUnauthenticated) {
		t.Fatalf("an anonymous read of `viewer` = %v, want unauthenticated — "+
			"every question on this surface needs a credential now", err)
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
	if _, err := askAsSeat(t, viewerSources(t, work), "ana", "work_my_work", nil); err != nil {
		t.Fatalf("work_my_work with no handle: %v", err)
	}
	if work.myWorkQuery.Handle != "ana" {
		t.Errorf("read %q's day, want the caller's own seat", work.myWorkQuery.Handle)
	}
}

// NAMING SOMEBODY ELSE TAKES THE LEAD RELATION OR fleet:operate, and the
// whole scope rule is decoration without it: any reader could name any handle.
//
// It used to be "any operator credential", which made every token in Tier A a
// reader of every seat's day. It is [authz.ClassOwnOrLead]'s answer now —
// the owner, whoever leads them, or the deployment's admin grant — which is the
// same rule the tracker's own writer enforces at the record.
func TestReadingAnotherPersonsDayNeedsTheLeadRelation(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	// A COLLEAGUE, holding every grant BUT fleet:operate, against a chart
	// that reports no relation: the most authority a caller can have and
	// still be refused, which is what makes the refusal about the rule.
	colleague := viewerSources(t, work)
	_, err := askHolding(t, colleague, "ana", "work_my_work",
		map[string]any{"handle": "bo"},
		iam.GrantStateRead, iam.GrantWorkWrite)
	if !errors.Is(err, queries.ErrUnauthorized) {
		t.Fatalf("a colleague read bo's day = %v, want unauthorized", err)
	}
	if work.myWorkQuery.Handle != "" {
		t.Errorf("the reader was called with %q anyway", work.myWorkQuery.Handle)
	}

	// AND THEIR LEAD MAY, which the refusal above must not be mistaken
	// for: "handles other than your own are refused" would make a lead's
	// own screen unreachable.
	led := viewerSources(t, work)
	led.Chart = leadsChart{lead: "ana", report: "bo"}
	if _, err := askHolding(t, led, "ana", "work_my_work",
		map[string]any{"handle": "bo"},
		iam.GrantStateRead); err != nil {

		t.Fatalf("ana reading her report's day: %v", err)
	}
	if work.myWorkQuery.Handle != "bo" {
		t.Errorf("read %q's day, want the handle the lead named",
			work.myWorkQuery.Handle)
	}

	// AND A NODE THAT COULD NOT READ ITS CHART SAYS SO, rather than
	// refusing: 503 is retried and 403 sends a lead to ask for authority
	// they hold.
	blind := viewerSources(t, &stubWork{})
	blind.Chart = authz.NoChart{}
	if _, err := askHolding(t, blind, "ana", "work_my_work",
		map[string]any{"handle": "bo"},
		iam.GrantStateRead); !errors.Is(err, queries.ErrUnavailable) {

		t.Errorf("an unreadable chart answered %v, want unavailable", err)
	}
}

// leadsChart reports exactly one management relation.
type leadsChart struct {
	flatChart
	lead, report string
}

func (c leadsChart) Leads(_ context.Context, actor, subject string) (bool, error) {
	return actor == c.lead && subject == c.report, nil
}

// A CALLER NOBODY IS BOUND TO IS REFUSED FOR PARAMETERS, NOT FOR AUTHORITY.
// Nobody was denied anything: there is no person to answer about, and telling
// somebody to present a different credential is the wrong remedy for a company
// that has not bound theirs.
func TestAnUnbindableCallerIsRefusedForWantOfAHandle(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, viewerSources(t, &stubWork{}))
	_, err := r.Answer(everyGrant(t), "work_my_work", nil)
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("a seatless caller naming no handle = %v, want bad_params", err)
	}
	if errors.Is(err, queries.ErrUnauthorized) {
		t.Error("refused as unauthorized; the remedy is a binding, not a credential")
	}
}

// THE INBOX IS ONE PERSON'S, on the same rule.
func TestTheInboxIsScopedTheSameWay(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAsSeat(t, viewerSources(t, work), "ana", "work_inbox", nil); err != nil {
		t.Fatalf("work_inbox with no handle: %v", err)
	}
	if work.inboxQuery.Handle != "ana" {
		t.Errorf("read %q's inbox, want the caller's own", work.inboxQuery.Handle)
	}
	if _, err := askHolding(t, viewerSources(t, work), "ana", "work_inbox",
		map[string]any{"handle": "bo"},
		iam.GrantStateRead); !errors.Is(err, queries.ErrUnauthorized) {

		t.Errorf("a colleague read bo's inbox = %v, want unauthorized", err)
	}
}

// EVERY INBOX FILTER REACHES THE READER. A filter the surface accepts and
// drops is a page showing more than the person asked for, silently — the same
// failure the board's own parameter sweep exists to catch.
func TestEveryInboxFilterReachesTheReader(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	_, err := askAsSeat(t, viewerSources(t, work), "ana", "work_inbox", map[string]any{
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
	_, err := askAsSeat(t, viewerSources(t, &stubWork{}), "ana", "work_inbox",
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
	if _, err := askAsSeat(t, viewerSources(t, work), "ana", "work_inbox",
		map[string]any{"since": "CREWLET_TRACKER_LOG@2:41"}); err != nil {
		t.Fatalf("a whole position: %v", err)
	}
	if work.inboxQuery.Since.Generation != 2 || work.inboxQuery.Since.Seq != 41 {
		t.Errorf("the position reached the reader as %s", work.inboxQuery.Since)
	}
	if _, err := askAsSeat(t, viewerSources(t, work), "ana", "work_inbox",
		map[string]any{"since": "41"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a bare sequence = %v, want bad_params", err)
	}
}

// MY WORK, MY INBOX AND MY PERSON RECORD ARE THE CALLER'S WHEN THEY NAME NOBODY.
//
// `viewer=` is gone from the query surface: the viewer is the caller's own
// seat, and every personal question that names no handle is about them. One
// table over the three, because they are one rule — and the one that went
// untested was the third: `work_person` defaulted through the same function
// the other two do, and nothing said so.
//
// Each is asked as a caller holding every grant, so the answer is whose record
// was READ rather than whether the read was allowed; the handle a stub was
// asked for is the whole assertion. The control is naming somebody else, which
// must reach them — a default that ignored the argument would pass the first
// half and fail this.
func TestMyWorkAndPeopleDefaultToTheCaller(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		what  string
		asked func(*stubWork) string
	}{
		{"work_my_work", func(w *stubWork) string { return w.myWorkQuery.Handle }},
		{"work_inbox", func(w *stubWork) string { return w.inboxQuery.Handle }},
		{"work_person", func(w *stubWork) string { return w.personQuery.Handle }},
	} {
		t.Run(c.what, func(t *testing.T) {
			t.Parallel()
			work := &stubWork{}
			if _, err := askAsSeat(t, viewerSources(t, work), "ana", c.what, nil); err != nil {
				t.Fatalf("%s with no handle: %v", c.what, err)
			}
			if got := c.asked(work); got != "ana" {
				t.Errorf("%s with no handle read %q, want the caller's own seat",
					c.what, got)
			}
			if _, err := askAsSeat(t, viewerSources(t, work), "ana", c.what,
				map[string]any{"handle": "bo"}); err != nil {
				t.Fatalf("%s naming bo: %v", c.what, err)
			}
			if got := c.asked(work); got != "bo" {
				t.Errorf("%s naming bo read %q — the default is overriding the "+
					"argument", c.what, got)
			}
		})
	}
}
