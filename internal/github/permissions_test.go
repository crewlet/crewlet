package github_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// EVERY PERMISSION AND LEVEL GITHUB OFFERS AN AGENT, AND NOTHING ELSE.
//
// The catalogue is what the Agents tab offers and what the validator
// accepts, so a refusal here is a refusal a person reads: each case names
// what it must say.
func TestPermissionsAcceptOnlyWhatGitHubOffersAnAgent(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		raw     map[string]string
		want    github.Permissions
		refuses string
	}{
		"every catalogue permission at write": {
			raw: map[string]string{
				"contents": "write", "issues": "write", "pull_requests": "write",
				"checks": "write", "actions": "write", "workflows": "write",
			},
			want: github.Permissions{
				"contents": "write", "issues": "write", "pull_requests": "write",
				"checks": "write", "actions": "write", "workflows": "write",
			},
		},
		"an empty level is No access": {
			raw:  map[string]string{"contents": "read", "issues": ""},
			want: github.Permissions{"contents": "read"},
		},
		"metadata is accepted and never stored": {
			raw:  map[string]string{"metadata": "read", "checks": "read"},
			want: github.Permissions{"checks": "read"},
		},
		"workflows has no read": {
			raw:     map[string]string{"contents": "write", "workflows": "read"},
			refuses: "Workflows offers No access or Read and write",
		},
		"workflows needs contents write": {
			raw:     map[string]string{"contents": "read", "workflows": "write"},
			refuses: "needs Contents (Read and write)",
		},
		"a denied permission": {
			raw:     map[string]string{"administration": "write"},
			refuses: "never granted to an agent",
		},
		"a permission outside the catalogue": {
			raw:     map[string]string{"pages": "read"},
			refuses: "the choices are Contents, Issues, Pull requests, Checks, Actions, Deployments, Workflows",
		},
		"an unknown level": {
			raw:     map[string]string{"issues": "admin"},
			refuses: `Issues offers No access, Read-only or Read and write, not "admin"`,
		},
		"metadata above read": {
			raw:     map[string]string{"metadata": "write"},
			refuses: "metadata is always Read-only",
		},
	} {
		got, problems := github.ParsePermissions(tc.raw)
		if tc.refuses != "" {
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.refuses) }) {
				t.Errorf("%s: problems = %q, want one saying %q", name, problems, tc.refuses)
			}
			continue
		}
		if len(problems) > 0 {
			t.Errorf("%s: refused %q", name, problems)
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s: parsed %v, want %v", name, got, tc.want)
		}
	}
}

// METADATA RIDES ON EVERY TOKEN AND EVERY MANIFEST, because without it a
// token cannot resolve a repository; a seat set to No access everywhere still
// asks for it.
func TestEveryTokenAsksForMetadata(t *testing.T) {
	t.Parallel()
	if got := (github.Permissions{}).Token(); got["metadata"] != "read" || len(got) != 1 {
		t.Errorf("an empty seat's token asks for %v", got)
	}
	got := github.Permissions{"issues": github.LevelWrite}.Token()
	if got["metadata"] != "read" || got["issues"] != "write" || len(got) != 2 {
		t.Errorf("token = %v", got)
	}
}

// A RAISE IS TOLD FROM A NARROWING, and only an administration-class raise
// to Read and write asks for a fresh step-up (D10).
func TestRaisesAndAdministrationRaises(t *testing.T) {
	t.Parallel()
	before := github.Permissions{"contents": "read", "issues": "write"}
	after := github.Permissions{"contents": "write", "issues": "read", "checks": "read", "workflows": "write"}
	if got := before.Raises(after); !slices.Equal(got, []string{"checks", "contents", "workflows"}) {
		t.Errorf("raises = %v", got)
	}
	if got := before.AdministrationRaises(after); !slices.Equal(got, []string{"contents", "workflows"}) {
		t.Errorf("administration raises = %v", got)
	}
	// Narrowing never steps up, and holding write already is no raise.
	if got := after.AdministrationRaises(before); len(got) != 0 {
		t.Errorf("a narrowing asked for a step-up: %v", got)
	}
	if got := after.AdministrationRaises(after); len(got) != 0 {
		t.Errorf("an unchanged seat asked for a step-up: %v", got)
	}
	// Issues and Checks to write are raises, never administration.
	plain := github.Permissions{"issues": "write", "checks": "write", "actions": "write"}
	if got := (github.Permissions{}).AdministrationRaises(plain); len(got) != 0 {
		t.Errorf("a raise outside Contents and Workflows asked for a step-up: %v", got)
	}
}

// WHAT A SEAT NEEDS AND WHAT IS HELD, by key and level, metadata included.
func TestShortNamesWhatIsNotHeld(t *testing.T) {
	t.Parallel()
	want := github.Permissions{"contents": "write", "issues": "read"}
	held := map[string]string{"contents": "read", "issues": "write"}
	if got := want.Short(held); !slices.Equal(got, []string{"contents", "metadata"}) {
		t.Errorf("short = %v", got)
	}
	held["contents"], held["metadata"] = "write", "read"
	if got := want.Short(held); len(got) != 0 {
		t.Errorf("short = %v with everything held", got)
	}
}

// THE TIERS MIGRATE TO GITHUB'S OWN PERMISSIONS, as research section 8
// records them, and a typo is reported rather than silently migrated.
func TestTiersMigrateToGitHubsPermissions(t *testing.T) {
	t.Parallel()
	for tier, tc := range map[string]struct {
		workflows bool
		want      github.Permissions
	}{
		"read_only": {want: github.Permissions{
			"contents": "read", "issues": "read", "pull_requests": "read",
			"checks": "read", "actions": "read", "deployments": "read",
		}},
		"review": {want: github.Permissions{
			"contents": "read", "issues": "write", "pull_requests": "write",
			"checks": "read", "actions": "read", "deployments": "read",
		}},
		"full_access": {workflows: true, want: github.Permissions{
			"contents": "write", "issues": "write", "pull_requests": "write",
			"checks": "write", "actions": "read", "deployments": "read", "workflows": "write",
		}},
		// The opt-in only ever rode on full access.
		"": {workflows: true, want: github.Permissions{
			"contents": "read", "issues": "read", "pull_requests": "read",
			"checks": "read", "actions": "read", "deployments": "read",
		}},
	} {
		got, ok := github.TierPermissions(tier, tc.workflows)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("%q: migrated to %v (known %v), want %v", tier, got, ok, tc.want)
		}
		if _, problems := github.ParsePermissions(got.Strings()); len(problems) > 0 {
			t.Errorf("%q migrated to a set the validator refuses: %q", tier, problems)
		}
	}
	if _, ok := github.TierPermissions("administrator", false); ok {
		t.Error("an unknown tier migrated as if it were known")
	}
}

// THE MIGRATION DROPS NOTHING: every permission a tier's tokens carried is
// one the catalogue offers, at the same level, so a migrated seat's tokens
// carry exactly what they did (Metadata rides on every token anyway).
func TestNoTierPermissionIsDroppedByTheMigration(t *testing.T) {
	t.Parallel()
	for _, tier := range github.Tiers {
		for _, workflows := range []bool{false, true} {
			held := github.Access{Tier: tier, Workflows: workflows}.Permissions()
			got, _ := github.TierPermissions(string(tier), workflows)
			for key, level := range held {
				if key == "metadata" {
					continue
				}
				if string(got[key]) != level {
					t.Errorf("%s (workflows %v) carried %s=%s and migrates to %q",
						tier, workflows, key, level, got[key])
				}
			}
		}
	}
}

// THE SENTENCE USES GITHUB'S NAMES, in catalogue order.
func TestDescribeUsesGitHubsNames(t *testing.T) {
	t.Parallel()
	got := github.Permissions{"workflows": "write", "contents": "write", "issues": "read"}.Describe()
	if got != "Contents: Read and write, Issues: Read-only, Workflows: Read and write" {
		t.Errorf("describe = %q", got)
	}
	if got := (github.Permissions{}).Describe(); got != "No access" {
		t.Errorf("an empty seat reads %q", got)
	}
	if got := github.Names([]string{"pull_requests", "metadata", "pages"}); got != "Pull requests, Metadata, pages" {
		t.Errorf("names = %q", got)
	}
}

// tierPerms is a retired tier's permissions, for fixtures written against the
// tiers: what the migration gives a seat that held it.
func tierPerms(tier string, workflows bool) github.Permissions {
	perms, ok := github.TierPermissions(tier, workflows)
	if !ok {
		panic("tierPerms: unknown tier " + tier)
	}
	return perms
}
