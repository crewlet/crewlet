package sandbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/redact"
)

// PushedRef is one ref an executor moved on Gitlet, as the push rows under its
// launch record it: per (repository, ref), the old value of the run's FIRST
// push and the new value of its LAST. The seat acts on where a branch ended
// up, and a rebase loop is fifty rows of one branch.
type PushedRef struct {
	// Repo is the repository's name as the run was given it ([RunRepo.Repo]).
	Repo string
	Ref  string
	// Old is gitref.ZeroOID where the run created the ref, and New where it
	// deleted it.
	Old string
	New string
	// Pushes is how many of the run's pushes moved the ref.
	Pushes int
	// Change is the live change the ref heads once the run is collected
	// (web!42), "" for one that heads none: a tag, a deleted branch, a branch
	// no change proposes ([RunPushes.Changes]).
	Change string
}

// RunPushes reads what an executor pushed to Gitlet, from the push rows under
// its launch: the ENGINE's account of the delivery, beside the run's own
// `Delivered:` lines, which a box can write as it likes. The engine implements
// it over Gitlet's reads.
type RunPushes interface {
	// Pushed is run's push record ([PushRecord]): every ref its launch moved
	// on the repositories it may push to ([Pushable]). An error is the
	// record UNREAD, and never stands for "nothing pushed".
	Pushed(ctx context.Context, run PendingRun) (PushRecord, error)
	// Changes names each of record's refs, read at or past [PushRecord.At],
	// with the live change it heads ([PushedRef.Change]). Where open is set
	// it also opens the change each of run's repositories asks for
	// ([RunRepo.Open]) from the branch the run delivered, ONCE A LAUNCH so a
	// retried collection on any node opens nothing twice, and answers each
	// ask in the order run was given its repositories ([OpenedChange]);
	// otherwise it opens nothing and answers no ask. An ask refused or
	// unreachable is an entry saying so; an error is the changes UNREAD:
	// record's refs answered as given, nothing opened.
	Changes(ctx context.Context, run PendingRun, record PushRecord, open bool) (named []PushedRef,
		opened []OpenedChange, err error)
}

// PushRecord is what a run's push record says it moved, as [RunPushes.Pushed]
// read it.
type PushRecord struct {
	// Refs is every ref the run moved, each collapsed per (repository, ref),
	// each repository in the order the run was given them and its refs in the
	// order the run FIRST pushed them, at most [MaxPushedRefs] of them; More
	// is what the read had no room to hold.
	Refs []PushedRef
	More int
	// At is the record's position on the code log, where its one barrier put
	// the read. THE COLLECTION'S ONLY BARRIER: every push of the run, every
	// `git push -o change` open they carried and every open an earlier
	// collection of the launch saw land is at or below it, so
	// [RunPushes.Changes] reads at `session` floored here and answers what a
	// second linearizable read would, with no second barrier per run; an open
	// still in flight is the operation ledger's to answer. Empty for a run
	// given no repository to push to.
	//
	// A statelog.Position TOKEN (`<stream>@<generation>:<sequence>`), as
	// [PendingRun.CodeFloor] is: this package passes it through unread, so it
	// links no state log.
	At string
}

// MaxPushedRefs is the most refs of a run's push record the engine holds while
// reading it, and so the most a result carries before [Coordinator.fitResult];
// past it a ref is counted, not held. A box chooses how many refs it moves, so
// holding all could cost a node hundreds of megabytes, and no more could ever
// be listed: [boundPushed] lists a prefix within [MaxDeliveredRefBytes], each
// [PushedRef.line] at least [minPushedLineBytes]. FIRST-push order, not last,
// so a late re-push cannot move a ref past refs never held and make the list
// depend on the cap. A ref past the cap is counted once per push that moved
// it (telling two apart would mean holding their names): the elided count is
// exact up to this many refs, and past it may overstate, never understate.
const MaxPushedRefs = MaxDeliveredRefBytes / minPushedLineBytes

// minPushedLineBytes is a floor under every line a pushed ref renders to: its
// repository's name, its ref — at least "refs/x/y", the shortest a ref has —
// and a twelve-digit object id or the words that stand for one, after the
// "- " every line opens with. TestEveryPushedLineIsAtLeastItsFloor holds every
// shape to it at its shortest, which is what makes [MaxPushedRefs] exact.
const minPushedLineBytes = 16

// shortOIDBytes is how much of an object id a pushed ref's line prints: a name
// to read beside the branch, not an id to act on (code_resolve and the records
// carry ids whole). Twelve rather than git's seven, so two commits of one busy
// repository are not told apart by luck.
const shortOIDBytes = 12

// Pushable is the repositories of repos a run may push to — those given it with
// access write — in their order. A run given a repository to read pushes
// nothing to it: its box's push is refused there before anything is recorded.
func Pushable(repos []RunRepo) []RunRepo {
	var out []RunRepo
	for _, r := range repos {
		if r.Access == RepoWrite {
			out = append(out, r)
		}
	}
	return out
}

// repoNames is the names of repos, in their order; nil for none.
func repoNames(repos []RunRepo) []string {
	var out []string
	for _, r := range repos {
		out = append(out, r.Repo)
	}
	return out
}

// line is how the run's report names one pushed ref: repository, ref, where it
// went (created, deleted, moved, or back where it started), its push count
// when above one, and the change it heads if any. [boundPushed] fits a
// result's refs by it.
func (p PushedRef) line() string {
	var where string
	switch {
	case p.Old == gitref.ZeroOID && p.New == gitref.ZeroOID:
		where = "created and deleted again"
	case p.Old == gitref.ZeroOID:
		where = "created at " + shortOID(p.New)
	case p.New == gitref.ZeroOID:
		where = "deleted (it was " + shortOID(p.Old) + ")"
	case p.Old == p.New:
		where = "back at " + shortOID(p.New) + ", where it started"
	default:
		where = shortOID(p.Old) + " → " + shortOID(p.New)
	}
	line := "- " + p.Repo + " " + p.Ref + " " + where
	if p.Pushes > 1 {
		line += fmt.Sprintf(" (%d pushes)", p.Pushes)
	}
	if p.Change != "" {
		line += ", change " + p.Change
	}
	return line
}

// shortOID is the first [shortOIDBytes] of an object id.
func shortOID(oid string) string {
	if len(oid) > shortOIDBytes {
		return oid[:shortOIDBytes]
	}
	return oid
}

// boundPushed is a run's pushed refs as its result carries them, names
// redacted and the list held to what the run's own delivered refs leave of
// budget, with how many more the record names than are listed: more, plus
// every one past the bound.
//
// ONE BUDGET WITH THE DELIVERED REFS, counted as the report renders them: both
// are the run's refs in one text, and two budgets could disagree on how much
// survives. A PREFIX, NEVER A SELECTION (no skipping to a shorter ref), so a
// capped read holds every candidate ([MaxPushedRefs]). REDACTED as a
// delivered ref is ([boundRefs]): a box can make a ref name a credential.
// Gitlet's grammar keeps refs valid UTF-8, so none needs the delivered refs'
// other rule.
func boundPushed(refs []PushedRef, more, budget int) ([]PushedRef, int) {
	var kept []PushedRef
	size := 0
	for i, ref := range refs {
		ref.Repo, ref.Ref = redact.Secrets(ref.Repo), redact.Secrets(ref.Ref)
		if size += len(ref.line()); size > budget {
			return kept, more + len(refs) - i
		}
		kept = append(kept, ref)
	}
	return kept, more
}

// pushedText is what the run's report states of its pushes to Gitlet, "" for
// a run given no repository there.
//
// NEVER OMITTED FOR A RUN THAT HAD REPOSITORIES: its reviewer must not infer
// "nothing pushed" from silence. A run that pushed nothing says so; one whose
// record was unread says THAT and names its branches, never a partial list;
// one given repositories only to read says it pushed none, without a read.
func pushedText(run PendingRun, result Result) string {
	switch {
	case len(run.Repos) == 0:
		return ""
	case len(Pushable(run.Repos)) == 0:
		return "Pushed to Gitlet: nothing — it was given its repositories there to read, " +
			"not to push to."
	case result.PushedUnread != "":
		return "Pushed to Gitlet: unknown — its push record could not be read (" +
			result.PushedUnread + "); the branches it worked on are " + branchesOf(Pushable(run.Repos)) + "."
	case len(result.Pushed) == 0 && result.PushedElided == 0:
		return "Pushed to Gitlet: nothing — no push of this run was recorded."
	}
	lines := []string{"Pushed to Gitlet (its push record):"}
	for _, ref := range result.Pushed {
		lines = append(lines, ref.line())
	}
	if result.PushedElided > 0 {
		lines = append(lines, fmt.Sprintf("- and %d more ref(s) its push record names, not listed here",
			result.PushedElided))
	}
	return strings.Join(lines, "\n")
}

// branchesOf names each repository's branch, "web: seats/swe/ENG-42".
func branchesOf(repos []RunRepo) string {
	named := make([]string, len(repos))
	for i, r := range repos {
		named[i] = r.Repo + ": " + r.Branch
	}
	return strings.Join(named, "; ")
}

// PushedRecords is pushed as the records carry it ([types.PushedRefRecord]):
// the launch's own (`executor_collected`) and the resumed turn's executor
// phase record.
func PushedRecords(pushed []PushedRef) []types.PushedRefRecord {
	if len(pushed) == 0 {
		return nil
	}
	out := make([]types.PushedRefRecord, len(pushed))
	for i, p := range pushed {
		out[i] = types.PushedRefRecord{Repo: p.Repo, Ref: p.Ref, Old: p.Old, New: p.New, Pushes: p.Pushes,
			Change: p.Change}
	}
	return out
}
