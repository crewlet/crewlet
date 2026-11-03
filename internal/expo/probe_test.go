package expo_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

func probe(t *testing.T, f *fakeExpo, values map[string]string) (setup.Verdict, error) {
	t.Helper()
	return expo.Probe(f.server(t))(context.Background(), setup.Check{
		Field: "access_token", Value: func(field string) string { return values[field] },
	})
}

func typed(token, account string) map[string]string {
	return map[string]string{"access_token": token, "account_name": account}
}

// A TOKEN THAT WORKS SAYS SO, naming who it acts as, its role and the
// account's projects, having written nothing at Expo.
func TestTheExpoProbePassesAWorkingToken(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	v, err := probe(t, f, typed(provisioner, testAccountName))
	if err != nil || v.Outcome != setup.VerifyWorks {
		t.Fatalf("verdict = %+v, %v", v, err)
	}
	for _, want := range []string{"the robot provisioner", "Admin role", testAccountName, "3 projects"} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("%q does not say %q", v.Detail, want)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("the probe wrote %v", w)
	}
	f.onlyExpo(t)
}

// EACH FAILURE NAMES ITS CAUSE, and none repeats the token.
func TestTheExpoProbeNamesEachFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		values  map[string]string
		arrange func(*fakeExpo)
		outcome setup.VerifyOutcome
		says    string
	}{
		{"a refused token", typed("fake-wrong-token", testAccountName), nil, setup.VerifyRefused, "Expo refused the token"},
		{"an account the token cannot see", typed(provisioner, "someone-else"), nil, setup.VerifyRefused,
			`no account named "someone-else"`},
		{"a token without Admin", typed(provisioner, testAccountName), func(f *fakeExpo) {
			f.viewerPerms = []expo.Permission{expo.PermissionView, expo.PermissionPublish}
		}, setup.VerifyRefused, "holds PUBLISH, VIEW on example-org, not the Admin role"},
		{"a role Expo will not read", typed(provisioner, testAccountName), func(f *fakeExpo) {
			f.refuse["CrewletViewerPermissions"] = fakeRefusal{code: "UNAUTHORIZED_ERROR", message: "nope"}
		}, setup.VerifyRefused, "refused it reading its role on the account"},
		{"Expo down", typed(provisioner, testAccountName), func(f *fakeExpo) {
			f.refuse["CrewletViewer"] = fakeRefusal{status: http.StatusBadGateway, message: "bad gateway"}
		}, setup.VerifyUnreachable, "502"},
		{"a query Expo's schema refuses", typed(provisioner, testAccountName), func(f *fakeExpo) {
			f.refuse["CrewletAppCount"] = fakeRefusal{code: "GRAPHQL_VALIDATION_FAILED", message: "Cannot query field"}
		}, setup.VerifyUnreachable, "could not answer the check reading the account's projects"},
	} {
		f := newFakeExpo()
		if tc.arrange != nil {
			tc.arrange(f)
		}
		v, err := probe(t, f, tc.values)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if v.Outcome != tc.outcome || !strings.Contains(v.Detail, tc.says) {
			t.Errorf("%s: verdict %+v, want %s saying %q", tc.name, v, tc.outcome, tc.says)
		}
		if strings.Contains(v.Detail, tc.values["access_token"]) {
			t.Errorf("%s: the verdict repeats the token", tc.name)
		}
	}
}

// A CHECK THAT CANNOT BE MADE IS SAID BEFORE ANY CALL.
func TestTheExpoProbeRefusesAnUnusableCheck(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	for _, values := range []map[string]string{typed("", testAccountName), typed(provisioner, " ")} {
		if _, err := probe(t, f, values); !errors.Is(err, setup.ErrUnusableInput) {
			t.Errorf("%v: %v, want unusable", values, err)
		}
	}
	_, err := expo.Probe(f.server(t))(context.Background(), setup.Check{
		Field: "account_name", Value: func(string) string { return "x" },
	})
	if !errors.Is(err, setup.ErrNotCheckable) {
		t.Errorf("the account name was checked on its own: %v", err)
	}
	if len(f.operations()) != 0 {
		t.Errorf("an unusable check called Expo: %v", f.operations())
	}
}

// THE FORM IS THE ACCOUNT, THE TOKEN SEALED AS EXPO_ACCESS_TOKEN AND A HIDDEN
// SWITCH, every field reported absent for a company that has not started.
func TestTheExpoRequirements(t *testing.T) {
	t.Parallel()
	none := func(string) (string, bool) { return "", false }
	reqs := expo.Requirements(nil, none)
	var fields []string
	for _, r := range reqs {
		fields = append(fields, r.Field)
		if r.Present {
			t.Errorf("%s is present on a company that has not started", r.Field)
		}
		if r.Field == "access_token" && (r.SecretName != "EXPO_ACCESS_TOKEN" || !r.Checkable ||
			r.Kind != setup.KindSecret || len(r.CheckWith) != 1 || r.CheckWith[0] != "account_name") {
			t.Errorf("access_token = %+v", r)
		}
	}
	if strings.Join(fields, ",") != "account_name,access_token,enabled" {
		t.Fatalf("fields = %v", fields)
	}
	held := func(name string) (string, bool) { return "x", name == "EXPO_ACCESS_TOKEN" }
	reqs = expo.Requirements(&config.Expo{Enabled: true, Provisioning: &config.ExpoProvisioning{
		AccountName: testAccountName, AccessToken: "${EXPO_ACCESS_TOKEN}",
	}}, held)
	for _, r := range reqs {
		if !r.Present {
			t.Errorf("%s is absent on a connected company", r.Field)
		}
	}
	if !strings.Contains(expo.Summary(), "robot user") {
		t.Errorf("summary = %q", expo.Summary())
	}
}

// THE COMPANY PASS REPORTS A MISSING ACCOUNT AND A PROVISIONER WITHOUT ADMIN
// AS A PERSON'S, and a refused token as the credential's error.
func TestTheExpoCompanyPassFindings(t *testing.T) {
	t.Parallel()
	run := func(f *fakeExpo, account string) (*expo.Result, error) {
		c, err := expo.NewClient(expo.ClientOptions{Token: provisioner, HTTP: f.server(t)})
		if err != nil {
			t.Fatal(err)
		}
		return expo.Reconcile(context.Background(), expo.Options{Client: c, Account: account})
	}
	res, err := run(newFakeExpo(), testAccountName)
	if err != nil || len(res.Findings()) != 0 {
		t.Fatalf("a working provisioner = %+v, %v", res, err)
	}
	res, err = run(newFakeExpo(), "someone-else")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Findings(); len(got) != 1 || got[0].Kind != integration.FindingUnknownTier ||
		got[0].Subject != "integrations.expo.provisioning.account_name" || got[0].Remedy == "" {
		t.Errorf("a missing account = %+v", got)
	}
	f := newFakeExpo()
	f.refuse["CrewletViewer"] = fakeRefusal{code: "UNAUTHORIZED_ERROR", message: "revoked"}
	if _, err := run(f, testAccountName); !errors.Is(err, integration.ErrCredentialRejected) ||
		!strings.Contains(err.Error(), "integrations.expo.provisioning.access_token") {
		t.Errorf("a refused token = %v, want a rejected credential naming the field", err)
	}
	f = newFakeExpo()
	f.refuse["CrewletAppCount"] = fakeRefusal{status: http.StatusServiceUnavailable}
	if _, err := run(f, testAccountName); err == nil || errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("Expo unavailable = %v, want a fault the loop retries", err)
	}
}
