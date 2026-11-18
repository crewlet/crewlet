package sandbox

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/redact"
)

// A run may be launched on a change ([RunRepo.Change]) and may ask to open
// one from the branch it delivers ([RunRepo.Open]). The engine opens it at the
// run's collection, once a launch, from what the push record says was pushed:
// not by the box (`git push -o change`), because a harness pushes as it goes
// and a change opened at its first push asks reviewers to read work in
// progress. Each moved ref names the change its branch heads
// ([PushedRef.Change]) and each ask's fate is [Result.Opened]; both reach the
// run's report ([runReport]), so reviewers read a change, not a branch.

// RunChange is the change an executor works on: its Gitlet id, and its
// number on its repository, which a person names it by (web!42).
type RunChange struct {
	ID     string `json:"id"`
	Number int64  `json:"number"`
}

// problem is why c names no change, or "".
func (c RunChange) problem() string {
	switch {
	case !canonicalID(c.ID):
		return fmt.Sprintf("is named by %q, which is not a UUID in canonical form", c.ID)
	case c.Number < 1:
		return fmt.Sprintf("is numbered %d, and a change's number is at least 1", c.Number)
	}
	return ""
}

// OpenChange is an executor's ask to open a change from its branch once it
// finishes. Target is the whole refs/heads/ ref, resolved at launch (the
// default branch then, if none was asked), so the collection proposes what the
// run was told; Title "" takes the delivered commit's subject. Gitlet's title
// rule is applied at launch and at the open; this package holds the title to
// valid UTF-8 (the row's JSON) and [MaxChangeTitleBytes] (rowbytes.go).
type OpenChange struct {
	Target string `json:"target"`
	Title  string `json:"title,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
}

// problem is why o is no change a run on branch may open, or "".
func (o OpenChange) problem(branch string) string {
	target, ok := strings.CutPrefix(o.Target, branchRoot)
	switch {
	case !ok || !gitref.ValidBranch(target):
		return fmt.Sprintf("proposes into %q, which is no branch under %s", o.Target, branchRoot)
	case target == branch:
		return fmt.Sprintf("proposes %s into itself", o.Target)
	case !utf8.ValidString(o.Title):
		return "names a title that is not valid UTF-8"
	case len(o.Title) > MaxChangeTitleBytes:
		return fmt.Sprintf("names a title of %d bytes, past the %d a change's title may have", len(o.Title),
			MaxChangeTitleBytes)
	}
	return ""
}

// MaxChangeTitleBytes bounds [OpenChange.Title]: Gitlet's own bound
// (internal/gitlet's MaxChangeTitleBytes, which this package does not import;
// the engine's TestARunsChangeTitleIsBoundAsGitletBoundsIt holds them equal).
// It is enforced here too because the title rides the run's row
// ([PendingRun.Repos]), whose budget counts it at this bound, so
// [PendingStore.GrantRepo] holds it rather than trusting the resolver.
const MaxChangeTitleBytes = 256

// branchRoot is where a repository's branches are, as a ref names them.
const branchRoot = "refs/heads/"

// OpenedChange is what became of one repository's ask to open a change
// ([RunRepo.Open]), in the run's repository order: the change the branch
// heads (opened by this launch or already there), or why none was.
type OpenedChange struct {
	// Repo and Branch are as the run was given them.
	Repo   string
	Branch string
	// Change is the change the branch heads (web!42), "" for none.
	Change string
	// Opened is true when this launch opened Change; false when the branch
	// already headed it (the run's own `git push -o change`, or anybody).
	Opened bool
	// Unopened is why none was opened, "" where Change names one; it may be
	// Gitlet's refusal in its own words.
	Unopened string
}

// line is how the run's report names what became of one ask.
func (o OpenedChange) line() string {
	switch {
	case o.Opened:
		return fmt.Sprintf("- %s opened from %s for review", o.Change, o.Branch)
	case o.Change != "":
		return fmt.Sprintf("- %s already headed %s, so no other was opened", o.Branch, o.Change)
	}
	return fmt.Sprintf("- %s: no change opened from %s — %s", o.Repo, o.Branch, o.Unopened)
}

// asked is one entry per change run asked to open, in repository order, each
// unopened for why: for a collection that opens none.
func asked(run PendingRun, why string) []OpenedChange {
	var out []OpenedChange
	for _, r := range run.Repos {
		if r.Open != nil {
			out = append(out, OpenedChange{Repo: r.Repo, Branch: r.Branch, Unopened: why})
		}
	}
	return out
}

// Why an ask was not opened, for a run that never reached the open.
const (
	unopenedAsked = "the run stopped to ask a question before it finished, so its work was not " +
		"proposed for review: the job that carries it on opens the change"
	unopenedFailed = "the run did not succeed, so its work was not proposed for review"
)

// unopenedUnread is why no ask was opened when the push record was unreadable.
func unopenedUnread(why string) string {
	return "what the run pushed could not be read (" + why + "), so no change was opened from it"
}

// unopenedNoChanges is why no ask was opened when Gitlet's changes of the
// run's branches could not be read.
func unopenedNoChanges(why string) string {
	return "the changes its branches head could not be read (" + why + "), so none was opened"
}

// fitOpened holds the asks to the record's bounds: redacted (a refusal quotes
// a branch, a name the box chose) and clipped as a diagnostic. There is at most
// one entry per repository, so at most [MaxRunRepos].
func fitOpened(opened []OpenedChange) []OpenedChange {
	for i := range opened {
		opened[i].Branch = redact.Secrets(opened[i].Branch)
		opened[i].Unopened = events.ClipDiagnostic(redact.Secrets(opened[i].Unopened))
	}
	return opened
}

// openedText is the report's account of the asks, "" for a run with none.
func openedText(result Result) string {
	if len(result.Opened) == 0 {
		return ""
	}
	lines := []string{"Changes on Gitlet (opened at the run's collection):"}
	for _, o := range result.Opened {
		lines = append(lines, o.line())
	}
	return strings.Join(lines, "\n")
}
