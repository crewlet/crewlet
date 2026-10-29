package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/redact"
)

// PushedRef is one ref a coding run moved on the engine's own code host, as the
// push rows the code host wrote under its launch record it: per (repository,
// ref), the old value of the run's FIRST push of it and the new value of its
// LAST — what the ref became, never every step it took, since a rebase loop is
// fifty rows of one branch and the seat acts on where the branch ended up.
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
}

// RunPushes reads what a coding run pushed to the engine's own code host, from
// the push rows the code host wrote under its launch — the ENGINE's account of
// the run's delivery there, beside the run's own `Delivered:` lines, which a
// box can write as it likes. Declared here, by its consumer; the engine
// implements it over the code host's reads.
type RunPushes interface {
	// Pushed is every ref run's launch moved on the repositories it may push
	// to ([Pushable]), each collapsed per (repository, ref), each repository
	// in the order the run was given them and its refs in the order the run
	// FIRST pushed them, at most [MaxPushedRefs] of them; more is what the
	// read had no room to hold. An error is the record UNREAD, and never
	// stands for "nothing pushed".
	Pushed(ctx context.Context, run PendingRun) (refs []PushedRef, more int, err error)
}

// MaxPushedRefs is the most refs of a run's push record the engine holds while
// it reads it, and so the most a collected result carries before its fit
// ([Coordinator.fitResult]): past it, a ref the run moved is counted rather
// than held.
//
// A BOX CHOOSES HOW MANY REFS IT MOVES — up to a thousand commands a push, a
// thousand pushes a seat's day by default, every one a name its generated code
// wrote — so a reader that held every ref it met would hold, for one run in a
// loop, hundreds of megabytes on the node that collects it. And nothing past
// this many could ever be listed: a result lists its refs up to
// [MaxDeliveredRefBytes] in all, a pushed ref by the line its text renders
// ([PushedRef.line]), which is never shorter than [minPushedLineBytes], and the
// list stops at the first that does not fit ([boundPushed]) — so the refs past
// the first MaxPushedRefs, in the order the reader keeps, are never in a list,
// and holding them would change nothing anybody reads but their count.
//
// THE ORDER IS THE FIRST PUSH, NOT THE LAST, for exactly that reason: a ref
// re-pushed late would move in an order of last pushes past refs the reader
// had no room to hold, and which refs a capped reader lists would then depend
// on the cap. In the order a ref first appears the held refs are the list's
// candidates whatever comes after them.
//
// A REF PAST THE CAP IS COUNTED ONCE FOR EACH PUSH THAT MOVED IT: telling two
// of them apart would mean holding their names, which is what the cap exists
// not to do. So the count of refs a record leaves out is exact for every run
// that moved at most this many refs, and past that it can overstate them,
// never understate them — a run that moved more refs than a list can show is
// told its list is short either way.
const MaxPushedRefs = MaxDeliveredRefBytes / minPushedLineBytes

// minPushedLineBytes is a floor under every line a pushed ref renders to: its
// repository's name, its ref — at least "refs/x/y", the shortest a ref has —
// and a twelve-digit object id or the words that stand for one, after the
// "- " every line opens with. TestEveryPushedLineIsAtLeastItsFloor holds every
// shape to it at its shortest, which is what makes [MaxPushedRefs] exact.
const minPushedLineBytes = 16

// shortOIDBytes is how much of an object id a pushed ref's line prints: twelve
// hex digits, a name for the executor to read beside the branch rather than an
// id to act on — it reads a branch's commit whole through code_resolve, and
// the phase record carries every id whole. Twelve rather than git's seven, so
// two commits of one busy repository's line are not told apart by luck.
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

// line is how the resumed executor's text names one pushed ref: its repository
// and ref, then where it went — created, deleted, moved, or back where it
// started — and how many of the run's pushes moved it where that is more than
// one. What a result's refs are fitted by ([boundPushed]).
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
	return line
}

// shortOID is the first [shortOIDBytes] of an object id.
func shortOID(oid string) string {
	if len(oid) > shortOIDBytes {
		return oid[:shortOIDBytes]
	}
	return oid
}

// boundPushed is a run's pushed refs as its result carries them — each
// repository's name and ref redacted, and the list held to what is left of
// [MaxDeliveredRefBytes] once the run's own delivered refs are counted — with
// how many more the record names than are listed: more, the refs the read had
// no room to hold, and every one past the bound.
//
// ONE BUDGET WITH THE DELIVERED REFS, the lines counted as the resumed
// executor reads them: both lists are the run's refs, on one record and in one
// text, and two budgets would let them disagree about how much of one run's
// delivery survives. The run's own lines first, as they always were; the push
// record has what they leave.
//
// A PREFIX, NEVER A SELECTION: the list stops at the first ref that does not
// fit, rather than skipping it for a shorter one after it, so what a capped
// read holds is always every candidate ([MaxPushedRefs]).
//
// REDACTED, here where every collected result passes, for the reason a
// delivered ref is ([boundRefs]): a ref is a name the box chose, which it can
// make a credential, and every reader of the record reads these. A ref is
// valid UTF-8 by the code host's grammar, so none needs the delivered refs'
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

// pushedText is what the resumed executor is told of a run's pushes to the
// engine's own code host, "" for a run given no repository there.
//
// NEVER OMITTED FOR A RUN THAT HAD REPOSITORIES, because "the run pushed
// nothing" is the one fact the executor must not infer from silence: a run
// that pushed nothing is told so, and one whose record could not be read is
// told THAT, with what to read instead — never a list the executor would take
// for the whole of it. A run given its repositories to read is told it pushed
// none, which its grants decide without a read.
func pushedText(run PendingRun, result Result) string {
	switch {
	case len(run.Repos) == 0:
		return ""
	case len(Pushable(run.Repos)) == 0:
		return "Pushed to this company's code host: nothing — it was given its repositories there to read, " +
			"not to push to."
	case result.PushedUnread != "":
		return "Pushed to this company's code host: unknown — its push record could not be read (" +
			result.PushedUnread + "); read the branches it worked on (" + branchesOf(Pushable(run.Repos)) +
			") with code_resolve before you report them."
	case len(result.Pushed) == 0 && result.PushedElided == 0:
		return "Pushed to this company's code host: nothing — no push of this run was recorded."
	}
	lines := []string{"Pushed to this company's code host (its push record):"}
	for _, ref := range result.Pushed {
		lines = append(lines, ref.line())
	}
	if result.PushedElided > 0 {
		lines = append(lines, fmt.Sprintf("- and %d more ref(s) its push record names, not listed here; "+
			"read them with code_resolve", result.PushedElided))
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

// recloneText is what a parked run whose box was reclaimed is told to call
// run_sandbox with for its repositories on the engine's own code host: each
// repository on the branch the run worked on — the branch it pushed to, so its
// work is in the fresh clone — with its access where it was read and its cone
// where it had one. The base is left out: it defaults to the branch's head.
//
// THE VALUE, NOT A DESCRIPTION OF IT: the branch a call that names none works
// on is the turn's default, and a run whose call named its own would be cloned
// on another branch than the one its work is on; so every repository is named
// with its branch, as run_sandbox's `repos` takes them. JSON, the tool's own
// argument, so nothing is left to the executor to transcribe.
func recloneText(repos []RunRepo) string {
	type again struct {
		Repo   string     `json:"repo"`
		Branch string     `json:"branch"`
		Access RepoAccess `json:"access,omitempty"`
		Paths  []string   `json:"paths,omitempty"`
	}
	value := make([]again, len(repos))
	for i, r := range repos {
		value[i] = again{Repo: r.Repo, Branch: r.Branch, Paths: r.Paths}
		if r.Access != RepoWrite {
			// AS IT WAS: a call that names no access is given write where
			// the seat may push, and a repository this run was given to read
			// stays one.
			value[i].Access = r.Access
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		// A slice of structs of strings always encodes; a failure is a broken
		// build, not an input.
		panic(fmt.Sprintf("sandbox: encode a run's repositories: %v", err))
	}
	return string(raw)
}

// PushedRecords is pushed as the phase records carry it ([types.PushedRefRecord]):
// the run's own record and the resumed executor's.
func PushedRecords(pushed []PushedRef) []types.PushedRefRecord {
	if len(pushed) == 0 {
		return nil
	}
	out := make([]types.PushedRefRecord, len(pushed))
	for i, p := range pushed {
		out[i] = types.PushedRefRecord{Repo: p.Repo, Ref: p.Ref, Old: p.Old, New: p.New, Pushes: p.Pushes}
	}
	return out
}
