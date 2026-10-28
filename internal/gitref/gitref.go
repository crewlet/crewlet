// Package gitref is THE GRAMMAR OF GIT NAMES THE ENGINE AND ITS CONFIGURATION
// SHARE: repository names, refs, branch names, protection patterns and object
// ids, with their bounds — and where the seats' own branches live
// ([SeatsRoot], [ReservedSeatBranch]), which a push and a repository's default
// branch are both judged by.
//
// Configuration validates a document with it at `crewlet validate`, the code
// domain decides a push with it, and the sandbox judges with it the branch a
// coding run names — the one a parked run records for its re-seed, and each
// one a run's report says it delivered. Two copies of one grammar drift
// (ADR-0008) — a name `crewlet validate` accepts and a push then refuses, or
// the reverse, or a branch a run records that no ref can be — and the code
// domain imports config, so the grammar lives below both. It imports nothing
// but the standard library for the same reason: whatever needs a git name
// judged takes this package and nothing with it.
//
// # Git's rules are the floor
//
// [ValidRef] is `git check-ref-format` implemented in Go, and [MatchPattern]
// is `git for-each-ref`'s glob over whole components. Neither is trusted to
// have been transcribed correctly: the tests run every case of both tables
// through real git and hold the Go rule to git's verdict, and each row
// records git's answer beside this package's, so a row whose two answers
// differ has to say why and a git that changes its mind fails the suite
// instead of quietly disagreeing with it. The matcher is then held to the
// rule its doc states, written out plainly in the tests, on every shape a
// short pattern and ref can take — because it is written for speed, and the
// git table only samples.
//
// # Where this package is stricter than git
//
// Each is stated at its rule with its reason, and each is a row of the tests
// where git's answer is the other one:
//
//   - a whole ref is at most [MaxRefNameBytes], and so a branch at most
//     [MaxBranchBytes] (git bounds neither a ref nor a component);
//   - a ref is valid UTF-8 (git takes any byte from 0x80 up);
//   - a ref has the form refs/<namespace>/<name> (git takes refs/x);
//   - a branch is never `@` (git takes it outside a repository);
//   - a literal protection pattern matches only the identical ref (git's
//     for-each-ref also matches it as a prefix up to a `/`).
//
// Each narrows what git would take to a name the engine carries correctly —
// across JSON, on every client's disk, and in a protection rule that reaches
// exactly as far as it says.
//
// # Measured
//
// On four vCPUs of a 2.8 GHz Xeon shared with other builds, with Go 1.27
// (go test -bench . -count 6, medians; pattern_test.go names each arm's
// pattern and ref), a parsed rule ([Pattern.Match]) asked about a ref costs
// 4.9 ns for a literal and 42 ns for the typical glob on a 34-byte release
// branch (refs/heads/release/**). A rule with at most one ** compares each of
// its parts once, at the ref's start or at its end, so on the deepest ref a
// push may name (122 components below refs/heads/) refs/heads/**/hotfix costs
// 37 ns, what it costs on a shallow one. Only a run of parts between two **s
// is searched for along the ref: refs/heads/**/hotfix/** costs 0.74 µs on
// that deepest ref, and the costliest rule found within MaxPatternComponents
// — three globs and a fourth that fails, between two **s, each glob a piece
// for every byte of the ref's components, so the run is tried at every place
// and fails at its last part each time — costs 11 µs on a ref of six-byte
// components, and between 10 and 13 µs at every width from one byte to
// twenty-four (narrow components are many places, wide ones many pieces).
// That is the bound the grammar sets: a caller asking R rules about C refs
// pays at most R × C × 13 µs of a core (the code domain's decide asks its
// repository's rules about one round's commands), where a rule bounded by its
// bytes alone costs 85 µs — a run of fifty-nine globs between two **s, tried
// at every split of the deepest ref. None of them allocates. Reading a rule
// ([ParsePattern]) costs 71 ns for a literal and 0.32 µs and one allocation
// for a glob, once a rule; the one-off [MatchPattern], which checks both and
// reads the rule on every call, costs 0.14 µs for a literal, 0.38 µs for the
// typical glob and 13 µs for the costliest, and allocates nothing. [ValidRef],
// which every command's ref passes once before any rule is asked about it,
// costs 0.16 µs on the release branch and 0.72 µs on the deepest ref.
package gitref

import (
	"strings"
	"unicode/utf8"
)

// MaxRefNameBytes is the longest ref a push may create or move.
//
// 255: a ref is a chain of path components on every client's disk (a loose
// ref is a file under .git/refs), and 255 bytes is the longest file name every
// filesystem git supports takes, so a ref no longer than that fits on any of
// them whichever of its components holds its bytes. Bounding the WHOLE ref
// there, rather than each component, is stricter than git (which bounds
// neither) and is what bounds the bytes a push of N refs carries: N refs of at
// most this length, whatever their shape.
const MaxRefNameBytes = 255

// MaxBranchBytes is the longest branch [ValidBranch] accepts: 244 bytes, what
// [MaxRefNameBytes] leaves after refs/heads/.
//
// DERIVED, NEVER A BOUND OF ITS OWN: a branch is the ref refs/heads/<branch>,
// so a longer one is a ref no push can create. It is the figure a caller
// states where it carries a branch by its name alone — the sandbox's parked
// run, a run's delivered refs — so that what it says it keeps is what a ref
// can hold.
const MaxBranchBytes = MaxRefNameBytes - len("refs/heads/")

// MaxRepoNameBytes is the longest repository name, of either form: 63, the
// longest DNS label (RFC 1035), because a PROJECT repository's name IS one — a
// host-name label as RFC 1123 has it, see [ValidProjectName] — and that label
// is the narrowest name most of what a deployment runs on takes: a host, a
// container, most Kubernetes objects. So whatever a deployment names after a
// repository takes the name unchanged, with nothing to escape, shorten or hash
// on the way. A seat's SCRATCH repository ([ScratchName]) is two such labels
// joined by a dot, a DNS-1123 subdomain, held to the same 63 bytes in all, so
// one bound reads every name whichever form it takes.
const MaxRepoNameBytes = 63

// ZeroOID is git's null object id: the old value of a create and the new
// value of a delete.
const ZeroOID = "0000000000000000000000000000000000000000"

// OIDHexLen is the length of an object id in hex. The engine's repositories
// are object-format sha1: a sha256 repository spells every object id in 64
// digits, which is another grammar, and git's interoperation between the two
// formats is experimental.
const OIDHexLen = 40

// ValidProjectName reports whether name is a PROJECT repository's name: one
// host-name label (RFC 1123) in lowercase — letters, digits and hyphens,
// beginning and ending with a letter or a digit, at most MaxRepoNameBytes in
// all (^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$). Every name code.repos declares,
// and every project repository a tool creates, takes it.
//
// No dot, so no name ends in .git — the suffix a git URL conventionally
// carries and a client strips (cloning …/web.git makes the directory web), so
// a name that carried it would be one address for two repositories. And no
// dot is also what tells a project's name from a seat's scratch repository's
// ([ScratchName]), the one form with a dot in it. Lowercase only, so a name is
// one name on a case-insensitive filesystem too.
func ValidProjectName(name string) bool {
	return len(name) <= MaxRepoNameBytes && validLabel(name)
}

// ScratchName reports whether name is a SEAT'S OWN repository's name —
// <handle>.<slug>: exactly ONE dot, each side a host-name label as
// [ValidProjectName] has it, at most MaxRepoNameBytes in all — and the handle
// it carries, which names the seat it belongs to. The whole is then a DNS-1123
// subdomain, which a deployment can name things after unchanged as it can a
// label; a trailing hyphen on the handle would not be one, so a seat whose
// handle is not a label has no scratch repository rather than a name nothing
// can be named after.
//
// The slug is never "git": <handle>.git is indistinguishable from the URL of
// the repository named <handle> with git's optional .git suffix, which a
// client strips — one address for two repositories.
func ScratchName(name string) (handle string, ok bool) {
	if len(name) > MaxRepoNameBytes {
		return "", false
	}
	handle, slug, found := strings.Cut(name, ".")
	if !found || !validLabel(handle) || !validLabel(slug) || slug == "git" {
		return "", false
	}
	return handle, true
}

// ValidRepoName reports whether name is a repository name of EITHER form — a
// project's ([ValidProjectName]) or a seat's scratch repository's
// ([ScratchName]): what every reader of a repository's name holds it to, the
// code host's admission, a resolve and every tool among them. Which form a
// name takes decides what the repository is FOR, so a reader that needs the
// form asks the narrower predicate.
func ValidRepoName(name string) bool {
	if ValidProjectName(name) {
		return true
	}
	_, ok := ScratchName(name)
	return ok
}

// validLabel reports one lowercase host-name label (RFC 1123) of any length:
// letters, digits and hyphens, beginning and ending with a letter or a digit.
// The length is the caller's, since a scratch name bounds its two labels
// together.
func validLabel(s string) bool {
	if s == "" || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ValidOID reports whether oid is a SHA-1 object id: exactly OIDHexLen
// lowercase hex digits. ZeroOID is one.
//
// Lowercase only, because git prints ids that way and an id is compared byte
// for byte everywhere the engine stores one: an upper-case spelling would be
// a second id for the same object.
//
// It reads the bytes of a line as it reads a string, because the one rule is
// also asked of every line of a file — a cache's packed-refs, merged after
// every push, up to code.MaxRefsPerRepo lines — where a string made of each
// id only to ask it would be an allocation a line, and a second copy of the
// rule the cheaper way round.
func ValidOID[S ~string | ~[]byte](oid S) bool {
	if len(oid) != OIDHexLen {
		return false
	}
	for i := 0; i < len(oid); i++ {
		switch c := oid[i]; {
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// ValidRef reports whether ref is a ref a push may create or move. It is git's
// check-ref-format (without --allow-onelevel, --refspec-pattern or
// --normalize) and three rules of the engine's own, each stricter than git:
//
//  1. at most MaxRefNameBytes, for the reason given there;
//  2. valid UTF-8 — git takes any byte from 0x80 up, but a ref crosses JSON on
//     every wire this engine has, where invalid UTF-8 is replaced, so two
//     different refs would arrive as one;
//  3. refs/ then at least two more components — git's check-ref-format takes
//     refs/x, but a ref a push names is a name inside a namespace, and every
//     rule the engine keys on a ref (a branch, a tag, a protection) reads the
//     namespace first.
//
// Git's own rules, as git-check-ref-format(1) and refs.c state them: at least
// two components (which rule 3 narrows); no component is empty, begins with
// '.' or ends with ".lock"; no ".." and no "@{" anywhere; no byte below 0x20,
// no 0x7f, and none of space ~ ^ : ? * [ \; the ref does not end with '/' or
// '.'; and it is not the single string "@". A component may END with '.', and
// a ref may hold '@', '{' or ']' on their own: only the sequences above are
// refused.
func ValidRef(ref string) bool {
	if len(ref) > MaxRefNameBytes || !utf8.ValidString(ref) || !strings.HasPrefix(ref, "refs/") {
		return false
	}
	components, ok := refFormat(ref, false)
	return ok && components >= 3
}

// refFormat is git's check_refname_format over s but for the count of its
// components, which it returns for the caller's own rule: every component
// non-empty, not beginning with '.' and not ending with ".lock"; no ".." and
// no "@{"; no byte refused; s not ending with '.'. A sequence git refuses
// never spans a '/' (".." across one is "./.", whose second half begins with
// '.'; "@{" across one is "@/{"), so reading s a component at a time is
// reading it whole. It reads s once, a byte at a time, because every ref a
// push names passes through it before any rule is asked about it.
//
// With star, '*' is read as an ordinary byte rather than refused, except that
// two together are a component of their own or nothing: that is how a
// protection pattern is checked, since a glob component that is a valid
// component with each * read as a letter is one some ref can match. No rule
// here reads a '*' any other way — it is not '.', '@' or part of ".lock" — so
// the reading is exactly the component with each * replaced by a letter.
func refFormat(s string, star bool) (components int, ok bool) {
	start := 0       // where the component being read begins
	doubled := false // it holds "**"
	var last byte    // the byte before this one in the component, 0 at its start
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '/' {
			c := s[start:i]
			if c == "" || strings.HasSuffix(c, ".lock") || doubled && c != "**" {
				return 0, false
			}
			components++
			start, doubled, last = i+1, false, 0
			continue
		}
		b := s[i]
		switch {
		case refused[b] && (b != '*' || !star):
			return 0, false
		case b == '.' && (i == start || last == '.'):
			return 0, false
		case b == '{' && last == '@':
			return 0, false
		case b == '*' && last == '*':
			doubled = true
		}
		last = b
	}
	return components, !strings.HasSuffix(s, ".")
}

// refused marks the bytes git refuses anywhere in a ref: every byte below
// 0x20, 0x7f, and space ~ ^ : ? * [ \. A table rather than a search of that
// list, because every byte of every ref a push names passes through it.
var refused = func() (t [256]bool) {
	for b := range 0x20 {
		t[b] = true
	}
	for _, b := range []byte("\x7f ~^:?*[\\") {
		t[b] = true
	}
	return t
}()

// ValidBranch reports whether branch is a branch name: refs/heads/<branch>
// is a ValidRef, so the name is valid UTF-8 and at most [MaxBranchBytes], and
// git's two branch rules hold — it is not HEAD and does not begin with '-'
// (git check-ref-format --branch refuses both: HEAD would be refs/heads/HEAD,
// which every revision expression reads as the symbolic HEAD, and a leading
// '-' is an option to every command that takes a branch).
//
// Stricter than git: it is not "@". Outside a repository git accepts it, but
// "@" alone is HEAD in every revision expression — with a branch named @ and
// HEAD on main, `git rev-parse --symbolic-full-name @` names refs/heads/main —
// so a branch named @ is one no revision expression can name by its own name:
// `git log @` is the history of whatever HEAD is on.
func ValidBranch(branch string) bool {
	return branch != "HEAD" && branch != "@" && !strings.HasPrefix(branch, "-") &&
		ValidRef("refs/heads/"+branch)
}

// SeatsRoot is the directory of branches every seat's own namespace lives
// under: a seat's namespace is refs/heads/seats/<handle>/, NEVER
// refs/heads/<handle>/. A handle is a slug of a role name — docs, qa,
// release, infra — and those are the prefixes people name their own branches
// under, so a namespace at the handle alone would hand a full_access seat
// that only READS a repository every docs/* or release/* branch a person
// pushed there, to force, to delete, or to create one a pipeline deploys. No
// branch convention uses seats/, so whose a ref is is decidable from the ref
// alone.
//
// Here rather than in the code domain because two judges read it — the
// domain deciding a push, and the configuration validating a repository's
// default branch — and the domain imports the configuration.
const SeatsRoot = "refs/heads/seats/"

// seatsBranch is SeatsRoot as a branch name: "seats/".
const seatsBranch = "seats/"

// ReservedSeatBranch reports the two kinds of branch under refs/heads/seats
// that are nobody's: seats itself, and seats/<one component> — one seat's
// namespace as a branch.
//
// A ref is never created beside a directory of its own name — the code
// domain refuses it (ref_conflict), because every clone holding the one could
// not fetch the other — so a branch named seats would block every seat's
// namespace on its repository, and one named seats/<x> the namespace of the
// seat whose handle is x, one the chart has now or may add later. Reserved,
// neither can ever exist: no push creates one, whoever pushes, and no
// repository takes one as its default branch.
func ReservedSeatBranch(branch string) bool {
	if branch == strings.TrimSuffix(seatsBranch, "/") {
		return true
	}
	rest, under := strings.CutPrefix(branch, seatsBranch)
	return under && !strings.Contains(rest, "/")
}
