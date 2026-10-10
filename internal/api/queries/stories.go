// The two story questions: a page of them, and one by its turn.

package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The page sizes of a story list.
//
// SMALLER THAN THE TURNS LIST'S, and for the reason a story costs more than a
// row: each is one fleet read of its turn's whole record ([FleetEvents.Turn])
// on top of the listing, where a turns row is an aggregate the listing already
// folded. Ten is a screen of story cards; twenty is what one read may ask for,
// at [storyReads] in flight, before a page waits on more fleet round trips than
// a person waits for.
const (
	DefaultStoryPage = 10
	MaxStoryPage     = 20
)

// storyReads is how many turns' records one story page reads at once. Each is
// a scatter to every node of the fleet, so the bound is on the fleet's work as
// much as this node's: four keeps a full page to five rounds of the fleet's
// read budget at worst, without one person's screen holding twenty scatters
// open against every other reader's.
const storyReads = 4

// StoriesAnswer is a page of turn stories.
type StoriesAnswer struct {
	Stories []TurnStory `json:"stories"`
	// Next is the cursor the next page resumes from — the turns list's own,
	// opaque — and absent at the end. A page can hold fewer stories than it
	// listed turns, or none, and still have a next: the sessions that are not
	// stories are left out after the listing, never instead of a cursor.
	Next string `json:"next,omitempty"`
	// Complete is false when a node that may hold part of this history did
	// not answer — the one fact of the fleet's coverage a member is owed,
	// without the node ids behind it, which are how the engine is run.
	Complete bool `json:"complete"`
}

// StoryAnswer is one turn's story.
type StoryAnswer struct {
	Story    TurnStory `json:"story"`
	Complete bool      `json:"complete"`
}

// turnStories answers a page of the sessions a member may read, newest first:
// those on one work item (`work_item=<backend>:<id>`, the identity `turns`
// takes), one seat's (`seat=<handle>`), or the ones one PERSON set going
// (`party=<handle>`).
//
// # Party is under the personal rule
//
// Which sessions a person started — every comment they left that woke a seat,
// every task they assigned — is a record about that person, so `party` is read
// by them and by the leads whose line they are in, exactly as their inbox is
// ([Sources.viewerParty]). It names a handle, the one the `viewer` answer
// gives; it matches the two names a person's writes carry — their handle, and
// the credential bound to them, since a write through a key is attributed to
// the key — on the trigger of the session's opening record.
func (s Sources) turnStories(ctx context.Context, p Params) (any, error) {
	q := store.TurnQuery{
		// THE WHOLE HISTORY THE STORIES CAN BE TOLD FROM, not the turns
		// list's default week: a task's activity is read back to its
		// start, and its stories end where the event history does.
		SinceDays: store.MaxTurnDays,
		Limit:     Clamp(p.Int("limit", 0), DefaultStoryPage, MaxStoryPage),
	}
	item, err := workItemParam(p)
	if err != nil {
		return nil, err
	}
	q.WorkItem = item
	agentID, err := s.seatParam(p)
	if err != nil {
		return nil, err
	}
	q.AgentID = agentID
	if party := strings.TrimSpace(p.String("party")); party != "" {
		if !org.ValidHandle(party) {
			return nil, fmt.Errorf("%w: party=%q is not a handle — name the person "+
				"by their seat's handle, as the viewer answer gives it", ErrBadParams, party)
		}
		who, refused := s.viewerParty(ctx, party)
		if refused != nil {
			return nil, refused
		}
		q.StartedBy = []string{who.Handle, who.OperatorID}
	}
	before, err := turnCursorParam(p)
	if err != nil {
		return nil, err
	}
	q.Before = before

	page, coverage, err := s.Events.Turns(ctx, q)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(page.Turns))
	for _, turn := range page.Turns {
		ids = append(ids, turn.TurnID)
	}
	stories, complete, err := s.storiesOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := StoriesAnswer{Stories: stories, Complete: complete && coverage.Complete}
	if page.Next != nil {
		out.Next = encodeTurnCursor(*page.Next)
	}
	return out, nil
}

// turnStory answers one session's story by its turn id.
//
// A SESSION THAT IS NOT A STORY IS NOT FOUND, never a refusal: a direct message
// or an exchange between agents answered "you may not read this" would tell the
// caller it happened, which is the one fact the rule withholds.
func (s Sources) turnStory(ctx context.Context, p Params) (any, error) {
	id := strings.TrimSpace(p.String("turn_id"))
	if id == "" {
		return nil, badParams("turn_id", "", nil)
	}
	stories, complete, err := s.storiesOf(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	if len(stories) == 0 {
		return nil, ErrNotFound
	}
	return StoryAnswer{Story: stories[0], Complete: complete}, nil
}

// storiesOf tells the stories of the turns named, in the order named, leaving
// out every one that is not a story, and says whether every node that may hold
// part of them answered.
//
// THE TURNS' OWN RECORDS ARE READ BESIDE THE TRACKER'S AND THE PAGES' ACCOUNT
// OF THE SAME RUNS — where each sits on its task and what it changed — each
// asked once for the whole page rather than per story. Those two are read at
// `stale`, as every other read beside the event history is ([pagereads.go]): a
// story is history, and no write a person makes is read back through one.
func (s Sources) storiesOf(ctx context.Context, ids []string) ([]TurnStory, bool, error) {
	rows := make([][]store.EventRecord, len(ids))
	covered := make([]eventfan.Coverage, len(ids))
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(storyReads)
	for i, id := range ids {
		group.Go(func() error {
			detail, coverage, err := s.Events.Turn(gctx, id)
			if err != nil {
				return err
			}
			rows[i], covered[i] = detail.Rows, coverage
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, false, err
	}
	complete := true
	for _, coverage := range covered {
		complete = complete && coverage.Complete
	}

	stale := statelog.Freshness{Level: statelog.ReadStale}
	places := map[string]tracker.TurnPlace{}
	tasks := map[string][]tracker.TurnWrite{}
	wrote := map[string][]pages.TurnWrite{}
	if s.Work != nil && len(ids) > 0 {
		var err error
		if places, err = s.Work.TurnPlaces(ctx, ids, stale); err != nil {
			return nil, false, err
		}
		if tasks, err = s.Work.TurnWrites(ctx, ids, stale); err != nil {
			return nil, false, err
		}
	}
	if s.Pages != nil && len(ids) > 0 {
		var err error
		if wrote, err = s.Pages.TurnWrites(ctx, ids, stale); err != nil {
			return nil, false, err
		}
	}

	organization := s.organization()
	viewer := s.storyViewer(ctx, organization)
	seat := func(identity string) string {
		if organization == nil || identity == "" {
			return ""
		}
		if role := organization.SeatByHandle(identity); role != nil {
			return role.Handle()
		}
		if role := organization.SeatByOperatorID(identity, s.Env); role != nil {
			return role.Handle()
		}
		return ""
	}

	out := make([]TurnStory, 0, len(ids))
	for i, id := range ids {
		in := storyInput{
			TurnID: id, Rows: rows[i], Tasks: tasks[id], Pages: wrote[id],
			Viewer: viewer, Seat: seat,
		}
		if place, ok := places[id]; ok {
			in.Place = &place
		}
		story, told := storyOf(in)
		if !told {
			continue
		}
		// A SESSION STILL RUNNING ON A NATIVE TASK is that task's next turn,
		// which only the task's own count can say — read for it alone, since
		// a page holds at most one running session per seat.
		if story.Outcome == OutcomeRunning && in.Place == nil && story.WorkItem != nil &&
			story.WorkItem.Backend == types.WorkNative && s.Work != nil {
			next, err := s.nextTurn(ctx, story.WorkItem.ID, stale)
			if err != nil {
				return nil, false, err
			}
			in.NextOrdinal = next
			story, _ = storyOf(in)
		}
		out = append(out, story)
	}
	return out, complete, nil
}

// nextTurn is the number a task's next counted turn will have: one past the
// turns its spend already counts. Zero for a task the tracker does not hold.
func (s Sources) nextTurn(ctx context.Context, task string, fresh statelog.Freshness) (int, error) {
	detail, err := s.Work.Task(ctx, task, tracker.DetailWants{}, fresh)
	switch {
	case err == nil:
		return detail.Task.Spend.Turns + 1, nil
	case errors.Is(err, tracker.ErrNoTask):
		return 0, nil
	}
	return 0, err
}

// storyViewer is the caller, as the notes they sent are signed: the seat their
// key is linked to and the key itself.
func (s Sources) storyViewer(ctx context.Context, organization *org.Organization) storyViewer {
	caller := callerFrom(ctx)
	viewer := storyViewer{OperatorID: caller.ID}
	if seat := s.seatIn(organization, caller.ID); seat != nil {
		viewer.Handle = seat.Handle()
	}
	return viewer
}
