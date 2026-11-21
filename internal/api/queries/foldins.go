package queries

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/runanswer"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// FoldInItem is a fold-in waiting on a person's decision (internal/sandbox's
// FoldIn, ADR-0109's amendment "another writer's request joins only on
// approval"): who asked a busy seat for what, which running work it would
// join and whose apps it would run on, answered with `answer_executor` naming
// TurnID, LaunchID and ID. The shape a held call's approval is drawn in: a
// decision row of the Inbox.
type FoldInItem struct {
	ID       string `json:"id"`
	TurnID   string `json:"turn_id"`
	LaunchID string `json:"launch_id"`
	// Seat is the busy seat, and Requester whom its running work is for,
	// whose apps the request would run on.
	Seat      string `json:"seat"`
	Requester string `json:"requester"`
	// Asker is who wrote the request, Conversation where; Words their words
	// as the row keeps them, Cut how many characters more are where they were
	// written.
	Asker        string `json:"asker"`
	AskerKind    string `json:"asker_kind"`
	Conversation string `json:"conversation"`
	Words        string `json:"words" untrusted:"fence"`
	Cut          int    `json:"cut"`
	// PutTo is whom it was put to, the requester first.
	PutTo    []string           `json:"put_to"`
	AskedAt  time.Time          `json:"asked_at"`
	Decision *decision.Decision `json:"decision"`
	WorkItem *types.WorkItem    `json:"work_item,omitempty"`
}

// foldInItemOf is f on run as the Inbox draws it.
func foldInItemOf(run sandbox.PendingRun, f sandbox.FoldIn) *FoldInItem {
	return &FoldInItem{ID: f.ID, TurnID: run.TurnID, LaunchID: f.Launch, Seat: run.AgentHandle,
		Requester: run.Requester, Asker: f.Asker, AskerKind: string(f.AskerKind), Conversation: f.Conversation,
		Words: f.Words, Cut: f.Cut, PutTo: slices.Clone(f.PutTo), AskedAt: f.AskedAt,
		Decision: sandbox.FoldInDecision(run, f), WorkItem: run.WorkItem}
}

// foldInFor is where fold-in f on run stands for the caller on record:
// put to them, and theirs to answer as the answer's own write decides it
// ([runanswer.FoldIn]) at the read's proof, never the asker's own.
func (s Sources) foldInFor(ctx context.Context, run sandbox.PendingRun, f sandbox.FoldIn,
	record string) (put, may bool, err error) {

	put = slices.Contains(f.PutTo, record)
	principal, how := iam.From(ctx)
	if how != iam.Resolved || iam.RecordOwner(principal) == f.Asker {
		return put, false, nil
	}
	a, err := runanswer.FoldIn(run, iam.RecordOwner(principal))
	if errors.Is(err, runanswer.ErrUnreadableSeat) {
		log.WarnContext(ctx, "fold_in_unreadable_seat", "turn_id", run.TurnID, "error", err.Error())
		return put, false, nil
	}
	if err != nil {
		return false, false, err
	}
	d := authz.Decide(authz.WithCheck(ctx), principal, a.Action, a.Object, s.Chart, s.clock())
	if d.Unknown() {
		return false, false, fmt.Errorf("%w: this node cannot decide yet whether the caller may answer a "+
			"fold-in of %s's run: %w", ErrUnavailable, run.AgentHandle, d.Err)
	}
	return put, d.Allowed, nil
}
