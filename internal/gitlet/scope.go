package gitlet

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: every object its apply may write AND every object its
// decide read (M5-D33), stated by the writer and readable at every version, so
// a node behind on any of it decides nothing from it. A node that cannot
// decode a record files the deferral under it; a later read or write probes it.
//
// The framework computes containment from a left-to-right hierarchy, so the
// alphabet nests under the domain; a repository's path is beside its name's,
// not under it, because the id never changes while a purge releases the name.
// Carried as RESOLVED PATHS, unlike the tracker's and the knowledge base's
// terms: a writer knows every object by id, so a build that cannot interpret
// a path still indexes it.

// The path alphabet. The domain letter `c` is distinct from every other
// domain's, so a deferral row read out of a stuck fleet names its domain.
const (
	pathDomain     = "c"
	pathName       = "n"
	pathRepo       = "r"
	pathActivation = "a"
	pathChange     = "c"
	pathChecks     = "k"
	pathPerson     = "p"
	pathViewed     = "v"
)

// NamePath is a repository name's claim.
func NamePath(token string) string { return pathDomain + "/" + pathName + "/" + token }

// RepoPath is one repository: its row, its refs, its packs, its pushes, its
// protection rules and its viewed key, everything a record on its subject may
// write.
func RepoPath(id string) string { return pathDomain + "/" + pathRepo + "/" + id }

// ChangePath is one change's conversation: everything a record on the
// change's own subject may write (reviewers, comments, participants,
// counters). BESIDE the repository's path, since a change is filed by its id.
// A record on a change's subject names BOTH paths ([ChangeWriter]): its decide
// reads the change's row and the repository's status, which the repository's
// records write, and the deletion gate reads its repository off this scope
// ([gatedRepo]). A MERGEABILITY also names [ChecksPath]: the merge gate it
// judges reads them ([JudgeGate], [Mergeability.ChecksPosition]).
func ChangePath(id string) string { return pathDomain + "/" + pathChange + "/" + id }

// ChecksPath is one repository's checks: its check rows and their logs.
// BESIDE the repository's path because the APPLIES write apart: a check's
// record never writes the repository's rows, and of the repository's records
// only its purge writes a check's, each step naming both ([Store.PurgeStep]). The DECIDES do not read apart, so a check's
// record also names [RepoPath] and the [ChangePath] of each change it reads
// (checkswriter.go's checksScope), and a mergeability, a merge and an enqueue
// name this path for the checks a gate judges. The deletion gate reads the
// repository off the checks subject, never the scope ([gatedRepo]).
func ChecksPath(repoID string) string { return pathDomain + "/" + pathChecks + "/" + repoID }

// PersonPath is one person's code inbox: the marks a record on their subject
// writes, and nothing else. BESIDE the changes' paths: a mark's decide reads
// notices records on changes write, but makes none of them stale, and the
// inbox reads the domain's path, which covers both.
func PersonPath(handle string) string { return pathDomain + "/" + pathPerson + "/" + handle }

// ViewedPath is one person's viewed marks: the rows a viewed record writes.
// BESIDE their inbox's path, so a retained viewed record holds back no inbox
// mark.
func ViewedPath(handle string) string { return pathDomain + "/" + pathViewed + "/" + handle }

// ActivationPath is the activation fence's row.
func ActivationPath() string { return pathDomain + "/" + pathActivation }

// DomainPath is the whole domain: what a gate names, what a listing reads, and
// what an unreadable scope reads as.
func DomainPath() string { return pathDomain }

// MaxScopePaths caps a record's scope at what the widest record names: a
// checks dispatch names its repository, its checks and each change it queues
// at (up to [MaxDispatchRows]), a landing up to [MaxLandEntries] changes
// beside the same two; so 502. NO WRITER WIDENS A SET TO [DomainPath()] TO FIT
// IT ([validScope] refuses a wider one): a domain-wide scope meets every
// deferred record of every repository, and on a node that cannot decode one
// (any older node in a rolling upgrade) every later write and scoped read is
// answered `deferred`. Named change by change, a record meets only a deferred
// record about something its decide read. Cost: a change's path is 43 bytes
// as JSON, so about 22 KB beside a payload held to [MaxDispatchBytes], and
// the framework's deferral probe reads the roots as one row-set CTE
// (TestTheWidestDispatchNamesEachChangeItQueuesAt).
const MaxScopePaths = 2 + max(MaxDispatchRows, MaxLandEntries)

// Scope is a record's scope on the wire.
//
// ITS DECODER NEVER FAILS: a missing, non-array, empty or malformed scope
// ([wellFormed]) reads as [DomainPath()], the widest claim. A node reading a
// newer build's record can only honestly read a blast radius it cannot parse
// as "everything"; an error would fail the envelope pass that lets such a
// record be filed.
type Scope []string

// UnmarshalJSON reads a scope, widening anything unreadable to the domain.
func (s *Scope) UnmarshalJSON(b []byte) error {
	var paths []string
	if err := json.Unmarshal(b, &paths); err != nil || len(paths) == 0 {
		*s = Scope{DomainPath()}
		//nolint:nilerr // WIDEST-ON-UNREADABLE IS THE CONTRACT: this runs on
		// a node decoding a newer build's record ([Scope]).
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

// validScope refuses a scope a WRITER cannot mean: empty (claiming the record
// makes nothing stale, which an undecodable record must never claim), over
// [MaxScopePaths], or a path malformed or outside the alphabet. The reader is
// lenient ([Scope.UnmarshalJSON]); a writer that cannot say what it touches is
// a bug.
func validScope(paths []string) error {
	if len(paths) == 0 {
		return errors.New("gitlet: a record's scope names nothing, which would " +
			"say it makes no read stale")
	}
	if len(paths) > MaxScopePaths {
		return fmt.Errorf("gitlet: a record's scope names %d paths, past the %d the "+
			"widest record this domain forms names (gitlet.MaxScopePaths): a writer "+
			"naming more is a bug, and naming the whole domain instead would make "+
			"the record meet every deferred record of every repository", len(paths),
			MaxScopePaths)
	}
	for _, p := range paths {
		if !wellFormed(p) {
			return fmt.Errorf("gitlet: scope path %q is malformed", p)
		}
		if p != DomainPath() && p != statelog.FrameworkScope &&
			!strings.HasPrefix(p, pathDomain+statelog.ScopeSeparator) {
			return fmt.Errorf("gitlet: scope path %q is outside this domain's "+
				"alphabet, which begins every path with %q", p, pathDomain)
		}
	}
	return nil
}
