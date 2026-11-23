package metaads

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// The company's own system user's tokens on the fake (user 101).
const (
	companyRead    = "EAAGcompanyREADxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	companyOperate = "EAAGcompanyOPERATExxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
)

// lane is Meta's access session over a fake Graph API, its sealed values in
// vals by name, as the secret store holds them.
type lane struct {
	s    *accessSession
	f    *graphFake
	vals map[string]string
	doc  *config.Company
	sink string
}

// newLane is a company on the fake business with an operate seat on act_111
// set to its own identity, connecting the admin system user's token where admin.
func newLane(t *testing.T, admin bool) *lane {
	t.Helper()
	f := newGraphFake()
	f.tokens[companyRead] = &fakeToken{user: "101", app: fakeApp, kind: "SYSTEM_USER",
		scopes: []string{"ads_read", "pages_show_list"}}
	f.tokens[companyOperate] = &fakeToken{user: "101", app: fakeApp, kind: "SYSTEM_USER",
		scopes: []string{"ads_management", "pages_read_engagement", "pages_show_list"}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	on := true
	block := &config.MetaAds{Enabled: true, AppID: fakeApp, AppSecret: "${META_ADS_APP_SECRET}",
		ReadToken: "${META_ADS_READ_TOKEN}", OperateToken: "${META_ADS_OPERATE_TOKEN}", BusinessID: fakeBusiness}
	vals := map[string]string{"META_ADS_APP_SECRET": fakeSecret, "META_ADS_READ_TOKEN": companyRead,
		"META_ADS_OPERATE_TOKEN": companyOperate}
	if admin {
		block.AdminSystemUserToken = "${META_ADS_ADMIN_SYSTEM_USER_TOKEN}"
		vals["META_ADS_ADMIN_SYSTEM_USER_TOKEN"] = fakeAdmin
	}
	doc := &config.Company{Name: "Example Co", Integrations: config.Integrations{MetaAds: block},
		Roles: []config.Role{{Name: "Growth Marketer", Handle: "growth", Integrations: config.RoleIntegrations{
			MetaAds: &config.RoleMetaAds{Enabled: &on, Level: "operate", AdAccounts: []string{"act_111"},
				Identity: "own"}}}}}
	sess, err := accessVendor{}.open(seataccess.OpenInput{Config: doc, Env: config.NewResolver(config.MapSource(vals))},
		srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	l := &lane{s: sess.(*accessSession), f: f, vals: vals, doc: doc,
		sink: seataccess.SinkName(integration.KindMetaAds, SeatTokenField, "growth")}
	l.s.resolve = func(ref string) string {
		if name, ok := strings.CutPrefix(ref, "${"); ok {
			return vals[strings.TrimSuffix(name, "}")]
		}
		return ref
	}
	return l
}

// input is the seat's lane at level, its record rec.
func (l *lane) input(level string, rec *seataccess.IdentityRecord) seataccess.IdentityInput {
	return seataccess.IdentityInput{Seat: seataccess.SeatRef{Handle: "growth", Name: "Growth Marketer"},
		Lane: seataccess.LaneMetaAds, Record: rec, Sinks: map[string]string{SeatTokenField: l.sink},
		Want: seataccess.Desired{Enabled: true, Level: level, Scope: []string{"act_111"},
			Identity: seataccess.IdentityOwn}}
}

// keep keeps what KeepIdentity answered as the sync does: sealed, recorded.
func (l *lane) keep(t *testing.T, kept seataccess.IdentityKept) *seataccess.IdentityRecord {
	t.Helper()
	if kept.Check.Status != seataccess.IdentityReady {
		t.Fatalf("not ready: %+v", kept.Check)
	}
	l.vals[l.sink] = kept.Minted[0].Value
	return &seataccess.IdentityRecord{Way: kept.Way, ID: kept.ID, Name: kept.Name, Check: &kept.Check,
		Credential: &seataccess.Credential{Variable: l.sink}}
}

func TestMakeItForMeKeepsAReadyEmployeeSystemUserNamedByTheSharedRule(t *testing.T) {
	l := newLane(t, true)
	in := l.input("operate", nil)
	in.Way = seataccess.WayMade
	kept, err := l.s.KeepIdentity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Check.Status != seataccess.IdentityReady || kept.Name != "crewlet-growth" || kept.Way != seataccess.WayMade {
		t.Fatalf("kept %+v", kept)
	}
	if kept.Check.Role != "Manage campaigns and View performance" || kept.Check.Needs != kept.Check.Role {
		t.Fatalf("role %q needs %q", kept.Check.Role, kept.Check.Needs)
	}
	if len(kept.Minted) != 1 || kept.Minted[0].Field != SeatTokenField || kept.Minted[0].Revoke == nil {
		t.Fatalf("minted %+v, want one token that revokes itself if it cannot be sealed", kept.Minted)
	}
	if got := len(kept.Check.Reads); got != 6 || !strings.HasPrefix(kept.Check.Reads[0].Label, "Make crewlet-growth") {
		t.Fatalf("reads %+v, want the three steps of the make and the three reads of the check", kept.Check.Reads)
	}
	scopes := l.f.tokens[kept.Minted[0].Value].scopes
	if slices.Contains(scopes, "business_management") || !slices.Contains(scopes, "pages_read_engagement") {
		t.Fatalf("minted with %v: the operate level's base and the company's operate permissions, no business_management", scopes)
	}
}

func TestAHandedInTokenIsCheckedBeforeItIsKept(t *testing.T) {
	cases := []struct {
		name   string
		token  func(l *lane) string
		status seataccess.IdentityStatus
		refuse string
	}{
		{name: "an employee assigned at the level is ready", status: seataccess.IdentityReady,
			token: func(l *lane) string {
				l.f.users = append(l.f.users, systemUser{ID: "160", Name: "crewlet-growth", Role: "EMPLOYEE"})
				l.f.tasks["160"] = map[string][]string{"act_111": {taskAdvertise, taskAnalyze}}
				return l.f.handToken("160", "ads_read", "ads_management")
			}},
		{name: "a token short of the level's tasks is a wrong role", status: seataccess.IdentityWrongRole,
			token: func(l *lane) string {
				l.f.users = append(l.f.users, systemUser{ID: "160", Name: "crewlet-growth", Role: "EMPLOYEE"})
				l.f.tasks["160"] = map[string][]string{"act_111": {taskAnalyze}}
				return l.f.handToken("160", "ads_read", "ads_management")
			}},
		{name: "the company's own system user is refused", refuse: "company's system user",
			token: func(l *lane) string {
				l.f.tasks["101"] = map[string][]string{"act_111": {taskAdvertise, taskAnalyze}}
				return l.f.handToken("101", "ads_read", "ads_management")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLane(t, false)
			in := l.input("operate", nil)
			in.Way, in.Value = seataccess.WayHandedIn, tc.token(l)
			kept, err := l.s.KeepIdentity(context.Background(), in)
			if tc.refuse != "" {
				var r *seataccess.Refusal
				if !errors.As(err, &r) || !strings.Contains(r.Said, tc.refuse) {
					t.Fatalf("err %v, want a refusal saying %q", err, tc.refuse)
				}
				return
			}
			if err != nil || kept.Check.Status != tc.status {
				t.Fatalf("kept %+v, %v, want %s", kept.Check, err, tc.status)
			}
			if kept.Minted[0].Revoke != nil {
				t.Fatal("a person's token would be revoked if it could not be sealed; it grants what they gave it")
			}
		})
	}
}

func TestASystemUserAnotherAgentKeepsIsRefused(t *testing.T) {
	l := newLane(t, false)
	l.f.users = append(l.f.users, systemUser{ID: "160", Name: "crewlet-pm", Role: "EMPLOYEE"})
	l.f.tasks["160"] = map[string][]string{"act_111": {taskAdvertise, taskAnalyze}}
	in := l.input("operate", nil)
	in.Way, in.Value = seataccess.WayHandedIn, l.f.handToken("160", "ads_read", "ads_management")
	in.Records = map[string]seataccess.Record{"pm": {Lanes: map[seataccess.Lane]seataccess.LaneRecord{
		seataccess.LaneMetaAds: {Identity: &seataccess.IdentityRecord{ID: "160", Name: "crewlet-pm"}}}}}
	_, err := l.s.KeepIdentity(context.Background(), in)
	var r *seataccess.Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Said, "@pm") {
		t.Fatalf("err %v, want a refusal naming @pm", err)
	}
}

func TestAtTheCapTheAgentStaysOnTheCompanysSystemUser(t *testing.T) {
	l := newLane(t, true)
	for i := range 9 {
		l.f.users = append(l.f.users, systemUser{ID: "30" + string(rune('0'+i)), Name: "other", Role: "EMPLOYEE"})
	}
	in := l.input("operate", nil)
	in.Way = seataccess.WayMade
	kept, err := l.s.KeepIdentity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Check.Status != seataccess.IdentityNone || !strings.Contains(kept.Check.Blocked, "all 10 are used") ||
		!strings.Contains(kept.Check.Blocked, "company's system user") || len(kept.Minted) != 0 {
		t.Fatalf("kept %+v, want nothing kept and the cap said", kept)
	}
	signer, err := seataccess.ResolveIdentity(integration.KindMetaAds, identitySpec, "growth", in.Want,
		&seataccess.IdentityRecord{Attempt: &kept.Check})
	if err != nil || signer.Own {
		t.Fatalf("signer %+v, %v: an agent past the cap calls as the company's system user", signer, err)
	}
}

func TestMakeItForMeWithoutTheAdminTokenSaysHowToGetIt(t *testing.T) {
	l := newLane(t, false)
	in := l.input("operate", nil)
	in.Way = seataccess.WayMade
	_, err := l.s.KeepIdentity(context.Background(), in)
	var r *seataccess.Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Remedy, "Admin system user's token") {
		t.Fatalf("err %v", err)
	}
	g := accessVendor{}.IdentityGuide(l.doc, in.Seat, in.Lane, in.Want)
	if g.Make == nil || g.Make.Unavailable == "" || g.Name != "crewlet-growth" || g.Back.Kind != seataccess.BackToken {
		t.Fatalf("guide %+v", g)
	}
}

func TestAKeptTokenIsRenewedBeforeItsEndAndTheOldOneRevoked(t *testing.T) {
	l := newLane(t, true)
	in := l.input("operate", nil)
	in.Way = seataccess.WayMade
	kept, err := l.s.KeepIdentity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	rec := l.keep(t, kept)
	old := l.vals[l.sink]
	expires := time.Unix(l.f.tokens[old].expires, 0)
	l.s.now = func() time.Time { return expires.Add(-renewAt - time.Hour) }
	got, err := l.s.RefreshIdentity(context.Background(), l.input("operate", rec))
	if err != nil || len(got.Minted) != 0 {
		t.Fatalf("a token with more than %s left was renewed: %+v, %v", renewAt, got, err)
	}
	l.s.now = func() time.Time { return expires.Add(-renewAt + time.Hour) }
	got, err = l.s.RefreshIdentity(context.Background(), l.input("operate", rec))
	if err != nil || len(got.Minted) != 1 || got.Minted[0].Value == old || got.Retire == nil {
		t.Fatalf("refresh %+v, %v, want a new token and the old one's revoke", got, err)
	}
	if l.f.tokens[old].dead {
		t.Fatal("the old token was revoked before the new one was sealed")
	}
	if err := got.Retire(context.Background()); err != nil || !l.f.tokens[old].dead {
		t.Fatalf("retire: %v, old dead %v", err, l.f.tokens[old].dead)
	}
	// THE SAME PASS CHECKS NEXT, its resolver still holding the old token.
	check, err := l.s.CheckIdentity(context.Background(), l.input("operate", rec))
	if err != nil || check.Status != seataccess.IdentityReady {
		t.Fatalf("the check after the renewal read %+v, %v: it must read the renewed token", check, err)
	}
}

func TestAMadeSystemUserFollowsItsAgentsLevelAndAdAccounts(t *testing.T) {
	l := newLane(t, true)
	in := l.input("read", nil)
	in.Way = seataccess.WayMade
	kept, err := l.s.KeepIdentity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	rec := l.keep(t, kept)
	got, err := l.s.RefreshIdentity(context.Background(), l.input("read", rec))
	if err != nil || len(got.Minted) != 0 {
		t.Fatalf("an unchanged agent was remade: %+v, %v", got, err)
	}
	raised := l.input("operate", rec)
	raised.Want.Scope = []string{"act_222"}
	got, err = l.s.RefreshIdentity(context.Background(), raised)
	if err != nil || len(got.Minted) != 1 {
		t.Fatalf("refresh %+v, %v", got, err)
	}
	if tasks := l.f.tasks[kept.ID]; !slices.Equal(tasks["act_222"], seatTasks(growth.LevelOperate)) || tasks["act_111"] != nil {
		t.Fatalf("tasks %v, want act_222 at the operate set and act_111 taken", tasks)
	}
	if !slices.Contains(l.f.tokens[got.Minted[0].Value].scopes, "ads_management") {
		t.Fatal("the new token lacks the operate level's ads_management")
	}
	if err := got.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	check, err := l.s.CheckIdentity(context.Background(), raised)
	if err != nil || check.Status != seataccess.IdentityReady {
		t.Fatalf("check after the remake: %+v, %v", check, err)
	}
}

func TestDroppingAnIdentityLeavesItsSystemUserNothing(t *testing.T) {
	for _, admin := range []bool{true, false} {
		l := newLane(t, true)
		in := l.input("operate", nil)
		in.Way = seataccess.WayMade
		kept, err := l.s.KeepIdentity(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		rec := l.keep(t, kept)
		if !admin {
			l.s.graph.admin = ""
		}
		dropped, err := l.s.DropIdentity(context.Background(), l.input("operate", rec))
		if err != nil || !slices.Equal(dropped.Revoked, []string{SeatTokenField}) || dropped.Waiting != nil {
			t.Fatalf("admin %v: dropped %+v, %v", admin, dropped, err)
		}
		if !l.f.tokens[kept.Minted[0].Value].dead {
			t.Fatalf("admin %v: the token still works", admin)
		}
		if admin && len(l.f.tasks[kept.ID]) != 0 {
			t.Fatalf("its ad accounts stay assigned: %v", l.f.tasks[kept.ID])
		}
	}
}

func TestACheckFindsATokenRevokedAtMeta(t *testing.T) {
	l := newLane(t, true)
	in := l.input("operate", nil)
	in.Way = seataccess.WayMade
	kept, err := l.s.KeepIdentity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	rec := l.keep(t, kept)
	l.f.tokens[l.vals[l.sink]].dead = true
	check, err := l.s.CheckIdentity(context.Background(), l.input("operate", rec))
	if err != nil || check.Status != seataccess.IdentityRefused || check.Reads[0].OK {
		t.Fatalf("check %+v, %v, want refused at the token's read", check, err)
	}
	rec.Check = &check
	if _, err := seataccess.ResolveIdentity(integration.KindMetaAds, identitySpec, "growth", in.Want, rec); err == nil {
		t.Fatal("a refused identity still signs; its calls must stop, never fall back to the company's")
	}
}

// A REPLACEMENT REVOKES THE OLD TOKEN ONLY ONCE THE NEW ONE IS SEALED: the keep
// leaves it working, and its Retire, which the sync calls after sealing,
// revokes it.
func TestAKeptReplacementRevokesTheOldTokenOnlyAfterSealing(t *testing.T) {
	l := newLane(t, true)
	in := l.input("operate", nil)
	in.Way = seataccess.WayMade
	first, err := l.s.KeepIdentity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	rec := l.keep(t, first)
	old := l.vals[l.sink]
	again := l.input("operate", rec)
	again.Way = seataccess.WayMade
	next, err := l.s.KeepIdentity(context.Background(), again)
	if err != nil || next.Check.Status != seataccess.IdentityReady || next.Retire == nil {
		t.Fatalf("keep %+v, %v", next, err)
	}
	if l.f.tokens[old].dead {
		t.Fatal("the old token was revoked before the new one was sealed")
	}
	if err := next.Retire(context.Background()); err != nil || !l.f.tokens[old].dead {
		t.Fatalf("retire: %v, old dead %v", err, l.f.tokens[old].dead)
	}
}

// A FAULT AFTER THE MINT LEAVES NO LIVE TOKEN, and Meta down is a fault to
// retry, never the person's refusal.
func TestAFaultDuringMakeItForMeLeavesNothingAlive(t *testing.T) {
	l := newLane(t, true)
	l.f.refuse["GET /me/adaccounts"] = map[string]any{"code": 1, "message": "An unknown error occurred"}
	in := l.input("operate", nil)
	in.Way = seataccess.WayMade
	_, err := l.s.KeepIdentity(context.Background(), in)
	if _, refused := errors.AsType[*seataccess.Refusal](err); err == nil || refused {
		t.Fatalf("err %v, want a fault", err)
	}
	for v, tok := range l.f.tokens {
		if strings.HasPrefix(v, "EAAGseat") && !tok.dead {
			t.Fatal("the token minted before the fault is still alive")
		}
	}
	l = newLane(t, true)
	l.f.refuse["POST /"+fakeBusiness+"/system_users"] = map[string]any{"status": 503, "code": 2,
		"message": "Service temporarily unavailable"}
	_, err = l.s.KeepIdentity(context.Background(), in)
	if _, refused := errors.AsType[*seataccess.Refusal](err); err == nil || refused {
		t.Fatalf("Meta down answered %v, want a fault the next ask retries", err)
	}
}

// A DROP THAT CANNOT REVOKE KEEPS THE IDENTITY rather than forget a live token.
func TestADropThatCannotRevokeForgetsNothing(t *testing.T) {
	l := newLane(t, true)
	in := l.input("operate", nil)
	in.Way = seataccess.WayMade
	kept, err := l.s.KeepIdentity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	rec := l.keep(t, kept)
	l.s.graph = nil
	if _, err := l.s.DropIdentity(context.Background(), l.input("operate", rec)); err == nil {
		t.Fatal("a drop with no app credentials reported the token revoked")
	}
}

// A FORMER SYSTEM USER'S TOKEN Crewlet could not revoke stays sealed: each
// pass revokes it again, and the check reports it gone once Meta refuses it,
// while the kept one keeps working.
func TestAFormerSystemUsersTokenIsRevokedOnALaterPass(t *testing.T) {
	l := newLane(t, false)
	l.f.users = append(l.f.users, systemUser{ID: "150", Name: "crewlet-old", Role: "EMPLOYEE"},
		systemUser{ID: "160", Name: "crewlet-growth", Role: "EMPLOYEE"})
	l.f.tasks["160"] = map[string][]string{"act_111": {taskAdvertise, taskAnalyze}}
	former := l.f.handToken("150", "ads_read", "ads_management")
	field := seataccess.RetiringField(identitySpec, "150")
	retiringSink := seataccess.SinkName(integration.KindMetaAds, field, "growth")
	l.vals[retiringSink] = former
	l.vals[l.sink] = l.f.handToken("160", "ads_read", "ads_management")
	in := l.input("operate", &seataccess.IdentityRecord{ID: "160", Name: "crewlet-growth",
		Retiring: []seataccess.RetiringIdentity{{ID: "150", Name: "crewlet-old"}}})
	in.Sinks[field] = retiringSink
	check, err := l.s.CheckIdentity(context.Background(), in)
	if err != nil || len(check.RetiringGone) != 0 {
		t.Fatalf("check %+v, %v: a former token Meta still accepts is not gone", check, err)
	}
	if _, err := l.s.RefreshIdentity(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	check, err = l.s.CheckIdentity(context.Background(), in)
	if err != nil || !slices.Equal(check.RetiringGone, []string{"150"}) || check.Status != seataccess.IdentityReady {
		t.Fatalf("check %+v, %v: the refresh revoked the former token, and the kept one works", check, err)
	}
}
