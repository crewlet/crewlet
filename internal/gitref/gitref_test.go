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
//
// It is every caller's rule — the configuration's default branch, a push's
// branch, the branch a coding run records — so a case any of them depends on
// is a row here, held to git, never a table of a caller's own.
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
		{"ünïcode/branch", true, true, ""},
		{"a./b", true, true, ""}, // a part may END with '.'
		{"a@b", true, true, ""},  // '@' alone is no sequence git refuses
		{"w!ip,x;y#z{}", true, true, ""},
		{"HEAD", false, false, ""},
		{"-a", false, false, ""},
		{"-", false, false, ""},
		{"a..b", false, false, ""},
		{"a.lock", false, false, ""},
		{"wip.lock/t1", false, false, ""},
		{".a", false, false, ""},
		{"wip/.t1", false, false, ""},
		{"wip.", false, false, ""},
		{"a b", false, false, ""},
		{"wip\tt1", false, false, ""},
		{"wip\x7ft1", false, false, ""},
		{"wip~1", false, false, ""},
		{"wip^", false, false, ""},
		{"wip:x", false, false, ""},
		{"wip?", false, false, ""},
		{"wip*", false, false, ""},
		{"wip[", false, false, ""},
		{`wip\x`, false, false, ""},
		{"/wip", false, false, ""},
		{"wip/", false, false, ""},
		{"wip//t1", false, false, ""},
		{"wip@{1}", false, false, ""},
		{"@{-1}", false, false, ""},
		{"", false, false, ""},
		{strings.Repeat("a", gitref.MaxBranchBytes), true, true, ""},
		{strings.Repeat("a", gitref.MaxBranchBytes+1), true, false, "past MaxBranchBytes: refs/heads/ and the name are past MaxRefNameBytes"},
		{"wip\xff", true, false, "a ref crosses JSON, where invalid UTF-8 is replaced and two refs arrive as one"},
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

// THE TWO BRANCHES UNDER seats THAT ARE NOBODY'S are seats itself and one
// component below it — the root and one seat's namespace, as branches — and
// nothing else: a branch inside a namespace is the seat's own, and a name that
// merely begins with seats is a person's.
func TestOnlyTheRootAndASeatsNamespaceAreReservedBranches(t *testing.T) {
	t.Parallel()
	for branch, reserved := range map[string]bool{
		"seats":               true,
		"seats/ana":           true,
		"seats/release-bot":   true,
		"seats/main":          true,
		"seats/ana/x":         false, // inside ana's namespace: ana's own
		"seats/ana/x/y":       false,
		"seatsx":              false,
		"seats-ana":           false,
		"seats.ana":           false,
		"x/seats":             false,
		"x/seats/ana":         false,
		"Seats":               false, // refs are case-sensitive; Seats is no seat's
		"main":                false,
		"refs/heads/seats":    false, // a branch NAME, never a whole ref
		"refs/heads/seats/ab": false,
	} {
		if got := gitref.ReservedSeatBranch(branch); got != reserved {
			t.Errorf("ReservedSeatBranch(%q) = %v, want %v", branch, got, reserved)
		}
	}
	if !strings.HasPrefix(gitref.SeatsRoot, "refs/heads/") ||
		!gitref.ReservedSeatBranch(strings.TrimSuffix(strings.TrimPrefix(gitref.SeatsRoot, "refs/heads/"), "/")) {
		t.Errorf("SeatsRoot %q is not refs/heads/ and the reserved root branch", gitref.SeatsRoot)
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

// A PROJECT'S NAME IS ONE LABEL, a seat's scratch repository's is TWO, and a
// name of either form is a repository's name — each form exactly as its
// predicate states it, so the kind a name says a repository is cannot be read
// two ways.
//
// Mutation: let ScratchName take two dots, a `git` slug, a side ending in a
// hyphen or 64 bytes in all, or let ValidProjectName take a dot — each red.
func TestARepositoryNameIsOneOfTwoForms(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 31) + "." + strings.Repeat("b", 31) // 63 in all
	for _, tc := range []struct {
		name    string
		project bool
		handle  string // the scratch name's handle, "" for none
	}{
		{name: "nimbus-web", project: true},
		{name: "a", project: true},
		{name: "0", project: true},
		{name: "0day", project: true},
		{name: "a-b", project: true},
		{name: "a--b", project: true}, // a label may hold hyphens together, only not at an end
		{name: strings.Repeat("a", gitref.MaxRepoNameBytes), project: true},
		{name: "a" + strings.Repeat("-", gitref.MaxRepoNameBytes-2) + "0", project: true},
		{name: ""},
		{name: "-a"},
		{name: "web-"}, // no label ends in a hyphen
		{name: "-"},
		{name: strings.Repeat("a", gitref.MaxRepoNameBytes-1) + "-"},
		{name: "A"},
		{name: "nimbus-Web"},
		{name: "a_b"},
		{name: "a/b"},
		{name: "a b"},
		{name: strings.Repeat("a", gitref.MaxRepoNameBytes+1)},
		{name: "é"},

		// THE SCRATCH FORM: <handle>.<slug>, one dot, two labels, 63 in all.
		{name: "alice.web", handle: "alice"},
		{name: "a.b", handle: "a"},
		{name: "qa-bot.x2", handle: "qa-bot"},
		{name: "alice.gi", handle: "alice"},   // a slug that only begins as git's
		{name: "alice.gits", handle: "alice"}, // or that is longer
		{name: long, handle: strings.Repeat("a", 31)},
		{name: long + "b"},    // 64 in all
		{name: "a.git"},       // <handle>.git is <handle>'s URL with git's suffix
		{name: "alice.web.x"}, // two dots
		{name: ".web"},        // no handle
		{name: "alice."},      // no slug
		{name: "alice-.web"},  // a handle that is not a label
		{name: "alice.-web"},
		{name: "Alice.web"},
		{name: "alice..web"},
	} {
		handle, scratch := gitref.ScratchName(tc.name)
		if got := gitref.ValidProjectName(tc.name); got != tc.project {
			t.Errorf("ValidProjectName(%q) = %v, want %v", tc.name, got, tc.project)
		}
		if handle != tc.handle || scratch != (tc.handle != "") {
			t.Errorf("ScratchName(%q) = (%q, %v), want (%q, %v)", tc.name, handle, scratch, tc.handle,
				tc.handle != "")
		}
		if got, want := gitref.ValidRepoName(tc.name), tc.project || tc.handle != ""; got != want {
			t.Errorf("ValidRepoName(%q) = %v, want %v — either form, and only those", tc.name, got, want)
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
		if got := gitref.ValidOID([]byte(tc.oid)); got != tc.want {
			t.Errorf("ValidOID of the bytes of %q = %v, want %v as of the string", tc.oid, got, tc.want)
		}
	}
	line := []byte(hex + " refs/heads/main\n")
	if allocs := testing.AllocsPerRun(10, func() { gitref.ValidOID(line[:gitref.OIDHexLen]) }); allocs != 0 {
		t.Errorf("ValidOID of a line's bytes allocated %v times, want none", allocs)
	}
	if len(gitref.ZeroOID) != gitref.OIDHexLen || strings.Trim(gitref.ZeroOID, "0") != "" {
		t.Errorf("ZeroOID %q is not %d zeros", gitref.ZeroOID, gitref.OIDHexLen)
	}
}
