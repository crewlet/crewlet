package aws

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

func policyMember(name, path string) string {
	return "<member><PolicyName>" + name + "</PolicyName><Path>" + path + "</Path><Arn>arn:aws:iam::aws:policy" +
		path + name + "</Arn></member>"
}

// THE CATALOGUE IS AWS'S OWN, every page, less what no grant offers: the
// policies that hand out identity, the organization, billing or every service,
// and those AWS made for a service to act as itself.
func TestGrantablePoliciesAreAWSsCatalogueLessTheRefused(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{answer: func(form url.Values) (int, string) {
		if form.Get("Action") != "ListPolicies" {
			return iamError(http.StatusBadRequest, "Unexpected")
		}
		if form.Get("Marker") == "" {
			return iamOK("ListPolicies", "<IsTruncated>true</IsTruncated><Marker>p2</Marker><Policies>"+
				policyMember("AmazonS3FullAccess", "/")+policyMember("AdministratorAccess", "/")+
				policyMember("IAMFullAccess", "/")+policyMember("AWSLambdaRole", "/service-role/")+"</Policies>")
		}
		return iamOK("ListPolicies", "<IsTruncated>false</IsTruncated><Policies>"+
			policyMember("AmazonEC2ReadOnlyAccess", "/")+policyMember("ViewOnlyAccess", "/job-function/")+
			policyMember("AWSBillingReadOnlyAccess", "/")+"</Policies>")
	}}
	got, err := f.client(t).GrantablePolicies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if want := []string{"AmazonEC2ReadOnlyAccess", "AmazonS3FullAccess", "ViewOnlyAccess"}; !slices.Equal(names, want) {
		t.Fatalf("offered %v, want %v", names, want)
	}
	f.mu.Lock()
	forms := slices.Clone(f.calls)
	f.mu.Unlock()
	if len(forms) != 2 || forms[0].Get("Scope") != "AWS" || forms[0].Get("PolicyUsageFilter") != "PermissionsPolicy" {
		t.Fatalf("listed with %v", forms)
	}
	for _, p := range got {
		if write := p.Name == "AmazonS3FullAccess"; p.Write() != write {
			t.Errorf("%s reads as write %v", p.Name, p.Write())
		}
	}
}

// EVERY REFUSED POLICY STAYS REFUSED, so a grant never hands an agent identity
// administration, the organization, billing, secrets or the audit trail.
func TestRefusedPoliciesCoverTheNeverGrantedCategories(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"AdministratorAccess", "AdministratorAccess-Amplify", "PowerUserAccess", "IAMFullAccess", "IAMReadOnlyAccess",
		"AWSSSOMasterAccountAdministrator", "AWSOrganizationsFullAccess", "AWSAccountManagementFullAccess",
		"AWSBillingConductorFullAccess", "AWSBudgetsActionsWithAWSResourceControlAccess", "SecretsManagerReadWrite",
		"AWSKeyManagementServicePowerUser", "AWSCloudTrail_FullAccess", "AWSControlTowerServiceRolePolicy",
	} {
		if Refused(ManagedPolicy{Name: name, Path: "/"}) == "" {
			t.Errorf("%s is offered as a grant", name)
		}
	}
	for _, name := range []string{"AmazonS3FullAccess", "AmazonRDSReadOnlyAccess", "CloudWatchLogsFullAccess"} {
		if why := Refused(ManagedPolicy{Name: name, Path: "/"}); why != "" {
			t.Errorf("%s is refused: %s", name, why)
		}
	}
}

// A GRANT ATTACHES BESIDE THE LEVEL AND READS BACK AS ITSELF: a Read role
// granted AmazonS3FullAccess holds it beside Read, observes as Read with the
// grant, and a lapse detaches it alone. Mutation: drop the grants from want
// and the policy is never attached.
func TestAGrantAttachesBesideTheLevelAndLapsesAlone(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	ctx := context.Background()
	name := Company("acme").SeatRoleName(SeatSlug("sre"))
	roles := map[string]*iamRole{name: {id: "AROASRE", policies: conn.Policies(Access{Level: config.AWSLevelRead})}}
	_, s := fakeAccount(t, roles)
	rec := recorded("sre", "AROASRE")
	seat := seataccess.SeatRef{Handle: "sre"}
	s3 := "arn:aws:iam::aws:policy/AmazonS3FullAccess"
	want := seataccess.Desired{Enabled: true, Level: config.AWSLevelRead, Grants: []seataccess.Grant{{Permission: s3}}}
	steps := accessVendor{}.Steps(seat, seataccess.LaneAWS, seataccess.OpGrants, want, seataccess.Observed{})
	if len(steps) != 1 || steps[0].Verb != seataccess.VerbGrant || !strings.Contains(steps[0].Text, "AmazonS3FullAccess") {
		t.Fatalf("steps %+v", steps)
	}
	res, err := s.Apply(ctx, seataccess.ApplyInput{Seat: seat, Step: steps[0], Want: want, Record: &rec})
	if err != nil || res.Settling == nil || !slices.Contains(roles[name].policies, s3) {
		t.Fatalf("grant: %+v %v, holds %v", res, err, roles[name].policies)
	}
	got, err := s.Observe(ctx, seataccess.ObserveInput{Seats: []seataccess.SeatRef{seat},
		Records: map[string]seataccess.Record{"sre": rec}})
	if err != nil {
		t.Fatal(err)
	}
	if o := got["sre"][seataccess.LaneAWS]; o.Level != config.AWSLevelRead || len(o.Grants) != 1 || o.Grants[0].Permission != s3 {
		t.Fatalf("observed %+v, want Read with the grant", o)
	}

	lapsed := seataccess.Desired{Enabled: true, Level: config.AWSLevelRead}
	steps = accessVendor{}.Steps(seat, seataccess.LaneAWS, seataccess.OpGrants, lapsed,
		seataccess.Observed{Grants: []seataccess.Grant{{Permission: s3}}})
	if len(steps) != 1 || steps[0].Verb != seataccess.VerbUngrant {
		t.Fatalf("lapse steps %+v", steps)
	}
	if _, err := s.Apply(ctx, seataccess.ApplyInput{Seat: seat, Step: steps[0], Want: lapsed, Record: &rec}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(roles[name].policies, conn.Policies(Access{Level: config.AWSLevelRead})) {
		t.Fatalf("after the lapse the role holds %v, want Read alone", roles[name].policies)
	}
}

// A POLICY NO GRANT OFFERS IS NEVER ATTACHED, even from a document written
// around the save: the step is refused and nothing is written.
func TestARefusedPolicyIsNeverAttached(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	name := Company("acme").SeatRoleName(SeatSlug("sre"))
	roles := map[string]*iamRole{name: {id: "AROASRE", policies: conn.Policies(Access{Level: config.AWSLevelRead})}}
	f, s := fakeAccount(t, roles)
	rec := recorded("sre", "AROASRE")
	for _, arn := range []string{"arn:aws:iam::aws:policy/AdministratorAccess",
		"arn:aws:iam::123456789012:policy/mine", "arn:aws:iam::aws:policy/service-role/AWSLambdaRole"} {
		want := seataccess.Desired{Enabled: true, Level: config.AWSLevelRead, Grants: []seataccess.Grant{{Permission: arn}}}
		step := accessVendor{}.Steps(seataccess.SeatRef{Handle: "sre"}, seataccess.LaneAWS, seataccess.OpGrants, want,
			seataccess.Observed{})[0]
		_, err := s.Apply(context.Background(), seataccess.ApplyInput{Seat: seataccess.SeatRef{Handle: "sre"},
			Step: step, Want: want, Record: &rec})
		if r, ok := errors.AsType[*seataccess.Refusal](err); !ok || r.Actor != seataccess.ByYou {
			t.Errorf("%s: %v, want a refusal", arn, err)
		}
	}
	if slices.Contains(f.actions(), "AttachRolePolicy") {
		t.Error("a refused policy was attached")
	}
}

// THE SESSION'S CATALOGUE IS AWS'S, NAMED AND MARKED: each entry is the
// policy's ARN, named as IAM names it, applying to the whole account, read or
// write by its name; a stack that cannot list them yet offers none.
func TestTheGrantCatalogueOffersAWSManagedPolicies(t *testing.T) {
	t.Parallel()
	var denied atomic.Bool
	f := &fakeIAM{answer: func(form url.Values) (int, string) {
		if denied.Load() {
			return iamError(http.StatusForbidden, "AccessDenied")
		}
		return iamOK("ListPolicies", "<IsTruncated>false</IsTruncated><Policies>"+
			policyMember("AmazonS3FullAccess", "/")+policyMember("ViewOnlyAccess", "/job-function/")+"</Policies>")
	}}
	s := &accessSession{client: f.client(t)}
	entries, scope, err := s.GrantCatalogue(context.Background(), seataccess.LaneAWS)
	if err != nil || scope != nil || len(entries) != 2 {
		t.Fatalf("%+v %v %v", entries, scope, err)
	}
	s3, view := entries[0], entries[1]
	if s3.ID != "arn:aws:iam::aws:policy/AmazonS3FullAccess" || s3.Name != "AmazonS3FullAccess" || !s3.Write ||
		s3.ScopeKind != seataccess.GrantScopeAccount || view.Write || view.Group != jobFunctions {
		t.Fatalf("entries %+v", entries)
	}
	denied.Store(true)
	old := &accessSession{client: f.client(t)}
	if entries, _, err := old.GrantCatalogue(context.Background(), seataccess.LaneAWS); err != nil || len(entries) != 0 {
		t.Fatalf("an older stack: %+v %v, want nothing offered", entries, err)
	}
}

// EVERY POLICY THE CATALOGUE OFFERS CAN BE ASKED FOR, each at its longest,
// one named at IAM's longest policy name ([seataccesstest.AskFaults]).
func TestEveryPolicyTheCatalogueOffersCanBeAsked(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{answer: func(url.Values) (int, string) {
		return iamOK("ListPolicies", "<IsTruncated>false</IsTruncated><Policies>"+
			policyMember("AmazonS3FullAccess", "/")+policyMember("ViewOnlyAccess", "/job-function/")+
			policyMember(strings.Repeat("P", maxPolicyName), "/")+"</Policies>")
	}}
	s := &accessSession{client: f.client(t)}
	entries, scope, err := s.GrantCatalogue(t.Context(), seataccess.LaneAWS)
	if err != nil || len(entries) != 3 {
		t.Fatalf("%+v %v", entries, err)
	}
	opts := seataccess.Options{Lane: seataccess.LaneAWS, Grants: entries, GrantScope: scope}
	for _, fault := range seataccesstest.AskFaults(opts) {
		t.Error(fault)
	}
}

// maxPolicyName is the longest name IAM gives a policy.
const maxPolicyName = 128
