package aws

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"
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
