package gitlet

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: every object its apply may write and every object its
// decide read (M5-D33), stated by the WRITER and readable at every version. A
// SUPERSET of what the apply writes, because a record decided from rows a
// record this node could not decode left stale is as wrong as one that writes
// over them: a write is probed against the paths it reads, and a record names
// what it read, so a node behind on any of it decides nothing from it.
//
// It is what a node that cannot decode a record files the deferral under, and
// what a later read or write probes to discover it is behind. The framework
// knows one thing about a path — a hierarchy written left to right, from which
// it computes containment — so the alphabet nests: everything is under the
// domain, and a repository's path is beside its name's rather than under it,
// because a repository is filed by its id, which never changes, while its name
// is a claim on an address that a purge releases for another repository to
// take.
//
// CARRIED AS RESOLVED PATHS rather than as terms a reader composes, which is
// the difference from the tracker's and the knowledge base's scope: every
// code record's objects are known to its writer by id alone, so there is no
// container a reader would have to look up, and a build that cannot interpret
// a path still indexes it.

// The path alphabet. The domain letter is `c`, distinct from every other
// domain's, so a deferral row an operator reads out of a stuck fleet names its
// domain.
const (
	pathDomain     = "c"
	pathName       = "n"
	pathRepo       = "r"
	pathActivation = "a"
	pathChange     = "c"
	pathChecks     = "k"
	pathPerson     = "p"
)

// NamePath is a repository name's claim.
func NamePath(token string) string { return pathDomain + "/" + pathName + "/" + token }

// RepoPath is one repository: its row, its refs, its packs, its pushes and its
// protection rules — everything a record on its subject may write.
func RepoPath(id string) string { return pathDomain + "/" + pathRepo + "/" + id }

// ChangePath is one change's conversation: who is asked to review it, its
// comments, who takes part and its counters — everything a record on the
// change's own subject may write. BESIDE the repository's path rather than
// under it, as a repository's is beside its name's: a change is filed by its
// id, and its repository is named in each record's payload. A record on a
// change's subject names BOTH paths ([ChangeWriter]): its decide reads what
// the repository's records write — the change's row, the repository's status
// — so a record of the repository's this node could not decode leaves the
// conversation's decide reading stale rows, and the scope probe must find it;
// and the deletion gate reads the repository the record is about off this
// scope, never the payload ([gatedRepo]). A MERGEABILITY names the
// repository's checks too ([ChecksPath]): the readiness it stamps judges the
// merge gate, which reads them ([JudgeGate]), beside the position of its
// head's checks it was judged at ([Mergeability.ChecksPosition]).
func ChangePath(id string) string { return pathDomain + "/" + pathChange + "/" + id }

// ChecksPath is one repository's checks: every row a record on its checks
// subject may write — its check rows and their logs. BESIDE the repository's
// path rather than under it, as a change's is, because the two APPLIES write
// apart: a check's record never writes the repository's rows, and of the
// repository's own records only its purge writes a check's — whose every
// step names this path beside the repository's ([Store.PurgeStep]). The
// DECIDES do not read apart, so a record names what its decide reads beside
// what its apply writes (M5-D33): a check's write, decided by the
// [ChecksWriter], reads the repository's row on every arm — archived or
// deleted, and the owner and visibility a seat's rerun is judged by — and the
// changes a dispatch queues at, a rerun's authors are read from and a failed
// head's routing names, so its record names [RepoPath] beside this path and
// [ChangePath] of each of those changes (checkswriter.go's checksScope); and
// a mergeability, a merge and an enqueue read the checks a gate judges, so
// theirs name this path. The deletion gate reads the repository off the
// checks subject alone, never the scope ([gatedRepo]).
func ChecksPath(repoID string) string { return pathDomain + "/" + pathChecks + "/" + repoID }

// PersonPath is one person's code inbox: the marks a record on their subject
// writes, and nothing else. BESIDE the repositories' and the changes' paths,
// as a change's is beside its repository's: a mark's decide reads the notices
// it names, which the records on changes' subjects write, but a mark makes
// none of them stale, and the inbox reads the domain's path, which covers
// both.
func PersonPath(handle string) string { return pathDomain + "/" + pathPerson + "/" + handle }

// ActivationPath is the activation fence's row.
func ActivationPath() string { return pathDomain + "/" + pathActivation }

// DomainPath is the whole domain: what a gate names, what a listing reads, and
// what an unreadable scope reads as.
func DomainPath() string { return pathDomain }

// MaxScopePaths caps a record's declared scope at what the widest record this
// domain forms names: a checks dispatch names its repository and its checks
// beside the path of each change it queues at — up to [MaxDispatchRows] of
// them, since each head and each attempt is a row of it — and a landing names
// up to [MaxLandEntries] changes beside the same two. So it is two more than
// the wider of those, 502, and NO WRITER WIDENS A SET TO [DomainPath()] TO FIT
// IT: one whose set is wider is a bug [validScope] refuses.
//
// It was sixteen, a writer past it naming the domain, and that was an outage
// of the whole code domain on every node holding ANY record it cannot decode,
// as every older node of a rolling upgrade does: a routine edit of a checks
// file, or its first adoption, makes every live change into its target due
// again, so a target with fifteen open changes dispatched under the domain.
// That record met the deferred one, of whatever repository, and was retained
// beside it; its domain scope then met every later record of every repository,
// each retained in turn, and every write and every scoped read there was
// answered `deferred`. Named change by change, a record meets only a deferred
// record about something its decide read.
//
// The width is the code domain's own choice and costs what the framework
// carries: a change's path is 40 bytes, 43 as JSON, so the widest scope adds
// about 22 KB to a record whose payload is held to [MaxDispatchBytes], half of
// [MaxRecordBytes]; the framework's deferral probe reads the roots as one
// row-set CTE rather than a chain of ORs (statelog's tables.go, probed clean
// there at a thousand), which every widest dispatch's decide runs
// (TestTheWidestDispatchNamesEachChangeItQueuesAt), and a retained record's
// index rows insert in chunks.
const MaxScopePaths = 2 + max(MaxDispatchRows, MaxLandEntries)

// Scope is a record's scope on the wire.
//
// ITS DECODER NEVER FAILS: a missing, non-array, empty or malformed scope — a
// non-string element, an empty string, a leading or trailing "/", an empty
// segment, whitespace in a segment — reads as [DomainPath()], the widest
// claim. This runs on a node reading a newer build's record, and the only
// honest reading of a blast radius it cannot parse is "everything"; an error
// here would fail the envelope pass that exists so such a record can be filed.
type Scope []string

// UnmarshalJSON reads a scope, widening anything unreadable to the domain.
func (s *Scope) UnmarshalJSON(b []byte) error {
	var paths []string
	if err := json.Unmarshal(b, &paths); err != nil || len(paths) == 0 {
		*s = Scope{DomainPath()}
		//nolint:nilerr // WIDEST-ON-UNREADABLE IS THE CONTRACT: this runs on
		// a node decoding a record a newer build wrote, and returning the
		// decode error here would fail the envelope pass that exists so
		// such a record can be filed, gated and reprocessed rather than
		// dropped.
		return nil
	}
	for _, p := range paths {
		if !wellFormed(p) {
			*s = Scope{DomainPath()}
			return nil
		}
	}
	*s = Scope(paths)
	return nil
}

// Set is the scope as the framework names it, normalised.
func (s Scope) Set() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string(s)}.Normalised()
}

// wellFormed reports a path the framework can order: non-empty segments with
// no whitespace, and no separator at either end.
func wellFormed(p string) bool {
	if p == "" || strings.HasPrefix(p, statelog.ScopeSeparator) ||
		strings.HasSuffix(p, statelog.ScopeSeparator) {
		return false
	}
	for _, seg := range strings.Split(p, statelog.ScopeSeparator) {
		if seg == "" || strings.ContainsAny(seg, " \t\r\n") {
			return false
		}
	}
	return true
}

// validScope refuses a scope a WRITER cannot mean: empty — which would claim
// the record makes nothing stale, the one claim a record no build may be able
// to decode must never make — over [MaxScopePaths], or a path malformed or
// outside the alphabet. The reader is lenient ([Scope.UnmarshalJSON]); the
// writer is not, because a writer that cannot say what it touches is a bug.
func validScope(paths []string) error {
	if len(paths) == 0 {
		return errors.New("code: a record's scope names nothing, which would " +
			"say it makes no read stale")
	}
	if len(paths) > MaxScopePaths {
		return fmt.Errorf("code: a record's scope names %d paths, past the %d the "+
			"widest record this domain forms names (gitlet.MaxScopePaths): a writer "+
			"naming more is a bug, and naming the whole domain instead would make "+
			"the record meet every deferred record of every repository", len(paths),
			MaxScopePaths)
	}
	for _, p := range paths {
		if !wellFormed(p) {
			return fmt.Errorf("code: scope path %q is malformed", p)
		}
		if p != DomainPath() && p != statelog.FrameworkScope &&
			!strings.HasPrefix(p, pathDomain+statelog.ScopeSeparator) {
			return fmt.Errorf("code: scope path %q is outside this domain's "+
				"alphabet, which begins every path with %q", p, pathDomain)
		}
	}
	return nil
}
