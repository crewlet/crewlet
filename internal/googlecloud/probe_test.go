package googlecloud_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// probeWorld is the Test button against the fake.
type probeWorld struct {
	f     *fakeGoogle
	clock *testClock
	hc    *http.Client
	key   string
}

func newProbeWorld(t *testing.T) *probeWorld {
	t.Helper()
	key, pemText := fakeKey(t)
	clock := newTestClock()
	f := newFakeGoogle(t, &key.PublicKey, clock)
	return &probeWorld{f: f, clock: clock, hc: f.server(t), key: pemText}
}

func (w *probeWorld) check(t *testing.T, values map[string]string) (setup.Verdict, error) {
	t.Helper()
	form := map[string]string{"provisioner": provisioner, "provider": testProvider, "federation_key": w.key}
	for k, v := range values {
		form[k] = v
	}
	verdict, err := googlecloud.ProbeWith(t.Context(), testCompany, func(field string) string { return form[field] },
		w.hc, w.clock.now, w.clock.sleep)
	if err == nil {
		setuptest.Said(t, "the Google Cloud probe", verdict, form["federation_key"])
	}
	return verdict, err
}

// said is everything a verdict tells a person, its summary and every note.
func said(v setup.Verdict) string {
	return strings.Join(append([]string{v.Summary}, v.Notes...), " ")
}

// A WHOLE CONNECTION WORKS AND SAYS WHAT IT REACHES: the projects agents may
// be given by name and ID, each half set-up one with what it lacks, and the
// identity project's account count against Google's default quota.
func TestAWholeConnectionWorksAndSaysWhatItReaches(t *testing.T) {
	t.Parallel()
	w := newProbeWorld(t)
	verdict, err := w.check(t, nil)
	if err != nil || verdict.Outcome != setup.VerifyWorks {
		t.Fatalf("verdict %+v, %v", verdict, err)
	}
	for _, want := range []string{
		provisioner, "Agents may be given 2 projects", "Acme Web (acme-web)",
		projectHalf + " is not offered", "Administer", "holds 1 of Google's default quota of 100",
	} {
		if !strings.Contains(said(verdict), want) {
			t.Errorf("the detail %q does not say %q", said(verdict), want)
		}
	}
	if strings.Contains(said(verdict), projectOther) {
		t.Errorf("a project nobody set up for Crewlet is named: %q", said(verdict))
	}
	w.f.onlyGoogle(t)
}

// EACH HALF THAT REFUSES IS NAMED WITH ITS FIX, and nothing the probe signed or
// minted reaches the sentence.
func TestEachRefusedHalfIsNamedWithItsFix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(*fakeGoogle)
		says    []string
	}{
		{"an untrusted key", func(f *fakeGoogle) {
			other, _ := fakeKey(t)
			f.pub = &other.PublicKey
		}, []string{"Security Token Service", "crewlet:acme", "Cloud Shell block"}},
		{"no Workload Identity User", func(f *fakeGoogle) {
			f.refuse["POST /v1/projects/-/serviceAccounts/"+provisioner+":generateAccessToken"] = http.StatusForbidden
		}, []string{"Federation works", "Workload Identity User", "principal://iam.googleapis.com/"}},
		{"an API not enabled", func(f *fakeGoogle) {
			delete(f.projects[identity].Services, "iamcredentials.googleapis.com")
		}, []string{"iamcredentials.googleapis.com", identity}},
		{"no Service Usage Viewer", func(f *fakeGoogle) {
			f.refuse["GET /v1/projects/"+identity+"/services"] = http.StatusForbidden
		}, []string{"Service Usage Viewer"}},
		{"no Service Account Admin", func(f *fakeGoogle) {
			f.refuse["POST /v1/projects/"+identity+":testIamPermissions"] = http.StatusForbidden
		}, []string{"its own permissions on " + identity}},
	} {
		w := newProbeWorld(t)
		w.f.edit(tc.breakIt)
		verdict, err := w.check(t, nil)
		if err != nil || verdict.Outcome != setup.VerifyRefused {
			t.Errorf("%s: %+v, %v; want refused", tc.name, verdict, err)
			continue
		}
		for _, want := range tc.says {
			if !strings.Contains(said(verdict), want) {
				t.Errorf("%s: %q does not say %q", tc.name, said(verdict), want)
			}
		}
		if strings.Contains(said(verdict), "eyJ") || strings.Contains(said(verdict), "ya29.") {
			t.Errorf("%s: a token reached the sentence: %q", tc.name, said(verdict))
		}
	}
}

// AN ACCOUNT THAT IS NEITHER THE PROVISIONER NOR CREWLET'S IS NAMED: the
// provisioner administers every account in the identity project.
func TestAForeignAccountInTheIdentityProjectIsNamed(t *testing.T) {
	t.Parallel()
	w := newProbeWorld(t)
	w.f.edit(func(f *fakeGoogle) {
		f.accounts["300"] = &fakeAccount{ID: "deploy", Email: "deploy@" + identity + ".iam.gserviceaccount.com", UniqueID: "300"}
	})
	verdict, err := w.check(t, nil)
	if err != nil || verdict.Outcome != setup.VerifyWorks || !strings.Contains(said(verdict), "deploy@"+identity) {
		t.Fatalf("verdict %+v, %v; want the foreign account named", verdict, err)
	}
}

// AN INCOMPLETE FORM IS NOT CHECKED, and only the provider is asked to be.
func TestAnIncompleteFormIsNotChecked(t *testing.T) {
	t.Parallel()
	w := newProbeWorld(t)
	for _, field := range []string{"provisioner", "provider", "federation_key"} {
		if _, err := w.check(t, map[string]string{field: ""}); !errors.Is(err, setup.ErrUnusableInput) {
			t.Errorf("no %s: %v, want unusable input", field, err)
		}
	}
	if _, err := w.check(t, map[string]string{"provider": "crewlet-engine"}); !errors.Is(err, setup.ErrUnusableInput) {
		t.Errorf("a malformed provider: %v, want unusable input", err)
	}
	probe := googlecloud.Probe(w.hc)
	if _, err := probe(context.Background(), setup.Check{Field: "provisioner"}); !errors.Is(err, setup.ErrNotCheckable) {
		t.Errorf("checking the provisioner on its own: %v, want not checkable", err)
	}
}
