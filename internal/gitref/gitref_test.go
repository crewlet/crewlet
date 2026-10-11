package gitref_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitref"
)

// refCases is every ref verdict this package states, beside git's. git is git
// check-ref-format's verdict, WRITTEN DOWN and re-measured on every run, so
// the table cannot drift from the git it describes — and GitRefFormat's on
// every row, since that helper claims to be git's rule and nothing more; ours
// is ValidRef's; why is non-empty exactly when they differ, naming the rule of
// ValidRef's doc under which this package is the stricter one.
var refCases = []struct {
	ref       string
	git, ours bool
	why       string
}{
	// Valid, to git and to us.
	{"refs/heads/main", true, true, ""},
	{"refs/heads/release/1.2", true, true, ""},
	{"refs/x/y", true, true, ""},      // any namespace
	{"refs/heads/@", true, true, ""},  // only the WHOLE ref "@" is refused
	{"refs/heads/a@", true, true, ""}, // '@' alone is no sequence git refuses
	{"refs/heads/a@b", true, true, ""},
	{"refs/heads/a{b}", true, true, ""}, // '{' is refused only after '@'
	{"refs/heads/{", true, true, ""},
	{"refs/heads/a]b", true, true, ""},      // ']' is not '['
	{"refs/heads/-a", true, true, ""},       // a leading '-' is a BRANCH rule
	{"refs/heads/HEAD", true, true, ""},     // HEAD is a BRANCH rule
	{"refs/heads/é", true, true, ""},        // UTF-8 beyond ASCII is a ref
	{"refs/heads/a./b", true, true, ""},     // a component may END with '.'
	{"refs/heads/a.lock.b", true, true, ""}, // .lock only as a suffix
	{"refs/heads/x.LOCK", true, true, ""},   // and only in lowercase
	{"refs/heads/lock", true, true, ""},
	{"refs/heads/" + strings.Repeat("a", 244), true, true, ""}, // exactly MaxRefNameBytes

	// Refused by git's own rules, and so by us.
	{"refs/heads/a@{b", false, false, ""},
	{"refs/heads/@{", false, false, ""},
	{"refs/heads/a..b", false, false, ""},
	{"refs/heads/...", false, false, ""},
	{"refs/heads/..", false, false, ""},
	{"refs/heads/.", false, false, ""},
	{"refs/heads/.a", false, false, ""},
	{"refs/heads/a/.b", false, false, ""},
	{"refs/heads/.lock", false, false, ""},
	{"refs/heads/a.lock", false, false, ""},
	{"refs/heads/a.b.lock", false, false, ""},
	{"refs/heads/a.lock.lock", false, false, ""},
	{"refs/heads/a.lock/b", false, false, ""},
	{"refs/heads/a/b.lock/c", false, false, ""},
	{"refs/heads/a/", false, false, ""},
	{"refs/heads//a", false, false, ""},
	{"/refs/heads/a", false, false, ""},
	{"refs/heads/a.", false, false, ""},
	{"refs/heads/a/..", false, false, ""},
	{"refs/heads/a b", false, false, ""},
	{"refs/heads/a~b", false, false, ""},
	{"refs/heads/a^b", false, false, ""},
	{"refs/heads/a:b", false, false, ""},
	{"refs/heads/a?b", false, false, ""},
	{"refs/heads/a*b", false, false, ""},
	{"refs/heads/a[b", false, false, ""},
	{`refs/heads/a\b`, false, false, ""},
	{"refs/heads/a\x01b", false, false, ""},
	{"refs/heads/a\x1f", false, false, ""},
	{"refs/heads/a\tb", false, false, ""},
	{"refs/heads/a\x7fb", false, false, ""},
	{"@", false, false, ""},
	{"", false, false, ""},
	{"refs", false, false, ""}, // one component: git's one-level rule
	{"a", false, false, ""},
	{"main", false, false, ""},
	{"HEAD", false, false, ""},
	{"lock", false, false, ""},
	{"é", false, false, ""},
	{"\xff", false, false, ""},
	{"a.", false, false, ""},

	// Stricter than git.
	{"refs/x", true, false, "rule 3: refs/ then at least two more components"},
	{"refs/@", true, false, "rule 3: refs/ then at least two more components"},
	{"refs/heads/" + strings.Repeat("a", 245), true, false, "rule 1: 256 bytes, past MaxRefNameBytes"},
	{"refs/heads/" + strings.Repeat("a", 250), true, false, "rule 1: 261 bytes, past MaxRefNameBytes"},
	{"refs/heads/\xff", true, false, "rule 2: not UTF-8"},
	{"refs/heads/a\x80", true, false, "rule 2: not UTF-8"},
	{"refs/heads/\xc3", true, false, "rule 2: a UTF-8 sequence cut short"},
}

// TestValidRefAgreesWithGit holds ValidRef to git check-ref-format on every
// row of refCases: git's verdict and GitRefFormat's are asserted equal to the
// written one, and ValidRef's to ours — so a rule transcribed wrong, a git
// whose answer changed, a helper that stopped being git's rule and a row
// whose stricter-than-git reason went missing each fail.
func TestValidRefAgreesWithGit(t *testing.T) {
	c := gittest.NewClient(t)
	for _, tc := range refCases {
		if (tc.why != "") != (tc.git != tc.ours) {
			t.Errorf("%q: git %v, ours %v, why %q — a row says why exactly when the two differ", tc.ref, tc.git, tc.ours, tc.why)
		}
		_, stderr, err := c.Try(t, "", nil, "check-ref-format", tc.ref)
		if got := gitVerdict(t, tc.ref, err, stderr, 1); got != tc.git {
			t.Errorf("git check-ref-format %q is %v, the table says %v", tc.ref, got, tc.git)
		}
		if got := gitref.GitRefFormat(tc.ref); got != tc.git {
			t.Errorf("GitRefFormat(%q) = %v, git check-ref-format says %v", tc.ref, got, tc.git)
		}
		if got := gitref.ValidRef(tc.ref); got != tc.ours {
			t.Errorf("ValidRef(%q) = %v, want %v (git: %v; %s)", tc.ref, got, tc.ours, tc.git, tc.why)
		}
	}
}

// TestValidBranchAgreesWithGitBranchRules holds ValidBranch to git
// check-ref-format --branch, run outside any repository so git reads the name
// as it is written rather than expanding @{-1} or @ against a HEAD.
func TestValidBranchAgreesWithGitBranchRules(t *testing.T) {
	c := gittest.NewClient(t)
	for _, tc := range []struct {
		branch    string
		git, ours bool
		why       string
	}{
		{"main", true, true, ""},
		{"release/1.2", true, true, ""},
		{"feat/x", true, true, ""},
		{"x/HEAD", true, true, ""}, // only the whole name HEAD is refused
		{"HEAD/x", true, true, ""},
		{"x-", true, true, ""}, // only a LEADING '-'
		{"HEAD", false, false, ""},
		{"-a", false, false, ""},
		{"-", false, false, ""},
		{"a..b", false, false, ""},
		{"a.lock", false, false, ""},
		{".a", false, false, ""},
		{"a b", false, false, ""},
		{"@{-1}", false, false, ""},
		{"", false, false, ""},
		{strings.Repeat("a", 244), true, true, ""},
		{strings.Repeat("a", 245), true, false, "refs/heads/ and the name are past MaxRefNameBytes"},
		{"@", true, false, "@ alone is HEAD in every revision expression"},
	} {
		if (tc.why != "") != (tc.git != tc.ours) {
			t.Errorf("%q: git %v, ours %v, why %q — a row says why exactly when the two differ", tc.branch, tc.git, tc.ours, tc.why)
		}
		// git dies on a name it refuses as a branch: exit 128, never 1.
		_, stderr, err := c.Try(t, "", nil, "check-ref-format", "--branch", tc.branch)
		if got := gitVerdict(t, tc.branch, err, stderr, 128); got != tc.git {
			t.Errorf("git check-ref-format --branch %q is %v, the table says %v", tc.branch, got, tc.git)
		}
		if got := gitref.ValidBranch(tc.branch); got != tc.ours {
			t.Errorf("ValidBranch(%q) = %v, want %v (git: %v; %s)", tc.branch, got, tc.ours, tc.git, tc.why)
		}
	}
}

// gitVerdict reads a check-ref-format run: a clean exit is valid, the exit
// status git refuses a name with is invalid, and anything else — a usage
// error, a signal, a deadline — is not a verdict at all and fails the test
// rather than being read as either.
func gitVerdict(t *testing.T, name string, err error, stderr string, refused int) bool {
	t.Helper()
	if err == nil {
		return true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == refused {
		return false
	}
	t.Fatalf("git check-ref-format gave no verdict on %q: %v\n%s", name, err, stderr)
	return false
}

func TestValidRepoName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"nimbus-web", true},
		{"a", true},
		{"0", true},
		{"0day", true},
		{"a-b", true},
		{"a--b", true}, // a label may hold hyphens together, only not at an end
		{strings.Repeat("a", gitref.MaxRepoNameBytes), true},
		{"a" + strings.Repeat("-", gitref.MaxRepoNameBytes-2) + "0", true},
		{"", false},
		{"-a", false},
		{"web-", false}, // no label ends in a hyphen
		{"-", false},
		{strings.Repeat("a", gitref.MaxRepoNameBytes-1) + "-", false},
		{"A", false},
		{"nimbus-Web", false},
		{"a_b", false},
		{"a.git", false},
		{"a.b", false},
		{"a/b", false},
		{"a b", false},
		{strings.Repeat("a", gitref.MaxRepoNameBytes+1), false},
		{"é", false},
	} {
		if got := gitref.ValidRepoName(tc.name); got != tc.want {
			t.Errorf("ValidRepoName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidOID(t *testing.T) {
	hex := "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		oid  string
		want bool
	}{
		{hex, true},
		{gitref.ZeroOID, true},
		{strings.Repeat("f", gitref.OIDHexLen), true},
		{hex[:39], false},
		{hex + "8", false},
		{strings.ToUpper(hex), false},
		{"0123456789ABCDEF0123456789abcdef01234567", false},
		{"g123456789abcdef0123456789abcdef01234567", false},
		{" 123456789abcdef0123456789abcdef01234567", false},
		{"", false},
		{hex + hex[:24], false}, // a sha256 id is another grammar
	} {
		if got := gitref.ValidOID(tc.oid); got != tc.want {
			t.Errorf("ValidOID(%q) = %v, want %v", tc.oid, got, tc.want)
		}
	}
	if len(gitref.ZeroOID) != gitref.OIDHexLen || strings.Trim(gitref.ZeroOID, "0") != "" {
		t.Errorf("ZeroOID %q is not %d zeros", gitref.ZeroOID, gitref.OIDHexLen)
	}
}
