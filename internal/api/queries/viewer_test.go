// Who the caller is, and what that entitles them to read.

package queries_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// viewerCompany links three keys to three people and leaves a fourth person
// linked to nobody, under ONE lead: Ana leads Bo, so Bo is in Ana's line and
// nobody is in Bo's — which is what makes "somebody in my line" and "somebody
// else" two real cases rather than a typo.
const viewerCompany = `
name: Acme
roles:
  - name: Ana Diaz
    handle: ana
    kind: human
    manages: [Bo Lang]
    contact:
      crewlet_operator_id: ops-1
  - name: Bo Lang
    handle: bo
    kind: human
    contact:
      slack_user_id: U0COLLEAGUE
      crewlet_operator_id: ops-2
  - name: Cy Moss
    handle: cy
    kind: human
    contact:
      crewlet_operator_id: ops-3
  - name: Dee Park
    handle: dee
    kind: human
    contact:
      slack_user_id: U0DEE
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

// viewerAs asks `viewer` of sources as caller.
func viewerAs(t *testing.T, sources queries.Sources, caller auth.Principal) map[string]any {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, sources)
	answered, err := r.Answer(t.Context(), "viewer", nil, caller)
	return answerMap(t, answered, err)
}

// THE ANSWER IS EXACTLY THE DOCUMENTED SHAPE, every field present for every
// caller: the dashboard's Viewer mirrors these keys, and one missing for one
// kind of caller is a screen reading `undefined` as "no".
func TestTheViewerAnswersEveryFieldForEveryCaller(t *testing.T) {
	t.Parallel()
	want := []string{"acts", "admins", "config_managed_by", "config_writer", "handle",
		"kind", "line", "linked", "name", "project", "reach", "role", "token_id"}
	for name, caller := range map[string]auth.Principal{
		"a linked member":   asMember("ops-1"),
		"an unlinked admin": asAdmin("ops-nobody"),
		"nobody":            nobody,
	} {
		got := viewerAs(t, viewerSources(t, &stubWork{}), caller)
		keys := slices.Sorted(maps.Keys(got))
		if !slices.Equal(keys, want) {
			t.Errorf("%s: the viewer answered %v, want exactly %v", name, keys, want)
		}
	}
}

// THE KEY RESOLVES TO A PERSON, AND THE PERSON TO THEIR LINE. Two steps that
// have both existed since the engine shipped — a key maps to an id, a seat
// names one in its contact block — and the chart says whose line is whose.
func TestTheViewerIsTheSeatTheKeyIsLinkedToAndTheirLine(t *testing.T) {
	t.Parallel()
	got := viewerAs(t, viewerSources(t, &stubWork{}), asMember("ops-1"))
	for field, want := range map[string]any{
		"token_id": "ops-1", "role": "member", "reach": "member", "linked": true,
		"handle": "ana", "name": "Ana Diaz", "kind": "human",
	} {
		if got[field] != want {
			t.Errorf("%s = %v, want %v", field, got[field], want)
		}
	}
	if line, ok := got["line"].([]string); !ok || !slices.Equal(line, []string{"bo"}) {
		t.Errorf("line = %#v, want [bo]: Ana leads Bo", got["line"])
	}
	// A PERSON WHO LEADS NOBODY HAS AN EMPTY LINE — an array, never null.
	bo := viewerAs(t, viewerSources(t, &stubWork{}), asAdmin("ops-2"))
	if line, ok := bo["line"].([]string); !ok || len(line) != 0 {
		t.Errorf("Bo's line = %#v, want an empty array", bo["line"])
	}
	if bo["role"] != "admin" || bo["reach"] != "admin" {
		t.Errorf("an admin key answered role %v reach %v", bo["role"], bo["reach"])
	}
}

// AN UNLINKED KEY IS AN ORDINARY STATE, not an error. The remedy is a line of
// company configuration, and a screen that renders an error cannot say so —
// which is why the key and its role are answered even when no seat links it.
func TestAnUnlinkedKeyAnswersItsIDAndRoleAndNoSeat(t *testing.T) {
	t.Parallel()
	got := viewerAs(t, viewerSources(t, &stubWork{}), asAdmin("ops-nobody"))
	if got["token_id"] != "ops-nobody" || got["role"] != "admin" || got["linked"] != false {
		t.Errorf("an unlinked admin key answered %v", got)
	}
	if got["handle"] != "" {
		t.Errorf("handle = %v, want none: no seat names ops-nobody", got["handle"])
	}
	if line, ok := got["line"].([]string); !ok || len(line) != 0 {
		t.Errorf("line = %#v, want an empty array", got["line"])
	}
}

// AND A CALLER WITH NO KEY IS A THIRD STATE. No key at all is neither an
// unlinked one nor a linked one, and the three take three different sentences
// on screen — so the answer has to keep them apart.
func TestACallerWithNoKeyIsNobody(t *testing.T) {
	t.Parallel()
	got := viewerAs(t, viewerSources(t, &stubWork{}), nobody)
	for field, want := range map[string]any{
		"token_id": "", "role": "", "reach": "public", "linked": false, "handle": "",
	} {
		if got[field] != want {
			t.Errorf("%s = %v, want %v", field, got[field], want)
		}
	}
}

// WHOM TO ASK is named to anybody holding a key — and to a stranger, nobody:
// the people who can open the engine are not the public face's to publish.
func TestTheAdminsAreNamedToAKeyHolderAndToNobodyElse(t *testing.T) {
	t.Parallel()
	sources := viewerSources(t, &stubWork{})
	sources.Access = &queries.AccessPosture{Keys: []queries.AccessKey{
		{ID: "ops-1", Role: config.RoleMember},
		{ID: "ops-2", Role: config.RoleAdmin},
		{ID: "ops-3", Role: config.RoleAdmin},
		{ID: "ci", Role: config.RoleAdmin},
	}}
	want := []queries.ViewerAdmin{{Handle: "bo", Name: "Bo Lang"}, {Handle: "cy", Name: "Cy Moss"}}
	got := viewerAs(t, sources, asMember("ops-1"))
	if admins, ok := got["admins"].([]queries.ViewerAdmin); !ok || !slices.Equal(admins, want) {
		t.Errorf("a member is told the admins are %#v, want %v — the linked admin "+
			"keys' people in handle order, and never an unlinked key", got["admins"], want)
	}
	stranger := viewerAs(t, sources, nobody)
	if admins, ok := stranger["admins"].([]queries.ViewerAdmin); !ok || len(admins) != 0 {
		t.Errorf("a stranger is told the admins are %#v, want an empty array", stranger["admins"])
	}
}

// A DISABLED GUARD IS NEVER A PERSON. With `api.auth.disabled` every caller
// is stamped with the reserved id, so a seat bound to that id would hand
// whoever reaches the engine that person's dashboard — their inbox, their
// queue, their name on every write. The literal is refused where the company
// is read, naming the field; a `${VAR}` cannot be, because its value lives in
// an environment validation may not see, so the resolution drops it instead —
// here through the lookup a node hands its sources ([queries.Sources.Env]),
// which is what every consumer of the binding reads.
func TestADisabledGuardIsNeverAPerson(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.API.Auth.Disabled = true
	caller, ok := auth.New(&b).Principal("")
	if !ok || caller.ID != org.ReservedOperatorID {
		t.Fatalf("a disabled guard answered %+v/%v, want the reserved id", caller, ok)
	}

	literal := strings.Replace(viewerCompany, "crewlet_operator_id: ops-1",
		"crewlet_operator_id: "+caller.ID, 1)
	_, err := config.ParseCompany([]byte(literal))
	if !errors.Is(err, org.ErrReservedOperatorID) {
		t.Fatalf("a seat bound to %q parsed with %v, want it refused", caller.ID, err)
	}
	if !strings.Contains(err.Error(), "crewlet_operator_id") {
		t.Errorf("the refusal does not name the field: %v", err)
	}

	cfg, err := config.ParseCompany([]byte(strings.Replace(viewerCompany,
		"crewlet_operator_id: ops-1",
		"crewlet_operator_id: ${CREWLET_TEST_BOUND_OPERATOR}", 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := viewerAs(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		Env:     handedOnly(t, "CREWLET_TEST_BOUND_OPERATOR", "Anonymous"),
		Work:    &stubWork{},
	}, caller)
	if got["handle"] != "" || got["linked"] != false {
		t.Errorf("the disabled guard's caller is seat %v; a reference resolving "+
			"to the reserved id must bind nobody", got["handle"])
	}
	if got["token_id"] != caller.ID || got["reach"] != "admin" {
		t.Errorf("token_id = %v reach = %v, want the reserved id the guard stamped, as an admin",
			got["token_id"], got["reach"])
	}
}

// THE VIEWER NAMES WHAT IT MAY ACT ON, and only a person may act on anything.
// The act transport admits a key linked to a seat and nobody else
// (ADR-0024), so the list a screen enables its controls from is that
// transport's own for a linked key and EMPTY for everybody else — an array
// either way, because "may do nothing" is a value a screen tests, not an
// absence it has to guess the meaning of.
func TestTheViewerNamesWhatItMayAct(t *testing.T) {
	t.Parallel()
	served := []string{"create_work_item", "mark_inbox"}
	sources := viewerSources(t, &stubWork{})
	sources.OperatorActs = func() []string { return served }
	for _, tc := range []struct {
		name   string
		caller auth.Principal
		want   []string
	}{
		{"a linked member key", asMember("ops-1"), served},
		{"a linked admin key", asAdmin("ops-1"), served},
		{"an unlinked key", asAdmin("ops-nobody"), []string{}},
		{"a caller with no key", nobody, []string{}},
		{"a disabled guard's caller", asAdmin(org.ReservedOperatorID), []string{}},
	} {
		acts, ok := viewerAs(t, sources, tc.caller)["acts"].([]string)
		if !ok {
			t.Errorf("%s: acts is not an array", tc.name)
			continue
		}
		if !slices.Equal(acts, tc.want) {
			t.Errorf("%s: acts = %v, want %v", tc.name, acts, tc.want)
		}
	}
	// AND A NODE WITH NOTHING TO SERVE ANSWERS AN EMPTY LIST to a linked
	// person too, rather than a null a screen would read as "not loaded".
	if acts, ok := viewerAs(t, viewerSources(t, &stubWork{}), asMember("ops-1"))["acts"].([]string); !ok || len(acts) != 0 {
		t.Errorf("a linked person on a node serving no acts got %#v", acts)
	}
}

// THE PERSONAL QUESTIONS DEFAULT TO THE CALLER'S OWN SEAT.
//
// `work_my_work` was once registered for operators only and demanded a handle,
// which made the one screen a human teammate would live on both unreachable to
// them and, for an operator, a stranger's day.
func TestAPersonalQuestionDefaultsToTheCallersOwnSeat(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAs(t, viewerSources(t, work), "work_my_work", nil, asMember("ops-1")); err != nil {
		t.Fatalf("work_my_work with no handle: %v", err)
	}
	if work.myWorkQuery.Who.Handle != "ana" {
		t.Errorf("read %q's day, want the caller's own seat", work.myWorkQuery.Who.Handle)
	}
}

// A PERSON READS THEIR OWN DAY AND THEIR LINE'S, AND NOBODY ELSE'S — and an
// admin key adds nothing to that (ADR-0031): the role is how much of the
// machine a key reaches, and a person's day is not the machine's. A lead
// reading a report's day is a real thing to do, so the rule is not "handles
// other than your own are refused"; a teammate reading their lead's, or an
// admin who leads nobody reading anybody's, is refused FORBIDDEN, because a
// different key would not put them in that line.
func TestAPersonReadsTheirLinesDayAndNobodyElses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		caller auth.Principal
		handle string
		served bool
	}{
		{"a member reading a report's day", asMember("ops-1"), "bo", true},
		{"a member reading their lead's day", asMember("ops-2"), "ana", false},
		{"a member reading a peer outside their line", asMember("ops-3"), "bo", false},
		{"an admin who leads nobody, reading a person's day", asAdmin("ops-3"), "bo", false},
		{"an admin no seat links, reading a person's day", asAdmin("ops"), "bo", false},
	} {
		work := &stubWork{}
		_, err := askAs(t, viewerSources(t, work), "work_my_work",
			map[string]any{"handle": tc.handle}, tc.caller)
		switch {
		case tc.served && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.served && work.myWorkQuery.Who.Handle != tc.handle:
			t.Errorf("%s: read %q's day, want %q", tc.name, work.myWorkQuery.Who.Handle, tc.handle)
		case !tc.served && !errors.Is(err, queries.ErrForbidden):
			t.Errorf("%s = %v, want forbidden", tc.name, err)
		case !tc.served && work.myWorkQuery.Who.Named():
			t.Errorf("%s: the reader was called with %+v anyway", tc.name, work.myWorkQuery.Who)
		}
	}
}

// A CALLER NOBODY IS LINKED TO IS REFUSED FOR PARAMETERS, NOT FOR AUTHORITY.
// Nobody was denied anything: there is no person to answer about, and telling
// somebody to present a different key is the wrong remedy for a company that
// has not linked theirs.
func TestAnUnlinkableCallerIsRefusedForWantOfAHandle(t *testing.T) {
	t.Parallel()
	_, err := askNative(t, viewerSources(t, &stubWork{}), "work_my_work", nil)
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("no linked key and no handle = %v, want bad_params", err)
	}
	if errors.Is(err, queries.ErrUnauthorized) || errors.Is(err, queries.ErrForbidden) {
		t.Error("refused for authority; the remedy is configuration, not a key")
	}
}

// THE INBOX IS ONE PERSON'S, on the same rule.
func TestTheInboxIsScopedTheSameWay(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAs(t, viewerSources(t, work), "work_inbox", nil, asMember("ops-1")); err != nil {
		t.Fatalf("work_inbox with no handle: %v", err)
	}
	if work.inboxQuery.Who.Handle != "ana" {
		t.Errorf("read %q's inbox, want the caller's own", work.inboxQuery.Who.Handle)
	}
	if _, err := askAs(t, viewerSources(t, work), "work_inbox",
		map[string]any{"handle": "ana"}, asAdmin("ops-2")); !errors.Is(err, queries.ErrForbidden) {
		t.Errorf("a report's admin key reading their lead's inbox = %v, want forbidden", err)
	}
}

// EVERY INBOX FILTER REACHES THE READER. A filter the surface accepts and
// drops is a page showing more than the person asked for, silently — the same
// failure the board's own parameter sweep exists to catch.
func TestEveryInboxFilterReachesTheReader(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	_, err := askAsAna(t, viewerSources(t, work), "work_inbox", map[string]any{
		"unread":       true,
		"primary_only": true,
		"snoozed":      "only",
		"reasons":      "mention,asked",
		"limit":        7,
		"cursor":       "c-9",
	})
	if err != nil {
		t.Fatalf("work_inbox: %v", err)
	}
	q := work.inboxQuery
	if !q.Unread || !q.PrimaryOnly || q.Snoozed != tracker.SnoozeOnly {
		t.Errorf("the three filters reached the reader as %+v", q)
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
	_, err := askAsAna(t, viewerSources(t, &stubWork{}), "work_inbox",
		map[string]any{"reasons": "shouted_at"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("an unknown reason = %v, want bad_params", err)
	}
	if !strings.Contains(err.Error(), "mention") {
		t.Errorf("the refusal %q does not name what would have worked", err)
	}
}

// THE SNOOZED SCOPE DEFAULTS TO HIDING THEM, AND A SCOPE THIS BUILD DOES NOT
// KNOW IS REFUSED. The reader refuses the zero value, so the surface is what
// makes "my inbox" mean "not what I put off"; and a misspelt scope answered as
// the default would be the very defect the scope replaced — a Snoozed tab
// listing the whole inbox.
func TestTheSnoozedScopeDefaultsToExcludeAndRefusesAnUnknownOne(t *testing.T) {
	t.Parallel()
	work := &stubWork{}
	if _, err := askAsAna(t, viewerSources(t, work), "work_inbox",
		map[string]any{}); err != nil {
		t.Fatalf("work_inbox: %v", err)
	}
	if work.inboxQuery.Snoozed != tracker.SnoozeExclude {
		t.Errorf("an absent `snoozed` reached the reader as %q, want %q",
			work.inboxQuery.Snoozed, tracker.SnoozeExclude)
	}
	_, err := askAsAna(t, viewerSources(t, &stubWork{}), "work_inbox",
		map[string]any{"snoozed": "bogus"})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("snoozed=bogus = %v, want bad_params", err)
	}
	for _, scope := range []string{"exclude", "include", "only"} {
		if !strings.Contains(err.Error(), scope) {
			t.Errorf("the refusal %q does not name %q", err, scope)
		}
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
	if _, err := askAsAna(t, viewerSources(t, work), "work_inbox",
		map[string]any{"since": "CREWLET_TRACKER_LOG@2:41"}); err != nil {
		t.Fatalf("a whole position: %v", err)
	}
	if work.inboxQuery.Since.Generation != 2 || work.inboxQuery.Since.Seq != 41 {
		t.Errorf("the position reached the reader as %s", work.inboxQuery.Since)
	}
	if _, err := askAsAna(t, viewerSources(t, work), "work_inbox",
		map[string]any{"since": "41"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a bare sequence = %v, want bad_params", err)
	}
}

// THE VIEWER SAYS WHERE THEIR CREATE LANDS, from the chart the answer was
// read from and through the engine's one derivation (`engine.ProjectOfSeat`,
// the seat's own, else its team's, else the nearest ancestor's) — the project
// `create_work_item` files a person's work into when it names none — so
// "Create task" can say where rather than a screen working out a second
// answer. The bound seat's, and "" for a caller with no seat or a seat whose
// teams own none.
func TestTheViewerSaysWhereTheirCreateLands(t *testing.T) {
	t.Parallel()
	const company = `
name: Acme
units:
  - name: Engineering
    type: department
    project: ENG
    children:
      - name: Platform
        type: team
        roles:
          - name: Ana Diaz
            handle: ana
            kind: human
            contact:
              crewlet_operator_id: ops-1
roles:
  - name: Bo Lang
    handle: bo
    kind: human
    contact:
      crewlet_operator_id: ops-2
`
	cfg, err := config.ParseCompany([]byte(company))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := queries.Sources{Company: func() *config.Company { return cfg }, Work: &stubWork{}}

	if got := viewerAs(t, s, asMember("ops-1")); got["project"] != "ENG" {
		t.Errorf("project = %v, want ENG: Ana's team owns none, so her department's", got["project"])
	}

	// A SEAT WITH NO PROJECT ANYWHERE ABOVE IT answers "", the value a
	// create must fill in — never a project borrowed from somebody else.
	if got := viewerAs(t, s, asMember("ops-2")); got["project"] != "" {
		t.Errorf("a seat no team holds: project = %v, want \"\"", got["project"])
	}
	if got := viewerAs(t, s, nobody); got["project"] != "" {
		t.Errorf("a caller with no key: project = %v, want \"\"", got["project"])
	}
}

// WHO MAY CHANGE THE COMPANY DOCUMENT is on the viewer, so the dashboard
// draws a managed document read-only rather than offering saves the config
// surface refuses. It takes an ADMIN key (ADR-0031), and company_writers
// narrows which admin keys when the document is managed (ADR-0030) — and the
// writers are named to an admin only.
func TestTheViewerSaysWhetherItMayChangeTheCompany(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		writers   []string
		caller    auth.Principal
		writer    bool
		managedBy []string
	}{
		"an unmanaged document is every admin's": {nil, asAdmin("ops-1"), true, []string{}},
		"a listed writer may":                    {[]string{"ops-1"}, asAdmin("ops-1"), true, []string{"ops-1"}},
		"any other admin may not, and is told":   {[]string{"gitops"}, asAdmin("ops-1"), false, []string{"gitops"}},
		"a member may not, unmanaged or not": {nil, asMember("ops-2"), false,
			[]string{}},
		"a member is not told who writes it": {[]string{"gitops"}, asMember("ops-2"), false,
			[]string{}},
		"a caller with no key is told nothing": {[]string{"gitops"}, nobody, false, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := viewerSources(t, &stubWork{})
			s.Access = &queries.AccessPosture{Keys: []queries.AccessKey{
				{ID: "ops-1", Role: config.RoleAdmin},
				{ID: "ops-2", Role: config.RoleMember},
				{ID: "gitops", Role: config.RoleAdmin},
			}, CompanyWriters: tc.writers}
			got := viewerAs(t, s, tc.caller)
			if got["config_writer"] != tc.writer {
				t.Errorf("config_writer = %v, want %v", got["config_writer"], tc.writer)
			}
			managedBy, ok := got["config_managed_by"].([]string)
			if !ok || !slices.Equal(managedBy, tc.managedBy) {
				t.Errorf("config_managed_by = %#v, want %v", got["config_managed_by"], tc.managedBy)
			}
		})
	}
}
