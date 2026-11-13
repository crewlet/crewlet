package aws

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// fakeStack answers the Test button's reads with the account as this engine
// renders it for regions, after edit has changed what it would answer.
func fakeStack(t *testing.T, regions []string, edit func(action, name string, doc *obj, extra *string)) *fakeIAM {
	t.Helper()
	conn := mustConnection(t)
	issuer := conn.CA.Cert.Subject.CommonName
	byARN := map[string]string{}
	for _, n := range append([]string{boundaryName}, levelNames()...) {
		byARN[conn.resolve(policyARN(n))] = n
	}
	own := map[string][2]obj{
		conn.resolve(provisionerName): {provisionerTrustDoc(issuer), provisionerDoc()},
		conn.resolve(brokerName):      {brokerTrustDoc(issuer), brokerDoc()},
	}
	render := func(d obj) string { return url.PathEscape(string(compactJSON(concrete(d, conn.vars())))) }
	f := &fakeIAM{grants: func(cn, profile string) bool {
		return cn == conn.resolve(provisionerCN) && profile == conn.ProvisionerProfileARN
	}}
	f.answer = func(form url.Values) (int, string) {
		action := form.Get("Action")
		switch action {
		case "GetPolicy":
			return iamOK(action, "<Policy><DefaultVersionId>v2</DefaultVersionId></Policy>")
		case "GetPolicyVersion":
			name := byARN[form.Get("PolicyArn")]
			d := levelDocOrBoundary(name, regions)
			edit(action, name, &d, nil)
			return iamOK(action, "<PolicyVersion><Document>"+render(d)+"</Document></PolicyVersion>")
		case "GetRole":
			name := form.Get("RoleName")
			trust, version := own[name][0], "<member><Key>crewlet:stack-version</Key><Value>"+StackVersion+"</Value></member>"
			edit(action, name, &trust, &version)
			return iamOK(action, "<Role><RoleName>"+name+"</RoleName><AssumeRolePolicyDocument>"+render(trust)+
				"</AssumeRolePolicyDocument><Tags>"+version+"</Tags></Role>")
		case "ListRolePolicies":
			return iamOK(action, "<PolicyNames><member>"+form.Get("RoleName")+"</member></PolicyNames><IsTruncated>false</IsTruncated>")
		case "ListAttachedRolePolicies":
			attached := ""
			edit(action, form.Get("RoleName"), nil, &attached)
			return iamOK(action, "<AttachedPolicies>"+attached+"</AttachedPolicies><IsTruncated>false</IsTruncated>")
		case "GetRolePolicy":
			name := form.Get("RoleName")
			d := own[name][1]
			edit(action, name, &d, nil)
			return iamOK(action, "<PolicyDocument>"+render(d)+"</PolicyDocument>")
		case "GetAccountSummary":
			return iamOK(action, "<SummaryMap><entry><key>Roles</key><value>40</value></entry>"+
				"<entry><key>RolesQuota</key><value>1000</value></entry></SummaryMap>")
		}
		return iamError(http.StatusBadRequest, "InvalidAction")
	}
	return f
}

// THE TEST BUTTON HOLDS THE ACCOUNT TO WHAT THIS ENGINE RENDERS: a stack as
// rendered reads clean, and each object edited by hand, a stack from another
// version, or an identity split undone is named.
func TestCheckStackNamesEveryDifference(t *testing.T) {
	t.Parallel()
	regions := []string{"us-east-2", "us-east-1"}
	none := func(string, string, *obj, *string) {}
	r, err := fakeStack(t, regions, none).client(t).CheckStack(context.Background(), regions, false)
	if err != nil || len(r.Drift) != 0 || len(r.Split) != 0 || r.Outdated() || r.Roles != 40 || r.RolesQuota != 1000 {
		t.Fatalf("a rendered stack reads %+v, %v", r, err)
	}
	for _, tc := range []struct {
		name   string
		edit   func(action, name string, doc *obj, extra *string)
		grants func(cn, profile string) bool
		drift  string
		check  func(StackReport) bool
	}{
		{name: "another Region list", drift: "crewlet-acme-seat-boundary", edit: func(a, n string, d *obj, _ *string) {
			if a == "GetPolicyVersion" && n == boundaryName {
				*d = boundaryDoc([]string{"eu-west-1"})
			}
		}},
		{name: "a widened level", drift: "crewlet-acme-level-read", edit: func(a, n string, d *obj, _ *string) {
			if a == "GetPolicyVersion" && n == levelRead {
				*d = doc(stmt("CrewletRead", "Allow", []string{"*"}))
			}
		}},
		{name: "a managed policy on the provisioner", drift: "crewlet-acme-provisioner holds 1 managed",
			edit: func(a, n string, _ *obj, x *string) {
				if a == "ListAttachedRolePolicies" && strings.HasSuffix(n, "provisioner") {
					*x = "<member><PolicyArn>arn:aws:iam::aws:policy/AdministratorAccess</PolicyArn></member>"
				}
			}},
		{name: "a broker trusting the provisioner", drift: "trust policy of crewlet-acme-broker",
			edit: func(a, n string, d *obj, _ *string) {
				if a == "GetRole" && strings.HasSuffix(n, "broker") {
					*d = rolesAnywhereTrust("x", "StringLike", "*")
				}
			}},
		{name: "an older stack", edit: func(a, _ string, _ *obj, x *string) {
			if a == "GetRole" {
				*x = ""
			}
		}, check: StackReport.Outdated},
		{name: "a seat certificate opening the provisioner", grants: func(cn, profile string) bool { return true },
			check: func(r StackReport) bool { return len(r.Split) == 2 }},
	} {
		if tc.edit == nil {
			tc.edit = none
		}
		f := fakeStack(t, regions, tc.edit)
		if tc.grants != nil {
			f.grants = tc.grants
		}
		r, err := f.client(t).CheckStack(context.Background(), regions, false)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if tc.drift != "" && !slices.ContainsFunc(r.Drift, func(s string) bool { return strings.Contains(s, tc.drift) }) {
			t.Errorf("%s: drift %q does not name %q", tc.name, r.Drift, tc.drift)
		}
		if tc.check != nil && !tc.check(r) {
			t.Errorf("%s: %+v", tc.name, r)
		}
	}
}
