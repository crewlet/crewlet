package delegated

import (
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/gitref"
)

// Unmet is one reason the engine puts a delegate's allow to people instead:
// its closed word, as the access log records it, and the sentence people and
// the delegate read.
type Unmet struct {
	Word string
	Says string
}

// The closed words of the merge and deploy conditions ([KindPRMerge],
// [KindDeploy]), each read from the code host when the verdict is given.
const (
	// UnmetUnread is a condition the engine could not read, so a person
	// decides.
	UnmetUnread = "facts_unread"
	// UnmetHeadMoved is a change whose head is not the one asked about.
	UnmetHeadMoved = "head_moved"
	// UnmetNotOpen is a change that is closed or merged.
	UnmetNotOpen = "not_open"
	// UnmetNoChecks is a target branch that requires no check, so nothing
	// shows the change passed CI.
	UnmetNoChecks = "no_required_checks"
	// UnmetChecks is a required check that has not passed on the commit.
	UnmetChecks = "checks_not_passed"
	// UnmetMergeable is a change that does not merge cleanly into its target.
	UnmetMergeable = "not_mergeable"
	// UnmetNotReady is a precondition of the target's own merge gate unmet.
	UnmetNotReady = "not_ready"
	// UnmetApproval is a head nobody but its authors approved.
	UnmetApproval = "no_approval"
	// UnmetUnclassified is a change touching more paths than the engine
	// reads, so the always-escalate paths cannot be ruled out.
	UnmetUnclassified = "paths_unclassified"
	// UnmetNotMerged is a change a deploy names that has not merged.
	UnmetNotMerged = "not_merged"
	// UnmetNotContained is a deploy commit that does not hold a change it
	// names, or is not on the changes' branch.
	UnmetNotContained = "not_contained"
)

// PathClass is one kind of repository path no delegate lets a change through
// on: a merge or a deploy of a change touching one goes to people.
type PathClass struct {
	// Name is the class's closed word, as the audit records it.
	Name string
	// Category is the always-escalate category it is a case of.
	Category string
	// Says is what the class is, as the delegate and a person read it.
	Says  string
	globs []string
}

// The path classes' names.
const (
	PathCIPipelines    = "ci_pipelines"
	PathCodeOwners     = "code_owners"
	PathSecretFiles    = "secret_files"
	PathAccessPolicies = "access_policies"
	PathInfrastructure = "infrastructure"
	PathMigrations     = "migrations"
	PathCompany        = "crewlet_company"
	// PathRule is a glob the rule added (delegated_decisions.rules[].
	// always_escalate_paths).
	PathRule = "rule_path"
)

// pathClasses are the built-in classes, matched by [gitref.PathGlob].
//
// THE CONVENTIONAL PLACES ONLY: a repository can keep a secret or a migration
// anywhere, which is why a rule adds its own globs and the delegate is told to
// escalate what it cannot judge. A match too many costs a person's minute.
var pathClasses = []PathClass{
	{Name: PathCIPipelines, Category: CategorySecrets,
		Says: "changes a CI pipeline, which runs with the repository's secrets",
		globs: []string{".github/workflows/**", ".github/actions/**", ".gitlab-ci.yml", ".gitlab/ci/**",
			".circleci/**", ".buildkite/**", "**/Jenkinsfile", "azure-pipelines.yml",
			".crewlet/**"}},
	{Name: PathCodeOwners, Category: CategoryAccess,
		Says:  "changes who owns and approves the code",
		globs: []string{"CODEOWNERS", ".github/CODEOWNERS", ".gitlab/CODEOWNERS", "docs/CODEOWNERS"}},
	{Name: PathSecretFiles, Category: CategorySecrets,
		Says: "touches a file that holds keys or secrets",
		globs: []string{"**/.env", "**/.env.*", "**/*.pem", "**/*.key", "**/*.p12", "**/*.pfx", "**/*.jks",
			"**/*.keystore", "**/id_rsa", "**/id_ed25519", "**/.npmrc", "**/.pypirc", "**/secrets/**"}},
	{Name: PathAccessPolicies, Category: CategoryAccess,
		Says:  "changes access policies or roles",
		globs: []string{"**/iam/**", "**/rbac/**", "**/*.rego"}},
	{Name: PathInfrastructure, Category: CategoryBudget,
		Says: "changes infrastructure the company runs and pays for",
		globs: []string{"**/*.tf", "**/*.tfvars", "**/terraform/**", "**/pulumi/**", "**/Pulumi.yaml",
			"**/Pulumi.*.yaml"}},
	{Name: PathMigrations, Category: CategoryDeletion,
		Says:  "changes a database migration, which can drop or rewrite data",
		globs: []string{"**/migrations/**", "**/migrate/**", "**/*.sql"}},
	{Name: PathCompany, Category: CategoryCompany,
		Says:  "changes a Crewlet company document",
		globs: []string{"**/company.yaml", "**/crewlet.yaml"}},
}

// PathClasses is every built-in path class, in the order a reader is told.
func PathClasses() []PathClass { return slices.Clone(pathClasses) }

// parsedClasses are the built-in classes' globs, read once.
var parsedClasses = sync.OnceValue(func() [][]gitref.PathGlob {
	out := make([][]gitref.PathGlob, len(pathClasses))
	for i, c := range pathClasses {
		for _, glob := range c.globs {
			g, err := gitref.ParsePathGlob(glob)
			if err != nil {
				panic("delegated: built-in path glob " + glob + ": " + err.Error())
			}
			out[i] = append(out[i], g)
		}
	}
	return out
})

// Globs are the class's globs, as its documentation lists them.
func (c PathClass) Globs() []string { return slices.Clone(c.globs) }

// PathMatch is one class a change's paths touch, and the first path of them
// it matched.
type PathMatch struct {
	Class, Category, Says string
	Path                  string
}

// EscalatingPaths is every class, and every glob the rule added, that one of
// paths touches, each once with the first path it matched. A rule's glob that
// does not parse matches every path: config refuses one, and a glob nobody can
// read is no reason to let a change through.
func EscalatingPaths(paths, rule []string) []PathMatch {
	var out []PathMatch
	for i, globs := range parsedClasses() {
		c := pathClasses[i]
		if path, ok := firstMatch(paths, globs); ok {
			out = append(out, PathMatch{Class: c.Name, Category: c.Category, Says: c.Says, Path: path})
		}
	}
	for _, glob := range rule {
		g, err := gitref.ParsePathGlob(glob)
		path, ok := "", len(paths) > 0
		if ok {
			path = paths[0]
		}
		if err == nil {
			path, ok = firstMatch(paths, []gitref.PathGlob{g})
		}
		if ok {
			out = append(out, PathMatch{Class: PathRule, Says: "touches " + glob + ", which the rule always " +
				"puts to people", Path: path})
		}
	}
	return out
}

// firstMatch is the first of paths a glob of globs matches.
func firstMatch(paths []string, globs []gitref.PathGlob) (string, bool) {
	for _, p := range paths {
		for _, g := range globs {
			if g.Match(p) {
				return p, true
			}
		}
	}
	return "", false
}
