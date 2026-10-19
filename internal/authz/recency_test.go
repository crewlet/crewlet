package authz_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
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
// it is a step-up verb somebody added without deciding — shipped open to a
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

// A VERB NOTHING UNDOES ASKS A RECENT PROOF, AND THE PURGES AND CHAT'S
// DESTRUCTIONS ARE SUCH VERBS.
//
// What a person approves before a gesture says it cannot be undone where the
// row says so (authz.IrreversibleOf), so the row is held to both halves: each
// verb that destroys for good is marked, and a marked verb asks the step-up
// window, which is the decision "nothing undoes it" was made for. Mutation:
// unmark a purge, or mark a verb that asks no recent proof, and this fails.
func TestAVerbNothingUndoesAsksARecentProof(t *testing.T) {
	t.Parallel()
	for _, a := range []authz.Action{authz.ActionWorkPurge, authz.ActionPagePurge,
		authz.ActionArtifactPurge, authz.ActionChatErase, authz.ActionChatPrune,
		authz.ActionChatRetention} {
		if gone, _ := authz.IrreversibleOf(a); !gone {
			t.Errorf("%s destroys for good and its row does not say so", a)
		}
	}
	for _, a := range authz.Actions() {
		gone, ok := authz.IrreversibleOf(a)
		if !ok {
			t.Errorf("%s is listed and IrreversibleOf does not know it", a)
		}
		if r, _ := authz.RecencyOf(a); gone && r != iam.RecencyStepUp {
			t.Errorf("%s is undone by nothing and asks %q, not a recent proof", a, r)
		}
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

// ENDING EVERY SESSION IN THE COMPANY IS ADMIN'S, AND ASKS A RECENT PROOF.
//
// It ends every person's authority and every machine token in one record, so
// it is admin's alone — every app together does not reach it, and a refusal
// names admin and nothing else — and it asks the step-up window of an
// administrator whose proof is old. Mutation: admit on any app and the
// colleague holding both is let through; drop the window and the stale
// administrator is.
func TestEndingEverySessionInTheCompanyIsAdmins(t *testing.T) {
	t.Parallel()
	decide := func(p iam.Principal) authz.Decision {
		return authz.Decide(t.Context(), p, authz.ActionSessionInvalidate,
			authz.Object{Kind: authz.KindCompany}, authz.NoChart{}, decidedAt)
	}
	for _, c := range []struct {
		name  string
		holds []iam.Grant
	}{
		{"every app", iam.Apps},
		{"nothing", nil},
	} {
		d := decide(person("sam.sre", c.holds...))
		if d.Allowed || d.Reason != authz.ReasonNoGrant ||
			!slices.Equal(d.Grants, []iam.Grant{iam.GrantAdmin}) {
			t.Errorf("%s decided %+v, want refused naming exactly [admin]", c.name, d)
		}
	}
	admin := person("ana.admin", iam.GrantAdmin)
	if d := decide(admin); !d.Allowed || d.Reason != authz.ReasonGrant {
		t.Errorf("admin decided %+v, want admitted on the permission", d)
	}
	admin.ReauthAt = decidedAt.Add(-2 * time.Hour).Add(time.Hour)
	if d := decide(admin); d.Allowed || d.Reason != authz.ReasonStepUp ||
		d.Recency != iam.RecencyStepUp {
		t.Errorf("admin on a proof two hours old decided %+v, want a step-up "+
			"naming %s", d, iam.RecencyStepUp)
	}
}

// ENDING SOMEBODY ELSE'S SESSIONS ASKS FOR THE ADMINISTRATOR'S PROOF, AND
// ENDING YOUR OWN ASKS FOR NONE.
//
// One verb, two arms: a person who finds an intruder in their account signs
// out everywhere at once, whatever their proof's age, and an administrator
// ending a colleague's sessions — and every machine token the colleague holds
// — is asked `step_up`, as every other directory write is. A proof two hours
// old is outside it; the controls are the same administrator a minute
// after proving, and the colleague ending their own on a week-old cookie.
//
// Mutation: read the row's one window for both arms and the week-old cookie
// is refused; ask the self window of both and the stale administrator is
// admitted.
func TestEndingSomebodyElsesSessionsAsksForTheAdministratorsProof(t *testing.T) {
	t.Parallel()
	stale := func(p iam.Principal, age time.Duration) iam.Principal {
		p.ReauthAt = decidedAt.Add(-age).Add(time.Hour)
		return p
	}
	colleague := person("bob.second")
	about := func(p iam.Principal) authz.Object {
		return authz.Object{Kind: authz.KindPerson, Owner: p.ID.String()}
	}

	admin := stale(person("ana.admin", iam.GrantAdmin), 2*time.Hour)
	d := authz.Decide(t.Context(), admin, authz.ActionSessionEnd, about(colleague),
		authz.NoChart{}, decidedAt)
	if d.Allowed || d.Reason != authz.ReasonStepUp || d.Recency != iam.RecencyStepUp {
		t.Errorf("an administrator whose proof is two hours old ending a "+
			"colleague's sessions decided %+v, want a step-up refusal naming %s",
			d, iam.RecencyStepUp)
	}
	fresh := stale(person("ana.admin", iam.GrantAdmin), time.Minute)
	if d := authz.Decide(t.Context(), fresh, authz.ActionSessionEnd,
		about(colleague), authz.NoChart{}, decidedAt); !d.Allowed ||
		d.Reason != authz.ReasonGrant {
		t.Errorf("the same administrator a minute after proving decided %+v, "+
			"want admitted on the permission", d)
	}
	weekOld := stale(colleague, 7*24*time.Hour)
	if d := authz.Decide(t.Context(), weekOld, authz.ActionSessionEnd,
		about(weekOld), authz.NoChart{}, decidedAt); !d.Allowed ||
		d.Reason != authz.ReasonSelf {
		t.Errorf("a person ending their own sessions on a week-old cookie "+
			"decided %+v, want admitted as themselves with no proof asked", d)
	}
}

// NO TOOL ASKS FOR A PROOF, NOR DOES ANY VERB A TOOL ASKS FROM INSIDE ITSELF.
//
// A verb a seat's tools are served under carries the tool's own name, with no
// dot, and a tool that must read a row before it knows what it is about asks
// the same gate a dotted verb next — archiving a project, renaming a page,
// re-routing a task ([toolVerbs]). A seat has no keyboard to prove anything
// at, and the operator's MCP surface — the same tools, for a person's
// assistant — is not a step-up surface: either kind of row asking for a proof
// would refuse every seat that holds the grant, with a sentence telling a
// model to sign in again. A self arm's own window ([proofAsked]) counts too:
// it is the one a seat meets on its own record.
//
// Mutation: give `work.project.archive` a step-up, or `set_priorities`'s self
// arm one, and this goes red naming the tool or internal/agent/builtin, where
// the archive tool asks it.
func TestNoToolAsksForAProof(t *testing.T) {
	t.Parallel()
	for _, v := range toolVerbs(t) {
		if r, asks := proofAsked(v.verb); asks {
			t.Errorf("%s asks for a %s proof, which no seat can give", v.how, r)
		}
	}
}

// EVERY ADMIN ROW ASKS FOR A PROOF, or says here why not.
//
// Held by the CLASS rather than by a second list of verbs: a row that only an
// administrator may take changes the company, the deployment or its people,
// so it asks for a step-up unless it is one of the named exceptions below,
// each with its reason. A new admin row therefore either asks for a proof or
// arrives in this diff with a sentence defending why it does not. The PURGES
// are deliberately not excused: nothing undoes one.
func TestEveryAdminRowAsksForAProofOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	unproved := map[authz.Action]string{
		authz.ActionConfigRead: "a read of the company document changes nothing",
		authz.ActionPrinciplesRead: "a read of the company's principles changes " +
			"nothing",
		authz.ActionSecretList: "a listing of credential names changes nothing, " +
			"and hands over no value",
		authz.ActionSetupRead: "a read of what the integrations need changes " +
			"nothing",
		authz.ActionFleetRead: "a read of the deployment's own controls " +
			"changes nothing",
		authz.ActionAuditRead: "a read of the trail changes nothing",
		authz.ActionCatalogueWrite: "a tool, asked through the operator MCP " +
			"and a seat's registry, neither of which is a step-up surface",
		authz.ActionSkillPageWrite: "asked from inside the page tools, on the " +
			"Doclet surface the design leaves out of step-up",
		authz.ActionArtifactUsage: "a read of what the company's artifacts " +
			"hold against their quotas changes nothing",
	}
	walked := 0
	for _, a := range authz.Actions() {
		class, _ := authz.ClassOf(a)
		recency, _ := authz.RecencyOf(a)
		if class != authz.ClassAdmin {
			continue
		}
		walked++
		why, excused := unproved[a]
		switch {
		case recency.Demands() && excused:
			t.Errorf("%s asks for a %s proof and is also excused (%q) — drop "+
				"the excuse", a, recency, why)
		case !recency.Demands() && !excused:
			t.Errorf("%s is admin's and asks for no proof, with nothing here "+
				"saying why", a)
		}
	}
	if walked == 0 {
		t.Fatal("no row is admin's, so this walk certifies nothing")
	}
	for a := range unproved {
		if class, ok := authz.ClassOf(a); !ok || class != authz.ClassAdmin {
			t.Errorf("%s is excused and is not an admin row", a)
		}
	}
}

// A STALE PROOF IS REFUSED EXACTLY WHERE THE ROW ASKS FOR ONE.
//
// One principal holding every permission, at four ages of proof, against
// every verb — a seat for the seat-only ones and a person for the rest —
// nothing it is refused may be a permission or a relation, so every refusal
// here is the recency and nothing else. Inside the hour nothing is refused,
// which is the control for the two ages that refuse every step-up row.
func TestAStaleProofIsRefusedExactlyWhereTheRowAsksForOne(t *testing.T) {
	t.Parallel()
	// A SEAT-ONLY ROW IS DECIDED AS A SEAT, every other as a person: each
	// is refused to the other kind before any proof is asked. The person is
	// BOUND TO THEIR SEAT, as every person is (ADR-0032), so a verb made as
	// a seat is not refused them before the proof either.
	holding := func(age time.Duration, kind iam.Kind) iam.Principal {
		p := iam.Principal{ID: uuid.New(), Login: "jane.doe", Seat: "jane",
			Kind: kind, Stage: iam.StageActive, Grants: iam.AllGrants}
		if kind == iam.KindSeat {
			p.Login, p.Seat = "sre", "sre"
		}
		if age < 0 {
			return p // nothing ever proved
		}
		p.ReauthAt = decidedAt.Add(-age).Add(time.Hour)
		return p
	}
	for _, c := range []struct {
		name  string
		age   time.Duration
		stale map[iam.Recency]bool
	}{
		{"proved a minute ago", time.Minute, nil},
		{"proved forty minutes ago", 40 * time.Minute, nil},
		{"proved two hours ago", 2 * time.Hour,
			map[iam.Recency]bool{iam.RecencyStepUp: true}},
		{"never proved", -1, map[iam.Recency]bool{iam.RecencyStepUp: true}},
	} {
		for _, a := range authz.Actions() {
			p := holding(c.age, iam.KindPerson)
			if seatOnly, _ := authz.SeatOnlyOf(a); seatOnly {
				p = holding(c.age, iam.KindSeat)
			}
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
					"holding every permission was stopped by something other "+
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
// takes no admin path), a comment it wrote, a container admin covers — so the only refusal left for a principal holding every permission
// is how recently it proved who it is.
func everythingAbout(p iam.Principal) authz.Object {
	return authz.Object{Kind: authz.KindPerson, ID: p.Login, Owner: p.Login,
		Author: p.Login, Container: "PLATFORM", ContainerKind: authz.KindProject}
}

// THE PROOF IS ASKED AFTER THE RULE ADMITS.
//
// A principal who could never take the verb is told what it lacks. Asked
// first, the proof would send them through a step-up only to be refused for a
// permission the step-up was never going to give them.
func TestAPrincipalTheRuleRefusesIsNotSentToStepUp(t *testing.T) {
	t.Parallel()
	stale := iam.Principal{ID: uuid.New(), Login: "jane.doe",
		Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantWorklet}}
	d := authz.Decide(t.Context(), stale, authz.ActionSecretReveal,
		authz.Object{Kind: authz.KindCompany}, authz.NoChart{}, decidedAt)
	if d.Allowed || d.Reason != authz.ReasonNoGrant || d.Recency != "" {
		t.Errorf("a stale caller without admin decided %+v, want no_grant "+
			"and no window", d)
	}
	// And the UNKNOWN arm is still unknown: a chart that cannot answer is
	// "ask me again", which a proof would not change.
	d = authz.Decide(t.Context(), personLeading("cto"), authz.ActionOrgWrite,
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
	stale := person("jane.doe", iam.GrantAdmin)
	stale.ReauthAt = decidedAt.Add(-time.Second)
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
			body[authz.DetailWindow] != string(iam.RecencyStepUp) {
			t.Errorf("reason %v window %v, want step_up naming %s",
				body[authz.DetailReason], body[authz.DetailWindow],
				iam.RecencyStepUp)
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
