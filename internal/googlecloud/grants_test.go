package googlecloud_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A ROLE WRITES UNLESS EVERY PERMISSION ONLY READS, failing closed on a verb
// this package does not know, so a grant of it asks for a step-up.
func TestARoleWritesUnlessEveryPermissionReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		perms []string
		write bool
	}{
		{[]string{"pubsub.topics.get", "pubsub.topics.list", "resourcemanager.projects.getIamPolicy"}, false},
		{[]string{"compute.instances.aggregatedList", "logging.logEntries.list"}, false},
		{[]string{"pubsub.topics.get", "pubsub.topics.create"}, true},
		{[]string{"storage.objects.frobnicate"}, true},
	} {
		if got := googlecloud.RoleWrites(tc.perms); got != tc.write {
			t.Errorf("%v writes %v, want %v", tc.perms, got, tc.write)
		}
	}
}

// grantsList gives the provisioner one more Project IAM Admin condition on
// project: the grants list naming roles.
func grantsList(f *fakeGoogle, project string, roles ...string) {
	f.projects[project].Policy.Bindings = append(f.projects[project].Policy.Bindings, googlecloud.Binding{
		Role: "roles/resourcemanager.projectIamAdmin", Members: []string{"serviceAccount:" + provisioner},
		Condition: condition(roles),
	})
}

// gcpGrant is a stored grant of role on projects, as the Agents tab writes it.
func gcpGrant(id, role string, expires time.Time, projects ...string) config.SeatGrant {
	return config.SeatGrant{
		ID: id, Permission: role, Scope: projects, ExpiresAt: expires,
		GrantedBy: "Ada", GrantedAt: time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC), Reason: "a topic for the release",
	}
}

const pubsubEditor = "roles/pubsub.editor"

// withPubSub adds Pub/Sub Editor to Google's roles and to projectWeb's grants
// list.
func withPubSub(w *syncWorld) {
	w.f.edit(func(f *fakeGoogle) {
		f.roles[pubsubEditor] = googlecloud.Role{Name: pubsubEditor, Title: "Pub/Sub Editor", Stage: "GA",
			Permissions: []string{"pubsub.topics.create", "pubsub.topics.get", "pubsub.topics.list"}}
		grantsList(f, projectWeb, pubsubEditor, "roles/monitoring.editor")
	})
}

// A GRANT BINDS BESIDE THE LEVEL: a Viewer seat granted Pub/Sub Editor holds
// it on its project beside Viewer's roles, one write per role, and a
// converged pass writes nothing and reads the account back as Viewer with the
// grant. Mutation: drop the grants from converge and the role is never bound.
func TestAGrantBindsBesideTheLevel(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	withPubSub(w)
	w.sync()
	w.settle()
	w.seat().Grants = []config.SeatGrant{gcpGrant("gaaaaaaaaaa", pubsubEditor, time.Time{})}
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("granting reported %+v", report.Findings)
	}
	viewer, _ := googlecloud.LevelRoles(config.GoogleCloudLevelViewer, projectWeb)
	want := append(slices.Clone(viewer), pubsubEditor)
	slices.Sort(want)
	if got := w.roles(projectWeb); !slices.Equal(got, want) {
		t.Fatalf("on %s the seat holds %v, want Viewer and Pub/Sub Editor", projectWeb, got)
	}
	w.settle()
	writes := len(w.f.wrote())
	if report := w.sync(); len(report.Findings) != 0 {
		t.Errorf("a converged pass reported %+v", report.Findings)
	}
	if got := w.f.wrote(); len(got) != writes {
		t.Errorf("a converged pass wrote %q", got[writes:])
	}
	lr := w.record().Lanes[seataccess.LaneGoogleCloud]
	if lr.Observed.Level != config.GoogleCloudLevelViewer || len(lr.Observed.Grants) != 1 ||
		lr.Observed.Grants[0].Permission != pubsubEditor {
		t.Errorf("observed %q with %+v, want viewer with Pub/Sub Editor", lr.Observed.Level, lr.Observed.Grants)
	}
}

// A GRANT OF A HIGHER LEVEL'S ROLE SURVIVES THE LEVEL'S WRITES: a Viewer seat
// granted Monitoring Editor, an Operator role, keeps it although the Operator
// list is taken off, and reads back as Viewer, never as a flapping level.
func TestAGrantOfAHigherLevelsRoleIsKeptAndNeverFlaps(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	withPubSub(w)
	w.seat().Grants = []config.SeatGrant{gcpGrant("gbbbbbbbbbb", "roles/monitoring.editor", time.Time{})}
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("turning on reported %+v", report.Findings)
	}
	if !slices.Contains(w.roles(projectWeb), "roles/monitoring.editor") {
		t.Fatalf("on %s the seat holds %v, want Monitoring Editor beside Viewer", projectWeb, w.roles(projectWeb))
	}
	for range 2 {
		w.settle()
		writes := len(w.f.wrote())
		w.sync()
		if got := w.f.wrote(); len(got) != writes {
			t.Fatalf("a converged pass wrote %q", got[writes:])
		}
	}
	if lr := w.record().Lanes[seataccess.LaneGoogleCloud]; lr.Observed.Level != config.GoogleCloudLevelViewer {
		t.Errorf("observed %q, want viewer", lr.Observed.Level)
	}
}

// A GRANT NEVER OUTLIVES ITS EXPIRY: the pass inside [seataccess.LapseLead]
// removes the role and leaves the level.
func TestAGrantLeavesTheAccountBeforeItsExpiry(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	withPubSub(w)
	expires := w.clock.now().Add(4 * time.Hour)
	w.seat().Grants = []config.SeatGrant{gcpGrant("gcccccccccc", pubsubEditor, expires)}
	w.sync()
	if !slices.Contains(w.roles(projectWeb), pubsubEditor) {
		t.Fatalf("the seat holds %v, want Pub/Sub Editor", w.roles(projectWeb))
	}
	w.clock.at(expires.Add(-seataccess.LapseLead + time.Second))
	w.sync()
	w.wantRoles(projectWeb, config.GoogleCloudLevelViewer)
}

// A GRANT A PROJECT'S LIST DOES NOT NAME IS REFUSED THERE, naming the role
// and the condition to add it to, and nothing is asked of Google for it.
func TestAGrantOutsideTheGrantsListIsRefusedNamingIt(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb, projectData)
	withPubSub(w)
	w.sync()
	w.settle()
	w.seat().Grants = []config.SeatGrant{gcpGrant("gdddddddddd", pubsubEditor, time.Time{}, projectData)}
	report := w.sync()
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "grants list") ||
		!strings.Contains(report.Findings[0].Detail, projectData) {
		t.Fatalf("findings = %+v, want the grant refused naming %s's grants list", report.Findings, projectData)
	}
	if slices.Contains(w.roles(projectData), pubsubEditor) {
		t.Error("a role outside the project's grants list was bound")
	}
}

// THE CATALOGUE IS THE GRANTS LISTS' ROLES, named as Google names them, read
// or write by their permissions, on the company's offered projects.
func TestTheGrantCatalogueIsTheGrantsLists(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	withPubSub(w)
	session, err := w.vendor.Open(t.Context(), w.open())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := seataccess.ReadOptions(t.Context(), session, seataccess.LaneGoogleCloud)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := opts.Grantable(pubsubEditor)
	if !ok || e.Name != "Pub/Sub Editor" || !e.Write || e.Group != "pubsub" || e.ScopeKind != "project" || e.InLevel != "" {
		t.Fatalf("Pub/Sub Editor is offered as %+v (offered %v)", e, ok)
	}
	if m, ok := opts.Grantable("roles/monitoring.editor"); !ok || m.InLevel != config.GoogleCloudLevelOperator {
		t.Errorf("Monitoring Editor is offered as %+v (offered %v), want it marked as Operator's", m, ok)
	}
	if _, ok := opts.Grantable("roles/browser"); ok {
		t.Error("a role no grants list names is offered")
	}
	if !slices.Equal(opts.GrantScope, opts.Scope) {
		t.Errorf("a grant may name %v, want the offered projects %v", opts.GrantScope, opts.Scope)
	}
}
