package queries_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// storySources is a node holding three sessions of partyCompany's people: one
// Cy set going on a task, a direct message Ana sent a seat, and a page comment
// Ana left through her own key — so a story list is asked across all three
// ways a session can relate to a person and a published surface.
func storySources(t *testing.T) (queries.Sources, *stubWork) {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(partyCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	log := openStore(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	seq := 0
	write := func(payload events.Payload) {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		seq++
		if err := log.Append(t.Context(), store.EventRecord{
			ID: payload.EventType() + "-" + time.Duration(seq).String(), Type: payload.EventType(),
			Source: "engine", Category: "lifecycle", Time: at.Add(time.Duration(seq) * time.Second),
			Tags: store.ExtractTags(raw), Payload: raw,
		}); err != nil {
			t.Fatal(err)
		}
	}
	item := &types.WorkItem{Backend: types.WorkNative, ID: "t-412", Key: "ENG-412", Project: "ENG"}
	session := func(run string, trigger types.Trigger, charged *types.WorkItem, kind types.ChannelKind) {
		write(types.AgentTurnStarted{Agent: "a-swe", AgentHandle: "swe", RoleName: "SWE",
			TurnID: run, Trigger: trigger, WorkItem: charged, StartedAt: at})
		write(types.AgentTurnCompleted{Agent: "a-swe", RoleName: "SWE", TurnID: run,
			Decision: "done", WorkItem: charged})
		write(types.TurnCompleted{Agent: "a-swe", AgentHandle: "swe", TurnID: run,
			ReviewOutcome: "done", DurationMS: 60_000,
			Interactions: []types.InboundInteraction{{ChannelKind: kind}}})
	}
	session("run-task", types.Trigger{Type: "external_notification", Integration: tracker.Source,
		Summary: "cy assigned ENG-412", Actor: "cy"}, item, types.ChannelInternal)
	session("run-dm", types.Trigger{Type: "external_notification", Integration: "slack",
		Summary: "Message from Ana: are we hiring?", Actor: "ana"}, nil, types.ChannelDM)
	session("run-page", types.Trigger{Type: "external_notification", Integration: pages.Source,
		Summary: "ana commented on Runbook", Actor: "ops-1"}, nil, types.ChannelInternal)

	work := &stubWork{
		places: map[string]tracker.TurnPlace{"run-task": {TaskID: "t-412", Key: "ENG-412",
			Title: "Fix billing webhook", Ordinal: 3}},
		writes: map[string][]tracker.TurnWrite{"run-task": {{TaskID: "t-412", Key: "ENG-412",
			Title: "Fix billing webhook", Kind: tracker.ChangeComment, At: at}}},
	}
	return queries.Sources{
		Company: func() *config.Company { return cfg },
		Events:  fleetOf(log),
		Work:    work,
	}, work
}

func storyIDs(t *testing.T, got any) []string {
	t.Helper()
	page, ok := got.(queries.StoriesAnswer)
	if !ok {
		t.Fatalf("turn_stories answered %T", got)
	}
	ids := []string{}
	for _, story := range page.Stories {
		ids = append(ids, story.TurnID)
	}
	slices.Sort(ids)
	return ids
}

// A PERSON'S SESSIONS ARE READ UNDER THE PERSONAL RULE, AND ONLY THE ONES THAT
// ARE STORIES ARE TOLD.
//
// Ana reads the sessions she set going — under both her names, since a comment
// through her own key is attributed to the key — and the direct message she
// sent is LEFT OUT rather than told as an empty card: that she messaged a seat
// is the exchange's own fact. She reads Cy's because Cy is in her line, with
// what the session left behind; Cy may not read hers.
func TestAPersonsStoriesAreTheirOwnAndTheirLines(t *testing.T) {
	t.Parallel()
	sources, work := storySources(t)
	ana, cy := asMember("ops-1"), asMember("ops-2")

	got, err := askAs(t, sources, "turn_stories", map[string]any{"party": "ana"}, ana)
	if err != nil {
		t.Fatalf("turn_stories for herself: %v", err)
	}
	if ids := storyIDs(t, got); !slices.Equal(ids, []string{"run-page"}) {
		t.Errorf("Ana's stories = %v, want the page comment alone — the direct message "+
			"is hers and the seat's", ids)
	}
	if page := got.(queries.StoriesAnswer); !page.Complete {
		t.Errorf("a one-node fleet's page says it is incomplete")
	}

	got, err = askAs(t, sources, "turn_stories", map[string]any{"party": "cy"}, ana)
	if err != nil {
		t.Fatalf("turn_stories for a report: %v", err)
	}
	if ids := storyIDs(t, got); !slices.Equal(ids, []string{"run-task"}) {
		t.Fatalf("Cy's stories = %v, want the session he set going", ids)
	}
	story := got.(queries.StoriesAnswer).Stories[0]
	if story.WorkItem == nil || story.WorkItem.Key != "ENG-412" || story.WorkItem.Ordinal != 3 ||
		story.WorkItem.Title != "Fix billing webhook" {
		t.Errorf("work item = %+v, want ENG-412's third session by its title", story.WorkItem)
	}
	if len(story.LeftBehind) != 1 || story.LeftBehind[0].Key != "ENG-412" {
		t.Errorf("left behind = %+v, want the comment the run wrote", story.LeftBehind)
	}
	if story.Why.By != "cy" {
		t.Errorf("why.by = %q, want the person who set it going", story.Why.By)
	}
	if !slices.Equal(work.writesAsked, []string{"run-task"}) {
		t.Errorf("the tracker was asked what %v changed — one read for the page's runs", work.writesAsked)
	}

	if _, err := askAs(t, sources, "turn_stories", map[string]any{"party": "ana"}, cy); !errors.Is(err, queries.ErrForbidden) {
		t.Errorf("Cy reading his lead's sessions answered %v, want forbidden", err)
	}
	if _, err := askAs(t, sources, "turn_stories", map[string]any{"party": "ENG-412"}, ana); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a party that is not a handle answered %v, want bad params", err)
	}
}

// A WORK ITEM'S STORIES ARE THE SESSIONS CHARGED TO IT, by the identity the
// turns list takes.
func TestAWorkItemsStoriesAreTheSessionsChargedToIt(t *testing.T) {
	t.Parallel()
	sources, _ := storySources(t)
	got, err := askAs(t, sources, "turn_stories", map[string]any{"work_item": "native:t-412"},
		asMember("ops-2"))
	if err != nil {
		t.Fatalf("turn_stories: %v", err)
	}
	if ids := storyIDs(t, got); !slices.Equal(ids, []string{"run-task"}) {
		t.Errorf("ENG-412's stories = %v, want the one session charged to it", ids)
	}
}

// ONE STORY IS ASKED BY ITS TURN, AND A SESSION THAT IS NOT A STORY IS NOT
// FOUND — never refused, which would tell the caller it happened.
func TestOneStoryIsAskedByItsTurn(t *testing.T) {
	t.Parallel()
	sources, _ := storySources(t)
	member := asMember("ops-2")
	got, err := askAs(t, sources, "turn_story", map[string]any{"turn_id": "run-page"}, member)
	if err != nil {
		t.Fatalf("turn_story: %v", err)
	}
	if one, ok := got.(queries.StoryAnswer); !ok || one.Story.TurnID != "run-page" ||
		one.Story.Why.Origin != queries.OriginPage || one.Story.Why.By != "ana" {
		t.Errorf("turn_story = %+v, want the page comment, set going by Ana's key", got)
	}
	for _, id := range []string{"run-dm", "no-such-run"} {
		if _, err := askAs(t, sources, "turn_story", map[string]any{"turn_id": id}, member); !errors.Is(err, queries.ErrNotFound) {
			t.Errorf("turn_story %s answered %v, want not found", id, err)
		}
	}
	if _, err := askAs(t, sources, "turn_story", nil, member); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("turn_story with no turn answered %v, want bad params", err)
	}
}
