package aws

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// iamRole is one seat role the fake account holds.
type iamRole struct {
	id       string
	policies []string
	inline   []string
}

// fakeAccount is an account whose seat roles the fake IAM lists, reads and
// changes, recording every write.
func fakeAccount(t *testing.T, roles map[string]*iamRole) (*fakeIAM, *accessSession) {
	t.Helper()
	f := &fakeIAM{}
	f.answer = func(form url.Values) (int, string) {
		name := form.Get("RoleName")
		role := roles[name]
		switch action := form.Get("Action"); action {
		case "ListRoles":
			var members string
			for n := range roles {
				members += "<member><RoleName>" + n + "</RoleName><Arn>arn:" + n + "</Arn></member>"
			}
			return iamOK(action, "<Roles>"+members+"</Roles><IsTruncated>false</IsTruncated>")
		case "GetRole":
			slug, _ := Company("acme").slugOf(name)
			return iamOK(action, strings.Replace(roleXML(name, seatTrust(t, slug)), "AROAEXAMPLE", role.id, 1))
		case "ListAttachedRolePolicies":
			var members string
			for _, arn := range role.policies {
				members += "<member><PolicyArn>" + arn + "</PolicyArn></member>"
			}
			return iamOK(action, "<AttachedPolicies>"+members+"</AttachedPolicies><IsTruncated>false</IsTruncated>")
		case "ListRolePolicies":
			var members string
			for _, n := range role.inline {
				members += "<member>" + n + "</member>"
			}
			return iamOK(action, "<PolicyNames>"+members+"</PolicyNames><IsTruncated>false</IsTruncated>")
		case "CreateRole":
			if roles[name] != nil {
				return iamError(http.StatusConflict, "EntityAlreadyExists")
			}
			roles[name] = &iamRole{id: "AROANEW"}
			return iamOK(action, strings.Replace(roleXML(name, form.Get("AssumeRolePolicyDocument")), "AROAEXAMPLE", "AROANEW", 1))
		case "AttachRolePolicy":
			role.policies = append(role.policies, form.Get("PolicyArn"))
			return iamOK(action, "")
		case "DetachRolePolicy":
			role.policies = slices.DeleteFunc(role.policies, func(a string) bool { return a == form.Get("PolicyArn") })
			return iamOK(action, "")
		case "DeleteRole":
			delete(roles, name)
			return iamOK(action, "")
		}
		return iamError(http.StatusBadRequest, "InvalidAction")
	}
	return f, &accessSession{client: f.client(t), await: time.Millisecond}
}

func recorded(handle, id string) seataccess.Record {
	name := Company("acme").SeatRoleName(SeatSlug(handle))
	return seataccess.Record{Kind: integration.KindAWS, Handle: handle,
		Account: &seataccess.Account{ID: id, Name: name, State: seataccess.AccountActive}}
}

// THE VENDOR AGREES WITH THE DOCUMENT: one lane, three fixed levels, the
// session issued at use and nothing to rotate.
func TestTheAWSCatalogIsTheDocuments(t *testing.T) {
	t.Parallel()
	cat := accessVendor{}.Catalog()
	if cat.Kind != integration.KindAWS || !slices.Equal(cat.Credentials, config.SeatSealedFields("aws")) ||
		len(cat.Lanes) != 1 || cat.Lanes[0].Lane != seataccess.LaneAWS || cat.Lanes[0].Rotatable || cat.Lanes[0].Scoped {
		t.Fatalf("%+v", cat)
	}
	opts, _ := (&accessSession{client: &Client{conn: mustConnection(t)}}).Options(context.Background(), seataccess.LaneAWS)
	var ids []string
	for _, l := range opts.Levels {
		ids = append(ids, l.ID)
		if l.Administers != (l.ID == config.AWSLevelAdminister) || l.Note == "" {
			t.Errorf("level %+v", l)
		}
	}
	if !slices.Equal(ids, config.AWSLevels()) {
		t.Errorf("levels %v, want %v", ids, config.AWSLevels())
	}
}

// OFF DENIES BEFORE IT DETACHES, and every op names what it does in IAM; a
// mint or rotate has nothing to take.
func TestAWSStepsDenyBeforeTheyDetach(t *testing.T) {
	t.Parallel()
	steps := func(op seataccess.Op, have string) []seataccess.Step {
		return accessVendor{}.Steps(seataccess.SeatRef{Handle: "sre"}, seataccess.LaneAWS, op,
			seataccess.Desired{Enabled: true, Level: config.AWSLevelRead}, seataccess.Observed{Level: have})
	}
	off := steps(seataccess.OpDisable, config.AWSLevelAdminister)
	if len(off) != 2 || off[0].Verb != seataccess.VerbDisable || !strings.Contains(off[0].Text, "deny-all") ||
		off[1].Verb != seataccess.VerbRevoke || off[1].Index != 1 || off[0].Note == "" {
		t.Errorf("off: %+v", off)
	}
	if lower := steps(seataccess.OpLevel, config.AWSLevelAdminister); len(lower) != 1 || lower[0].Note == "" {
		t.Errorf("leaving Administer carries no caveat: %+v", lower)
	}
	if create := steps(seataccess.OpCreate, ""); len(create) != 2 || create[1].Verb != seataccess.VerbGrant {
		t.Errorf("create: %+v", create)
	}
	for _, op := range []seataccess.Op{seataccess.OpMint, seataccess.OpRotate, seataccess.OpRevoke, seataccess.OpRetire} {
		if got := steps(op, ""); got != nil {
			t.Errorf("%s: %+v", op, got)
		}
	}
}

// A ROLE IS THE SEAT'S ONLY WHERE THE LEDGER RECORDS IT, by name and role ID:
// an unrecorded role, or a role of the name with another ID, reads as absent.
// Its level is one only where its policies are exactly a level's and the
// company's IAM read option, so a change of the option is a level step.
func TestObserveReadsOnlyRecordedRoles(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	name := func(h string) string { return Company("acme").SeatRoleName(SeatSlug(h)) }
	operate := conn.Policies(Access{Level: config.AWSLevelOperate})
	roles := map[string]*iamRole{
		name("sre"):      {id: "AROASRE", policies: operate},
		name("intruder"): {id: "AROAX", policies: operate},
		name("swapped"):  {id: "AROANEWER", policies: operate},
		name("off"):      {id: "AROAOFF", policies: conn.Policies(Access{Off: true})},
		name("foreign"):  {id: "AROAF", policies: append(slices.Clone(operate), "arn:aws:iam::aws:policy/AdministratorAccess")},
	}
	_, s := fakeAccount(t, roles)
	in := seataccess.ObserveInput{
		Seats: []seataccess.SeatRef{{Handle: "sre"}, {Handle: "intruder"}, {Handle: "absent"}},
		Records: map[string]seataccess.Record{
			"sre": recorded("sre", "AROASRE"), "swapped": recorded("swapped", "AROAOLD"),
			"off": recorded("off", "AROAOFF"), "foreign": recorded("foreign", "AROAF"),
		},
	}
	got, err := s.Observe(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	at := func(h string) seataccess.Observed { return got[h][seataccess.LaneAWS] }
	if o := at("sre"); !o.Exists || !o.Enabled || o.Level != config.AWSLevelOperate || o.AccountID != "AROASRE" {
		t.Errorf("sre: %+v", o)
	}
	for _, h := range []string{"intruder", "swapped", "absent"} {
		if at(h).Exists {
			t.Errorf("%s read as the seat's own role: %+v", h, at(h))
		}
	}
	if o := at("off"); !o.Exists || o.Enabled {
		t.Errorf("off: %+v", o)
	}
	if o := at("foreign"); !o.Exists || o.Level != "" {
		t.Errorf("a role holding a foreign policy reads as a level: %+v", o)
	}
	s.iamRead = true
	if got, _ := s.Observe(context.Background(), in); got["sre"][seataccess.LaneAWS].Level != "" {
		t.Error("a role without the company's IAM read option reads as its level")
	}
}

// A CREATE MAKES THE ROLE, THEN ATTACHES ITS LEVEL; OFF DENIES, THEN DETACHES;
// REMOVE DELETES. A role of the name the ledger does not record is refused.
func TestApplyTakesEachOpInIAM(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	ctx := context.Background()
	roles := map[string]*iamRole{}
	f, s := fakeAccount(t, roles)
	s.iamRead = true
	seat := seataccess.SeatRef{Handle: "sre"}
	want := seataccess.Desired{Enabled: true, Level: config.AWSLevelOperate}
	create := accessVendor{}.Steps(seat, seataccess.LaneAWS, seataccess.OpCreate, want, seataccess.Observed{})
	made, err := s.Apply(ctx, seataccess.ApplyInput{Seat: seat, Step: create[0], Want: want})
	if err != nil || made.AccountID != "AROANEW" {
		t.Fatalf("create: %+v %v", made, err)
	}
	if _, err := s.Apply(ctx, seataccess.ApplyInput{Seat: seat, Step: create[1], Want: want,
		Prior: []seataccess.StepResult{made}}); err != nil {
		t.Fatal(err)
	}
	role := roles[made.Account]
	if !slices.Equal(role.policies, conn.Policies(Access{Level: config.AWSLevelOperate, IAMRead: true})) {
		t.Fatalf("a new Operate role holds %v", role.policies)
	}
	rec := recorded("sre", "AROANEW")
	off := accessVendor{}.Steps(seat, seataccess.LaneAWS, seataccess.OpDisable, seataccess.Desired{}, seataccess.Observed{})
	res, err := s.Apply(ctx, seataccess.ApplyInput{Seat: seat, Step: off[0], Record: &rec})
	if err != nil || !res.Disabled || !slices.Contains(role.policies, conn.resolve(denyAllARN)) || len(role.policies) != 4 {
		t.Fatalf("off's first step: %+v %v, holds %v", res, err, role.policies)
	}
	if _, err := s.Apply(ctx, seataccess.ApplyInput{Seat: seat, Step: off[1], Record: &rec}); err != nil ||
		!slices.Equal(role.policies, []string{conn.resolve(denyAllARN)}) {
		t.Fatalf("off's second step: %v, holds %v", err, role.policies)
	}
	remove := accessVendor{}.Steps(seat, seataccess.LaneAWS, seataccess.OpRemove, seataccess.Desired{}, seataccess.Observed{})
	if res, err := s.Apply(ctx, seataccess.ApplyInput{Seat: seat, Step: remove[0], Record: &rec}); err != nil ||
		!res.Gone || roles[made.Account] != nil {
		t.Fatalf("remove: %+v %v", res, err)
	}
	roles[made.Account] = &iamRole{id: "AROAHAND"}
	_, err = s.Apply(ctx, seataccess.ApplyInput{Seat: seat, Step: create[0], Want: want})
	if r, ok := errors.AsType[*seataccess.Refusal](err); !ok || r.Actor != seataccess.ByYou ||
		!strings.Contains(r.Said, "no record") {
		t.Errorf("an unrecorded role of the name: %v", err)
	}
	if n := len(f.actions()); n == 0 {
		t.Fatal("no IAM call was made")
	}
}

// A ROLE HOLDING WHAT THE PROVISIONER CANNOT CHANGE IS THE PERSON'S: a level
// step refuses it, naming the grant, and writes nothing.
func TestALevelStepRefusesAForeignGrant(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	name := Company("acme").SeatRoleName(SeatSlug("sre"))
	roles := map[string]*iamRole{name: {id: "AROASRE", policies: conn.Policies(Access{Level: config.AWSLevelRead}),
		inline: []string{"hand-made"}}}
	f, s := fakeAccount(t, roles)
	rec := recorded("sre", "AROASRE")
	want := seataccess.Desired{Enabled: true, Level: config.AWSLevelOperate}
	step := accessVendor{}.Steps(seataccess.SeatRef{Handle: "sre"}, seataccess.LaneAWS, seataccess.OpLevel, want, seataccess.Observed{})
	_, err := s.Apply(context.Background(), seataccess.ApplyInput{Seat: seataccess.SeatRef{Handle: "sre"}, Step: step[0],
		Want: want, Record: &rec})
	if r, ok := errors.AsType[*seataccess.Refusal](err); !ok || !strings.Contains(r.Said, "hand-made") {
		t.Fatalf("%v", err)
	}
	if slices.Contains(f.actions(), "AttachRolePolicy") {
		t.Error("a write was made beside a foreign grant")
	}
}
