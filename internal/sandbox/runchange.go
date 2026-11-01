package sandbox

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/redact"
)

// A CODING RUN AND THE CHANGES OF THE ENGINE'S OWN CODE HOST (M4-decisions
// D33): a run may be launched ON a change — given the change's source branch
// to work on, from its head ([RunRepo.Change]) — and may ask to OPEN one from
// the branch it delivers ([RunRepo.Open]), which the engine does at the run's
// collection, once a launch, from what the push record says the run pushed.
// Every ref a collected run moved is named by the live change its branch
// heads ([PushedRef.Change]), and what became of each ask to open one is the
// run's result ([Result.Opened]): both read by the engine ([RunPushes.Changes])
// and both stated in the run's report ([runReport]), so its reviewer and the
// record read the change rather than a branch, and nobody opens one twice.
//
// AT THE COLLECTION, NOT BY THE BOX: a box could open a change itself — `git
// push -o change` — but a coding agent pushes as it goes, and a change opened
// at its first push asks its reviewers to read work in progress. The engine
// opens it once the work is done, from where the branch ended up, as the
// seat the run is for, carrying the work item its launch was charged to.

// RunChange is the change a coding run works on: its id, which the change is
// named by on the code host, and its number on its repository, which a person
// and a seat name it by (web!42).
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

// OpenChange is a coding run's ask to open a change from the branch it works
// on once it finishes: the branch it proposes the run's work into — whole,
// under refs/heads/, RESOLVED AT LAUNCH (the one asked, or the repository's
// default branch then), so the collection proposes what the run was told —
// its title, "" for the subject of the commit the run delivered its branch
// at, and whether it opens as a draft. The title is held to the code host's
// rule at the launch that resolves it and again by the open's decide; this
// package holds it to valid UTF-8 alone, which the run's row's JSON needs.
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
	}
	return ""
}

// branchRoot is where a repository's branches are, as a ref names them.
const branchRoot = "refs/heads/"

// OpenedChange is what became of one repository's ask to open a change
// ([RunRepo.Open]), in the order the run was given its repositories: the
// change the run's branch heads, named as a person names it — opened by this
// run's launch, or one the branch already headed — or why none was.
type OpenedChange struct {
	// Repo and Branch are the repository and the branch as the run was given
	// them.
	Repo   string
	Branch string
	// Change is the change the branch heads, web!42, and "" where none was
	// opened and the branch heads none.
	Change string
	// Opened is a change this run's launch opened; false beside Change, a
	// change the branch already headed — the run's own pushes may have
	// opened it with `git push -o change`, or anybody may have.
	Opened bool
	// Unopened is why none was opened, "" where Change names one: a run
	// that did not finish its work, a branch it never pushed, the code
	// host's refusal in its own words, or an open whose fate is unknown.
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

// asked is the changes run asked to open: one entry each, in the order it
// was given its repositories, each saying why it was not opened — for a
// collection that opens none of them.
func asked(run PendingRun, why string) []OpenedChange {
	var out []OpenedChange
	for _, r := range run.Repos {
		if r.Open != nil {
			out = append(out, OpenedChange{Repo: r.Repo, Branch: r.Branch, Unopened: why})
		}
	}
	return out
}

// Why an ask to open a change was not, for the run that did not get as far as
// its collection opening it.
const (
	unopenedAsked = "the run stopped to ask a question before it finished, so its work was not " +
		"proposed for review: the job that carries it on opens the change"
	unopenedFailed = "the run did not succeed, so its work was not proposed for review"
)

// unopenedUnread is why no ask of a run whose push record could not be read
// was opened.
func unopenedUnread(why string) string {
	return "what the run pushed could not be read (" + why + "), so no change was opened from it"
}

// unopenedNoChanges is why no ask of a run was opened when the code host's
// changes of its branches could not be read.
func unopenedNoChanges(why string) string {
	return "the changes its branches head could not be read (" + why + "), so none was opened"
}

// fitOpened holds what became of a run's asks to the record's bounds: each
// sentence redacted — a refusal quotes a branch, a name the box chose — and
// clipped to the bound one diagnostic a record carries has. At most one entry
// a repository the run was given, so at most [MaxRunRepos] of them.
func fitOpened(opened []OpenedChange) []OpenedChange {
	for i := range opened {
		opened[i].Branch = redact.Secrets(opened[i].Branch)
		opened[i].Unopened = events.ClipDiagnostic(redact.Secrets(opened[i].Unopened))
	}
	return opened
}

// openedText is what the run's report states of the changes the run asked to
// open, "" for a run that asked for none.
func openedText(result Result) string {
	if len(result.Opened) == 0 {
		return ""
	}
	lines := []string{"Changes on this company's code host (opened at the run's collection):"}
	for _, o := range result.Opened {
		lines = append(lines, o.line())
	}
	return strings.Join(lines, "\n")
}
