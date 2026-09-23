// Who the caller is, and what that entitles them to read.

package queries_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

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
// binding in the identity directory, and a screen that renders an error cannot
// say so — which is why the login is answered even when no seat holds it.
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
// and, for an operator, a stranger's day. The rule that replaces it: your own
// seat when you name none, and anybody else's by the authority table's
// owner-or-lead rule.
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
	// AND THE REFUSAL IS THE DECISION'S OWN: the rule's reason and the
	// grant that would have admitted the caller, read off the decision
	// rather than written beside it. The literal this replaced named an
	// admin grant in a sentence nothing held against the table.
	var refusal *queries.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("refused with %T (%v), want the decision carried whole", err, err)
	}
	if refusal.Reason != authz.ReasonNotSelf {
		t.Errorf("reason = %q, want the owner-or-lead rule's own", refusal.Reason)
	}
	if !slices.Equal(refusal.Grants, []iam.Grant{iam.GrantFleetOperate}) {
		t.Errorf("grants = %v, want the admin grant that would have admitted it",
			refusal.Grants)
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

// EACH PERSONAL QUESTION ASKS ITS OWN VERB, rather than all of them asking
// whether the caller may read a person record.
//
// They are the same class today, so the answers agree; what a shared verb
// would cost is the day they stop agreeing. `conversations` is the case that
// proved it: it asked the person-record verb for a seat's audit trail, and an
// auditor holding the grant that governs that trail was refused it — see
// TestASeatsThreadsAreReadOnTheAuditGrant.
func TestEachPersonalQuestionAsksItsOwnVerb(t *testing.T) {
	t.Parallel()
	for what, verb := range map[string]authz.Action{
		"work_my_work": authz.ActionMyWork,
		"work_inbox":   authz.ActionInboxRead,
		"work_person":  authz.ActionPersonRead,
	} {
		// The question's own grant and nothing else, against a chart
		// reporting no relation: admitted to ASK, refused the record —
		// so the refusal is the verb's rather than the registration's.
		_, err := askHolding(t, viewerSources(t, &stubWork{}), "ana", what,
			map[string]any{"handle": "bo"}, iam.GrantStateRead)
		var refusal *queries.Refusal
		if !errors.As(err, &refusal) {
			t.Errorf("%s naming bo = %v, want a refusal", what, err)
			continue
		}
		if !strings.HasPrefix(refusal.What, string(verb)+" ") {
			t.Errorf("%s was decided as %q, want its own verb %s", what,
				refusal.What, verb)
		}
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

// A CALLER'S OWN RECORD IS READ UNDER THE NAME IT IS WRITTEN UNDER.
//
// The tools write a caller's inbox marks, pins, priorities and personal views
// under [iam.RecordOwner] — a bound person's seat, an unbound person's login,
// a token's login — and every personal question here read them back under the
// principal's SEAT: the same name for a bound person, and nothing at all for
// the other two, who were refused "not bound to a seat" for the record their
// own assistant had just written. The three shapes of caller, over every
// personal question and the strip, each read under exactly one name.
func TestACallersOwnRecordIsReadUnderTheNameItIsWrittenUnder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		caller iam.Principal
		want   string
	}{
		{"a bound person", iam.Principal{Kind: iam.KindPerson,
			Login: "ana.diaz", Seat: "ana"}, "ana"},
		{"an unbound person", iam.Principal{Kind: iam.KindPerson,
			Login: "jane.doe"}, "jane.doe"},
		{"an unbound token", iam.Principal{Kind: iam.KindMachine,
			Login: "token:ops"}, "token:ops"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.caller.ID, c.caller.Stage = uuid.New(), iam.StageActive
			c.caller.Grants = []iam.Grant{iam.GrantStateRead}
			ctx := iam.WithPrincipal(t.Context(), c.caller)
			if want := iam.ActorFor(c.caller).Name; want != c.want {
				t.Fatalf("this caller writes under %q; the case wants %q", want, c.want)
			}
			work := &stubWork{}
			r := queries.NewRegistry()
			queries.Register(r, viewerSources(t, work))
			for _, what := range []string{"work_my_work", "work_inbox", "work_person"} {
				if _, err := r.Answer(ctx, what, nil); err != nil {
					t.Fatalf("%s naming nobody: %v", what, err)
				}
			}
			// AND NAMING THEMSELVES BY THEIR LOGIN IS STILL THEM: a
			// bound person's login is theirs, and answering it as a
			// colleague's name would read a record nothing writes.
			if _, err := r.Answer(ctx, "work_inbox",
				map[string]any{"handle": c.caller.Login}); err != nil {
				t.Fatalf("work_inbox naming their own login: %v", err)
			}
			if _, err := r.Answer(ctx, "work_views",
				map[string]any{"container": "workspace"}); err != nil {
				t.Fatalf("work_views: %v", err)
			}
			if _, err := r.Answer(ctx, "work_items",
				map[string]any{"preset": "priorities"}); err != nil {
				t.Fatalf("work_items: %v", err)
			}
			viewer, err := r.Answer(ctx, "viewer", nil)
			got := answerMap(t, viewer, err)
			for question, read := range map[string]string{
				"work_my_work": work.myWorkQuery.Handle,
				"work_inbox":   work.inboxQuery.Handle,
				"work_person":  work.personQuery.Handle,
				"work_views":   work.views.Viewer,
				"work_items":   work.expandViewer.Handle,
				"viewer.owner": got["owner"].(string),
			} {
				if read != c.want {
					t.Errorf("%s read %q's record, want %q — the name its "+
						"own writes are made under", question, read, c.want)
				}
			}
		})
	}
}

// loginDirectory is the identity directory as a personal question reads it:
// `ana.diaz` holds the seat `ana`, `bo.smith` holds no seat, `dev.person` holds
// `dev`, and nobody else holds anything. err makes every lookup unknown.
type loginDirectory struct{ err error }

func (d loginDirectory) HolderRecord(_ context.Context, login string) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	switch login {
	case "ana.diaz":
		return "ana", nil
	case "bo.smith":
		return "bo.smith", nil
	case "dev.person":
		return "dev", nil
	}
	return "", fmt.Errorf("%w: %s", iam.ErrNoHolder, login)
}

// SOMEBODY ELSE'S LOGIN IS READ UNDER THEIR HOLDER'S RECORD, and decided on it.
//
// A question naming another person's login read the LITERAL login: an
// administrator asking for `ana.diaz`, whom the directory binds to the seat
// `ana`, read an empty inbox under the login while every notice of hers is
// kept under `ana` — and a lead asking by login was refused as leading nobody,
// because the lead relation is a fact about a seat. The name resolves through
// the one owner function the tools write through, and is decided on what it
// resolved to.
func TestSomebodyElsesLoginIsReadUnderTheirHoldersRecord(t *testing.T) {
	t.Parallel()
	admin := iam.Principal{ID: uuid.New(), Kind: iam.KindMachine, Login: "token:ops",
		Stage:  iam.StageActive,
		Grants: []iam.Grant{iam.GrantStateRead, iam.GrantFleetOperate}}
	lead := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Login: "lead.person",
		Seat: "lead", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantStateRead}}
	for _, c := range []struct {
		name   string
		caller iam.Principal
		asked  string
		want   string
	}{
		{"an admin asking for a bound person by login", admin, "ana.diaz", "ana"},
		{"an admin asking for an unbound person by login", admin, "bo.smith", "bo.smith"},
		{"a lead asking for their report by login", lead, "dev.person", "dev"},
	} {
		for _, question := range []string{"work_my_work", "work_inbox", "work_person"} {
			t.Run(c.name+"/"+question, func(t *testing.T) {
				t.Parallel()
				work := &stubWork{}
				s := viewerSources(t, work)
				s.Holders = loginDirectory{}
				s.Chart = leadsChart{lead: "lead", report: "dev"}
				r := queries.NewRegistry()
				queries.Register(r, s)
				if _, err := r.Answer(iam.WithPrincipal(t.Context(), c.caller),
					question, map[string]any{"handle": c.asked}); err != nil {
					t.Fatalf("%s for %q: %v", question, c.asked, err)
				}
				read := map[string]string{
					"work_my_work": work.myWorkQuery.Handle,
					"work_inbox":   work.inboxQuery.Handle,
					"work_person":  work.personQuery.Handle,
				}[question]
				if read != c.want {
					t.Errorf("%s for %q read %q's record, want %q", question,
						c.asked, read, c.want)
				}
			})
		}
	}
}

// A LOGIN THAT NAMES NOBODY IS NOT A ROSTER, and a directory that cannot say
// is 503 rather than "nobody".
//
// "Not found" before "you may not" would tell a caller with no authority over
// anybody which logins exist, so a login nobody holds is decided on the name
// as typed first — which nobody leads — and only the admin grant learns that
// it names no record.
func TestALoginThatNamesNobodyIsNotARosterForAQuestion(t *testing.T) {
	t.Parallel()
	admin := iam.Principal{ID: uuid.New(), Kind: iam.KindMachine, Login: "token:ops",
		Stage:  iam.StageActive,
		Grants: []iam.Grant{iam.GrantStateRead, iam.GrantFleetOperate}}
	lead := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Login: "lead.person",
		Seat: "lead", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantStateRead}}
	for _, c := range []struct {
		name   string
		caller iam.Principal
		dir    loginDirectory
		want   error
	}{
		{"an admin", admin, loginDirectory{}, queries.ErrNotFound},
		{"a caller with no authority over anybody", lead, loginDirectory{},
			queries.ErrUnauthorized},
		{"a directory this node cannot read", admin,
			loginDirectory{err: errors.New("the identity applier is behind")},
			queries.ErrUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			work := &stubWork{}
			s := viewerSources(t, work)
			s.Holders = c.dir
			r := queries.NewRegistry()
			queries.Register(r, s)
			_, err := r.Answer(iam.WithPrincipal(t.Context(), c.caller), "work_inbox",
				map[string]any{"handle": "ghost.person"})
			if !errors.Is(err, c.want) {
				t.Fatalf("work_inbox for a login nobody holds = %v, want %v", err, c.want)
			}
			if c.want == queries.ErrUnauthorized && errors.Is(err, queries.ErrNotFound) {
				t.Error("a refused caller was told the login names nobody")
			}
			if work.inboxQuery.Handle != "" {
				t.Errorf("the inbox was read under %q", work.inboxQuery.Handle)
			}
		})
	}
}

// A SEAT'S TRAIL IS STILL THE SEAT'S. The threads a seat said things in are
// the seat's, not a person record, so a caller bound to no seat who names none
// is refused for want of a handle — and the refusal names the remedy where it
// lives: a row in the identity directory, bound with `crewlet iam bind`,
// because the org chart it used to point at has no field that binds a
// credential any more.
func TestAnUnbindableCallerHasNoSeatTrailOfItsOwn(t *testing.T) {
	t.Parallel()
	s := viewerSources(t, &stubWork{})
	s.Conversations = &stubConversations{}
	r := queries.NewRegistry()
	queries.Register(r, s)
	_, err := r.Answer(everyGrant(t), "conversations", nil)
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("a seatless caller naming no seat = %v, want bad_params", err)
	}
	if errors.Is(err, queries.ErrUnauthorized) {
		t.Error("refused as unauthorized; the remedy is a binding, not a credential")
	}
	if msg := err.Error(); !strings.Contains(msg, "crewlet iam bind") ||
		strings.Contains(msg, "org chart") {
		t.Errorf("refusal = %q, want it to name `crewlet iam bind` and not the "+
			"org chart, which no longer binds a credential", msg)
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
