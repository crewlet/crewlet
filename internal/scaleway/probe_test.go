package scaleway_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/setup"
)

// probe runs the Test button against the fake with the provisioner's own
// values, each overridable.
func probe(t *testing.T, f *fakeScaleway, values map[string]string) setup.Verdict {
	t.Helper()
	typed := map[string]string{"organization_id": testOrg, "access_key": provAccess, "secret_key": provSecret}
	for k, v := range values {
		typed[k] = v
	}
	verdict, err := scaleway.Probe(f.server(t))(t.Context(), setup.Check{
		Field: "secret_key", Value: func(field string) string { return typed[field] },
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return verdict
}

// setRules replaces the provisioner's own rules.
func setRules(f *fakeScaleway, rules ...scalewayRule) {
	f.edit(func(f *fakeScaleway) {
		p := f.policies["pol-provisioner"]
		p.Rules = nil
		for _, r := range rules {
			org := testOrg
			p.Rules = append(p.Rules, scaleway.Rule{PermissionSetNames: r.sets, OrganizationID: &org, Condition: r.condition})
		}
	})
}

type scalewayRule struct {
	sets      []string
	condition string
}

var narrow = []string{"IAMApplicationManager", "IAMPolicyManager", "ProjectReadOnly"}

// THE NARROW PROVISIONER WORKS, and the sentence says plainly what the key can
// reach and that it should be short-lived, and recommends the IP pin it does
// not carry; with the pin, the recommendation goes.
func TestTheNarrowProvisionerWorksAndIsToldWhatItReaches(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	v := probe(t, f, nil)
	if v.Outcome != setup.VerifyWorks {
		t.Fatalf("verdict = %+v, want works", v)
	}
	for _, want := range []string{
		"crewlet-provisioner", "3 projects", "mint keys for, and grant anything to, any application",
		"short expiry and rotate it", "expires on 2027-10-01", "request.ip",
	} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("the verdict %q does not say %q", v.Detail, want)
		}
	}
	if strings.Contains(v.Detail, "\u2014") {
		t.Errorf("the verdict %q carries an em dash", v.Detail)
	}
	f.onlyScaleway(t)

	pinned := newFakeScaleway()
	setRules(pinned, scalewayRule{sets: narrow, condition: `request.ip == "203.0.113.7"`})
	if v := probe(t, pinned, nil); v.Outcome != setup.VerifyWorks || strings.Contains(v.Detail, "No rule limits it") {
		t.Errorf("a pinned provisioner: %+v, want works without the pin recommendation", v)
	}
}

// WHAT THE PROVISIONER CANNOT PROVISION WITH IS REFUSED, naming what to change.
// Mutation: read a condition on the resource as the IP pin, and the
// conditioned key rule works.
func TestAProvisionerThatCannotProvisionIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(f *fakeScaleway)
		typed map[string]string
		want  []string
	}{
		{
			name: "a missing set",
			setup: func(f *fakeScaleway) {
				setRules(f, scalewayRule{sets: []string{"IAMApplicationManager", "ProjectReadOnly"}})
			},
			want: []string{"IAMPolicyManager", "Organization scope"},
		},
		{
			name: "a name condition on the key rule",
			setup: func(f *fakeScaleway) {
				setRules(f, scalewayRule{sets: narrow, condition: `resource.name.startsWith("crewlet-")`})
			},
			want: []string{"IAMApplicationManager carries the condition", "no attribute naming its application", "request.ip"},
		},
		{
			name: "a condition refusing the direct read of its own key",
			setup: func(f *fakeScaleway) {
				setRules(f, scalewayRule{sets: narrow, condition: `request.user_agent != ""`})
				f.refuseDirectKey = true
			},
			want: []string{"refused the key reading itself"},
		},
		{
			name:  "a user's key",
			setup: func(f *fakeScaleway) { f.keys[provAccess].AppID, f.keys[provAccess].UserID = "", "user-1" },
			want:  []string{"belongs to a user", "application's key only"},
		},
		{
			name:  "another organization's application",
			setup: func(f *fakeScaleway) { f.apps[provAppID].Org = otherOrg },
			want:  []string{"not in the Organization " + testOrg},
		},
		{
			name:  "a mistyped secret",
			setup: func(*fakeScaleway) {},
			typed: map[string]string{"secret_key": "00000000-0000-4000-8000-0000000000ff"},
			want:  []string{"does not accept this secret key"},
		},
		{
			name:  "another key's access key",
			setup: func(*fakeScaleway) {},
			typed: map[string]string{"access_key": "SCWSOMEBODYELSE00001"},
			want:  []string{"no API key with this access key"},
		},
		{
			name:  "no policy read",
			setup: func(f *fakeScaleway) { f.refuse["policies"] = http.StatusForbidden },
			want:  []string{"its own policies", "IAMPolicyManager"},
		},
		{
			name:  "no Read level",
			setup: func(f *fakeScaleway) { delete(f.sets, "InstancesReadOnly") },
			want:  []string{"no agent can be given a level yet", "InstancesReadOnly"},
		},
	}
	for _, tc := range cases {
		f := newFakeScaleway()
		// BEFORE THE SERVER STARTS, so nothing else reads the fake yet.
		tc.setup(f)
		v := probe(t, f, tc.typed)
		if v.Outcome != setup.VerifyRefused {
			t.Errorf("%s: verdict = %+v, want refused", tc.name, v)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(v.Detail, want) {
				t.Errorf("%s: %q does not say %q", tc.name, v.Detail, want)
			}
		}
		if strings.Contains(v.Detail, provSecret) {
			t.Errorf("%s: the verdict quotes the secret", tc.name)
		}
	}
}

// IAMMANAGER STANDS IN FOR ALL THREE, and is said to be more than Crewlet
// needs; OrganizationManager stands in for projects alone.
func TestWiderSetsStandInAndAreNamed(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	setRules(f, scalewayRule{sets: []string{"IAMManager"}})
	if v := probe(t, f, nil); v.Outcome != setup.VerifyWorks || !strings.Contains(v.Detail, `also holds "IAMManager"`) {
		t.Errorf("IAMManager: %+v, want works with the excess named", v)
	}
	f = newFakeScaleway()
	setRules(f, scalewayRule{sets: []string{"OrganizationManager"}})
	if v := probe(t, f, nil); v.Outcome != setup.VerifyRefused ||
		!strings.Contains(v.Detail, `"IAMApplicationManager" and "IAMPolicyManager"`) {
		t.Errorf("OrganizationManager: %+v, want the two IAM sets missing", v)
	}
}

// A FIELD THAT CANNOT BE CHECKED, OR A FORM MISSING WHAT THE CHECK NEEDS, IS
// SAID AS SUCH AND SENDS NOTHING.
func TestAnIncompleteFormIsNotChecked(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	hc := f.server(t)
	for name, typed := range map[string]map[string]string{
		"no secret":       {"organization_id": testOrg, "access_key": provAccess},
		"no access key":   {"organization_id": testOrg, "secret_key": provSecret},
		"no organization": {"access_key": provAccess, "secret_key": provSecret},
	} {
		_, err := scaleway.Probe(hc)(t.Context(), setup.Check{
			Field: "secret_key", Value: func(field string) string { return typed[field] },
		})
		if err == nil {
			t.Errorf("%s: checked", name)
		}
	}
	if _, err := scaleway.Probe(hc)(t.Context(), setup.Check{Field: "access_key",
		Value: func(string) string { return "" }}); err == nil {
		t.Error("the access key alone was checked")
	}
	if len(f.requests) != 0 {
		t.Errorf("an incomplete form sent %v", f.requests)
	}
}
