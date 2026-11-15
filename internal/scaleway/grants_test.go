package scaleway_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
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

	w.advance(expires.Sub(w.clock) - seataccess.LapseLead + time.Second)
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
