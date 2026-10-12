package sandbox_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A BRANCH IS WHAT GIT SAYS ONE IS. Every case below is `git check-ref-format
// --branch`'s own verdict on the name, read off git 2.43 — except `@`, which
// git resolves to the branch HEAD is on rather than naming a branch of its own,
// so it is refused here as the revision it is.
func TestABranchIsANameGitAccepts(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"wip/t1":                true,
		"feature/retry-backoff": true,
		"crewlet/eng/retry":     true,
		"v1.2.0":                true,
		"ünïcode/branch":        true,
		"a./b":                  true,
		"a@b":                   true,
		"wip-":                  true,
		"_wip_":                 true,
		"w!ip,x;y#z{}":          true,
		"a/HEAD":                true,

		"":            false,
		"HEAD":        false,
		"@":           false,
		"-wip":        false,
		"wip..t1":     false,
		"wip t1":      false,
		"wip\tt1":     false,
		"wip\x7ft1":   false,
		"wip~1":       false,
		"wip^":        false,
		"wip:x":       false,
		"wip?":        false,
		"wip*":        false,
		"wip[":        false,
		`wip\x`:       false,
		"/wip":        false,
		"wip/":        false,
		"wip//t1":     false,
		"wip.":        false,
		".wip":        false,
		"wip/.t1":     false,
		"wip.lock":    false,
		"wip.lock/t1": false,
		"wip@{1}":     false,
	} {
		if got := sandbox.ValidBranch(name); got != want {
			t.Errorf("ValidBranch(%q) = %v, want %v", name, got, want)
		}
	}
}
