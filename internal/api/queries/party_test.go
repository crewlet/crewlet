// A person is one party with two identities, and the surface is what resolves
// the second one.

package queries_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/tracker"
)

// partyCompany links two keys to two seats and leaves a third seat linked to
// nobody, all three under Ana — which is what makes "somebody else's alias" a
// real case rather than the caller's own key read back, and lets Ana read the
// other two at all (a person reads their own record and their line's).
const partyCompany = `
name: Acme
roles:
  - name: Ana Diaz
    handle: ana
    kind: human
    manages: [Cy Ward, Bo Lang]
    contact:
      crewlet_operator_id: ops-1
  - name: Cy Ward
    handle: cy
    kind: human
    contact:
      crewlet_operator_id: ops-2
  - name: Bo Lang
    handle: bo
    kind: human
    contact:
      slack_user_id: U0COLLEAGUE
`

func partySources(t *testing.T, work *stubWork) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(partyCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return queries.Sources{
		Company: func() *config.Company { return cfg },
		Work:    work,
	}
}

// want asserts one resolved party, naming both halves in the failure because
// either one alone is a personal answer that is quietly missing half a
// person's work.
func wantParty(t *testing.T, got tracker.Party, handle, operatorID string) {
	t.Helper()
	if got.Handle != handle || got.OperatorID != operatorID {
		t.Errorf("the reader was asked about %+v, want handle %q with the "+
			"operator id %q bound to it", got, handle, operatorID)
	}
}

// THE PERSONAL QUESTIONS CARRY BOTH OF THE CALLER'S NAMES.
//
// A write made through somebody's own credential is attributed to the TOKEN,
// deliberately — a tracker whose author field is chosen by the writer is not
// an audit trail — so the rows a founder's own assistant filed name `ops-1`
// while the seat the dashboard answers for is `ana`. All three personal
// questions read row-attribution, so all three have to be handed both names;
// one of them fixed and two left looks, from the screen, exactly like none of
// them being fixed.
func TestThePersonalQuestionsCarryBothOfTheCallersIdentities(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	sources := partySources(t, work)

	if _, err := askAsAna(t, sources, "work_my_work", nil); err != nil {
		t.Fatalf("work_my_work: %v", err)
	}
	wantParty(t, work.myWorkQuery.Who, "ana", "ops-1")

	if _, err := askAsAna(t, sources, "work_inbox", nil); err != nil {
		t.Fatalf("work_inbox: %v", err)
	}
	wantParty(t, work.inboxQuery.Who, "ana", "ops-1")

	if _, err := askAsAna(t, sources, "work_person", nil); err != nil {
		t.Fatalf("work_person: %v", err)
	}
	wantParty(t, work.personQuery.Who, "ana", "ops-1")
}

// THE ALIAS BELONGS TO THE PERSON ASKED ABOUT, NOT TO THE CALLER.
//
// A lead reading a report's day must be handed THAT person's two names. Resolving the alias from the credential in the caller's own hand
// would answer about `cy` and then union in `ops-1`'s rows — which is the
// caller's own work appearing on somebody else's screen, and the one failure
// this seam could produce that is worse than the bug it fixes.
func TestTheAliasBelongsToThePersonAskedAbout(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAsAna(t, partySources(t, work), "work_my_work",
		map[string]any{"handle": "cy"}); err != nil {
		t.Fatalf("work_my_work for a colleague: %v", err)
	}
	wantParty(t, work.myWorkQuery.Who, "cy", "ops-2")
}

// AND SO DOES THE VIEW STRIP, whose personalisation is the same person's.
//
// A saved view is OWNED by whoever wrote it and a pin lives on their own
// record; both verbs exist only on the operator surface, so both carry the
// credential's name. Asked for under the seat's, a founder's own strip came
// back with neither. Naming nobody is still the SHARED strip — a real answer
// the sidebar and the board poll for, not a refusal.
func TestTheViewStripCarriesBothIdentitiesAndDegradesToShared(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	sources := partySources(t, work)

	if _, err := askAsAna(t, sources, "work_views",
		map[string]any{"container": "workspace", "viewer": "ana"}); err != nil {
		t.Fatalf("work_views: %v", err)
	}
	wantParty(t, work.views.Viewer, "ana", "ops-1")

	work = &stubWork{}
	if _, err := askNative(t, partySources(t, work), "work_views",
		map[string]any{"container": "workspace"}); err != nil {
		t.Fatalf("the shared strip: %v", err)
	}
	if work.views.Viewer.Named() {
		t.Errorf("an anonymous strip named %+v, want the shared one",
			work.views.Viewer)
	}
}

// A SEAT NOBODY'S TOKEN IS BOUND TO CARRIES NO ALIAS, which is the ordinary
// state of every agent seat and of every human who does not run the company.
// An empty alias must not reach the reader as a matchable identity — see
// [tracker.Party.Handles].
func TestASeatWithNoTokenCarriesNoAlias(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAsAna(t, partySources(t, work), "work_inbox",
		map[string]any{"handle": "bo"}); err != nil {
		t.Fatalf("work_inbox for an unbound seat: %v", err)
	}
	wantParty(t, work.inboxQuery.Who, "bo", "")
	if ids := work.inboxQuery.Who.Handles(); len(ids) != 1 || ids[0] != "bo" {
		t.Errorf("the identities are %v, want the handle alone", ids)
	}
}

// A HANDLE NO SEAT HOLDS IS IN NOBODY'S LINE, so it is refused like any other
// record outside the caller's line — to an admin key as to a member's
// (ADR-0031). Somebody who left, and a name still on old rows, belong to no
// lead the chart can name, and a key's role is not a line.
func TestAHandleNoSeatHoldsIsInNobodysLine(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAsAna(t, partySources(t, work), "work_my_work",
		map[string]any{"handle": "departed"}); !errors.Is(err, queries.ErrForbidden) {
		t.Fatalf("work_my_work for a handle nobody holds = %v, want forbidden", err)
	}
	if work.myWorkQuery.Who.Named() {
		t.Errorf("the reader was called with %+v anyway", work.myWorkQuery.Who)
	}
}

// THE SCOPE RULE SURVIVES THE NEW SHAPE.
//
// The party is resolved AFTER the authority check, so a caller naming somebody
// outside their line is still refused — an admin key no seat links included —
// and refused before the reader is called, rather than being handed a party
// it would happily answer.
func TestThePartyIsResolvedBehindTheScopeRule(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askNative(t, partySources(t, work), "work_my_work",
		map[string]any{"handle": "cy"}); !errors.Is(err, queries.ErrForbidden) {
		t.Fatalf("an unlinked admin's read of cy's day = %v, want forbidden", err)
	}
	if work.myWorkQuery.Who.Named() {
		t.Errorf("the reader was called with %+v anyway", work.myWorkQuery.Who)
	}
}
