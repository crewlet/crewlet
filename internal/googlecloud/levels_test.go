package googlecloud_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
)

// THE CREDENTIAL RULE: what no level may hold, Administer included, and what
// only Administer may hold. Mutation: drop a branch of either pattern, and
// the permission it covered is named here.
func TestTheLevelPatternsHoldTheCredentialRule(t *testing.T) {
	t.Parallel()
	for _, perm := range []string{
		"apikeys.keys.getKeyString", "secretmanager.versions.access", "container.secrets.get",
		"container.secrets.list", "container.serviceAccounts.createToken", "container.pods.exec",
		"container.pods.create", "container.pods.attach", "container.pods.portForward",
		"container.deployments.update", "container.cronJobs.create", "storage.hmacKeys.create",
		"iam.serviceAccounts.actAs", "iam.serviceAccounts.getAccessToken", "iam.serviceAccounts.signJwt",
		"iam.serviceAccounts.implicitDelegation", "iam.serviceAccounts.setIamPolicy",
		"iam.serviceAccountKeys.create", "iam.roles.update", "iam.roles.create",
		"resourcemanager.projects.setIamPolicy", "resourcemanager.folders.setIamPolicy",
		"run.jobs.runWithOverrides", "cloudkms.cryptoKeyVersions.useToDecrypt", "billing.accounts.get",
	} {
		if !googlecloud.Never(perm) {
			t.Errorf("%s is allowed at Administer", perm)
		}
		if got := googlecloud.Barred(config.GoogleCloudLevelAdminister, []string{perm}); len(got) != 1 {
			t.Errorf("Administer may hold %s", perm)
		}
	}
	for _, perm := range []string{
		"compute.instances.setMetadata", "compute.instances.setServiceAccount",
		"compute.projects.setCommonInstanceMetadata", "container.clusters.getCredentials",
		"cloudsql.users.create", "cloudsql.users.update", "cloudsql.sslCerts.create",
		"storage.buckets.setIamPolicy", "dns.managedZones.setIamPolicy",
	} {
		if !googlecloud.CredentialRule(perm) || googlecloud.Never(perm) {
			t.Errorf("%s must be Administer's alone", perm)
		}
		for _, level := range []string{config.GoogleCloudLevelViewer, config.GoogleCloudLevelOperator} {
			if got := googlecloud.Barred(level, []string{perm}); len(got) != 1 {
				t.Errorf("%s may hold %s", level, perm)
			}
		}
		if got := googlecloud.Barred(config.GoogleCloudLevelAdminister, []string{perm}); len(got) != 0 {
			t.Errorf("Administer may not hold %s", perm)
		}
	}
	for _, perm := range slices.Concat(googlecloud.CustomRolePermissions(),
		[]string{"container.clusters.update", "monitoring.alertPolicies.create", "run.jobs.run"}) {
		if googlecloud.Never(perm) || googlecloud.CredentialRule(perm) {
			t.Errorf("%s is wrongly refused below Administer", perm)
		}
	}
}

// EACH LEVEL IS ONE CONDITION'S LIST AND HOLDS THE ONE BELOW IT: no list is
// longer than Google's ten, no role is in two, and a level's roles are its
// list and every list below it.
func TestEachLevelIsOneListOnTopOfTheOneBelow(t *testing.T) {
	t.Parallel()
	lists := googlecloud.Lists(projectWeb)
	if len(lists) != len(googlecloud.Levels()) {
		t.Fatalf("%d lists for %d levels", len(lists), len(googlecloud.Levels()))
	}
	seen := map[string]bool{}
	var below []string
	for i, level := range googlecloud.Levels() {
		if len(lists[i]) > 10 {
			t.Errorf("%s's list holds %d roles: Google's hasOnly takes at most 10", level, len(lists[i]))
		}
		for _, role := range lists[i] {
			if seen[role] {
				t.Errorf("%s is in two lists", role)
			}
			seen[role] = true
		}
		below = append(below, lists[i]...)
		got, ok := googlecloud.LevelRoles(level, projectWeb)
		if !ok || !slices.Equal(got, slices.Sorted(slices.Values(below))) {
			t.Errorf("%s grants %v, want its list and every one below", level, got)
		}
	}
	if !slices.Contains(lists[1], googlecloud.CustomRole(projectWeb)) || slices.Contains(lists[1], googlecloud.CustomRole(projectData)) {
		t.Errorf("Operator's list %v does not name the project's own crewletOperator", lists[1])
	}
	if _, ok := googlecloud.LevelRoles("owner", projectWeb); ok {
		t.Error("a level this build does not offer has roles")
	}
}

// NO LEVEL NAMES A ROLE THAT HANDS OUT ANOTHER IDENTITY OR THE PROJECT ITSELF,
// whatever Google's role then holds.
func TestNoLevelNamesAnEscalatingRole(t *testing.T) {
	t.Parallel()
	for _, role := range []string{
		"roles/owner", "roles/editor", "roles/viewer", "roles/resourcemanager.projectIamAdmin",
		"roles/iam.serviceAccountTokenCreator", "roles/iam.serviceAccountUser", "roles/container.developer",
		"roles/container.admin", "roles/cloudsql.editor", "roles/run.admin", "roles/run.developer",
		"roles/iam.serviceAccountKeyAdmin", "roles/secretmanager.secretAccessor", "roles/storage.editor",
		"roles/storage.hmacKeyAdmin", "roles/compute.admin",
	} {
		if _, ok := googlecloud.PredefinedRoles()[role]; ok {
			t.Errorf("%s is in the level table", role)
		}
	}
	if !slices.IsSorted(googlecloud.CustomRolePermissions()) || len(googlecloud.CustomRolePermissions()) != 12 {
		t.Errorf("crewletOperator's list %v is not its 12 permissions, sorted", googlecloud.CustomRolePermissions())
	}
	for _, level := range googlecloud.Levels() {
		want := level != config.GoogleCloudLevelViewer
		if googlecloud.Administers(level) != want {
			t.Errorf("%s administers = %v, want %v", level, !want, want)
		}
	}
}
