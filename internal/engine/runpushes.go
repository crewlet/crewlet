package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// runPushes is the coordinator's [sandbox.RunPushes]: what a collected coding
// run pushed to the company's code host, read from the push rows the host
// wrote under its launch (code.read's `run_pushes`) — every page to the last,
// the commands each answered ok, collapsed per (repository, ref).
//
// THE FIRST PAGE LINEARIZABLE, one barrier a collected run that could push:
// its collection is where "it pushed nothing" must never be a stale answer.
// Every later page at session at the first page's position, so no node
// behind the snapshot the first page read answers a later one — leaving out
// pushes the run made before that snapshot — and no later page pays a barrier
// of its own. The barrier is the seats' to pay for — the code log census's
// seats' term counts it beside their other reads of the log, while
// code.GitReadsPerDay, the term for persons and their CI, does not (its doc
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
	// code is this node's read of the code host, asked at each collection: a
	// node starts and stops serving it with the company's revision, and the
	// coordinator outlives every revision.
	code func() (pushReader, error)
}

var _ sandbox.RunPushes = runPushes{}

// pushReader is the code host's read the reader pages through — the estate's
// [estate.CodeSeam.Read].
type pushReader interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
}

// codeRunPushes is the engine's reader of what its coding runs pushed: over the
// router's code seam where this node serves the company's code host, and
// otherwise an error naming why it does not — which the collection says
// rather than reading as "nothing pushed" (sandbox.Result.PushedUnread).
func (e *Engine) codeRunPushes() runPushes {
	return runPushes{code: func() (pushReader, error) {
		seam, ok := OperatorCode(e)
		if !ok {
			return nil, fmt.Errorf("engine: this node does not serve the company's code host now (%w)",
				e.CodeUnserved())
		}
		return seam, nil
	}}
}

// Pushed reads what run's launch pushed — see [runPushes].
func (p runPushes) Pushed(ctx context.Context, run sandbox.PendingRun) ([]sandbox.PushedRef, int, error) {
	repos := sandbox.Pushable(run.Repos)
	if len(repos) == 0 {
		return nil, 0, nil
	}
	launch, err := statelog.ParsePosition(run.CodeLaunch)
	switch {
	case err != nil:
		return nil, 0, fmt.Errorf("engine: run %s's launch position %q cannot be read: %w", run.TurnID,
			run.CodeLaunch, err)
	case launch.Stream != topics.CodeLogStream:
		return nil, 0, fmt.Errorf("engine: run %s's launch position %s is on another log than the code log",
			run.TurnID, launch)
	}
	reader, err := p.code()
	if err != nil {
		return nil, 0, err
	}
	ids := make([]string, len(repos))
	names := make(map[string]string, len(repos))
	for i, r := range repos {
		ids[i], names[r.RepoID] = r.RepoID, r.Repo
	}
	q := code.ReadQuery{Kind: code.ReadRunPushes,
		RunPushes: &code.RunPushesQuery{Run: run.LaunchID, Repos: ids, After: launch},
		Fresh:     statelog.Freshness{Level: statelog.ReadLinearizable}}
	var pushed pushCollapse
	for first := true; ; first = false {
		answer, readErr := reader.Read(ctx, q)
		if readErr != nil {
			return nil, 0, fmt.Errorf("engine: read what run %s pushed: %w", run.TurnID, readErr)
		}
		if addErr := pushed.add(answer.Pushes, names); addErr != nil {
			return nil, 0, fmt.Errorf("engine: read what run %s pushed: %w", run.TurnID, addErr)
		}
		switch {
		case answer.Next == "":
			return pushed.refs, pushed.more, nil
		case answer.Next == q.Cursor:
			// A PAGE THAT DID NOT MOVE would be read for ever: every page
			// the code host serves advances by at least one push.
			return nil, 0, fmt.Errorf("engine: read what run %s pushed: the code host answered cursor %q "+
				"again", run.TurnID, answer.Next)
		case first && (answer.Position.IsZero() || answer.Position.Stream != topics.CodeLogStream):
			return nil, 0, fmt.Errorf("engine: read what run %s pushed: its first page was answered at %s, "+
				"no position of the code log", run.TurnID, answer.Position)
		case first:
			q.Fresh = statelog.FloorAt(statelog.ReadSession, answer.Position)
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
// is refused: the read names only those, so such a row is the code host
// answering another read.
func (c *pushCollapse) add(rows []code.PushRow, names map[string]string) error {
	for _, row := range rows {
		name, given := names[row.RepoID]
		if !given {
			return fmt.Errorf("the code host answered push %s to repository %s, which the run was not given "+
				"to push to", row.PushID, row.RepoID)
		}
		for _, cmd := range row.Commands {
			if cmd.Result != code.ResultOK {
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
