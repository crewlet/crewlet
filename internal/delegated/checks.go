package delegated

import (
	"slices"
	"strings"
)

// Release is what a rule of [KindPRMerge] or [KindDeploy] adds to the
// engine's own conditions on a merge or a deploy: path globs that always go to
// people, and the checks it requires on a repository's branch.
type Release struct {
	// EscalatePaths are the rule's always_escalate_paths.
	EscalatePaths []string
	// RequiredChecks are the rule's required_checks.
	RequiredChecks []RequiredChecks
}

// RequiredChecks are the checks a rule requires on one repository's branch
// before a delegate's allow of a merge into it, or a deploy from it, stands,
// beside whatever the code host itself requires there.
//
// THE COMPANY NAMES THEM, THE ENGINE JUDGES THEM: a code host that enforces
// no check (a private GitHub repository on a plan without branch protection)
// still has CI, and a declared check passes only as that host reports it on
// exactly the commit decided, by a reporter that is not one of the company's
// own agents. What the delegate says changes none of it.
type RequiredChecks struct {
	// Repo is the repository as owner/name.
	Repo string
	// Branch is the branch a merge targets, or a deploy's changes merged into.
	Branch string
	// Checks are the checks' names, each as the host reports it.
	Checks []string
}

// DeclaredFor is every check lists require on repo's branch, each once and
// sorted. The repository is matched ignoring case, as GitHub names one; the
// branch and the checks exactly, as a ref and a report are matched.
func DeclaredFor(lists []RequiredChecks, repo, branch string) []string {
	var out []string
	for _, l := range lists {
		if strings.EqualFold(l.Repo, repo) && l.Branch == branch {
			out = append(out, l.Checks...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
