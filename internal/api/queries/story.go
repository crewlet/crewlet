// A turn told as a story a member may read.

package queries

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/slack"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// # What a story is, and why it is not the turn
//
// A turn's answer is everything its run published: the prompts it was sent,
// the tools it called and their arguments, the model and node that served it,
// what it cost and what the seat learned afterwards. ADR-0031 puts all of that
// on the admin side of the line — it is what the machine PROCESSED — and a
// teammate asking "what did the agent do on my task?" is owed an answer that
// carries none of it. So a STORY is a second reading of the same rows, built
// here by [storyOf], beside the `turn` answer and by the same band rule
// ([types.BandOf]): who did it, on which work, when and for how long, why it
// started, what it did and left behind, and how it ended — typed facts a screen
// words for a person, never a payload a screen filters.
//
// Every row the band rule places has a decision here ([storyReadings]), even
// when the decision is to withhold it, and a gate holds the table to the bands
// in both directions: a new kind of row reaches a story because somebody
// decided it should, never because it fell through.
//
// # Which sessions are stories at all
//
// A session is a story when it is CHARGED TO A WORK ITEM (ADR-0022) — the work
// is published, so what happened on it is — or when it was STARTED FROM A
// PUBLISHED SURFACE: a task, a page, a message in a channel others can read, a
// routine the company declares. Anything else is a conversation between two
// parties that the company did not publish — a direct message, an agent asking
// another agent outside any work — and it is not told, not even as an empty
// shell, because "Agent CTO had a 1:1 at 09:12" is already a fact about
// somebody's private exchange. A reflection pass is never a story either: it is
// what the seat learned, which is the machine's, and its rows are withheld from
// the story of the turn they follow.
//
// A STORY IS TURN-LEVEL DETAIL, so it lives as long as the event history does —
// thirty days on each data node (ADR-0021). What outlives that is the task's
// own count of its turns and their spend, which the tracker keeps.

// TurnStory is one agent session told for a member: the published facts of a
// turn and nothing the machine processed. See `TestEveryTurnStoryFieldIsClassified`
// for why each field may be here.
type TurnStory struct {
	// TurnID is the session's identity: what `turn_story` is asked by and
	// what keys a list. Not a trace id; the change feed already names it on
	// every change a session made.
	TurnID string `json:"turn_id"`
	// Seat is the handle of the agent whose session it was.
	Seat string `json:"seat"`
	// WorkItem is the work the session is charged to, absent when it is on
	// none.
	WorkItem *StoryItem `json:"work_item,omitempty"`
	// StartedAt is when the session began — its first record — and
	// DurationMS how long it worked: every segment's own measurement summed,
	// so the hours a parked session waited on a coding job are not counted
	// as work. Zero for a session that has finished no segment yet.
	StartedAt  time.Time `json:"started_at"`
	DurationMS int       `json:"duration_ms"`
	// Why is what set the session going.
	Why StoryWhy `json:"why"`
	// Did is what the session did beyond answering, in the order it did it.
	Did []StoryAction `json:"did"`
	// LeftBehind is what the session changed in the company's own work and
	// pages, as links, in the order the changes landed.
	LeftBehind []StoryLink `json:"left_behind"`
	// Outcome is how the session ended, or that it has not.
	Outcome StoryOutcome `json:"outcome"`
	// Helpers is how many short-lived helpers the agent handed parts of the
	// work to.
	Helpers int `json:"helpers"`
	// Consulted is the colleagues the agent asked, by handle, once each in
	// the order it first asked them. Names only: what was said between
	// agents is the machine's traffic.
	Consulted []string `json:"consulted"`
	// YourNote is the newest note THE CALLER sent this session while it ran,
	// and what became of it; absent when they sent none. Somebody else's note
	// is theirs to read.
	YourNote *StoryNote `json:"your_note,omitempty"`
}

// StoryItem is the work a session was charged to.
type StoryItem struct {
	Backend types.WorkBackend `json:"backend"`
	ID      string            `json:"id"`
	Key     string            `json:"key"`
	// Title is the task's title as it reads now, where the tracker holds
	// the task: a native one, not removed.
	Title string `json:"title,omitempty"`
	// Ordinal is which session on the task this is — "session 3" — by the
	// task's own count of its turns, so the number on a story and the
	// count in the task's own record are one fact. Zero where the task
	// cannot say: a vendor's issue, or a session counted on another item.
	Ordinal int `json:"ordinal,omitempty"`
}

// StoryOrigin is the kind of surface a session was set going from — which is
// also why it may be told.
type StoryOrigin string

// The origins a story names.
const (
	// OriginTask is a work item: the engine's own tracker, or a vendor's
	// issue tracker.
	OriginTask StoryOrigin = "task"
	// OriginPage is a knowledge-base page: the engine's own pages, or
	// Confluence.
	OriginPage StoryOrigin = "page"
	// OriginChat is a message in a PUBLIC channel — never a direct message,
	// and never a private channel or a group message either: each is read by
	// the people in it alone, which is not what the company published.
	OriginChat StoryOrigin = "chat"
	// OriginSchedule is a routine the company declares, which `schedules`
	// already shows everybody.
	OriginSchedule StoryOrigin = "schedule"
	// OriginColleague is another agent asking for help ON A WORK ITEM: the
	// ask inherits the item, so the session is told as work on it; who
	// asked is named, and what they asked is not.
	OriginColleague StoryOrigin = "colleague"
	// OriginPrivate is a session set going by something the company did not
	// publish — a direct message, an alert, an exchange between agents —
	// told only because it is charged to a work item, and so told without
	// what set it going.
	OriginPrivate StoryOrigin = "private"
)

// StoryOrigins is every origin, in the order above.
var StoryOrigins = []StoryOrigin{OriginTask, OriginPage, OriginChat, OriginSchedule,
	OriginColleague, OriginPrivate}

// Valid reports whether this build knows o.
func (o StoryOrigin) Valid() bool { return slices.Contains(StoryOrigins, o) }

// published reports whether a session from o may be told although it is
// charged to no work item.
func (o StoryOrigin) published() bool {
	switch o {
	case OriginTask, OriginPage, OriginChat, OriginSchedule:
		return true
	}
	return false
}

// StoryWhy is what set a session going.
type StoryWhy struct {
	Origin StoryOrigin `json:"origin"`
	// Summary is the trigger's own one-line label — "Message from Ana: the
	// numbers", "jane assigned ENG-412" — and empty for an origin the story
	// may not quote.
	Summary string `json:"summary,omitempty"`
	// By is the handle of the seat — a person or an agent — that set it
	// going, where the trigger names one the chart knows.
	By string `json:"by,omitempty"`
}

// StoryActionKind is one kind of thing a session did beyond answering.
type StoryActionKind string

// The actions a story names.
const (
	// ActionReadPage — it opened pages of the knowledge base, whole. A
	// search's ranked list is not a read, and neither is what the engine put
	// in front of it before it began.
	ActionReadPage StoryActionKind = "read_page"
	// ActionCodingStarted — it started a coding job.
	ActionCodingStarted StoryActionKind = "coding_started"
	// ActionCodingAsked — its coding job paused to ask people a question.
	ActionCodingAsked StoryActionKind = "coding_asked"
	// ActionCodingAnswered — an answer to that question reached the job and
	// it carried on.
	ActionCodingAnswered StoryActionKind = "coding_answered"
	// ActionCodingFinished — the coding job finished.
	ActionCodingFinished StoryActionKind = "coding_finished"
)

// StoryActionKinds is every action kind.
var StoryActionKinds = []StoryActionKind{ActionReadPage, ActionCodingStarted,
	ActionCodingAsked, ActionCodingAnswered, ActionCodingFinished}

// Valid reports whether this build knows k.
func (k StoryActionKind) Valid() bool { return slices.Contains(StoryActionKinds, k) }

// StoryAction is one thing a session did.
type StoryAction struct {
	Kind StoryActionKind `json:"kind"`
	At   time.Time       `json:"at"`
	// Pages are the pages a read opened, for ActionReadPage.
	Pages []StoryPage `json:"pages,omitempty"`
}

// StoryPage is one page a session read.
type StoryPage struct {
	// Backend is the knowledge base that holds it — `native` or
	// `confluence` — since a page id is an address inside one.
	Backend string `json:"backend"`
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
}

// StoryLinkKind is what a left-behind link points at.
type StoryLinkKind string

// The two kinds of thing a session leaves behind.
const (
	LinkTask StoryLinkKind = "task"
	LinkPage StoryLinkKind = "page"
)

// StoryLinkKinds is both.
var StoryLinkKinds = []StoryLinkKind{LinkTask, LinkPage}

// Valid reports whether this build knows k.
func (k StoryLinkKind) Valid() bool { return slices.Contains(StoryLinkKinds, k) }

// StoryLink is one change a session made to the company's work or pages.
type StoryLink struct {
	Kind StoryLinkKind `json:"kind"`
	ID   string        `json:"id"`
	// Key is a task's key; empty for a page.
	Key   string `json:"key,omitempty"`
	Title string `json:"title"`
	// Change is what the change was, in its own domain's vocabulary — the
	// tracker's change kinds for a task, the pages' for a page — which the
	// task's and the page's own history already word.
	Change string `json:"change"`
	// Status is the status a task was moved to, when the change moved one.
	Status string    `json:"status,omitempty"`
	At     time.Time `json:"at"`
}

// StoryOutcome is how a session ended, or that it has not.
type StoryOutcome string

// The outcomes a story names.
const (
	// OutcomeApproved — it finished and its own check passed the work.
	OutcomeApproved StoryOutcome = "approved"
	// OutcomeSentBack — it finished with its own check still asking for
	// another pass.
	OutcomeSentBack StoryOutcome = "sent_back"
	// OutcomeFailed — it did not finish: it broke, the engine stopped it,
	// a person stopped it, or its coding job was lost.
	OutcomeFailed StoryOutcome = "failed"
	// OutcomeWaitingOnYou — its coding job is paused on a question to
	// people; `decisions` says whom.
	OutcomeWaitingOnYou StoryOutcome = "waiting_on_you"
	// OutcomeRunning — it has not ended: working now, or a coding job is.
	OutcomeRunning StoryOutcome = "running"
)

// StoryOutcomes is every outcome.
var StoryOutcomes = []StoryOutcome{OutcomeApproved, OutcomeSentBack, OutcomeFailed,
	OutcomeWaitingOnYou, OutcomeRunning}

// Valid reports whether this build knows o.
func (o StoryOutcome) Valid() bool { return slices.Contains(StoryOutcomes, o) }

// StoryNote is the caller's own note to a running session and what became of
// it: read at a round, or expired unread because the session ended first.
type StoryNote struct {
	Note   string             `json:"note"`
	Status types.SteerOutcome `json:"status"`
	SentAt time.Time          `json:"sent_at"`
	// At is when it was read, or when it expired.
	At time.Time `json:"at"`
}

// storyViewer is whose note "your note" is: the caller's seat and the
// credential bound to it — a note sent from the dashboard names the token and
// the person behind it, one from a seat's own surface the handle alone.
type storyViewer struct {
	Handle     string
	OperatorID string
}

// storyInput is everything one story is told from: the turn's own rows, and
// the tracker's and the pages' account of the same run — where it sits on its
// task, and what it changed.
type storyInput struct {
	TurnID string
	// Rows are the turn's own events, oldest first, as the `turn` answer
	// reads them.
	Rows []store.EventRecord
	// Place is the tracker's place for this run, nil where it holds none;
	// NextOrdinal is the number the task's next turn will have, for a
	// session not counted on its task yet.
	Place       *tracker.TurnPlace
	NextOrdinal int
	// Tasks and Pages are what the run changed, oldest first.
	Tasks []tracker.TurnWrite
	Pages []pages.TurnWrite
	// Viewer is the caller, and Seat resolves an identity a trigger names —
	// a handle, or a credential bound to one — to a seat's handle, "" for
	// one the chart does not know.
	Viewer storyViewer
	Seat   func(identity string) string
}

// storyReading is what a story makes of one kind of row: Read folds it into
// the story being built, and a nil Read withholds it, with Why saying why.
type storyReading struct {
	Read func(b *storyBuilder, rec store.EventRecord)
	Why  string
}

// storyBuilder is a story while its rows are read.
type storyBuilder struct {
	story     TurnStory
	viewer    storyViewer
	opened    *types.AgentTurnStarted
	firstAt   time.Time
	itemStart *types.WorkItem
	itemEnd   *types.WorkItem
	// The NEWEST segment state: the last completion, and whether a resumed
	// start came after it — a parked turn that was resumed is running
	// again.
	ended        *types.AgentTurnCompleted
	resumedSince bool
	lostSince    bool
	decision     string
	interactions []types.InboundInteraction
	asking       bool
	consulted    map[string]bool
}

// storyReadings is what a story makes of every row the band rule places — the
// whole table, withheld rows included, held to [types.BandOf] in both
// directions by `TestEveryBandedRowHasAStoryReading`.
var storyReadings = map[string]storyReading{
	// ---- given -----------------------------------------------------------
	"agent_turn_started": {Read: (*storyBuilder).started,
		Why: "who, on which work, when, and what set it going"},
	"agent_turn_steered": {Read: (*storyBuilder).steered,
		Why: "the caller's own note and what became of it"},
	"prefetch_summary": {Why: "what the engine assembled into the prompt — the " +
		"machine's preparation, not the session's work"},
	"prompt.size": {Why: "a prompt's weight, which is the machine's"},

	// ---- did -------------------------------------------------------------
	"knowledge_read": {Read: (*storyBuilder).read,
		Why: "a page the agent opened whole; a search's ranked list and the " +
			"engine's own prefetch are not reads, and carry the query an agent typed"},
	"sandbox_run_started": {Read: (*storyBuilder).codingStarted,
		Why: "a coding job began"},
	"sandbox_clarification_requested": {Read: (*storyBuilder).asked,
		Why: "the coding job paused on a question to people — whom is decisions' to say"},
	"sandbox_run_answered": {Read: (*storyBuilder).answered,
		Why: "an answer reached the coding job"},
	"sandbox_run_completed": {Read: (*storyBuilder).finished,
		Why: "the coding job finished"},
	"subagent_batched": {Read: (*storyBuilder).helped,
		Why: "how many helpers it handed work to; their prompts and answers are the machine's"},
	"a2a_channel_opened": {Read: (*storyBuilder).consult,
		Why: "which colleague it asked, by name"},
	"a2a_message_sent": {Why: "what agents said to each other is the machine's " +
		"traffic; the channel already names who was asked"},
	"a2a_channel_closed": {Why: "the close of a channel the story already named"},
	"skill_used":         {Why: "which skills a seat loads is how it is run"},

	// ---- absorbed --------------------------------------------------------
	"agent_turn_completed": {Read: (*storyBuilder).completed,
		Why: "how a segment ended — the outcome, and the item a sole write charged"},
	"turn_completed": {Read: (*storyBuilder).learned,
		Why: "how long it worked, its own check's decision, and where it was asked"},
	"agent_phase_started": {Why: "a phase opening is the machine's step, not the work"},
	"agent_phase_completed": {Why: "a phase's prompts, response, tools and spend — " +
		"everything the machine processed"},
	"agent_turn_progress": {Why: "the live call's progress, which is never stored"},
	"auxiliary_spend":     {Why: "spend"},

	// ---- went wrong ------------------------------------------------------
	"sandbox_run_failed": {Read: (*storyBuilder).lost,
		Why: "a coding job lost after its session parked ends that session"},
	"turn.guard_breach": {Why: "the engine's reason for stopping it, which the " +
		"outcome already says it did"},
	"llm_unavailable":          {Why: "a provider failure — how the engine is run"},
	"budget_exhausted":         {Why: "a budget refusal — spend"},
	"provider_fallback":        {Why: "which model answered — how the engine is run"},
	"phase.tool_skill_blocked": {Why: "a refused tool skill — how the seat is run"},
	"skill_telemetry_write_failed": {Why: "the reflection's bookkeeping, which is " +
		"the machine's"},
	"agent_turn_stopped": {Why: "who stopped it is the pause's own record; the " +
		"outcome says it did not finish"},

	// ---- left behind (the reflection) ------------------------------------
	"episode_written":              {Why: "the seat's diary — what it learned, which is the machine's"},
	"persist_decider_completed":    {Why: "the seat's diary"},
	"counterparty_profile_updated": {Why: "what the seat learned about a person"},
	"reflection_completed":         {Why: "the reflection pass ending, which is never a story"},
	"skill_synthesized":            {Why: "a skill the seat drafted — how it is run"},
	"skill_refined":                {Why: "a skill the seat refined — how it is run"},
	"skill_revived":                {Why: "a skill brought back into use — how it is run"},
}

// The trigger and integration names a story's origin is read off, each from
// the payload or package that owns it rather than spelled here.
var (
	notificationTrigger = types.ExternalNotification{}.EventType()
	coalescedTrigger    = types.NotificationsCoalesced{}.EventType()
	scheduleTrigger     = types.TaskAssigned{}.EventType()
	colleagueTrigger    = types.A2ARequestType

	// taskSurfaces and pageSurfaces are the integrations whose wakes are
	// about a published work item or page; chatSurfaces are the chat
	// backends, published only in a channel.
	taskSurfaces = []string{tracker.Source, jira.Backend, github.Backend, gitlab.Backend}
	pageSurfaces = []string{pages.Source, confluence.Backend}
	chatSurfaces = []string{slack.Backend, mattermost.Backend}
)

// storyOf tells one session's story from everything it is told from, or
// answers false for a session that is not a story: one neither charged to a
// work item nor started from a published surface, and one that decided nobody
// was asking it to do anything, which did nothing to tell.
//
// A PURE FUNCTION OVER ITS INPUT, for the reason `textindex`'s arithmetic is
// one: what a story withholds is decided here and nowhere else, and a rule
// that can only be exercised through a fleet of event stores is a rule nobody
// re-measures.
func storyOf(in storyInput) (TurnStory, bool) {
	b := &storyBuilder{
		story: TurnStory{
			TurnID: in.TurnID, Did: []StoryAction{}, LeftBehind: []StoryLink{},
			Consulted: []string{},
		},
		viewer:    in.Viewer,
		consulted: map[string]bool{},
	}
	for _, rec := range in.Rows {
		if b.firstAt.IsZero() || rec.Time.Before(b.firstAt) {
			b.firstAt = rec.Time
		}
		if reading := storyReadings[rec.Type]; reading.Read != nil {
			reading.Read(b, rec)
		}
	}

	// THE WORK IT IS ON: the item a completion charged it to, which can be a
	// sole write the dispatch could not know, and otherwise the item it was
	// woken for.
	item := b.itemEnd
	if item == nil {
		item = b.itemStart
	}
	origin, quotable := b.origin()
	if item == nil && !origin.published() {
		return TurnStory{}, false
	}
	if b.decision == string(phase.Skipped) {
		return TurnStory{}, false
	}
	if item != nil {
		story := &StoryItem{Backend: item.Backend, ID: item.ID, Key: item.Key}
		if in.Place != nil && in.Place.TaskID == item.ID {
			story.Title, story.Key = in.Place.Title, in.Place.Key
			story.Ordinal = in.Place.Ordinal
		}
		b.story.WorkItem = story
	}
	b.story.Outcome = b.outcome()
	if b.story.WorkItem != nil && b.story.WorkItem.Ordinal == 0 && in.Place == nil &&
		b.story.Outcome == OutcomeRunning {
		// A SESSION STILL RUNNING has no row on its task yet — it is
		// recorded when a segment completes — so it is the task's next
		// turn, which is what the task's own count will call it.
		b.story.WorkItem.Ordinal = in.NextOrdinal
	}

	b.story.Why.Origin = origin
	if b.opened != nil {
		if quotable {
			b.story.Why.Summary = b.opened.Trigger.Summary
		}
		if in.Seat != nil {
			b.story.Why.By = in.Seat(b.opened.Trigger.Actor)
		}
	}
	// ITS FIRST RECORD, which is the instant `turn_stories` pages by — the
	// turns list orders and cuts its cursor on a turn's earliest row — so a
	// reader merging stories with another history by time cuts both at one
	// horizon. The opening record's own stamp is the same instant to within
	// a publish, and on a session whose opening aged out it is absent.
	b.story.StartedAt = b.firstAt.UTC()
	b.story.LeftBehind = leftBehind(in.Tasks, in.Pages)
	return b.story, true
}

// origin reads what set the session going off its dispatch's opening record,
// and whether the story may quote that trigger's label.
//
// A CHAT SURFACE IS PUBLISHED ONLY IN A CHANNEL, and the turn's own record is
// the only one that says which: its learning record names the kind of every
// message that woke it. A session still running has no such record, so a chat
// session is not told until it has one — a direct message read as a channel's
// would publish two people's exchange to everybody.
func (b *storyBuilder) origin() (StoryOrigin, bool) {
	if b.opened == nil {
		return OriginPrivate, false
	}
	trigger := b.opened.Trigger
	switch trigger.Type {
	case notificationTrigger, coalescedTrigger:
		switch {
		case slices.Contains(taskSurfaces, trigger.Integration):
			return OriginTask, true
		case slices.Contains(pageSurfaces, trigger.Integration):
			return OriginPage, true
		case slices.Contains(chatSurfaces, trigger.Integration) && b.inChannel():
			return OriginChat, true
		}
	case scheduleTrigger:
		return OriginSchedule, true
	case colleagueTrigger:
		return OriginColleague, false
	}
	return OriginPrivate, false
}

// inChannel reports whether every message that woke the session was in a
// public channel.
//
// NOT A GROUP. Both chat backends file a private channel and a multi-person
// direct message as [types.ChannelGroup] — a room, not one seat's inbox, which
// is the distinction a worker needs — and each is read by the people in it
// alone. A member outside that room was never shown the exchange, so the
// story of what it set going would publish it to them.
func (b *storyBuilder) inChannel() bool {
	if len(b.interactions) == 0 {
		return false
	}
	for _, in := range b.interactions {
		if in.ChannelKind != types.ChannelPublic {
			return false
		}
	}
	return true
}

// outcome is how the session ended, from its newest segment.
func (b *storyBuilder) outcome() StoryOutcome {
	switch {
	case b.ended == nil, b.resumedSince:
		return OutcomeRunning
	case b.ended.Suspended && b.lostSince:
		return OutcomeFailed
	case b.ended.Suspended && b.asking:
		return OutcomeWaitingOnYou
	case b.ended.Suspended:
		return OutcomeRunning
	case b.ended.Failed, b.ended.Stopped:
		return OutcomeFailed
	}
	// THE TURN'S OWN DECISION, which is its reviewer's except where the engine
	// overrode it: a session that ended with its check still asking for
	// another pass — out of iterations — was sent back, not approved.
	switch phase.Decision(b.decision) {
	case phase.SelfIterate:
		return OutcomeSentBack
	case phase.Failed:
		return OutcomeFailed
	}
	return OutcomeApproved
}

// decode reads a row's payload into v, reporting whether it could: a payload
// this build cannot read costs the story that row, never the story.
func decode(rec store.EventRecord, v any) bool {
	return len(rec.Payload) > 0 && json.Unmarshal(rec.Payload, v) == nil
}

func (b *storyBuilder) started(rec store.EventRecord) {
	var start types.AgentTurnStarted
	if !decode(rec, &start) {
		return
	}
	if b.story.Seat == "" {
		b.story.Seat = start.AgentHandle
	}
	if start.Resumed {
		// A RESUMED SEGMENT is the same session working again, and its
		// trigger is the box's completion rather than what asked for the
		// work — so it says the session runs, and nothing about why.
		if b.ended != nil {
			b.resumedSince = true
		}
		return
	}
	if b.opened == nil {
		b.opened = &start
		if start.WorkItem != nil && start.WorkItem.ID != "" {
			b.itemStart = start.WorkItem
		}
	}
}

func (b *storyBuilder) completed(rec store.EventRecord) {
	var end types.AgentTurnCompleted
	if !decode(rec, &end) {
		return
	}
	b.ended, b.resumedSince, b.lostSince = &end, false, false
	if end.WorkItem != nil && end.WorkItem.ID != "" {
		b.itemEnd = end.WorkItem
	}
	if end.Decision != "" {
		b.decision = end.Decision
	}
}

func (b *storyBuilder) learned(rec store.EventRecord) {
	var done types.TurnCompleted
	if !decode(rec, &done) {
		return
	}
	if b.story.Seat == "" {
		b.story.Seat = done.AgentHandle
	}
	b.story.DurationMS += done.DurationMS
	if done.ReviewOutcome != "" {
		b.decision = done.ReviewOutcome
	}
	if len(done.Interactions) > 0 {
		b.interactions = done.Interactions
	}
	if done.WorkItem != nil && done.WorkItem.ID != "" {
		b.itemEnd = done.WorkItem
	}
}

// steered keeps the caller's newest note, matched on either of the two names a
// person's note carries; anybody else's is theirs to read.
func (b *storyBuilder) steered(rec store.EventRecord) {
	var note types.AgentTurnSteered
	if !decode(rec, &note) {
		return
	}
	viewer := b.viewer
	mine := (viewer.Handle != "" && (note.SteeredBySeat == viewer.Handle ||
		note.SteeredBy == viewer.Handle)) ||
		(viewer.OperatorID != "" && note.SteeredBy == viewer.OperatorID)
	if !mine {
		return
	}
	b.story.YourNote = &StoryNote{
		Note: note.Note, Status: note.Outcome, SentAt: note.SentAt.UTC(), At: rec.Time.UTC(),
	}
}

func (b *storyBuilder) read(rec store.EventRecord) {
	var read types.KnowledgeRead
	if !decode(rec, &read) || read.Via != types.ReadViaGetPage || len(read.Pages) == 0 {
		return
	}
	action := StoryAction{Kind: ActionReadPage, At: rec.Time.UTC()}
	for _, page := range read.Pages {
		action.Pages = append(action.Pages, StoryPage{
			Backend: read.Backend, ID: page.ID, Title: page.Title,
		})
	}
	b.story.Did = append(b.story.Did, action)
}

func (b *storyBuilder) codingStarted(rec store.EventRecord) {
	b.story.Did = append(b.story.Did, StoryAction{Kind: ActionCodingStarted, At: rec.Time.UTC()})
	b.asking = false
}

func (b *storyBuilder) asked(rec store.EventRecord) {
	b.story.Did = append(b.story.Did, StoryAction{Kind: ActionCodingAsked, At: rec.Time.UTC()})
	b.asking = true
}

func (b *storyBuilder) answered(rec store.EventRecord) {
	var answer types.SandboxRunAnswered
	if !decode(rec, &answer) || answer.Outcome != types.AnswerResumed {
		return
	}
	b.story.Did = append(b.story.Did, StoryAction{Kind: ActionCodingAnswered, At: rec.Time.UTC()})
	b.asking = false
}

func (b *storyBuilder) finished(rec store.EventRecord) {
	b.story.Did = append(b.story.Did, StoryAction{Kind: ActionCodingFinished, At: rec.Time.UTC()})
	b.asking = false
}

func (b *storyBuilder) lost(store.EventRecord) {
	if b.ended != nil && b.ended.Suspended {
		b.lostSince = true
	}
}

func (b *storyBuilder) helped(rec store.EventRecord) {
	var batch types.SubagentBatched
	if decode(rec, &batch) {
		b.story.Helpers += batch.TaskCount
	}
}

func (b *storyBuilder) consult(rec store.EventRecord) {
	var channel types.A2AChannelOpened
	if !decode(rec, &channel) || channel.Target == "" || b.consulted[channel.Target] {
		return
	}
	// THE ASKS THIS SESSION MADE, not the one it answers: a session woken by
	// a colleague's ask opened no channel, and says who asked in `why`.
	if b.story.Seat != "" && channel.Requester != b.story.Seat {
		return
	}
	b.consulted[channel.Target] = true
	b.story.Consulted = append(b.story.Consulted, channel.Target)
}

// leftBehind merges what a run changed on the tracker and on the pages into
// one list in the order the changes landed, each distinct change once: a
// session that commented on one task three times commented on it.
func leftBehind(tasks []tracker.TurnWrite, wrote []pages.TurnWrite) []StoryLink {
	out := make([]StoryLink, 0, len(tasks)+len(wrote))
	seen := map[StoryLink]bool{}
	add := func(link StoryLink) {
		key := link
		key.At, key.Title = time.Time{}, ""
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, link)
	}
	for _, w := range tasks {
		add(StoryLink{Kind: LinkTask, ID: w.TaskID, Key: w.Key, Title: w.Title,
			Change: string(w.Kind), Status: w.Status, At: w.At.UTC()})
	}
	for _, w := range wrote {
		add(StoryLink{Kind: LinkPage, ID: w.PageID, Title: w.Title,
			Change: string(w.Kind), At: w.At.UTC()})
	}
	slices.SortStableFunc(out, func(a, b StoryLink) int { return a.At.Compare(b.At) })
	return out
}
