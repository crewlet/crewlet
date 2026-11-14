package scaleway_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/scaleway"
)

// memSink is a [provision.TokenSink] that keeps what it is given, and fails
// a Record of one name where a test asks it to.
type memSink struct {
	values map[string]string
	failOn string
}

func newMemSink() *memSink { return &memSink{values: map[string]string{}} }

func (s *memSink) Record(_ context.Context, name, value string) error {
	if name == s.failOn {
		return errors.New("the store is unreachable")
	}
	s.values[name] = value
	return nil
}

func (s *memSink) Forget(_ context.Context, names ...string) error {
	for _, name := range names {
		delete(s.values, name)
	}
	return nil
}

func (s *memSink) Discard(context.Context) error { return nil }
func (s *memSink) Flush(context.Context) error   { return nil }
func (s *memSink) Describe() string              { return "memory" }
func (s *memSink) NextStep() string              { return "" }
func (s *memSink) Value(_ context.Context, name string) (string, bool, error) {
	v, ok := s.values[name]
	return v, ok, nil
}

var _ provision.TokenSink = (*memSink)(nil)

const atlasAppName = "crewlet-atlas-acme"

// atlasPass runs one convergence of the atlas application against the fake,
// with what the sink holds as what is sealed.
func (f *fakeScaleway) atlasPass(t *testing.T, sink *memSink, want bool, edit ...func(*scaleway.AtlasOptions)) []integration.Finding {
	t.Helper()
	found, err := f.atlasPassErr(t, sink, want, edit...)
	if err != nil {
		t.Fatalf("ReconcileAtlas: %v", err)
	}
	return found
}

func (f *fakeScaleway) atlasPassErr(t *testing.T, sink *memSink, want bool, edit ...func(*scaleway.AtlasOptions)) ([]integration.Finding, error) {
	t.Helper()
	client, err := scaleway.NewClient(scaleway.ClientOptions{OrganizationID: testOrg, SecretKey: provSecret, HTTP: f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	opts := scaleway.AtlasOptions{
		Client: client, Company: "Acme", Want: want, Sink: sink,
		ApplicationID: sink.values[scaleway.AtlasApplicationName],
		AccessKey:     sink.values[scaleway.AtlasAccessName],
		SecretHeld:    sink.values[scaleway.AtlasSecretName] != "",
		Now:           func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
	}
	for _, e := range edit {
		e(&opts)
	}
	return scaleway.ReconcileAtlas(t.Context(), opts)
}

func titles(found []integration.Finding) []string {
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.Title)
	}
	return out
}

// THE ATLAS APPLICATION HOLDS READ-ONLY SETS AND NOTHING ELSE: every set is
// named, none is a wildcard over products, none writes, none is IAM, billing,
// secrets or keys, none reads an object's contents, and none is on the list
// no agent may hold.
//
// Mutation: add AllProductsReadOnly, ObjectStorageReadOnly or a FullAccess
// set to the table, and this fails.
func TestTheAtlasApplicationHoldsNamedReadOnlySetsAlone(t *testing.T) {
	t.Parallel()
	sets := scaleway.AtlasSets()
	if len(sets) != 9 {
		t.Fatalf("the atlas application holds %d sets, want the nine products the atlas draws: %v", len(sets), sets)
	}
	for _, set := range sets {
		if slices.Contains(scaleway.NeverGranted(), set) {
			t.Errorf("%s is on the list no agent may hold", set)
		}
		if set != "ObjectStorageBucketsRead" && !strings.HasSuffix(set, "ReadOnly") {
			t.Errorf("%s is not a read-only set", set)
		}
		for _, banned := range []string{"AllProducts", "IAM", "Billing", "Secret", "KeyManager", "SSHKeys", "Objects"} {
			if strings.Contains(set, banned) {
				t.Errorf("%s is a %s set", set, banned)
			}
		}
	}
}

// SWITCHED ON, THE PASS MAKES ONE APPLICATION, ONE POLICY AND ONE KEY, AND A
// SECOND PASS WRITES NOTHING: the application is named for the atlas and the
// company, its policy is one rule of the read-only sets on the Organization,
// which is every project, its key expires in 90 days and is sealed with its
// access key and the application's ID, and a converged pass is all reads.
//
// Mutation: write the policy on every pass, or mint a key while the sealed
// one works, and the second pass writes.
func TestThePassMakesTheAtlasApplicationOnceAndThenWritesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	sink := newMemSink()
	if found := f.atlasPass(t, sink, true); len(found) != 0 {
		t.Fatalf("the first pass found %+v", found)
	}
	app, ok := f.appNamed(atlasAppName)
	if !ok || !strings.Contains(app.Description, "Atlas") {
		t.Fatalf("no application named for the atlas: %+v", app)
	}
	policies := f.policiesOf(app.ID)
	if len(policies) != 1 || policies[0].Name != atlasAppName || len(policies[0].Rules) != 1 {
		t.Fatalf("the application's policies: %+v", policies)
	}
	rule := policies[0].Rules[0]
	if !rule.OnOrganization() || *rule.OrganizationID != testOrg || len(rule.ProjectIDs) != 0 ||
		!slices.Equal(rule.PermissionSetNames, scaleway.AtlasSets()) {
		t.Errorf("the rule: %+v", rule)
	}
	keys := f.keysOf(app.ID)
	if len(keys) != 1 || keys[0].Description != "crewlet atlas" ||
		!keys[0].ExpiresAt.Equal(f.now.Add(scaleway.KeyLifetime)) {
		t.Fatalf("the application's keys: %+v", keys)
	}
	want := map[string]string{
		scaleway.AtlasApplicationName: app.ID, scaleway.AtlasAccessName: keys[0].Access,
		scaleway.AtlasSecretName: keys[0].Secret,
	}
	if !maps.Equal(sink.values, want) {
		t.Errorf("sealed names %v, want the three", slices.Sorted(maps.Keys(sink.values)))
	}
	wrote := f.wrote()
	if found := f.atlasPass(t, sink, true); len(found) != 0 || !slices.Equal(f.wrote(), wrote) {
		t.Errorf("a converged pass found %+v and wrote %v", found, f.wrote()[len(wrote):])
	}
}

// A CHECK WRITES NOTHING: with no sink, nothing is made at Scaleway, and the
// check says the next pass makes it.
func TestACheckMakesNoAtlasApplication(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	found := f.atlasPass(t, newMemSink(), true, func(o *scaleway.AtlasOptions) { o.Sink = nil })
	if len(f.wrote()) != 0 || !slices.Equal(titles(found), []string{"Atlas application not made yet"}) {
		t.Errorf("a check wrote %v and found %v", f.wrote(), titles(found))
	}
	for _, finding := range found {
		if phase, _ := finding.Kind.Verdict(); phase != integration.PhaseReady {
			t.Errorf("%q is %s, want an advisory on a ready integration", finding.Title, phase)
		}
	}
}

// SWITCHED OFF, THE PASS DELETES THE APPLICATION, ITS POLICY AND ITS KEY, AND
// FORGETS WHAT IT SEALED; a company that never switched it on is never
// called about it.
//
// Mutation: forget the names and leave the application, and a key nobody
// holds keeps reading the Organization.
func TestSwitchedOffTheAtlasApplicationIsDeletedAndForgotten(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	sink := newMemSink()
	f.atlasPass(t, sink, false)
	if len(f.requests) != 0 {
		t.Fatalf("a company that never asked was called about the atlas: %v", f.requests)
	}
	f.atlasPass(t, sink, true)
	app, _ := f.appNamed(atlasAppName)
	if found := f.atlasPass(t, sink, false); len(found) != 0 {
		t.Fatalf("switching off found %+v", found)
	}
	if _, still := f.appNamed(atlasAppName); still || len(f.keysOf(app.ID)) != 0 || len(f.policiesOf(app.ID)) != 0 {
		t.Errorf("the application outlived the switch: %v", f.wrote())
	}
	if len(sink.values) != 0 {
		t.Errorf("sealed names left behind: %v", slices.Sorted(maps.Keys(sink.values)))
	}
	if _, kept := f.appNamed("crewlet-provisioner"); !kept {
		t.Error("the provisioner's own application was deleted")
	}
}

// AN APPLICATION SOMEBODY DELETED AT SCALEWAY IS NOT MADE AGAIN: that is a
// person revoking the atlas there. The pass says so, as an advisory, and
// writes nothing until the setting is switched off and on.
//
// Mutation: create an application whenever the recorded one is gone, and a
// revoke at Scaleway is undone within one pass.
func TestAnAtlasApplicationDeletedAtScalewayIsNotMadeAgain(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	sink := newMemSink()
	f.atlasPass(t, sink, true)
	app, _ := f.appNamed(atlasAppName)
	f.edit(func(f *fakeScaleway) {
		delete(f.apps, app.ID)
		for access, k := range f.keys {
			if k.AppID == app.ID {
				delete(f.keys, access)
			}
		}
	})
	wrote := f.wrote()
	found := f.atlasPass(t, sink, true)
	if !slices.Equal(titles(found), []string{"Atlas application deleted at Scaleway"}) || !slices.Equal(f.wrote(), wrote) {
		t.Fatalf("after the delete the pass found %v and wrote %v", titles(found), f.wrote()[len(wrote):])
	}
	// Off, then on, makes a new one.
	f.atlasPass(t, sink, false)
	f.atlasPass(t, sink, true)
	if again, ok := f.appNamed(atlasAppName); !ok || again.ID == app.ID {
		t.Errorf("switching off and on made no new application")
	}
}

// THE KEY IS REPLACED BEFORE IT LAPSES, AND THE NEW ONE IS SEALED BEFORE THE
// OLD ONE GOES: a key with fewer than 30 days left is replaced, the engine's
// older key is deleted after the new one is sealed, and a key somebody else
// added to the application is reported and never deleted.
//
// Mutation: delete the old key first, and a seal that fails leaves the atlas
// with no key at all.
func TestTheAtlasKeyIsRotatedAndAStrangersIsReported(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	sink := newMemSink()
	f.atlasPass(t, sink, true)
	app, _ := f.appNamed(atlasAppName)
	first := sink.values[scaleway.AtlasAccessName]
	f.edit(func(f *fakeScaleway) {
		f.now = f.now.Add(scaleway.KeyLifetime - scaleway.RenewWithin + time.Hour)
		f.keys["SCWSTRANGERKEY000001"] = &fakeKey{Access: "SCWSTRANGERKEY000001", Secret: "stranger",
			AppID: app.ID, Description: "added by hand", CreatedAt: f.now}
	})
	// A seal that fails takes the new key back and keeps the old one.
	sink.failOn = scaleway.AtlasSecretName
	if _, err := f.atlasPassErr(t, sink, true); err == nil {
		t.Fatal("a pass whose seal failed answered no error")
	}
	if keys := f.keysOf(app.ID); len(keys) != 2 || !slices.ContainsFunc(keys, func(k fakeKey) bool { return k.Access == first }) ||
		sink.values[scaleway.AtlasAccessName] != first {
		t.Fatalf("after a failed seal the application holds %+v and %s is sealed, want the old key kept and sealed",
			keys, sink.values[scaleway.AtlasAccessName])
	}
	sink.failOn = ""
	found := f.atlasPass(t, sink, true)
	second := sink.values[scaleway.AtlasAccessName]
	keys := f.keysOf(app.ID)
	if second == first || len(keys) != 2 || slices.ContainsFunc(keys, func(k fakeKey) bool { return k.Access == first }) {
		t.Fatalf("the rotation left %+v, sealed %s", keys, second)
	}
	if !slices.Equal(titles(found), []string{"API keys beside the Atlas application's own"}) ||
		!slices.Equal(found[0].Subjects, []string{"SCWSTRANGERKEY000001"}) {
		t.Errorf("the stranger's key: %+v", found)
	}
}

// THE POLICY IS HELD TO THE READ-ONLY SETS: rules somebody edited at
// Scaleway are written back, a policy somebody deleted is written again, and
// a policy somebody else attached is reported and never deleted.
//
// Mutation: skip the rule comparison, and a set somebody added to the
// atlas's own policy stays.
func TestTheAtlasPolicyIsHeldToItsReadOnlySets(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	sink := newMemSink()
	f.atlasPass(t, sink, true)
	app, _ := f.appNamed(atlasAppName)
	org := testOrg
	f.edit(func(f *fakeScaleway) {
		for _, p := range f.policies {
			if p.AppID == app.ID {
				p.Rules[0].PermissionSetNames = append(p.Rules[0].PermissionSetNames, "InstancesFullAccess")
			}
		}
		f.policies["pol-stranger"] = &fakePolicy{ID: "pol-stranger", Name: "extra", AppID: app.ID,
			Rules: []scaleway.Rule{{PermissionSetNames: []string{"IAMReadOnly"}, OrganizationID: &org}}}
	})
	found := f.atlasPass(t, sink, true)
	if !slices.Equal(titles(found), []string{"Policies Crewlet did not write on the Atlas application"}) {
		t.Fatalf("the stranger's policy: %v", titles(found))
	}
	own := slices.DeleteFunc(f.policiesOf(app.ID), func(p fakePolicy) bool { return p.Name != atlasAppName })
	if len(own) != 1 || !slices.Equal(own[0].Rules[0].PermissionSetNames, scaleway.AtlasSets()) {
		t.Fatalf("the engine's policy after the edit: %+v", own)
	}
	if len(f.policiesOf(app.ID)) != 2 {
		t.Error("the stranger's policy was deleted")
	}
	f.edit(func(f *fakeScaleway) { delete(f.policies, own[0].ID) })
	f.atlasPass(t, sink, true)
	own = slices.DeleteFunc(f.policiesOf(app.ID), func(p fakePolicy) bool { return p.Name != atlasAppName })
	if len(own) != 1 || !slices.Equal(own[0].Rules[0].PermissionSetNames, scaleway.AtlasSets()) {
		t.Errorf("the deleted policy was not written again: %+v", own)
	}
}

// A SET SCALEWAY DOES NOT LIST IS LEFT OUT AND SAID: the policy grants the
// rest, so one product the Organization cannot use costs the atlas that
// product alone.
func TestASetScalewayDoesNotListIsLeftOutOfTheAtlasPolicy(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	delete(f.sets, "KubernetesReadOnly")
	sink := newMemSink()
	found := f.atlasPass(t, sink, true)
	if !slices.Equal(titles(found), []string{"Atlas reads less at Scaleway"}) ||
		!strings.Contains(found[0].Detail, "KubernetesReadOnly") {
		t.Fatalf("the unlisted set: %+v", found)
	}
	app, _ := f.appNamed(atlasAppName)
	rule := f.policiesOf(app.ID)[0].Rules[0]
	if slices.Contains(rule.PermissionSetNames, "KubernetesReadOnly") || len(rule.PermissionSetNames) != 8 {
		t.Errorf("the rule: %v", rule.PermissionSetNames)
	}
}

// AN APPLICATION NOTHING RECORDS IS NEVER LEFT BEHIND: a policy Scaleway
// refuses, or an ID the store cannot seal, takes the application back; and
// an Organization that caps a key's lifetime is an advisory, with the
// application kept for the pass after the cap is raised.
//
// Mutation: return the seal's error without the undo, and every failed pass
// leaves one more application named for the atlas.
func TestAFailedCreateLeavesNoAtlasApplication(t *testing.T) {
	t.Parallel()
	for name, fault := range map[string]func(*fakeScaleway, *memSink){
		"a refused policy": func(f *fakeScaleway, _ *memSink) { f.refuse["create policy"] = 403 },
		"a failed seal":    func(_ *fakeScaleway, s *memSink) { s.failOn = scaleway.AtlasApplicationName },
	} {
		f := newFakeScaleway()
		sink := newMemSink()
		fault(f, sink)
		if _, err := f.atlasPassErr(t, sink, true); err == nil {
			t.Errorf("%s: the pass answered no error", name)
		}
		if _, left := f.appNamed(atlasAppName); left || len(sink.values) != 0 {
			t.Errorf("%s: an application or a sealed name was left: %v %v", name, f.wrote(), sink.values)
		}
	}
	f := newFakeScaleway()
	f.maxLifetime = 30 * 24 * time.Hour
	sink := newMemSink()
	found := f.atlasPass(t, sink, true)
	if !slices.Equal(titles(found), []string{"Atlas application has no key"}) || sink.values[scaleway.AtlasSecretName] != "" {
		t.Fatalf("a capped lifetime: %v, sealed %v", titles(found), slices.Sorted(maps.Keys(sink.values)))
	}
	if _, kept := f.appNamed(atlasAppName); !kept {
		t.Error("the application was not kept for the pass after the cap is raised")
	}
}

// THE COMPANY PASS SPARES THE ATLAS APPLICATION AND REPORTS A STRAY ONE: the
// recorded application is no orphan, and one under the same name that
// nothing records is reported, never touched.
func TestTheCompanyPassReportsAStrayAtlasApplication(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	sink := newMemSink()
	f.atlasPass(t, sink, true)
	f.edit(func(f *fakeScaleway) {
		f.apps["dddddddd-0000-4000-8000-000000000009"] = &fakeApp{ID: "dddddddd-0000-4000-8000-000000000009",
			Name: atlasAppName, Org: testOrg, CreatedAt: f.now}
	})
	client, err := scaleway.NewClient(scaleway.ClientOptions{OrganizationID: testOrg, SecretKey: provSecret, HTTP: f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	wrote := f.wrote()
	res, err := scaleway.Reconcile(t.Context(), scaleway.Options{
		Client: client, AccessKey: provAccess, Company: "Acme",
		AtlasApplicationID: sink.values[scaleway.AtlasApplicationName],
	})
	if err != nil {
		t.Fatal(err)
	}
	var stray []string
	for _, finding := range res.Findings() {
		if finding.Kind == integration.FindingRegistrationOrphaned {
			stray = append(stray, finding.Subjects...)
		}
	}
	if len(stray) != 1 || !strings.Contains(stray[0], "dddddddd-0000-4000-8000-000000000009") {
		t.Errorf("stray applications reported: %v", stray)
	}
	if !slices.Equal(f.wrote(), wrote) {
		t.Errorf("the company pass wrote %v", f.wrote()[len(wrote):])
	}
}
