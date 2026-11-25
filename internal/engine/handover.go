package engine

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN THAT DOES NOT CONTINUE ITS SESSION IS HANDED THAT SESSION'S NEWEST
// SUMMARY (docs/concepts/conversation-sessions.md, "What the run is handed"):
// the probe reads the summary the verdict's session is named by, the turn's
// own record's ([sandbox.ContinuedSession.Summary]) or the lost turn's row's
// on the executor log, and the runner tells it fenced in the brief
// (internal/agent/runner's lostSection). A summary is kept up to
// executorkeep.SummaryMaxBytes, which no brief has room for, so the probe
// hands its END within [handOverBytes].

// handOverFallbackWindow is the window a hand-over is held to where no member
// of the seat's chain states one (OpenCode, Qwen Code and the rest compact by
// a window this build does not read): 200,000, the least of the windows the
// runners that state one assume of a model they do not know (Claude Code's
// claudeFallbackWindow; Codex's is 272,000, Gemini CLI's larger).
const handOverFallbackWindow = 200_000

// handOverBytes is the most of a summary a brief hands: HALF of what the
// chain's least window leaves below its compaction point beside
// turn_engine.executor_min_budget_tokens (floor), at the four bytes a token the
// launch floor reads a brief at (harness.EstimateTokens). Half, because the
// rest of the brief and the run's own work before its first compaction share
// the other half: handed more, the run would compact the summary away before it
// worked as far as the summary tells. Never past what is kept, and 0 where the
// floor leaves no room.
func handOverBytes(w sandbox.ExecutorWindow, floor int) int {
	window := int64(w.Standard)
	if window <= 0 {
		window = handOverFallbackWindow
	}
	room := (w.Point(window) - int64(floor)) / 2
	return int(min(max(room, 0)*4, executorkeep.SummaryMaxBytes))
}

// handOver is basis with the summary of the session its verdict names, as its
// run is handed it ([handedSummary]), read through the launcher's estate router
// and session store. A summary kept and not read is logged
// (`executor_session_summary_unhanded`), never an error.
func (l *agentLauncher) handOver(ctx context.Context, basis runner.SessionBasis,
	own *executorkeep.Summary) runner.SessionBasis {

	basis, unread := handedSummary(ctx, l, basis, own, l.turn.RunID, staticFloor(l.company).static)
	if unread != nil {
		log.InfoContext(ctx, "executor_session_summary_unhanded", "seat", l.seat.Handle(), "turn_id", l.turn.RunID,
			"detail", unread.Error(), "outcome", "the run is told its newest summary could not be read")
	}
	return basis
}

// summarySource is where a lost session's summary is read: the name of it the
// lost turn's row on the executor log keeps (nil for none, or no row) with the
// place of the capture that row names, and its text, from the object store.
type summarySource interface {
	lostRow(ctx context.Context, turn string) (*executorkeep.Summary, executorkeep.CapturePlace, error)
	readSummary(ctx context.Context, turn string, summary executorkeep.Summary) (string, error)
}

// handedSummary is basis with the summary of the session its verdict names
// ([runner.SessionBasis.Summary]): own, the run's turn's record's, where the
// verdict loses that turn's own session, else the lost turn's row's, read from
// src; its end within [handOverBytes] of basis's window beside floor, and newer
// than what the run continues as [newerThanContinued] reads it against the
// capture the lost turn's row names. One kept and not read is SummaryUnread,
// with why.
func handedSummary(ctx context.Context, src summarySource, basis runner.SessionBasis,
	own *executorkeep.Summary, runTurn string, floor int) (runner.SessionBasis, error) {

	n := basis.NotContinued
	if n == nil {
		return basis, nil
	}
	turn, summary, rowPlace := runTurn, own, executorkeep.CapturePlace{}
	if n.LostTurn != "" {
		row, place, err := src.lostRow(ctx, n.LostTurn)
		if err != nil {
			basis.SummaryUnread = true
			return basis, fmt.Errorf("turn %s's row could not be read: %w", n.LostTurn, err)
		}
		turn, summary, rowPlace = n.LostTurn, row, place
	}
	if summary == nil {
		return basis, nil
	}
	bound := handOverBytes(basis.Window, floor)
	if bound <= 0 {
		basis.SummaryUnread = true
		return basis, fmt.Errorf("the seat's least window (%d tokens) leaves no room beside "+
			"turn_engine.executor_min_budget_tokens (%d)", basis.Window.Standard, floor)
	}
	text, err := src.readSummary(ctx, turn, *summary)
	if err != nil {
		basis.SummaryUnread = true
		return basis, fmt.Errorf("turn %s's summary could not be read: %w", turn, err)
	}
	kept, start := executorkeep.TailWithin(text, bound)
	basis.Summary = &runner.LostSummary{Text: kept, Turn: turn, At: summary.At, Bytes: summary.Bytes,
		Cut: start > 0 || summary.Cut, KeptCut: summary.Cut, Readable: n.LostTurn != "",
		Newer: newerThanContinued(*n, *summary, rowPlace)}
	return basis, nil
}

// lostRow is the summary the lost turn's row on the executor log names, nil
// where it names none or the row is gone, and the place of the capture the row
// names.
func (l *agentLauncher) lostRow(ctx context.Context, turn string) (*executorkeep.Summary,
	executorkeep.CapturePlace, error) {

	if l.engine.router == nil {
		return nil, executorkeep.CapturePlace{}, errors.New("this node has no estate router to read the " +
			"executor log through")
	}
	read, err := l.engine.router.ExecutorReads().Session(ctx, l.seat.Handle(), turn, executordomain.RelaunchRead())
	switch {
	case errors.Is(err, executordomain.ErrNotFound):
		return nil, executorkeep.CapturePlace{}, nil
	case err != nil:
		return nil, executorkeep.CapturePlace{}, err
	}
	row := read.Session
	return row.Summary, executorkeep.CapturePlace{LaunchSeq: row.LaunchSeq, CaptureSeq: row.CaptureSeq}, nil
}

// readSummary is the summary turn's record or row names, opened from the
// object store within a read's budget for a summary at its bound.
func (l *agentLauncher) readSummary(ctx context.Context, turn string, summary executorkeep.Summary) (string,
	error) {

	store, err := l.engine.executorSessionStore()
	if err != nil {
		return "", err
	}
	read, cancel := context.WithTimeout(ctx, objstore.ReadBudget(executorkeep.SummarySealedMaxBytes))
	defer cancel()
	body, err := store.OpenSummary(read, executordomain.Session{Handle: l.seat.Handle(), Turn: turn,
		Summary: &summary})
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	text, err := io.ReadAll(io.LimitReader(body, executorkeep.SummaryMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("the summary could not be read: %w", err)
	}
	return string(text), nil
}

// newerThanContinued reports whether summary, of the session n names, is newer
// than the session the run continues in its place: always for a fresh run; for
// an older capture of the run's own turn, or of the lost turn's own where n
// continues that turn's row (row, the capture it names), where its place is
// after that capture's; else the lost session is a later turn's.
func newerThanContinued(n executorkeep.NotContinued, summary executorkeep.Summary,
	row executorkeep.CapturePlace) bool {

	switch {
	case n.Instead != executorkeep.InsteadOlder:
		return true
	case n.Older != nil:
		return summary.Place.After(*n.Older)
	case n.LostTurn != "" && n.LostTurn == n.OlderTurn:
		return summary.Place.After(row)
	}
	return true
}
