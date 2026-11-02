package authz_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// --- fixtures ------------------------------------------------------------- //

// artifact is an artifact owned by owner, as a surface that has read its row
// forms the object: the owner, whether the owner is an agent, and its shares.
func artifact(owner string, shares ...artifacts.Share) authz.Object {
	return authz.Object{Kind: authz.KindArtifact, ID: "01j8artifact", Owner: owner,
		Shares: shares}
}

// agentOwned is [artifact] for an artifact an AGENT seat owns.
func agentOwned(owner string, shares ...artifacts.Share) authz.Object {
	o := artifact(owner, shares...)
	o.OwnerIsAgent = true
	return o
}

func seatShare(handle string, r artifacts.Role) artifacts.Share {
	return artifacts.Share{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat,
		ID: handle}, Role: r}
}

func unitShare(key string, r artifacts.Role) artifacts.Share {
	return artifacts.Share{Audience: artifacts.Audience{Kind: artifacts.AudienceUnit,
		ID: key}, Role: r}
}

func companyShare(r artifacts.Role) artifacts.Share {
	return artifacts.Share{Audience: artifacts.Audience{Kind: artifacts.AudienceCompany},
		Role: r}
}

// sharedRows is every row the shared class decides, so a walk over it covers
// the verbs added after it was written.
func sharedRows(t *testing.T) []authz.Action {
	t.Helper()
	var out []authz.Action
	for _, a := range authz.Actions() {
		if class, _ := authz.ClassOf(a); class == authz.ClassShared {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		t.Fatal("no row is decided by the shared class, so this walk certifies nothing")
	}
	return out
}

// holding is a person bound to the seat handle with every app a row could
// ask for, so the role is the only thing left to decide.
func holding(handle string) iam.Principal {
	return personLeading(handle, iam.Apps...)
}

// --- the ADR's gates -------------------------------------------------------- //

// A SHARED ROW ADMITS THE ROLE IT NAMES AND EVERY ROLE ABOVE, AND NOTHING
// BELOW.
//
// Walked over every row the shared class decides, every role on the ladder,
// and every way a role is held (a share naming the caller's seat, a unit
// they sit in, the whole company) with every app held, so the role is the
// only thing deciding. A role that includes the row's is admitted on the
// share ([authz.ReasonShared]) and stands at exactly that role; one below is
// refused naming the role it needs ([authz.ReasonRole], [authz.Decision.Role])
// and admin, its admin path. The owner holds every role. Mutation: compare
// ranks with > instead of >=, or read the unit share without asking the chart,
// and this goes red.
func TestASharedRowAdmitsTheRoleItNamesAndEveryRoleAbove(t *testing.T) {
	t.Parallel()
	for _, a := range sharedRows(t) {
		need, _ := authz.MinRoleOf(a)
		owner := authz.Decide(t.Context(), holding("jane"), a, artifact("jane"),
			nimbus(), decidedAt)
		if !owner.Allowed || owner.Reason != authz.ReasonSelf ||
			owner.Standing.Role != artifacts.RoleOwner {
			t.Errorf("%s: the owner decided %+v, want admitted as owner", a, owner)
		}
		for _, held := range artifacts.ShareableRoles {
			for way, object := range map[string]authz.Object{
				"a seat share":    artifact("jane", seatShare("sre", held)),
				"a unit share":    artifact("jane", unitShare("sre", held)),
				"a company share": artifact("jane", companyShare(held)),
			} {
				d := authz.Decide(t.Context(), holding("sre"), a, object, nimbus(), decidedAt)
				if d.Unknown() {
					t.Fatalf("%s, %s of %s: undecidable: %v", a, way, held, d.Err)
				}
				if held.Includes(need) {
					if !d.Allowed || d.Reason != authz.ReasonShared ||
						d.Standing.Role != held || d.Elevated {
						t.Errorf("%s needs %s; %s of %s decided %+v, want admitted "+
							"on the share at %s", a, need, way, held, d, held)
					}
					continue
				}
				if d.Allowed || d.Reason != authz.ReasonRole || d.Role != need ||
					!slices.Equal(d.Grants, []iam.Grant{iam.GrantAdmin}) ||
					d.Standing.Role != held {
					t.Errorf("%s needs %s; %s of %s decided %+v, want refused "+
						"naming %s and admin", a, need, way, held, d, need)
				}
			}
		}
	}
}

// A CALLER WITH NO RELATION IS TOLD THE ARTIFACT IS NOT FOUND.
//
// An artifact's id, title and existence are its owner's and its audience's to
// disclose, so a caller with no relation at all is answered EXACTLY as for an
// id nothing holds: [authz.Decision.NotFound], rendered `404 not_found` with
// no reason and no grants, byte for byte the answer a surface gives for an
// unknown id. Walked over every shared row and every kind of near miss: no
// share at all, a share naming somebody else's seat, a unit they do not sit
// in, an artifact of a person they lead, and every app held. The controls: a
// relation below the row's role is refused 403 naming the role (it is not a
// secret from somebody it was shared with), and an administrator is never
// told an existing artifact is not found. Mutation: answer ReasonRole for no
// relation, or render a not-shared refusal through the 403 arm, and this goes
// red.
func TestACallerWithNoRelationIsToldTheArtifactIsNotFound(t *testing.T) {
	t.Parallel()
	unknownID := httptest.NewRecorder()
	httpjson.Fail(unknownID, http.StatusNotFound, httpjson.CodeNotFound)

	for _, a := range sharedRows(t) {
		for name, object := range map[string]authz.Object{
			"no share at all":        artifact("jane"),
			"somebody else's seat":   artifact("jane", seatShare("sam", artifacts.RoleEditor)),
			"a unit they are not in": artifact("jane", unitShare("design", artifacts.RoleEditor)),
			// THE CTO LEADS THE SRE, and a person's artifact is still theirs.
			"their report's own": artifact("sre"),
		} {
			d := authz.Decide(t.Context(), holding("cto"), a, object, nimbus(), decidedAt)
			if !d.NotFound() || d.Reason != authz.ReasonNotShared {
				t.Errorf("%s, %s: decided %+v, want not found", a, name, d)
				continue
			}
			rec := httptest.NewRecorder()
			authz.EnvelopeRefusal(rec, httptest.NewRequest(http.MethodGet, "http://x/", nil),
				authz.Policy{Action: a}, d)
			if rec.Code != http.StatusNotFound || rec.Body.String() != unknownID.Body.String() ||
				rec.Header().Get("Content-Type") != unknownID.Header().Get("Content-Type") {
				t.Errorf("%s, %s: rendered %d %s, want exactly the unknown id's %d %s",
					a, name, rec.Code, rec.Body.String(), unknownID.Code,
					unknownID.Body.String())
			}
		}
		need, _ := authz.MinRoleOf(a)
		if need != artifacts.RoleViewer {
			below := authz.Decide(t.Context(), holding("sre"), a,
				artifact("jane", seatShare("sre", artifacts.RoleViewer)), nimbus(), decidedAt)
			if below.NotFound() || below.Reason != authz.ReasonRole {
				t.Errorf("%s: a viewer decided %+v, want refused naming the role", a, below)
			}
			rec := httptest.NewRecorder()
			authz.EnvelopeRefusal(rec, httptest.NewRequest(http.MethodGet, "http://x/", nil),
				authz.Policy{Action: a}, below)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: a viewer's refusal answered %d, want 403", a, rec.Code)
			}
		}
		admin := authz.Decide(t.Context(), person("ops.admin", iam.GrantAdmin), a,
			artifact("jane"), nimbus(), decidedAt)
		if admin.NotFound() || !admin.Allowed {
			t.Errorf("%s: an administrator decided %+v, want admitted", a, admin)
		}
	}
}

// A BAR ON THE VERB IS NO EXISTENCE ORACLE.
//
// An agent taking a person's verb and a token taking one that needs somebody
// present are refused before any class runs, and a surface reads the head
// first, so a bar rendered for an existing artifact while an unknown id is
// answered 404 would tell a caller with no relation that the artifact exists.
// Walked over every shared row a bar covers: a caller with no relation is told
// it is not found, byte for byte the unknown id's answer; the artifact's
// audience is still told the bar itself; an administrator is never told an
// existing artifact is not found; and a relation the chart cannot answer is
// unknown. Mutation: return the bar before asking the relations and the
// not-found cases go red; skip the relation check and the audience's do.
func TestABarOnTheVerbIsNoExistenceOracle(t *testing.T) {
	t.Parallel()
	down := chart{err: errors.New("behind")}
	for _, a := range sharedRows(t) {
		human, _ := authz.HumanOnlyOf(a)
		present, _ := authz.PresenceOf(a)
		var (
			stranger, audience iam.Principal
			bar                authz.Reason
		)
		switch {
		case human:
			stranger, audience = seat("sre", iam.Apps...), seat("sre", iam.Apps...)
			bar = authz.ReasonSeatRefused
		case present == authz.PresenceRequired:
			stranger, audience = throughToken(holding("sre")), throughToken(holding("sre"))
			bar = authz.ReasonTokenRefused
		default:
			continue
		}
		if d := authz.Decide(t.Context(), stranger, a, artifact("jane"), nimbus(),
			decidedAt); !d.NotFound() {
			t.Errorf("%s: a caller with no relation decided %+v, want not found", a, d)
		}
		if d := authz.Decide(t.Context(), audience, a,
			artifact("jane", seatShare("sre", artifacts.RoleViewer)), nimbus(),
			decidedAt); d.Allowed || d.Unknown() || d.Reason != bar {
			t.Errorf("%s: its audience decided %+v, want refused on %s", a, d, bar)
		}
		if d := authz.Decide(t.Context(), stranger, a,
			artifact("jane", unitShare("sre-team", artifacts.RoleViewer)), down,
			decidedAt); !d.Unknown() {
			t.Errorf("%s: a unit share the chart cannot answer decided %+v, want unknown",
				a, d)
		}
		admin := person("ops.admin", iam.GrantAdmin)
		if human {
			admin = seat("ops.admin", iam.GrantAdmin)
		} else {
			admin = throughToken(admin)
		}
		if d := authz.Decide(t.Context(), admin, a, artifact("jane"), nimbus(),
			decidedAt); d.NotFound() || d.Reason != bar {
			t.Errorf("%s: an administrator decided %+v, want refused on %s", a, d, bar)
		}
	}
}

// THE LEAD ARM OPENS ONLY AN AGENT OWNER'S ARTIFACTS.
//
// A lead holds owner-level on the artifacts their agents own, as they already
// steer, pause and read those agents' trails; on a person's artifact they hold
// nothing, because a person's draft is theirs. Walked over every shared row:
// the CTO, who leads the SRE in the fixture, is admitted on the lead
// ([authz.ReasonLead]) at owner when the SRE seat that owns the artifact is an
// agent, and told it is not found when the same seat's artifact is a
// person's. The chart is the relation's: reversed, the lead is admitted to
// nothing; unreadable, an agent owner's artifact is unknown and a person's is
// still a firm no, because the lead arm is never asked about it. Mutation:
// open the lead arm for every owner, or skip the chart, and this goes red.
func TestTheLeadArmOpensOnlyAnAgentOwnersArtifacts(t *testing.T) {
	t.Parallel()
	reversed := chart{leads: map[[2]string]bool{{"sre", "cto"}: true}}
	down := chart{err: errors.New("the running org is not built yet")}
	for _, a := range sharedRows(t) {
		lead := authz.Decide(t.Context(), holding("cto"), a, agentOwned("sre"), nimbus(),
			decidedAt)
		if !lead.Allowed || lead.Reason != authz.ReasonLead ||
			lead.Standing.Role != artifacts.RoleOwner {
			t.Errorf("%s: the lead of the agent owner decided %+v, want admitted "+
				"on the lead at owner", a, lead)
		}
		person := authz.Decide(t.Context(), holding("cto"), a, artifact("sre"), nimbus(),
			decidedAt)
		if !person.NotFound() {
			t.Errorf("%s: the lead of a person owner decided %+v, want not found", a, person)
		}
		if d := authz.Decide(t.Context(), holding("cto"), a, agentOwned("sre"), reversed,
			decidedAt); d.Allowed {
			t.Errorf("%s: admitted with the lead relation reversed (%s)", a, d.Reason)
		}
		if d := authz.Decide(t.Context(), holding("cto"), a, agentOwned("sre"), down,
			decidedAt); !d.Unknown() {
			t.Errorf("%s: an agent owner's artifact on a chart that cannot answer "+
				"decided %+v, want unknown", a, d)
		}
		if d := authz.Decide(t.Context(), holding("cto"), a, artifact("sre"), down,
			decidedAt); d.Unknown() || !d.NotFound() {
			t.Errorf("%s: a person's artifact on a chart that cannot answer decided "+
				"%+v, want not found without asking the chart", a, d)
		}
	}
}

// AN ARTIFACT ROW IS NEVER SHOWN BY ITS APP.
//
// A cross-app answer filters its rows by kind ([authz.Shows]), and an artifact
// belongs to no app, which that filter reads as everybody's. So an artifact
// row is never shown by its kind at all, whatever the reader holds, and goes
// through [authz.Visible], which reads the row: shown to its owner, to
// whoever it is shared with and to the lead of its agent owner, and to nobody
// else, an administrator included, who may OPEN any artifact (a recorded
// override) but is never handed private titles in a listing by it. A chart
// that cannot answer is unknown, never a dropped row. Mutation: let Shows
// answer an artifact by its app, or let Visible honour an elevated admission,
// and this goes red.
func TestAnArtifactRowIsNeverShownByItsApp(t *testing.T) {
	t.Parallel()
	for _, grants := range [][]iam.Grant{nil, {iam.GrantArtifacts},
		{iam.GrantWorklet, iam.GrantDoclet}, {iam.GrantAdmin}, iam.AllGrants} {
		if authz.Shows(person("jane.doe", grants...), authz.KindArtifact) {
			t.Errorf("Shows(%v, artifact) is true: an artifact shown by its kind is "+
				"every private artifact shown to every reader", grants)
		}
	}
	private := artifact("jane")
	shared := artifact("jane", seatShare("sre", artifacts.RoleViewer))
	for _, c := range []struct {
		name   string
		p      iam.Principal
		object authz.Object
		want   bool
	}{
		{"the owner", personLeading("jane"), private, true},
		{"somebody it is shared with", personLeading("sre"), shared, true},
		{"the lead of its agent owner", personLeading("cto"), agentOwned("sre"), true},
		{"somebody else", personLeading("sam", iam.Apps...), shared, false},
		{"an administrator with no relation", person("ops.admin", iam.GrantAdmin),
			private, false},
		{"an administrator it is shared with",
			personLeading("sre", iam.GrantAdmin), shared, true},
	} {
		got, err := authz.Visible(t.Context(), c.p, c.object, nimbus(), decidedAt)
		if err != nil || got != c.want {
			t.Errorf("%s: Visible = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, err := authz.Visible(t.Context(), personLeading("cto"), agentOwned("sre"),
		chart{err: authz.ErrNoChart}, decidedAt); !errors.Is(err, authz.ErrNoChart) {
		t.Errorf("a row the chart could not place answered err = %v, want the chart's", err)
	}
	// AND EVERY OTHER KIND IS STILL [authz.Shows]'s.
	for _, k := range authz.ObjectKinds {
		if k == authz.KindArtifact {
			continue
		}
		p := person("jane.doe", iam.GrantWorklet)
		got, err := authz.Visible(t.Context(), p, authz.Object{Kind: k}, nimbus(), decidedAt)
		if err != nil || got != authz.Shows(p, k) {
			t.Errorf("Visible(%s) = %v, %v; want Shows' %v", k, got, err, authz.Shows(p, k))
		}
	}
}

// --- the admin path --------------------------------------------------------- //

// THE ADMIN PATH ADMITS EVERY SHARED ROW, AND IS MARKED WHERE IT ALONE DID.
//
// An administrator with no relation is admitted on the permission, at owner,
// ELEVATED, which is what the surfaces record as an override of somebody's
// private work. One whose share already reaches the row is admitted as a
// sharer, AT THE SHARER'S ROLE, and is not marked: nothing records an override
// that did not happen, and nothing beyond the share reaches them through a
// role every surface would shape by. The same share short of the row is
// marked, at owner. And no chart failure ever reaches an administrator's
// answer, while it does reach a lead's: a share the chart could not settle
// sends them through the admin path rather than leaving their role unsettled.
// Mutation: check the admin path before the relations and the sharer is
// marked; stand the sharer at owner and the viewer's read goes red; after the
// unknown arm and the administrator on a lagging node is told it cannot tell.
func TestTheAdminPathIsMarkedExactlyWhereItAloneAdmitted(t *testing.T) {
	t.Parallel()
	admin := personLeading("ops", iam.GrantAdmin)
	viewer := artifact("jane", seatShare("ops", artifacts.RoleViewer))
	down := chart{err: errors.New("behind")}
	for _, c := range []struct {
		name     string
		action   authz.Action
		object   authz.Object
		chart    authz.Chart
		reason   authz.Reason
		elevated bool
		role     artifacts.Role
		related  artifacts.Role
	}{
		{"no relation, reading", authz.ActionArtifactRead, artifact("jane"), nimbus(),
			authz.ReasonGrant, true, artifacts.RoleOwner, ""},
		{"a viewer share, reading", authz.ActionArtifactRead, viewer, nimbus(),
			authz.ReasonShared, false, artifacts.RoleViewer, artifacts.RoleViewer},
		{"a viewer share, writing data", authz.ActionArtifactDataWrite, viewer, nimbus(),
			authz.ReasonShared, false, artifacts.RoleViewer, artifacts.RoleViewer},
		{"a viewer share, publishing", authz.ActionArtifactPublish, viewer, nimbus(),
			authz.ReasonGrant, true, artifacts.RoleOwner, artifacts.RoleViewer},
		{"a viewer share, trashing", authz.ActionArtifactTrash, viewer, nimbus(),
			authz.ReasonGrant, true, artifacts.RoleOwner, artifacts.RoleViewer},
		{"a unit share on a chart that cannot answer", authz.ActionArtifactRead,
			artifact("jane", unitShare("ops-team", artifacts.RoleEditor)), down,
			authz.ReasonGrant, true, artifacts.RoleOwner, ""},
		{"a viewer share beside a unit share the chart cannot settle",
			authz.ActionArtifactRead, artifact("jane", seatShare("ops", artifacts.RoleViewer),
				unitShare("ops-team", artifacts.RoleEditor)), down,
			authz.ReasonGrant, true, artifacts.RoleOwner, artifacts.RoleViewer},
		{"an agent's artifact on a chart that cannot answer", authz.ActionArtifactShare,
			agentOwned("sre"), down, authz.ReasonGrant, true, artifacts.RoleOwner, ""},
		{"a row nobody has read", authz.ActionArtifactPublish,
			authz.Object{Kind: authz.KindArtifact, Unresolved: true}, down,
			authz.ReasonGrant, true, artifacts.RoleOwner, ""},
		{"a row naming nobody", authz.ActionArtifactRead,
			authz.Object{Kind: authz.KindArtifact}, nimbus(), authz.ReasonGrant, true,
			artifacts.RoleOwner, ""},
	} {
		d := authz.Decide(t.Context(), admin, c.action, c.object, c.chart, decidedAt)
		if d.Unknown() || !d.Allowed || d.Reason != c.reason || d.Elevated != c.elevated ||
			d.Standing.Role != c.role || d.Standing.Related != c.related ||
			d.Standing.Err() != nil {
			t.Errorf("%s: decided %+v, want admitted (%s), elevated %v, standing "+
				"%s over %q, settled", c.name, d, c.reason, c.elevated, c.role, c.related)
		}
	}
	if st, err := authz.StandingOn(t.Context(), admin, viewer, nimbus(), decidedAt); err != nil ||
		st.Role != artifacts.RoleViewer || st.Elevated(artifacts.RoleEditor) {
		t.Errorf("StandingOn for an administrator holding a viewer share = %+v, %v; want "+
			"the viewer they are, never owner", st, err)
	}
	lead := authz.Decide(t.Context(), personLeading("cto", iam.Apps...),
		authz.ActionArtifactShare, agentOwned("sre"), down, decidedAt)
	if !lead.Unknown() {
		t.Errorf("a lead on a chart that cannot answer decided %+v, want unknown", lead)
	}
}

// --- what the relations resolve to ------------------------------------------ //

// THE STANDING IS THE BEST RELATION, AND UNKNOWN ONLY WHERE IT COULD MATTER.
//
// The role a write is stamped with is the BEST the caller's relations grant,
// not the first that clears the row: a viewer seat share beside an editor unit
// share is an editor. An arm the chart cannot answer leaves the decision
// unknown only where it could have admitted (a viewer unit share nobody can
// read changes nothing about a publish, and answering that 503 would send
// somebody back for the same refusal), while a read the seat share already
// admits is decided, and its standing reports itself unsettled, which
// [authz.StandingOn] answers as unknown. Mutation: stop at the first arm that
// admits, or answer unknown on every chart failure, and this goes red.
func TestTheStandingIsTheBestRelationAndUnknownOnlyWhereItCouldMatter(t *testing.T) {
	t.Parallel()
	down := chart{err: errors.New("behind")}
	sre := holding("sre")
	both := artifact("jane", seatShare("sre", artifacts.RoleViewer),
		unitShare("sre", artifacts.RoleEditor))

	best := authz.Decide(t.Context(), sre, authz.ActionArtifactRead, both, nimbus(), decidedAt)
	if !best.Allowed || best.Standing.Role != artifacts.RoleEditor || best.Standing.Err() != nil {
		t.Errorf("a viewer seat share beside an editor unit share decided %+v, want "+
			"admitted at a settled editor", best)
	}
	if st, err := authz.StandingOn(t.Context(), sre, both, nimbus(), decidedAt); err != nil ||
		st.Role != artifacts.RoleEditor {
		t.Errorf("StandingOn = %+v, %v; want editor", st, err)
	}

	read := authz.Decide(t.Context(), sre, authz.ActionArtifactRead, both, down, decidedAt)
	if read.Unknown() || !read.Allowed || read.Standing.Role != artifacts.RoleViewer ||
		!errors.Is(read.Standing.Err(), authz.ErrUnsettled) {
		t.Errorf("the read on a chart that cannot answer decided %+v, want admitted "+
			"at an unsettled viewer", read)
	}
	if _, err := authz.StandingOn(t.Context(), sre, both, down, decidedAt); !errors.Is(err,
		authz.ErrUnsettled) {
		t.Errorf("StandingOn on a chart that cannot answer: err = %v, want unsettled", err)
	}
	if d := authz.Decide(t.Context(), sre, authz.ActionArtifactPublish, both, down,
		decidedAt); !d.Unknown() {
		t.Errorf("a publish an unread editor share could admit decided %+v, want unknown", d)
	}
	viewerUnit := artifact("jane", unitShare("sre", artifacts.RoleViewer))
	if d := authz.Decide(t.Context(), sre, authz.ActionArtifactPublish, viewerUnit, down,
		decidedAt); d.Unknown() || d.Allowed {
		t.Errorf("a publish an unread viewer share could not admit decided %+v, want "+
			"a firm refusal", d)
	}
	if d := authz.Decide(t.Context(), sre, authz.ActionArtifactRead, viewerUnit, down,
		decidedAt); !d.Unknown() {
		t.Errorf("a read only an unread unit share could admit decided %+v, want unknown", d)
	}
	if st, err := authz.StandingOn(t.Context(), holding("sam"), both, nimbus(),
		decidedAt); err != nil || st.Role != "" {
		t.Errorf("StandingOn for somebody it is not shared with = %+v, %v; want none", st, err)
	}
	if st, err := authz.StandingOn(t.Context(), person("ops.admin", iam.GrantAdmin), both,
		down, decidedAt); err != nil || st.Role != artifacts.RoleOwner ||
		!st.Elevated(artifacts.RoleViewer) {
		t.Errorf("StandingOn for an administrator = %+v, %v; want an exact owner, elevated",
			st, err)
	}
}

// A SHARE THIS BUILD CANNOT READ GRANTS NOTHING.
//
// An audience kind a newer peer wrote (a public link, later) or a role off the
// ladder ranks zero and admits nobody, for the reason an unknown permission
// grants nothing: this build cannot open a door it has never heard of. A share
// naming the company admits every principal, an agent included, and a seat
// share admits the seat's holder only. Mutation: read an unknown kind as the
// company and the link's holder is admitted.
func TestAShareThisBuildCannotReadGrantsNothing(t *testing.T) {
	t.Parallel()
	link := artifacts.Share{Audience: artifacts.Audience{Kind: "link", ID: "x"},
		Role: artifacts.RoleEditor}
	for name, object := range map[string]authz.Object{
		"an audience kind this build does not know": artifact("jane", link),
		"a role off the ladder": artifact("jane",
			artifacts.Share{Audience: artifacts.Audience{Kind: artifacts.AudienceCompany},
				Role: "admin"}),
		"a role nobody filled in": artifact("jane", seatShare("sre", "")),
	} {
		if d := authz.Decide(t.Context(), holding("sre"), authz.ActionArtifactRead, object,
			nimbus(), decidedAt); !d.NotFound() {
			t.Errorf("%s: decided %+v, want not found", name, d)
		}
	}
	company := artifact("jane", companyShare(artifacts.RoleCommenter))
	for name, p := range map[string]iam.Principal{
		"a person": personLeading("sam"),
		"an agent": seat("ops-bot", iam.Apps...),
		"a service account on no seat": {ID: uuid.New(), Login: "ci:release",
			Kind: iam.KindMachine, Stage: iam.StageActive},
		"a person holding no app at all": person("pat.kim"),
	} {
		d := authz.Decide(t.Context(), p, authz.ActionArtifactRead, company, nimbus(),
			decidedAt)
		if !d.Allowed || d.Standing.Role != artifacts.RoleCommenter {
			t.Errorf("%s on a company share decided %+v, want admitted as commenter", name, d)
		}
	}
}

// --- the app, the agents, the human-only rows -------------------------------- //

// THE APP GATES AUTHORING, AND A SHARE ALONE OPENS PARTICIPATION.
//
// Somebody given no app reads, fills in and is present on every artifact
// shared with them; an editor without the app does everything an editor does
// except what makes or reshapes the artifact itself, and is refused naming
// exactly the app. Sharing is the owner's relation and asks no app. Creating
// asks nothing but the app. Mutation: drop the app from the publish row, or
// add it to the data write row, and this goes red.
func TestTheAppGatesAuthoringAndAShareAloneOpensParticipation(t *testing.T) {
	t.Parallel()
	editor := artifact("jane", seatShare("sre", artifacts.RoleEditor))
	without := personLeading("sre")
	withApp := personLeading("sre", iam.GrantArtifacts)
	for _, a := range sharedRows(t) {
		need, _ := authz.MinRoleOf(a)
		if !artifacts.RoleEditor.Includes(need) {
			continue
		}
		app, _ := authz.AppOf(a)
		d := authz.Decide(t.Context(), without, a, editor, nimbus(), decidedAt)
		switch app {
		case "":
			if !d.Allowed {
				t.Errorf("%s asks no app, and an editor without one decided %+v", a, d)
			}
		case iam.GrantArtifacts:
			if d.Allowed || d.Reason != authz.ReasonNoGrant ||
				!slices.Equal(d.Grants, []iam.Grant{iam.GrantArtifacts}) {
				t.Errorf("%s authors, and an editor without the app decided %+v, want "+
					"refused naming the app", a, d)
			}
			if d := authz.Decide(t.Context(), withApp, a, editor, nimbus(),
				decidedAt); !d.Allowed {
				t.Errorf("%s: an editor with the app decided %+v", a, d)
			}
		default:
			t.Errorf("%s names the app %q; an artifact row names the artifacts app "+
				"or none", a, app)
		}
	}
	for _, a := range []authz.Action{authz.ActionArtifactShare, authz.ActionArtifactUnshare} {
		if d := authz.Decide(t.Context(), personLeading("jane"), a, artifact("jane"),
			nimbus(), decidedAt); !d.Allowed {
			t.Errorf("%s: the owner holding no app decided %+v, want admitted", a, d)
		}
	}
	for _, c := range []struct {
		p     iam.Principal
		admit bool
	}{{personLeading("sam"), false}, {personLeading("sam", iam.GrantArtifacts), true},
		{person("ops.admin", iam.GrantAdmin), true}} {
		d := authz.Decide(t.Context(), c.p, authz.ActionArtifactCreate,
			authz.Object{Kind: authz.KindArtifact, Owner: "sam"}, nimbus(), decidedAt)
		if d.Allowed != c.admit {
			t.Errorf("create, holding %v: decided %+v, want admitted=%v", c.p.Grants, d, c.admit)
		}
	}
}

// AN AGENT NEVER MANAGES SOMEBODY ELSE'S WORK UNLESS THE COMPANY SAYS SO.
//
// By default an agent holds at most contributor on an artifact it does not own
// however it is shared and whoever it leads: it reads and fills in the data,
// and is refused as a seat, naming nothing because no share would change it,
// on every row above contributor. Its own artifacts are wholly its own. The
// company's setting lifts the cap. And an agent with no relation is still told
// the artifact is not found, so the cap discloses nothing. Mutation: apply the
// cap to the owner, or answer the capped agent [authz.ReasonRole], and this
// goes red.
func TestAnAgentNeverManagesSomebodyElsesWorkUnlessTheCompanySaysSo(t *testing.T) {
	t.Parallel()
	bot := seat("cto", iam.Apps...)
	for _, c := range []struct {
		name   string
		object authz.Object
		via    authz.Reason
		holds  artifacts.Role
	}{
		{"shared editor", artifact("jane", seatShare("cto", artifacts.RoleEditor)),
			authz.ReasonShared, artifacts.RoleEditor},
		{"the lead of the agent that owns it", agentOwned("sre"), authz.ReasonLead,
			artifacts.RoleOwner},
	} {
		for _, a := range sharedRows(t) {
			if only, _ := authz.HumanOnlyOf(a); only {
				continue
			}
			need, _ := authz.MinRoleOf(a)
			capped := authz.Decide(t.Context(), bot, a, c.object, nimbus(), decidedAt)
			switch {
			case artifacts.RoleContributor.Includes(need):
				if !capped.Allowed || capped.Standing.Role != artifacts.RoleContributor {
					t.Errorf("%s, %s: decided %+v, want admitted at contributor",
						c.name, a, capped)
				}
			default:
				if capped.Allowed || capped.Reason != authz.ReasonSeatRefused ||
					len(capped.Grants) != 0 {
					t.Errorf("%s, %s: decided %+v, want refused as a seat naming "+
						"nothing", c.name, a, capped)
				}
			}
			lifted := c.object
			lifted.AgentsManageOthers = true
			d := authz.Decide(t.Context(), bot, a, lifted, nimbus(), decidedAt)
			switch {
			case c.holds.Includes(need) && (!d.Allowed || d.Reason != c.via):
				t.Errorf("%s, %s, the company allowing it: decided %+v, want admitted "+
					"on %s", c.name, a, d, c.via)
			case !c.holds.Includes(need) && (d.Allowed || d.Reason != authz.ReasonRole):
				t.Errorf("%s, %s, the company allowing it: decided %+v, want refused "+
					"naming the role %s", c.name, a, d, need)
			}
			own := authz.Decide(t.Context(), bot, a, agentOwned("cto"), nimbus(), decidedAt)
			if !own.Allowed || own.Reason != authz.ReasonSelf {
				t.Errorf("%s on its own artifact: decided %+v, want admitted as owner", a, own)
			}
			if d := authz.Decide(t.Context(), bot, a, artifact("jane"), nimbus(),
				decidedAt); !d.NotFound() {
				t.Errorf("%s with no relation: decided %+v, want not found", a, d)
			}
		}
	}
	// A PERSON IS NEVER CAPPED: the setting is about agents.
	if d := authz.Decide(t.Context(), holding("cto"), authz.ActionArtifactPublish,
		artifact("jane", seatShare("cto", artifacts.RoleEditor)), nimbus(),
		decidedAt); !d.Allowed {
		t.Errorf("a person shared editor decided %+v, want admitted", d)
	}
}

// THE GESTURES THAT TAKE AN ARTIFACT OUT OF CIRCULATION, DISCARD A VERSION OR
// MOVE OWNERSHIP ARE A PERSON'S.
//
// Trash and restore are the owner's, a person's and the app's; a purge is an
// administrator's, a person's, present, and a recent proof's; discarding a
// newer version (force) is an editor's, a person's and present; a transfer is
// the owner's, a person's and present. Each case names the arm that decides.
// Mutation: drop humanOnly from trash, presence from force, or the step-up
// from the purge, and its case goes red.
func TestTheArtifactRowsOutOfCirculationArePeoples(t *testing.T) {
	t.Parallel()
	own := artifact("jane")
	editor := artifact("sam", seatShare("jane", artifacts.RoleEditor))
	jane := personLeading("jane", iam.Apps...)
	stale := jane
	stale.ReauthAt = decidedAt.Add(-time.Minute)
	admin := person("ops.admin", iam.GrantAdmin)
	staleAdmin := admin
	staleAdmin.ReauthAt = decidedAt.Add(-time.Minute)
	for _, c := range []struct {
		name   string
		p      iam.Principal
		action authz.Action
		object authz.Object
		admit  bool
		reason authz.Reason
	}{
		{"the owner trashes", jane, authz.ActionArtifactTrash, own, true, authz.ReasonSelf},
		{"an agent owner does not", seat("jane", iam.Apps...), authz.ActionArtifactTrash,
			agentOwned("jane"), false, authz.ReasonSeatRefused},
		{"nor restores", seat("jane", iam.Apps...), authz.ActionArtifactRestore,
			agentOwned("jane"), false, authz.ReasonSeatRefused},
		{"an editor does not trash", jane, authz.ActionArtifactTrash, editor, false,
			authz.ReasonRole},
		{"an owner purges nothing", jane, authz.ActionArtifactPurge, own, false,
			authz.ReasonNoGrant},
		{"an administrator purges", admin, authz.ActionArtifactPurge, own, true,
			authz.ReasonGrant},
		{"on a recent proof only", staleAdmin, authz.ActionArtifactPurge, own, false,
			authz.ReasonStepUp},
		{"and never through a token", throughToken(admin), authz.ActionArtifactPurge, own,
			false, authz.ReasonTokenRefused},
		{"an editor forces", jane, authz.ActionArtifactForce, editor, true,
			authz.ReasonShared},
		{"on any proof, at a keyboard", stale, authz.ActionArtifactForce, editor, true,
			authz.ReasonShared},
		{"never through a token", throughToken(jane), authz.ActionArtifactForce, editor,
			false, authz.ReasonTokenRefused},
		{"never as a seat", seat("jane", iam.Apps...), authz.ActionArtifactForce,
			agentOwned("jane"), false, authz.ReasonSeatRefused},
		{"the owner transfers", jane, authz.ActionArtifactTransfer, own, true,
			authz.ReasonSelf},
		{"never through a token", throughToken(jane), authz.ActionArtifactTransfer, own,
			false, authz.ReasonTokenRefused},
		{"never as a seat", seat("jane", iam.Apps...), authz.ActionArtifactTransfer,
			agentOwned("jane"), false, authz.ReasonSeatRefused},
		{"a viewer pins", personLeading("sre"), authz.ActionArtifactPin,
			artifact("jane", seatShare("sre", artifacts.RoleViewer)), true,
			authz.ReasonShared},
		{"an editor exports", jane, authz.ActionArtifactExport, editor, true,
			authz.ReasonShared},
		{"a viewer does not", personLeading("sre"), authz.ActionArtifactExport,
			artifact("jane", seatShare("sre", artifacts.RoleViewer)), false, authz.ReasonRole},
		{"usage is an administrator's", jane, authz.ActionArtifactUsage,
			authz.Object{Kind: authz.KindCompany}, false, authz.ReasonNoGrant},
		{"a ready report is everybody's", person("pat.kim"), authz.ActionArtifactReady,
			authz.Object{Kind: authz.KindArtifact}, true, authz.ReasonEverybody},
		{"listing is everybody's", person("pat.kim"), authz.ActionArtifactList,
			authz.Object{Kind: authz.KindArtifact}, true, authz.ReasonEverybody},
	} {
		d := authz.Decide(t.Context(), c.p, c.action, c.object, nimbus(), decidedAt)
		if d.Unknown() || d.Allowed != c.admit || d.Reason != c.reason {
			t.Errorf("%s (%s): decided %+v, want admitted=%v on %s", c.name, c.action, d,
				c.admit, c.reason)
		}
	}
}

// AN ARTIFACT NOBODY HAS READ IS READ, THEN DECIDED AGAIN.
//
// A tool's gate forms the object from its arguments, and the owner and the
// shares are on the stored row, so every caller but an administrator is told
// to read it and ask again (never admitted, never refused on what nobody
// read), and a caller without the app an authoring row asks is refused there
// and then, naming it, because no row could admit them. An object naming
// nobody at all is a caller who forgot to read it. Mutation: refuse the
// unread object outright and every surface lists no artifact verb before it
// has read the row.
func TestAnArtifactNobodyHasReadIsReadThenDecidedAgain(t *testing.T) {
	t.Parallel()
	unread := authz.Object{Kind: authz.KindArtifact, ID: "01j8artifact", Unresolved: true}
	for _, a := range sharedRows(t) {
		app, _ := authz.AppOf(a)
		holder := authz.Decide(t.Context(), holding("sre"), a, unread, nimbus(), decidedAt)
		if !errors.Is(holder.Err, authz.ErrUnresolved) {
			t.Errorf("%s: a caller holding every app decided %+v, want told to read it",
				a, holder)
		}
		if app != "" {
			without := authz.Decide(t.Context(), personLeading("sre"), a, unread, nimbus(),
				decidedAt)
			if without.Unknown() || without.Allowed ||
				!slices.Equal(without.Grants, []iam.Grant{app}) {
				t.Errorf("%s: a caller without %s decided %+v, want refused naming it",
					a, app, without)
			}
		}
		named := authz.Decide(t.Context(), holding("sre"), a,
			authz.Object{Kind: authz.KindArtifact}, nimbus(), decidedAt)
		if named.Unknown() || named.Allowed || named.Reason != authz.ReasonUnnamed {
			t.Errorf("%s: an object naming nobody decided %+v, want unnamed", a, named)
		}
	}
}

// --- the table's own walks --------------------------------------------------- //

// EVERY SHARED ROW NAMES A ROLE ON THE LADDER, AND NO OTHER ROW NAMES ONE.
//
// A zero role on a shared row reads as no requirement to somebody skimming the
// table, and admits nobody in fact ([artifacts.Role.Includes]): a verb
// somebody added without deciding. A role on any other row is read by no class
// and reads as a decision that was never made. Mutation: drop the role from
// get_artifact, or give one to a tracker row, and this goes red.
func TestEverySharedRowNamesARoleAndNoOtherRowDoes(t *testing.T) {
	t.Parallel()
	for _, a := range authz.Actions() {
		class, _ := authz.ClassOf(a)
		role, _ := authz.MinRoleOf(a)
		switch {
		case class == authz.ClassShared && !role.Valid():
			t.Errorf("%s is shared and names the role %q, which is not on the ladder",
				a, role)
		case class != authz.ClassShared && role != "":
			t.Errorf("%s is decided by %s and names the role %q, which nothing reads",
				a, class, role)
		}
	}
	if _, known := authz.MinRoleOf("no.such.verb"); known {
		t.Error("MinRoleOf knows a verb the table has no row for")
	}
}

// THE VERBS AN AGENT'S OWN WORK IS STEERED BY ARE ITS EDITS, AND ONLY THOSE.
//
// The requester ceiling spares an agent acting on what it owns on exactly the
// verbs that change it: creating, publishing, renaming, adding an asset,
// reshaping and filling in its data. A verb that discloses content is never
// one, or anybody who can message an agent could have it read its private work
// back to them; neither is sharing, which is that same read one step removed;
// and a person's own gesture has no turn to be steered in. Pinned by name, so
// a verb that becomes steerable arrives in the diff with its reason.
// Mutation: mark get_artifact or share_artifact steerable and this goes red.
func TestOnlyAnAgentsEditsAreSteerable(t *testing.T) {
	t.Parallel()
	want := []authz.Action{
		authz.ActionArtifactCreate, authz.ActionArtifactPublish, authz.ActionArtifactUpdate,
		authz.ActionArtifactAsset, authz.ActionArtifactSchema, authz.ActionArtifactDataWrite,
		// FILING ITS OWN WORK: an agent asked to put the report it made in
		// a folder, or to make a subfolder in a folder it owns, carries it
		// out as it carries out any other edit to its own work.
		authz.ActionArtifactFolderCreate, authz.ActionArtifactMove, authz.ActionArtifactFolderFile,
	}
	var got []authz.Action
	for _, a := range authz.Actions() {
		if steerable, _ := authz.SteerableOf(a); steerable {
			got = append(got, a)
			if only, _ := authz.HumanOnlyOf(a); only {
				t.Errorf("%s is steerable and human-only: no turn steers a person", a)
			}
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the steerable verbs are %v, want %v", got, want)
	}
	if _, known := authz.SteerableOf("no.such.verb"); known {
		t.Error("SteerableOf knows a verb the table has no row for")
	}
}
