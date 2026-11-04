package scaleway_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/scaleway"
)

// NO LEVEL GRANTS WHAT NONE MAY HOLD: IAM and organization writes, billing,
// secrets and keys, SSH keys, Cockpit token minting, object contents, domain
// registrar writes, Kubernetes' system:masters and the opaque bundles. The
// provisioner's own sets are on that list. Mutation: add ObservabilityFullAccess
// to Operate, and this fails.
func TestNoLevelGrantsANeverGrantedSet(t *testing.T) {
	t.Parallel()
	never := scaleway.NeverGranted()
	for _, set := range scaleway.ProvisionerSets() {
		if set != "ProjectReadOnly" && !slices.Contains(never, set) {
			t.Errorf("the provisioner's %s is not on the never-granted list", set)
		}
	}
	for _, level := range scaleway.Levels() {
		sets, ok := scaleway.ProjectSets(level)
		if !ok || len(sets) == 0 {
			t.Fatalf("%s grants nothing", level)
		}
		for _, set := range append(sets, scaleway.OrgSets()...) {
			if slices.Contains(never, set) {
				t.Errorf("%s grants %s, which no level may hold", level, set)
			}
		}
	}
}

// EACH LEVEL GRANTS EVERYTHING THE ONE BELOW IT DOES, AND MORE, and only
// Administer is an administration grant.
func TestEachLevelGrantsTheOneBelowItAndMore(t *testing.T) {
	t.Parallel()
	var below []string
	for _, level := range scaleway.Levels() {
		sets, _ := scaleway.ProjectSets(level)
		for _, set := range below {
			if !slices.Contains(sets, set) {
				t.Errorf("%s drops %s, which the level below it grants", level, set)
			}
		}
		if len(sets) <= len(below) {
			t.Errorf("%s grants nothing the level below it does not", level)
		}
		if scaleway.Administers(level) != (level == config.ScalewayLevelAdminister) {
			t.Errorf("%s administers %v", level, scaleway.Administers(level))
		}
		if note := scaleway.LevelNote(level); note == "" || strings.Contains(note, "\u2014") {
			t.Errorf("%s's note %q is empty or carries an em dash", level, note)
		}
		below = sets
	}
	if !slices.Equal(scaleway.Levels(), config.ScalewayLevels()) {
		t.Errorf("the level table %v is not the document's %v", scaleway.Levels(), config.ScalewayLevels())
	}
}

// A POLICY WRITTEN AT A LEVEL READS BACK AS THAT LEVEL ON ITS PROJECTS, and
// one with a set added or taken away is no level at all, so the plan rewrites
// it.
func TestAPolicyReadsBackAsItsLevel(t *testing.T) {
	t.Parallel()
	for _, level := range scaleway.Levels() {
		specs, err := scaleway.RulesFor(level, testOrg, []string{projectData, strings.ToUpper(projectWeb), projectData})
		if err != nil {
			t.Fatal(err)
		}
		got, projects := scaleway.LevelOf(rulesOf(specs))
		if got != level || !slices.Equal(projects, []string{projectWeb, projectData}) {
			t.Errorf("%s reads back as %q on %v", level, got, projects)
		}
		widened := rulesOf(specs)
		widened[0].PermissionSetNames = append(widened[0].PermissionSetNames, "SecretManagerFullAccess")
		if got, _ := scaleway.LevelOf(widened); got != "" {
			t.Errorf("%s with a set added reads as %q", level, got)
		}
	}
	if _, err := scaleway.RulesFor(config.ScalewayLevelRead, testOrg, nil); err == nil {
		t.Error("a policy on no project was written")
	}
	if _, err := scaleway.RulesFor("owner", testOrg, []string{projectWeb}); err == nil {
		t.Error("a level this build does not offer was written")
	}
}
