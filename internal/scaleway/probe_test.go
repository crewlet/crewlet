package scaleway_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
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
	setuptest.Said(t, "the Scaleway probe", verdict, typed["secret_key"])
	return verdict
}

// said is everything a verdict tells a person.
func said(v setup.Verdict) string {
	return setuptest.Spoken(v)
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

// farOff is an expiry no test run comes near, so a case about something else
// is not warned of it.
var farOff = time.Date(2099, 1, 2, 0, 0, 0, 0, time.UTC)

var (
	reach = setup.Warning{
		Title:  "It can mint keys for, and grant anything to, any application in the Organization.",
		Action: "Give it a short expiry and rotate it.",
	}
	unpinned = setup.Warning{
		Title:  "No rule limits it to a source address.",
		Action: "On an engine with a fixed egress address, add a condition on `request.ip` to its rule.",
	}
)

// THE NARROW PROVISIONER WORKS: the answer says what the key can manage, what
// it read are facts, and each risk is a warning carrying its action. Mutation:
// write a risk as a fact, or drop an action, and the lists differ.
func TestTheNarrowProvisionerWorksAndIsToldWhatItReaches(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	f.keys[provAccess].ExpiresAt = farOff
	v := probe(t, f, nil)
	if v.Outcome != setup.VerifyWorks {
		t.Fatalf("verdict = %+v, want works", v)
	}
	if want := "Works. The key can manage the agents' applications, policies and keys."; v.Summary != want {
		t.Errorf("summary = %q, want %q", v.Summary, want)
	}
	wantFacts := []setup.Fact{
		{Label: "Application", Value: "crewlet-provisioner"},
		{Label: "Projects", Value: "3"},
		{Label: "Expires", Value: farOff.Format(time.DateOnly)},
	}
	if !slices.Equal(v.Facts, wantFacts) {
		t.Errorf("facts = %+v, want %+v", v.Facts, wantFacts)
	}
	wantWarnings := []setup.Warning{reach, unpinned}
	if !slices.Equal(v.Warnings, wantWarnings) {
		t.Errorf("warnings = %+v, want %+v", v.Warnings, wantWarnings)
	}
	f.onlyScaleway(t)

	pinned := newFakeScaleway()
	pinned.keys[provAccess].ExpiresAt = farOff
	setRules(pinned, scalewayRule{sets: narrow, condition: `request.ip == "203.0.113.7"`})
	v = probe(t, pinned, nil)
	if !slices.Equal(v.Warnings, []setup.Warning{reach}) {
		t.Errorf("a pinned provisioner warns %+v, want only its reach", v.Warnings)
	}
	if i := slices.IndexFunc(v.Facts, func(fact setup.Fact) bool { return fact.Label == "Rule conditions" }); i < 0 ||
		v.Facts[i].Value != `request.ip == "203.0.113.7"` {
		t.Errorf("a pinned provisioner's facts = %+v, want its condition", v.Facts)
	}
}

// AN EXPIRY THAT IS A RISK IS A WARNING WITH ITS ACTION, on top of the date.
func TestAnExpiringProvisionerIsWarnedWithWhatToDo(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		expires time.Time
		value   string
		want    setup.Warning
	}{
		{"never", time.Time{}, "Never", setup.Warning{
			Title: "It never expires.", Action: "Give it an expiry of 30 days or less.",
		}},
		{"soon", time.Now().UTC().AddDate(0, 0, 3), time.Now().UTC().AddDate(0, 0, 3).Format(time.DateOnly), setup.Warning{
			Title: "It expires soon.", Action: "Create its replacement before " +
				time.Now().UTC().AddDate(0, 0, 3).Format(time.DateOnly) + ".",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeScaleway()
			f.keys[provAccess].ExpiresAt = c.expires
			v := probe(t, f, nil)
			if !slices.Contains(v.Facts, setup.Fact{Label: "Expires", Value: c.value}) {
				t.Errorf("facts = %+v, want it to expire %s", v.Facts, c.value)
			}
			if !slices.Contains(v.Warnings, c.want) {
				t.Errorf("warnings = %+v, want %+v", v.Warnings, c.want)
			}
		})
	}
}

// A SET BEYOND THE THREE AND A LEVEL THE ORGANIZATION CANNOT BE GRANTED are
// warnings naming what to do, where a sentence alone left a person nothing to
// act on. Mutation: drop a remedy, and the action is empty.
func TestWhatAProvisionerShouldChangeCarriesItsAction(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	setRules(f, scalewayRule{sets: []string{"IAMManager"}})
	v := probe(t, f, nil)
	if v.Outcome != setup.VerifyWorks || !slices.ContainsFunc(v.Warnings, func(w setup.Warning) bool {
		return strings.Contains(w.Title, `also holds "IAMManager"`) && strings.HasPrefix(w.Action, "Remove it from its policies")
	}) {
		t.Errorf("IAMManager: %+v, want its excess with the action to remove it", v)
	}
	f = newFakeScaleway()
	delete(f.sets, "InstancesFullAccess")
	v = probe(t, f, nil)
	if v.Outcome != setup.VerifyWorks || !slices.ContainsFunc(v.Warnings, func(w setup.Warning) bool {
		return strings.Contains(w.Title, "level is unavailable until") && strings.HasPrefix(w.Action, "Check the Organization")
	}) {
		t.Errorf("a level's gap: %+v, want its reason with its remedy", v)
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
			want: []string{`its policy does not grant "IAMPolicyManager".`, "Organization scope"},
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
			want:  []string{"no API key in the Organization has this access key"},
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
			if !strings.Contains(said(v), want) {
				t.Errorf("%s: %q does not say %q", tc.name, said(v), want)
			}
		}
	}
}

// IAMMANAGER STANDS IN FOR ALL THREE, and is said to be more than Crewlet
// needs; OrganizationManager stands in for projects alone.
func TestWiderSetsStandInAndAreNamed(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	setRules(f, scalewayRule{sets: []string{"IAMManager"}})
	if v := probe(t, f, nil); v.Outcome != setup.VerifyWorks || !strings.Contains(said(v), `also holds "IAMManager"`) {
		t.Errorf("IAMManager: %+v, want works with the excess named", v)
	}
	f = newFakeScaleway()
	setRules(f, scalewayRule{sets: []string{"OrganizationManager"}})
	if v := probe(t, f, nil); v.Outcome != setup.VerifyRefused ||
		!strings.Contains(said(v), `"IAMApplicationManager" and "IAMPolicyManager"`) {
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
