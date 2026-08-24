package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// The memory tools: what an agent can do with what it has already learned.
//
// All four read or write THIS seat's own memory, and none of them takes a
// handle. That is the point: an agent recalling another's episodes or writing
// into another's diary would make the per-seat memory a shared one, and the
// whole design of the learning subsystem — a diary keyed on the DERIVED agent
// id so a renamed handle orphans its rows rather than inheriting somebody
// else's — rests on the boundary holding.

// The tool wire names.
const (
	UseSkillTool          = "use_skill"
	QueryEpisodesTool     = "query_episodes"
	RefreshMemoryTool     = "refresh_memory"
	ReflectAndPersistTool = "reflect_and_persist"
	MarkOnboardedTool     = "mark_onboarded"
)

// Memory is what the memory tools need from the learning subsystem.
//
// Interfaces rather than the concrete stores, so a tool can be exercised
// without a database and so this package does not become a second place that
// knows how a skill is loaded.
type (
	// SkillStore is the synthesized-skill half.
	SkillStore interface {
		Get(ctx context.Context, handle, name string) (learning.Skill, bool, error)
		List(ctx context.Context, handle string, opts learning.ListOptions) ([]learning.Skill, error)
		MarkUsed(ctx context.Context, skillID string, at time.Time) learning.Use
	}

	// EpisodeStore is the per-turn memory half.
	EpisodeStore interface {
		Recent(ctx context.Context, handle string, limit int) ([]learning.Episode, error)
		ForConversation(ctx context.Context, handle, conversation string, limit int) ([]learning.Episode, error)
	}

	// DiaryStore is the durable-notes half.
	DiaryStore interface {
		Write(ctx context.Context, e learning.DiaryEntry) error
		Recent(ctx context.Context, agentID string, now time.Time, limit int) ([]learning.DiaryEntry, error)
	}

	// OnboardingStore records that a seat has finished its first-run pass.
	OnboardingStore interface {
		Mark(ctx context.Context, m learning.Marker, at time.Time) error
	}
)

// episodeLimit caps what a recall puts in front of a model.
//
// Recall goes into a prompt, and a dozen half-relevant memories crowd out the
// task they were fetched for — the same reasoning the store's own default
// encodes. Five is what a model reads; more is what it skims.
const episodeLimit = 5

// maxEpisodeLimit bounds what a model may ask for. A tool that honoured
// "limit: 500" would let one call spend a phase's whole context on history.
const maxEpisodeLimit = 25

// diaryNoteMax bounds one written note.
//
// A note is meant to be re-read in a later turn's prompt, so its cost is paid
// on every turn that recalls it, not once. Two thousand characters is a long
// paragraph and a short page — past that the model is writing a document, and
// a document belongs in the knowledge base where colleagues can read it too.
const diaryNoteMax = 2000

// --- use_skill ------------------------------------------------------------ //

type useSkill struct{ skills SkillStore }

var _ tools.SeatCallable = (*useSkill)(nil)

func (t *useSkill) Name() string { return UseSkillTool }

func (t *useSkill) Description() string {
	return "Load one of your own synthesized skills — the ones listed in " +
		"the `Synthesized skills you've learned` block of your prompt. " +
		"Returns the skill's full procedure. For a procedure the TEAM " +
		"published, search the knowledge base instead: those are shared " +
		"docs, not skills you distilled from your own turns."
}

func (t *useSkill) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill_name": map[string]any{
				"type":        "string",
				"description": "Exact name of one of your synthesized skills",
			},
		},
		"required": []any{"skill_name"},
	}
}

func (t *useSkill) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *useSkill) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	handle := turn.Handle()
	if handle == "" {
		return failed("use_skill can only be called during a turn, on behalf of a seat."), nil
	}
	if t.skills == nil {
		return failed("Skill synthesis is not configured on this deployment."), nil
	}
	name := strings.TrimSpace(argString(args, "skill_name"))
	if name == "" {
		return failed("use_skill needs a `skill_name`."), nil
	}

	// Keyed on THIS seat's handle, so a model naming another agent's skill
	// gets "you have no skill called that" rather than that agent's skill.
	sk, found, err := t.skills.Get(ctx, handle, name)
	if err != nil {
		return failed(fmt.Sprintf("Could not load %q: %v", clip(name), err)), nil
	}
	if !found {
		return failed(t.suggest(ctx, handle, name)), nil
	}

	// Recorded BEFORE the content goes out, and its failure ignored: the
	// telemetry is what answers "do agents actually load the skills the
	// synthesizer drafts", and a write that failed must not cost the agent
	// the skill it asked for.
	t.skills.MarkUsed(ctx, sk.ID, time.Now().UTC())

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", sk.Name)
	if sk.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", sk.Description)
	}
	b.WriteString(sk.Content)
	return tools.Result{Output: b.String()}, nil
}

// suggest turns "not found" into something the model can act on.
func (t *useSkill) suggest(ctx context.Context, handle, name string) string {
	have, err := t.skills.List(ctx, handle, learning.ListOptions{ExcludeStale: true})
	if err != nil || len(have) == 0 {
		return fmt.Sprintf("You have no synthesized skill called %q, and none at all yet. "+
			"Skills are distilled from your own completed turns.", clip(name))
	}
	names := make([]string, 0, len(have))
	for _, s := range have {
		names = append(names, s.Name)
		if len(names) == displayLimit {
			break
		}
	}
	return fmt.Sprintf("You have no synthesized skill called %q. Yours are: %s.",
		clip(name), strings.Join(names, ", "))
}

// --- query_episodes ------------------------------------------------------- //

type queryEpisodes struct{ episodes EpisodeStore }

var _ tools.SeatCallable = (*queryEpisodes)(nil)

func (t *queryEpisodes) Name() string { return QueryEpisodesTool }

func (t *queryEpisodes) Description() string {
	return "Recall your own recent turns — what you were asked, what you " +
		"did, how it went. Use it when the current task resembles work you " +
		"have done before, or to check what you already told someone. Pass " +
		"`conversation` to narrow it to one thread, issue or pull request."
}

func (t *queryEpisodes) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"conversation": map[string]any{
				"type": "string",
				"description": "Optional: narrow to one conversation " +
					"(a thread, issue or PR key)",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many turns to recall (default %d, max %d)",
					episodeLimit, maxEpisodeLimit),
			},
		},
	}
}

func (t *queryEpisodes) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *queryEpisodes) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	handle := turn.Handle()
	if handle == "" {
		return failed("query_episodes can only be called during a turn, on behalf of a seat."), nil
	}
	if t.episodes == nil {
		return failed("Episode memory is not configured on this deployment."), nil
	}
	limit := clampInt(argInt(args, "limit", episodeLimit), 1, maxEpisodeLimit)

	var (
		found []learning.Episode
		err   error
		scope string
	)
	if conversation := strings.TrimSpace(argString(args, "conversation")); conversation != "" {
		found, err = t.episodes.ForConversation(ctx, handle, conversation, limit)
		scope = fmt.Sprintf(" in %s", clip(conversation))
	} else {
		found, err = t.episodes.Recent(ctx, handle, limit)
	}
	if err != nil {
		return failed(fmt.Sprintf("Could not recall your turns: %v", err)), nil
	}
	if len(found) == 0 {
		return tools.Result{Output: fmt.Sprintf(
			"No earlier turns of yours%s. This is new work.", scope)}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Your %d most recent turns%s:\n\n", len(found), scope)
	for _, ep := range found {
		fmt.Fprintf(&b, "- %s", ep.StartedAt.Format(time.RFC3339))
		if ep.TaskSummary != "" {
			fmt.Fprintf(&b, " — %s", ep.TaskSummary)
		}
		b.WriteString("\n")
		if ep.ReviewOutcome != "" {
			fmt.Fprintf(&b, "    outcome: %s\n", ep.ReviewOutcome)
		}
	}
	return tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// --- refresh_memory ------------------------------------------------------- //

type refreshMemory struct{ diary DiaryStore }

var _ tools.SeatCallable = (*refreshMemory)(nil)

func (t *refreshMemory) Name() string { return RefreshMemoryTool }

func (t *refreshMemory) Description() string {
	return "Re-read your own durable notes — the facts you chose to keep " +
		"across turns. Use it when you need something you learned earlier " +
		"and it is not in front of you."
}

func (t *refreshMemory) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("How many notes (default %d, max %d)", episodeLimit, maxEpisodeLimit),
			},
		},
	}
}

func (t *refreshMemory) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *refreshMemory) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	agentID, why := seatAgentID(turn)
	if why != "" {
		return failed("refresh_memory " + why), nil
	}
	if t.diary == nil {
		return failed("Durable memory is not configured on this deployment."), nil
	}
	limit := clampInt(argInt(args, "limit", episodeLimit), 1, maxEpisodeLimit)

	entries, err := t.diary.Recent(ctx, agentID, time.Now().UTC(), limit)
	if err != nil {
		return failed(fmt.Sprintf("Could not read your notes: %v", err)), nil
	}
	if len(entries) == 0 {
		return tools.Result{Output: "You have no durable notes yet."}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your %d most recent notes:\n\n", len(entries))
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s] %s\n", e.Kind, e.Content)
	}
	return tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// --- reflect_and_persist -------------------------------------------------- //

type reflectAndPersist struct{ diary DiaryStore }

var _ tools.SeatCallable = (*reflectAndPersist)(nil)

func (t *reflectAndPersist) Name() string { return ReflectAndPersistTool }

func (t *reflectAndPersist) Description() string {
	return "Keep something you learned, so a later turn of yours can read " +
		"it. Use it for a durable FACT about how this company works — a " +
		"convention, a person's preference, where a thing lives — not for " +
		"what you did this turn, which is recorded for you. Keep it short: " +
		"you pay for it in every turn that recalls it."
}

func (t *reflectAndPersist) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The fact to keep, in one or two sentences",
			},
			"kind": map[string]any{
				"type": "string",
				"enum": []any{string(learning.DiaryLong), string(learning.DiaryShort)},
				"description": "`long` for something durable, `short` for " +
					"something that expires. Defaults to long.",
			},
		},
		"required": []any{"content"},
	}
}

func (t *reflectAndPersist) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *reflectAndPersist) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	agentID, why := seatAgentID(turn)
	if why != "" {
		return failed("reflect_and_persist " + why), nil
	}
	if t.diary == nil {
		return failed("Durable memory is not configured on this deployment."), nil
	}
	content := strings.TrimSpace(argString(args, "content"))
	switch {
	case content == "":
		return failed("reflect_and_persist needs `content`: the fact to keep."), nil
	case len(content) > diaryNoteMax:
		return failed(fmt.Sprintf(
			"That note is %d characters and the limit is %d. A note is re-read "+
				"in later prompts, so its cost is paid every time — keep the "+
				"fact and drop the narrative, or publish the long version to "+
				"the knowledge base where colleagues can read it too.",
			len(content), diaryNoteMax)), nil
	}

	kind := learning.DiaryKind(strings.TrimSpace(argString(args, "kind")))
	if kind != learning.DiaryShort {
		kind = learning.DiaryLong
	}
	entry := learning.DiaryEntry{
		ID: uuid.NewString(), AgentID: agentID, Kind: kind, Content: content,
		// Attributed to the tool, not to the reflection worker: an
		// operator reading the diary has to be able to tell what the
		// agent chose to keep from what a worker decided for it.
		Source: "tool:" + ReflectAndPersistTool,
		TurnID: turn.ID, CreatedAt: time.Now().UTC(),
	}
	if err := t.diary.Write(ctx, entry); err != nil {
		return failed(fmt.Sprintf("Could not keep that note: %v", err)), nil
	}
	return tools.Result{Output: "Kept. You will see it in later turns."}, nil
}

// --- mark_onboarded ------------------------------------------------------- //

type markOnboarded struct{ onboarding OnboardingStore }

var _ tools.SeatCallable = (*markOnboarded)(nil)

func (t *markOnboarded) Name() string { return MarkOnboardedTool }

func (t *markOnboarded) Description() string {
	return "Record that you have finished orienting yourself — you have " +
		"read what you needed and know how this company works. Call it " +
		"once, when the onboarding block stops being useful to you. It " +
		"will not appear again."
}

func (t *markOnboarded) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"notes": map[string]any{
				"type":        "string",
				"description": "Optional: what you learned while orienting",
			},
		},
	}
}

func (t *markOnboarded) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *markOnboarded) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	agentID, why := seatAgentID(turn)
	if why != "" {
		return failed("mark_onboarded " + why), nil
	}
	if t.onboarding == nil {
		return failed("Onboarding is not configured on this deployment."), nil
	}
	err := t.onboarding.Mark(ctx, learning.Marker{
		AgentID: agentID, Handle: turn.Handle(), Role: turn.Role(),
		// The chain hash is what makes the marker specific to THIS
		// org shape: a company whose management chain changed is a
		// different orientation, and a marker that ignored it would
		// leave a seat permanently un-onboarded to its new context.
		ChainHash: learning.ChainHash(turn.Org, turn.Seat),
		Summary:   clipTo(argString(args, "notes"), diaryNoteMax),
	}, time.Now().UTC())
	if err != nil {
		return failed(fmt.Sprintf("Could not record that: %v", err)), nil
	}
	return tools.Result{Output: "Recorded. The onboarding block will not appear again."}, nil
}

// seatAgentID resolves the DERIVED agent id for the acting seat.
//
// The derived id, never the handle: the diary keys on it so that renaming a
// handle cleanly ORPHANS the old rows rather than handing one seat's memory to
// whoever takes the name next.
func seatAgentID(turn *turnctx.Turn) (string, string) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return "", "can only be called during a turn, on behalf of a seat."
	}
	if turn.Org == nil {
		return "", "has no organization in scope."
	}
	id, ok := turn.Org.AgentIDFor(seat)
	if !ok {
		return "", fmt.Sprintf("is not available to %s, which is a %s seat.",
			seat.Handle(), org.KindHuman)
	}
	return id.String(), ""
}

func clampInt(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

func clipTo(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
