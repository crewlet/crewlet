package scaleway_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// grantOf is a stored grant of set on projects, lapsing at expires (zero for
// permanent), as the Agents tab's save writes one.
func grantOf(id, set string, expires time.Time, projects ...string) config.SeatGrant {
	return config.SeatGrant{
		ID: id, Permission: set, Scope: projects, ExpiresAt: expires,
		GrantedBy: "Ada", GrantedAt: time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC), Reason: "a cluster upgrade",
	}
}

// ruleWith is the policy's rule that holds set, and false where none does.
func ruleWith(p fakePolicy, set string) (projects []string, org bool, ok bool) {
	for _, r := range p.Rules {
		if slices.Contains(r.PermissionSetNames, set) {
			return r.ProjectIDs, r.OnOrganization(), true
		}
	}
	return nil, false, false
}

// A GRANT RIDES THE POLICY BESIDE ITS LEVEL: a Read seat granted
// InstancesFullAccess holds it on its own projects, KubernetesReadOnly on one
// other project in a rule of its own, and an organization set on the
// Organization, all in its one policy rewritten in place so its key stays; a
// converged pass writes nothing and reads the policy back as the level and the
// grants. Mutation: drop the grants from rulesFor and the policy never holds
// them.
func TestAGrantRidesThePolicyBesideItsLevel(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	w.f.edit(func(f *fakeScaleway) { f.sets["SupportTicketsReadOnly"] = "organization" })
	w.sync()
	app := w.record().Account.ID
	key, _ := w.sink.Held(sealedAccess)

	w.seat().Grants = []config.SeatGrant{
		grantOf("gaaaaaaaaaa", "InstancesFullAccess", time.Time{}),
		grantOf("gbbbbbbbbbb", "KubernetesReadOnly", time.Time{}, projectData),
		grantOf("gcccccccccc", "SupportTicketsReadOnly", time.Time{}),
	}
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("granting reported %+v", report.Findings)
	}
	pol := w.policy(app)
	if projects, _, ok := ruleWith(pol, "InstancesFullAccess"); !ok || !slices.Equal(projects, []string{projectWeb}) {
		t.Errorf("InstancesFullAccess is on %v (held %v), want the seat's project", projects, ok)
	}
	if projects, _, ok := ruleWith(pol, "KubernetesReadOnly"); !ok || !slices.Equal(projects, []string{projectData}) {
		t.Errorf("KubernetesReadOnly is on %v (held %v), want the other project alone", projects, ok)
	}
	if _, org, ok := ruleWith(pol, "SupportTicketsReadOnly"); !ok || !org {
		t.Errorf("SupportTicketsReadOnly is not on the Organization: %+v", pol.Rules)
	}
	if _, _, ok := ruleWith(pol, "InstancesReadOnly"); !ok {
		t.Errorf("the grants took Read's sets away: %+v", pol.Rules)
	}
	if after, _ := w.sink.Held(sealedAccess); after != key {
		t.Error("a grant replaced the seat's key; the policy is rewritten in place")
	}
	lr := w.record().Lanes[seataccess.LaneScaleway]
	if lr.Observed.Level != config.ScalewayLevelRead || len(lr.Observed.Grants) != 3 ||
		!slices.Equal(lr.Observed.Scope, []string{projectWeb}) {
		t.Errorf("observed %q on %v with %+v, want read on web with three grants",
			lr.Observed.Level, lr.Observed.Scope, lr.Observed.Grants)
	}

	writes := len(w.f.wrote())
	if report := w.sync(); len(report.Findings) != 0 {
		t.Errorf("a converged pass reported %+v", report.Findings)
	}
	if got := w.f.wrote(); len(got) != writes {
		t.Errorf("a converged pass wrote %q", got[writes:])
	}
	if len(w.drift) != 0 {
		t.Errorf("a grant read as drift: %+v", w.drift)
	}
}

// A GRANT NEVER OUTLIVES ITS EXPIRY: an expiring grant leaves the policy on the
// first pass inside [seataccess.LapseLead] of its expiry, and the key stays.
// Mutation: filter grants by their bare expiry and the pass a minute before it
// keeps the set.
func TestAGrantLeavesThePolicyBeforeItsExpiry(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	expires := w.clock.Add(4 * time.Hour)
	w.seat().Grants = []config.SeatGrant{grantOf("gdddddddddd", "KubernetesReadOnly", expires)}
	w.sync()
	app := w.record().Account.ID
	if _, _, ok := ruleWith(w.policy(app), "KubernetesReadOnly"); !ok {
		t.Fatalf("the policy is %+v, want KubernetesReadOnly", w.policy(app).Rules)
	}
	key, _ := w.sink.Held(sealedAccess)

	w.at(expires.Add(-seataccess.LapseLead + time.Second))
	w.sync()
	if _, _, ok := ruleWith(w.policy(app), "KubernetesReadOnly"); ok {
		t.Fatal("the pass inside the lapse lead left KubernetesReadOnly on the policy")
	}
	if after, _ := w.sink.Held(sealedAccess); after != key {
		t.Error("the lapse replaced the seat's key")
	}
	if len(w.drift) != 0 {
		t.Errorf("a lapse read as drift: %+v", w.drift)
	}
}

// A SET ADDED AT SCALEWAY IS A GRANT NOBODY MADE, read back as one and taken
// off by the next pass as drift, with the level kept.
func TestASetAddedAtScalewayIsTakenOff(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	w.sync()
	app := w.record().Account.ID
	w.f.edit(func(f *fakeScaleway) {
		for _, p := range f.policies {
			if p.AppID == app {
				p.Rules[0].PermissionSetNames = append(p.Rules[0].PermissionSetNames, "KubernetesReadOnly")
			}
		}
	})
	w.sync()
	if _, _, ok := ruleWith(w.policy(app), "KubernetesReadOnly"); ok {
		t.Fatal("a set added at Scaleway stayed on the policy")
	}
	if len(w.drift) == 0 {
		t.Error("the set added at Scaleway was taken off without being reported")
	}
}

// A SET SCALEWAY DOES NOT LIST IS REFUSED IN A CLAUSE the vendor's refusal
// reads after "Scaleway: ", naming the set, so the agent's row says Scaleway
// once. The Agents tab's catalogue check refuses such a grant first; this is
// the policy's own word when the sets moved between that read and the write.
// Mutation: word it as a sentence of its own ("Scaleway lists no ...") and the
// refusal reads "Scaleway: Scaleway lists".
func TestAnUnlistedSetIsRefusedInTheRefusalsClause(t *testing.T) {
	t.Parallel()
	_, err := scaleway.RulesWith(config.ScalewayLevelRead, "org", []string{projectWeb},
		[]seataccess.Grant{{Permission: "NoSuchSetAccess"}}, map[string]string{"InstancesReadOnly": "projects"})
	if err == nil {
		t.Fatal("a grant of a set Scaleway does not list was written into the policy")
	}
	said := "Scaleway: " + err.Error()
	if want := "Scaleway: no permission set a policy can grant at Scaleway is named NoSuchSetAccess"; said != want {
		t.Errorf("the refusal reads %q, want %q", said, want)
	}
}

// THE CATALOGUE IS SCALEWAY'S, LESS WHAT IS NEVER GRANTED: every listed set at
// a scope a policy takes, IAM, billing and the organization's managers left
// out, each marked read or write and with the level that already holds it.
func TestTheGrantCatalogueIsScalewaysLessTheNeverGranted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	session, err := w.vendor.Open(t.Context(), w.open())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := seataccess.ReadOptions(t.Context(), session, seataccess.LaneScaleway)
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{"IAMApplicationManager", "IAMPolicyManager", "IAMManager", "OrganizationManager",
		"BillingManager"} {
		if _, ok := opts.Grantable(never); ok {
			t.Errorf("%s is offered as a grant", never)
		}
	}
	for _, tc := range []struct {
		set     string
		kind    seataccess.GrantScopeKind
		write   bool
		inLevel string
	}{
		{"KubernetesReadOnly", "project", false, ""},
		{"InstancesFullAccess", "project", true, config.ScalewayLevelAdminister},
		{"OrganizationReadOnly", seataccess.GrantScopeAccount, false, config.ScalewayLevelRead},
	} {
		e, ok := opts.Grantable(tc.set)
		if !ok || e.ScopeKind != tc.kind || e.Write != tc.write || e.InLevel != tc.inLevel {
			t.Errorf("%s is offered as %+v (offered %v)", tc.set, e, ok)
		}
	}
	if !slices.Contains(opts.GrantScope, projectData) || !slices.Contains(opts.GrantScope, projectWeb) {
		t.Errorf("a grant may name %v, want the Organization's projects", opts.GrantScope)
	}
	if opts.GrantWhole != "the agent's own projects" || opts.GrantAccount != "the Organization" {
		t.Errorf("a grant naming none reaches %q, an Organization one %q", opts.GrantWhole, opts.GrantAccount)
	}
}

// A BUCKET'S SETTINGS ARE GRANTED FOR ONE CHANGE, NEVER ITS OBJECTS OR POLICY:
// a four-hour write no level holds, saying what else it allows, refused for good
// or longer. Mutation: drop it from oneChange and a permanent grant saves.
func TestABucketsSettingsAreGrantedForOneChange(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	session, err := w.vendor.Open(t.Context(), w.open())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := seataccess.ReadOptions(t.Context(), session, seataccess.LaneScaleway)
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{"ObjectStorageFullAccess", "ObjectStorageReadOnly", "ObjectStorageObjectsRead",
		"ObjectStorageObjectsWrite", "ObjectStorageBucketPolicyFullAccess"} {
		if _, ok := opts.Grantable(never); ok {
			t.Errorf("%s is offered as a grant", never)
		}
	}
	e, ok := opts.Grantable("ObjectStorageBucketsWrite")
	if !ok || e.ScopeKind != "project" || !e.Write || e.InLevel != "" || e.MaxHours != 4 {
		t.Fatalf("ObjectStorageBucketsWrite is offered as %+v (offered %v), want a four-hour project write", e, ok)
	}
	for _, says := range []string{"ACL and visibility", "creates buckets", "CORS, website, versioning, tags",
		"lifecycle rules, which can expire and delete objects", "public-read-write lets anyone",
		"every bucket in the projects", "reads no object", "writes no bucket policy"} {
		if !strings.Contains(e.Note, says) {
			t.Errorf("its note %q does not say %q", e.Note, says)
		}
	}
	before := seataccess.Desired{Enabled: true, Level: config.ScalewayLevelRead, Scope: []string{projectWeb}}
	for name, c := range map[string]struct {
		expires time.Time
		saves   bool
	}{
		"an hour":     {w.clock.Add(time.Hour), true},
		"four hours":  {w.clock.Add(4 * time.Hour), true},
		"five hours":  {w.clock.Add(5 * time.Hour), false},
		"permanently": {time.Time{}, false},
	} {
		after := before
		after.Grants = []seataccess.Grant{{Permission: "ObjectStorageBucketsWrite", ExpiresAt: c.expires,
			Target: "user-uploads", Request: "aaaaaaaaaa", Reason: "make it private"}}
		if err := seataccess.CheckGrants(opts, before, after, w.clock); (err == nil) != c.saves {
			t.Errorf("granting it %s: %v, want saved %v", name, err, c.saves)
		}
	}
}

// A BUCKET GRANT RIDES THE POLICY FOR ITS HOURS AND LEAVES IT, key unchanged and
// never read as drift; one stored for good is never applied. Mutation: skip the
// one-change case in CheckGrant and the permanent grant reaches the policy.
func TestABucketGrantRidesThePolicyForItsHoursAndLeaves(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	w.sync()
	app := w.record().Account.ID
	key, _ := w.sink.Held(sealedAccess)

	expires := w.clock.Add(time.Hour)
	g := grantOf("geeeeeeeeee", "ObjectStorageBucketsWrite", expires)
	g.Target, g.Request = "user-uploads", "aaaaaaaaaa"
	w.seat().Grants = []config.SeatGrant{g}
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("granting reported %+v", report.Findings)
	}
	if projects, _, ok := ruleWith(w.policy(app), "ObjectStorageBucketsWrite"); !ok ||
		!slices.Equal(projects, []string{projectWeb}) {
		t.Fatalf("ObjectStorageBucketsWrite is on %v (held %v), want the seat's own project", projects, ok)
	}
	for _, never := range []string{"ObjectStorageObjectsRead", "ObjectStorageObjectsWrite",
		"ObjectStorageBucketPolicyFullAccess"} {
		if _, _, ok := ruleWith(w.policy(app), never); ok {
			t.Errorf("the bucket grant brought %s with it", never)
		}
	}
	lr := w.record().Lanes[seataccess.LaneScaleway]
	if len(lr.Observed.Grants) != 1 || lr.Observed.Grants[0].Permission != "ObjectStorageBucketsWrite" {
		t.Errorf("observed grants %+v, want the bucket grant", lr.Observed.Grants)
	}
	if len(w.drift) != 0 {
		t.Errorf("the bucket grant read as drift: %+v", w.drift)
	}

	w.at(expires.Add(-seataccess.LapseLead + time.Second))
	w.sync()
	if _, _, ok := ruleWith(w.policy(app), "ObjectStorageBucketsWrite"); ok {
		t.Fatal("the pass inside the lapse lead left ObjectStorageBucketsWrite on the policy")
	}
	if after, _ := w.sink.Held(sealedAccess); after != key {
		t.Error("the bucket grant's lapse replaced the seat's key")
	}

	forGood := grantOf("gffffffffff", "ObjectStorageBucketsWrite", time.Time{})
	w.seat().Grants = []config.SeatGrant{forGood}
	report := w.sync()
	if _, _, ok := ruleWith(w.policy(app), "ObjectStorageBucketsWrite"); ok {
		t.Error("a bucket grant stored for good reached the policy")
	}
	if !slices.ContainsFunc(report.Findings, func(f integration.Finding) bool {
		return strings.Contains(f.Detail, "never permanently")
	}) {
		t.Errorf("findings %+v, want the permanent bucket grant reported as not applied", report.Findings)
	}
}

// EVERY GRANT THE CATALOGUE OFFERS CAN BE ASKED FOR, each at its longest,
// ObjectStorageBucketsWrite's note included ([seataccesstest.AskFaults]).
// Mutation: put the note and the sign-in and provisioner sentences back in
// each option's detail and this fails.
func TestEveryGrantTheCatalogueOffersCanBeAsked(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	session, err := w.vendor.Open(t.Context(), w.open())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := seataccess.ReadOptions(t.Context(), session, seataccess.LaneScaleway)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := opts.Grantable("ObjectStorageBucketsWrite"); !ok || e.Note == "" {
		t.Fatalf("ObjectStorageBucketsWrite is offered as %+v (offered %v), want it with its note", e, ok)
	}
	for _, fault := range seataccesstest.AskFaults(opts) {
		t.Error(fault)
	}
}
