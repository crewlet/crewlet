package authz_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY ROW STATES HOW RECENT A PROOF IT ASKS FOR.
//
// The zero recency is refused rather than read as "none", because read as none
// it is a sensitive verb somebody added without deciding — shipped open to a
// session proved last week, and indistinguishable in the table from a verb
// that was decided to need nothing.
func TestEveryRowStatesHowRecentAProofItAsksFor(t *testing.T) {
	t.Parallel()
	for _, a := range authz.Actions() {
		r, ok := authz.RecencyOf(a)
		if !ok {
			t.Errorf("%s is listed and RecencyOf does not know it", a)
			continue
		}
		if !r.Valid() {
			t.Errorf("%s states the recency %q, which is not one of %v — every "+
				"row decides whether it needs a step-up", a, r, iam.Recencies)
		}
	}
	if _, ok := authz.RecencyOf("no.such.verb"); ok {
		t.Error("RecencyOf knows a verb the table has no row for")
	}
}

// A SELF ARM'S OWN WINDOW NAMES A REAL ONE, ON A ROW THAT HAS A SELF ARM.
//
// The second window is read off the decision's reason, so one stated on a row
// whose class never admits anybody as themselves is a window no request can
// ever be asked — a rule somebody believes in and nothing enforces. Held
// BEHAVIOURALLY rather than against a list of classes: the row must admit
// somebody as themselves, over an object naming them by id or by login.
func TestASelfArmsOwnWindowIsARealOneOnARowThatHasOne(t *testing.T) {
	t.Parallel()
	for _, a := range authz.Actions() {
		own, _ := authz.SelfRecencyOf(a)
		if own == "" {
			continue
		}
		if !own.Valid() {
			t.Errorf("%s states the self arm's window %q, which is not one of "+
				"%v", a, own, iam.Recencies)
		}
		p := person("jane.doe", iam.AllGrants...)
		self := false
		for _, owner := range []string{p.ID.String(), p.Login} {
			d := authz.Decide(t.Context(), p, a, authz.Object{
				Kind: authz.KindPerson, Owner: owner, Author: owner,
			}, nimbus(), decidedAt)
			self = self || d.Allowed && d.Reason == authz.ReasonSelf
		}
		if !self {
			t.Errorf("%s states a window for its self arm and admits nobody "+
				"as themselves, so the window is asked of no request", a)
		}
	}
}

// ENDING EVERY SESSION IN THE COMPANY TAKES BOTH HATS.
//
// The deployment's grant runs a restore and the directory's decides who may
// act; this gesture is both at once — it ends every person's authority and
// every machine token in one record — so it asks for both, and
// internal/iamdomain's record holds the same rule. A refusal names what the
// caller LACKS, which is the only thing that would change the answer: an SRE
// holding the deployment's grant is told the directory's is missing. And it
// asks the sensitive window of a caller holding both.
//
// Mutation: admit on the deployment's grant alone and the SRE is let through;
// name both grants on every refusal and the SRE is told to ask for one they
// hold.
func TestEndingEverySessionInTheCompanyTakesBothHats(t *testing.T) {
	t.Parallel()
	decide := func(p iam.Principal) authz.Decision {
		return authz.Decide(t.Context(), p, authz.ActionSessionInvalidate,
			authz.Object{Kind: authz.KindCompany}, authz.NoChart{}, decidedAt)
	}
	for _, c := range []struct {
		name    string
		holds   []iam.Grant
		missing []iam.Grant
	}{
		{"the deployment's grant alone", []iam.Grant{iam.GrantFleetOperate},
			[]iam.Grant{iam.GrantPeopleManage}},
		{"the directory's grant alone", []iam.Grant{iam.GrantPeopleManage},
			[]iam.Grant{iam.GrantFleetOperate}},
		{"neither", nil, []iam.Grant{iam.GrantFleetOperate, iam.GrantPeopleManage}},
	} {
		d := decide(person("sam.sre", c.holds...))
		if d.Allowed || d.Reason != authz.ReasonNoGrant ||
			!slices.Equal(d.Grants, c.missing) {
			t.Errorf("%s decided %+v, want refused naming exactly %v", c.name,
				d, c.missing)
		}
	}
	both := person("ana.admin", iam.GrantFleetOperate, iam.GrantPeopleManage)
	if d := decide(both); !d.Allowed || d.Reason != authz.ReasonGrant {
		t.Errorf("both grants decided %+v, want admitted on the grant", d)
	}
	proof := decidedAt.Add(-40 * time.Minute)
	both.ReauthAt, both.SensitiveReauthAt = proof.Add(time.Hour), proof.Add(15*time.Minute)
	if d := decide(both); d.Allowed || d.Reason != authz.ReasonStepUp ||
		d.Recency != iam.RecencySensitive {
		t.Errorf("both grants on a proof forty minutes old decided %+v, want a "+
			"step-up naming %s", d, iam.RecencySensitive)
	}
}

// A SENSITIVE GESTURE ABOUT ANYBODY BUT THE CALLER NEEDS A GRANT NO MACHINE
// TOKEN CAN CARRY.
//
// The sensitive window's safety does not rest on who has proved what alone: a
// Tier A token is fresh in it by construction, because break-glass must reach
// a sensitive gesture on the day the provider is down. So every way into one
// that is not the caller acting on their OWN record asks for a grant that
// needs a person present ([iam.PersonPresentGrants]) — which internal/iamdomain
// refuses at a machine token's mint and internal/iam/credential strips from
// what one carries on every request. Walked over every sensitive row with a
// principal holding everything ELSE, freshly proved, asking about somebody
// else, about the company and about nobody: none is admitted. The one way left
// — the self arm, somebody changing how they themselves prove who they are —
// is closed to a machine token by the guard, which never proves one for the
// sensitive window (internal/api/auth holds that half).
//
// Mutation: gate a sensitive row on a grant a token can carry and it is
// admitted here.
func TestASensitiveGestureAboutAnybodyElseNeedsAGrantNoTokenCarries(t *testing.T) {
	t.Parallel()
	var carried []iam.Grant
	for _, g := range iam.AllGrants {
		if !slices.Contains(iam.PersonPresentGrants, g) {
			carried = append(carried, g)
		}
	}
	p := person("ci.pipeline", carried...)
	somebodyElse := uuid.New().String()
	sensitive := 0
	for _, a := range authz.Actions() {
		if r, _ := authz.RecencyOf(a); r != iam.RecencySensitive {
			continue
		}
		sensitive++
		for _, object := range []authz.Object{
			{Kind: authz.KindPerson, Owner: somebodyElse, ID: somebodyElse},
			{Kind: authz.KindCompany},
			{},
		} {
			if d := authz.Decide(t.Context(), p, a, object, nimbus(), decidedAt); d.Allowed {
				t.Errorf("%s about %+v admitted a principal holding no grant "+
					"that needs a person present (%s) — a machine token carrying "+
					"the same would reach it", a, object, d.Reason)
			}
		}
	}
	if sensitive == 0 {
		t.Fatal("the table has no sensitive row, so this walk certifies nothing")
	}
}

// ENDING SOMEBODY ELSE'S SESSIONS ASKS FOR THE ADMINISTRATOR'S PROOF, AND
// ENDING YOUR OWN ASKS FOR NONE.
//
// One verb, two arms: a person who finds an intruder in their account signs
// out everywhere at once, whatever their proof's age, and an administrator
// ending a colleague's sessions — and every machine token the colleague holds
// — is asked the ordinary window every other directory write asks. A proof two
// hours old is outside it; the controls are the same administrator a minute
// after proving, and the colleague ending their own on a week-old cookie.
//
// Mutation: read the row's one window for both arms and the week-old cookie
// is refused; ask the self window of both and the stale administrator is
// admitted.
func TestEndingSomebodyElsesSessionsAsksForTheAdministratorsProof(t *testing.T) {
	t.Parallel()
	stale := func(p iam.Principal, age time.Duration) iam.Principal {
		at := decidedAt.Add(-age)
		p.ReauthAt, p.SensitiveReauthAt = at.Add(time.Hour), at.Add(15*time.Minute)
		return p
	}
	colleague := person("bob.second")
	about := func(p iam.Principal) authz.Object {
		return authz.Object{Kind: authz.KindPerson, Owner: p.ID.String()}
	}

	admin := stale(person("ana.admin", iam.GrantPeopleManage), 2*time.Hour)
	d := authz.Decide(t.Context(), admin, authz.ActionSessionEnd, about(colleague),
		authz.NoChart{}, decidedAt)
	if d.Allowed || d.Reason != authz.ReasonStepUp || d.Recency != iam.RecencyStepUp {
		t.Errorf("an administrator whose proof is two hours old ending a "+
			"colleague's sessions decided %+v, want a step-up refusal naming %s",
			d, iam.RecencyStepUp)
	}
	fresh := stale(person("ana.admin", iam.GrantPeopleManage), time.Minute)
	if d := authz.Decide(t.Context(), fresh, authz.ActionSessionEnd,
		about(colleague), authz.NoChart{}, decidedAt); !d.Allowed ||
		d.Reason != authz.ReasonGrant {
		t.Errorf("the same administrator a minute after proving decided %+v, "+
			"want admitted on the grant", d)
	}
	weekOld := stale(colleague, 7*24*time.Hour)
	if d := authz.Decide(t.Context(), weekOld, authz.ActionSessionEnd,
		about(weekOld), authz.NoChart{}, decidedAt); !d.Allowed ||
		d.Reason != authz.ReasonSelf {
		t.Errorf("a person ending their own sessions on a week-old cookie "+
			"decided %+v, want admitted as themselves with no proof asked", d)
	}
}

// NO TOOL ASKS FOR A PROOF.
//
// A verb a seat's tools are served under carries the tool's own name, with no
// dot. A seat has no keyboard to prove anything at, and the operator's MCP
// surface — the same tools, for a person's assistant — is not a step-up
// surface: a tool row asking for a proof would refuse every seat that holds
// the grant, with a sentence telling a model to sign in again.
func TestNoToolAsksForAProof(t *testing.T) {
	t.Parallel()
	for _, a := range authz.Actions() {
		if strings.Contains(string(a), ".") {
			continue
		}
		if r, _ := authz.RecencyOf(a); r.Demands() {
			t.Errorf("the tool %s asks for a %s proof, which no seat can give", a, r)
		}
	}
}

// EVERY OPERATOR WRITE ASKS FOR A PROOF, or says here why not.
//
// Held by the GRANT rather than by a second list of verbs: a row gated on a
// capability that changes the deployment or the company's people asks for a
// step-up unless it is one of the named exceptions below, each with its
// reason. A new row on one of those grants therefore either asks for a proof or
// arrives in this diff with a sentence defending why it does not.
func TestEveryOperatorWriteAsksForAProofOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	writes := map[iam.Grant]bool{
		iam.GrantConfigWrite: true, iam.GrantSecretWrite: true,
		iam.GrantSecretRead: true, iam.GrantPeopleManage: true,
		iam.GrantFleetOperate: true,
	}
	unproved := map[authz.Action]string{
		authz.ActionFleetRead: "a read of the deployment's own controls " +
			"changes nothing",
		authz.ActionChartImportRead: "a read of the import ledger, which a " +
			"client polls after an import and which changes nothing",
		authz.ActionCatalogueWrite: "a tool, asked through the operator MCP " +
			"and a seat's registry, neither of which is a step-up surface",
		authz.ActionSkillPageWrite: "asked from inside the page tools, on the " +
			"work and knowledge surfaces the design leaves out of step-up",
		authz.ActionWorkPurge: "the work surface, which the design leaves " +
			"out of step-up — the admin grant and a person are its bar",
		authz.ActionPagePurge: "the knowledge surface, for the purge above's " +
			"reason",
	}
	for _, a := range authz.Actions() {
		class, _ := authz.ClassOf(a)
		grant, _ := authz.GrantOf(a)
		recency, _ := authz.RecencyOf(a)
		if class != authz.ClassOperator || !writes[grant] {
			continue
		}
		why, excused := unproved[a]
		switch {
		case recency.Demands() && excused:
			t.Errorf("%s asks for a %s proof and is also excused (%q) — drop "+
				"the excuse", a, recency, why)
		case !recency.Demands() && !excused:
			t.Errorf("%s is gated on %s and asks for no proof, with nothing "+
				"here saying why", a, grant)
		}
	}
	for a := range unproved {
		if _, ok := authz.RecencyOf(a); !ok {
			t.Errorf("%s is excused and the table has no row for it", a)
		}
	}
}

// A STALE PROOF IS REFUSED EXACTLY WHERE THE ROW ASKS FOR ONE — and the two
// windows are two windows.
//
// One principal holding every grant, at four ages of proof, against every
// verb: nothing it is refused may be a grant or a relation, so every refusal
// here is the recency and nothing else. The row that proves the windows are
// different is forty minutes: inside `step_up`, outside `step_up_sensitive`.
func TestAStaleProofIsRefusedExactlyWhereTheRowAsksForOne(t *testing.T) {
	t.Parallel()
	holding := func(age time.Duration) iam.Principal {
		p := iam.Principal{ID: uuid.New(), Login: "jane.doe",
			Kind: iam.KindPerson, Stage: iam.StageActive, Grants: iam.AllGrants,
			Colleague: iam.ColleagueWrite}
		if age < 0 {
			return p // nothing ever proved
		}
		at := decidedAt.Add(-age)
		p.ReauthAt = at.Add(time.Hour)
		p.SensitiveReauthAt = at.Add(15 * time.Minute)
		return p
	}
	for _, c := range []struct {
		name  string
		age   time.Duration
		stale map[iam.Recency]bool
	}{
		{"proved a minute ago", time.Minute, nil},
		{"proved forty minutes ago", 40 * time.Minute,
			map[iam.Recency]bool{iam.RecencySensitive: true}},
		{"proved two hours ago", 2 * time.Hour,
			map[iam.Recency]bool{iam.RecencyStepUp: true, iam.RecencySensitive: true}},
		{"never proved", -1,
			map[iam.Recency]bool{iam.RecencyStepUp: true, iam.RecencySensitive: true}},
	} {
		p := holding(c.age)
		for _, a := range authz.Actions() {
			recency, _ := authz.RecencyOf(a)
			object := everythingAbout(p)
			d := authz.Decide(t.Context(), p, a, object, nimbus(), decidedAt)
			if d.Unknown() {
				t.Fatalf("%s: %s was undecidable: %v", c.name, a, d.Err)
			}
			if c.stale[recency] {
				if d.Allowed || d.Reason != authz.ReasonStepUp || d.Recency != recency {
					t.Errorf("%s: %s (asks %s) decided %+v, want a step-up "+
						"refusal naming %s", c.name, a, recency, d, recency)
				}
				continue
			}
			if !d.Allowed {
				t.Errorf("%s: %s (asks %s) was refused (%s) — a principal "+
					"holding every grant was stopped by something other "+
					"than a proof it holds", c.name, a, recency, d.Reason)
			}
			if d.Recency != "" {
				t.Errorf("%s: %s allowed and named the window %q, which a "+
					"client would read as a step-up", c.name, a, d.Recency)
			}
		}
	}
}

// everythingAbout is an object every relation rule admits the principal on —
// its own record under its login (the self class compares nothing else and
// takes no admin grant), a comment it wrote, a container the admin grant
// covers — so the only refusal left for a principal holding every grant is
// how recently it proved who it is.
func everythingAbout(p iam.Principal) authz.Object {
	return authz.Object{Kind: authz.KindPerson, ID: p.Login, Owner: p.Login,
		Author: p.Login, Container: "PLATFORM", ContainerKind: authz.KindProject}
}

// THE PROOF IS ASKED AFTER THE RULE ADMITS.
//
// A principal who could never take the verb is told what it lacks. Asked
// first, the proof would send them through a step-up only to be refused by a
// grant the step-up was never going to give them.
func TestAPrincipalTheRuleRefusesIsNotSentToStepUp(t *testing.T) {
	t.Parallel()
	stale := iam.Principal{ID: uuid.New(), Login: "jane.doe",
		Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantStateRead}}
	d := authz.Decide(t.Context(), stale, authz.ActionSecretReveal,
		authz.Object{Kind: authz.KindCompany}, authz.NoChart{}, decidedAt)
	if d.Allowed || d.Reason != authz.ReasonNoGrant || d.Recency != "" {
		t.Errorf("a stale caller without the grant decided %+v, want no_grant "+
			"and no window", d)
	}
	// And the UNKNOWN arm is still unknown: a chart that cannot answer is
	// "ask me again", which a proof would not change.
	d = authz.Decide(t.Context(), personLeading("cto"), authz.ActionChartContent,
		authz.Object{Kind: authz.KindUnit, Container: "sre"},
		chart{err: authz.ErrNoChart}, decidedAt.Add(24*time.Hour))
	if !d.Unknown() {
		t.Errorf("an undecidable lead decided %+v, want unknown whatever the "+
			"proof's age", d)
	}
}

// A STEP-UP REFUSAL IS ITS OWN CODE AND NAMES THE WINDOW.
//
// The code is the one the sign-in surface already answers for the same fact —
// a client learns ONE spelling — and the window is what the dashboard's
// confirmation has to say and replay against. Asserted on the wire, through
// the router and through [authz.Admit], because those are the two ways a
// surface refuses.
func TestAStepUpRefusalIsItsOwnCodeAndNamesTheWindow(t *testing.T) {
	t.Parallel()
	stale := person("jane.doe", iam.GrantSecretRead, iam.GrantConfigRead)
	stale.SensitiveReauthAt = decidedAt.Add(-time.Second)
	guard := authz.ContextGuard(authz.NoChart{})
	check := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusForbidden {
			t.Fatalf("answered %d, want 403", rec.Code)
		}
		body := envelope(t, rec)
		if body["error"] != string(httpjson.CodeStepUpRequired) ||
			body["message"] != httpjson.CodeStepUpRequired.Message() {
			t.Errorf("answered %v, want step_up_required with its sentence", body)
		}
		if body[authz.DetailReason] != string(authz.ReasonStepUp) ||
			body[authz.DetailWindow] != string(iam.RecencySensitive) {
			t.Errorf("reason %v window %v, want step_up naming %s",
				body[authz.DetailReason], body[authz.DetailWindow],
				iam.RecencySensitive)
		}
	}
	t.Run("through the router", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		if err := authz.NewRouter(mux, guard).Handle("GET /secrets/{name}",
			authz.Policy{Action: authz.ActionSecretReveal}, ok200()); err != nil {
			t.Fatalf("mount: %v", err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://x/secrets/K", nil)
		mux.ServeHTTP(rec, req.WithContext(iam.WithPrincipal(req.Context(), stale)))
		check(t, rec)
	})
	t.Run("through Admit", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://x/secrets/K?reveal=true", nil)
		req = req.WithContext(iam.WithPrincipal(req.Context(), stale))
		if authz.Admit(rec, req, guard, authz.Policy{Action: authz.ActionSecretReveal}) {
			t.Fatal("Admit let a stale proof reveal a secret")
		}
		check(t, rec)
	})
	t.Run("the listing beside it asks for nothing", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://x/secrets", nil)
		req = req.WithContext(iam.WithPrincipal(req.Context(), stale))
		if !authz.Admit(rec, req, guard, authz.Policy{Action: authz.ActionSecretList}) {
			t.Fatalf("a read was refused %d on the age of a proof", rec.Code)
		}
	})
}
