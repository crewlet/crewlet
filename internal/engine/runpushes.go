package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// runPushes is the coordinator's [sandbox.RunPushes]: what a collected
// executor pushed to Gitlet, read from the push rows it
// wrote under its launch (code.read's `run_pushes`) — every page to the last,
// the commands each answered ok, collapsed per (repository, ref).
//
// THE FIRST PAGE LINEARIZABLE, one barrier a collected run that could push:
// its collection is where "it pushed nothing" must never be a stale answer.
// Every later page at session at the first page's position, so no node
// behind the snapshot the first page read answers a later one — leaving out
// pushes the run made before that snapshot — and no later page pays a barrier
// of its own, nor does the read of the changes the run's branches head
// ([runPushes.Changes]). The barrier is the seats' to pay for — the code log census's
// seats' term counts it beside their other reads of the log, while
// gitlet.GitReadsPerDay, the term for persons and their CI, does not (its doc
// says so) — and the read is only asked for a run given a repository to push
// to.
//
// THE REPOSITORIES ARE THE RUN'S OWN, from its row: those it was given to push
// to, by id in the order it was given them, and named as it was given them —
// the run could push nowhere else, and a rename since the launch does not
// change what the run was told it worked on. Above its launch position
// (`PendingRun.CodeLaunch`): nothing the run pushed applied below it.
//
// HELD TO WHAT A RESULT CAN LIST: a box chooses how many refs it moves, so the
// reader holds at most sandbox.MaxPushedRefs of them, in the order the run
// first pushed them, and counts the rest — its memory follows what a record
// can carry, never what a box in a loop pushed.
type runPushes struct {
	// code is this node's read of Gitlet, asked at each collection: a
	// node starts and stops serving it with the company's revision, and the
	// coordinator outlives every revision.
	code func() (pushReader, error)
}

var _ sandbox.RunPushes = runPushes{}

// pushReader is Gitlet as a collection asks it — the estate's
// [estate.CodeSeam]: its reads, which the reader pages through and names the
// run's branches' changes by; a change's lifecycle write, which opens the
// change a run asked for; and a read by object id, which titles one its ask
// named no title for.
type pushReader interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	Change(ctx context.Context, p gitlet.ChangeProposal) (gitlet.ChangeResult, error)
	GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)
}

// codeRunPushes is the engine's reader of what its executors pushed: over the
// router's code seam where this node serves Gitlet, and
// otherwise an error naming why it does not — which the collection says
// rather than reading as "nothing pushed" (sandbox.Result.PushedUnread).
func (e *Engine) codeRunPushes() runPushes {
	return runPushes{code: func() (pushReader, error) {
		seam, ok := OperatorCode(e)
		if !ok {
			return nil, fmt.Errorf("engine: this node does not serve Gitlet now (%w)",
				e.CodeUnserved())
		}
		return seam, nil
	}}
}

// Pushed reads what run's launch pushed — see [runPushes]. The record's
// position is its first page's: the barrier the collection pays, which every
// later read of the collection is floored at ([sandbox.PushRecord.At]).
func (p runPushes) Pushed(ctx context.Context, run sandbox.PendingRun) (sandbox.PushRecord, error) {
	repos := sandbox.Pushable(run.Repos)
	if len(repos) == 0 {
		return sandbox.PushRecord{}, nil
	}
	launch, err := statelog.ParsePosition(run.CodeLaunch)
	switch {
	case err != nil:
		return sandbox.PushRecord{}, fmt.Errorf("engine: run %s's launch position %q cannot be read: %w",
			run.TurnID, run.CodeLaunch, err)
	case launch.Stream != topics.CodeLogStream:
		return sandbox.PushRecord{}, fmt.Errorf("engine: run %s's launch position %s is on another log than "+
			"the code log", run.TurnID, launch)
	}
	reader, err := p.code()
	if err != nil {
		return sandbox.PushRecord{}, err
	}
	ids := make([]string, len(repos))
	names := make(map[string]string, len(repos))
	for i, r := range repos {
		ids[i], names[r.RepoID] = r.RepoID, r.Repo
	}
	q := gitlet.ReadQuery{Kind: gitlet.ReadRunPushes,
		RunPushes: &gitlet.RunPushesQuery{Run: run.LaunchID, Repos: ids, After: launch},
		Fresh:     statelog.Freshness{Level: statelog.ReadLinearizable}}
	var pushed pushCollapse
	var at statelog.Position
	for {
		answer, readErr := reader.Read(ctx, q)
		if readErr != nil {
			return sandbox.PushRecord{}, fmt.Errorf("engine: read what run %s pushed: %w", run.TurnID, readErr)
		}
		if at.IsZero() {
			// THE FIRST PAGE'S POSITION IS THE RECORD'S, whether or not a
			// page follows it: what the changes are read at, too.
			if answer.Position.IsZero() || answer.Position.Stream != topics.CodeLogStream {
				return sandbox.PushRecord{}, fmt.Errorf("engine: read what run %s pushed: its first page was "+
					"answered at %s, no position of the code log", run.TurnID, answer.Position)
			}
			at = answer.Position
			q.Fresh = statelog.FloorAt(statelog.ReadSession, at)
		}
		if addErr := pushed.add(answer.Pushes, names); addErr != nil {
			return sandbox.PushRecord{}, fmt.Errorf("engine: read what run %s pushed: %w", run.TurnID, addErr)
		}
		switch answer.Next {
		case "":
			return sandbox.PushRecord{Refs: pushed.refs, More: pushed.more, At: at.String()}, nil
		case q.Cursor:
			// A PAGE THAT DID NOT MOVE would be read for ever: every page
			// Gitlet serves advances by at least one push.
			return sandbox.PushRecord{}, fmt.Errorf("engine: read what run %s pushed: Gitlet answered "+
				"cursor %q again", run.TurnID, answer.Next)
		}
		q.Cursor = answer.Next
	}
}

// pushCollapse is a run's push rows as the result carries them: one entry per
// (repository, ref) the run's pushes moved, in the order it first pushed each
// — the first push's old value, the last push's new — at most
// sandbox.MaxPushedRefs of them, with a count of every move of a ref past
// those ([sandbox.MaxPushedRefs] says why such a ref is counted once a push).
// A command the decide refused moved nothing, and is left out.
type pushCollapse struct {
	held map[pushKey]int
	refs []sandbox.PushedRef
	more int
}

// pushKey is a ref of a repository, by the repository's id.
type pushKey struct{ repo, ref string }

// add folds a page of the run's push rows in, in their order — names the run's
// repositories by id. A row of a repository the run was not given to push to
// is refused: the read names only those, so such a row is Gitlet
// answering another read.
func (c *pushCollapse) add(rows []gitlet.PushRow, names map[string]string) error {
	for _, row := range rows {
		name, given := names[row.RepoID]
		if !given {
			return fmt.Errorf("push %s to repository %s came back from Gitlet, though the run was "+
				"not given that repository to push to", row.PushID, row.RepoID)
		}
		for _, cmd := range row.Commands {
			if cmd.Result != gitlet.ResultOK {
				continue
			}
			key := pushKey{repo: row.RepoID, ref: cmd.Ref}
			if i, held := c.held[key]; held {
				c.refs[i].New = cmd.New
				c.refs[i].Pushes++
				continue
			}
			if len(c.refs) == sandbox.MaxPushedRefs {
				c.more++
				continue
			}
			if c.held == nil {
				c.held = map[pushKey]int{}
			}
			c.held[key] = len(c.refs)
			c.refs = append(c.refs, sandbox.PushedRef{Repo: name, Ref: cmd.Ref, Old: cmd.Old, New: cmd.New,
				Pushes: 1})
		}
	}
	return nil
}
