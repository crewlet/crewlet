package builtin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
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
	//
	// WRITE TAKES THE NOTE AND NOTHING ELSE. The vector a later recall
	// ranks on is made by the store as it writes (see
	// [learning.DiaryEntry.Embedding]); a tool that embedded its own note
	// would be a second answer to where a diary row's vector comes from,
	// free to differ from the post-turn writer's — and what it costs when
	// they differ is a note that is stored and never recalled.
	DiaryStore interface {
		Write(ctx context.Context, e learning.DiaryEntry) error
		Recent(ctx context.Context, agentID string, now time.Time, limit int) ([]learning.DiaryEntry, error)
	}

	// OnboardingStore records that a seat has finished its first-run pass.
	OnboardingStore interface {
		Mark(ctx context.Context, m learning.Marker, at time.Time) error
	}
)

// noteLimit caps what refresh_memory puts in front of a model when the model
// names no limit.
//
// A note goes into a prompt, and a dozen half-relevant ones crowd out the task
// they were fetched for. Five is what a model reads; more is what it skims.
// Distinct from an episode's limit — that one is the company's
// learning.episodic.retrieval_limit, and a diary is not an episode history.
const noteLimit = 5

// maxEpisodeLimit bounds what a model may ask for. A tool that honoured
// "limit: 500" would let one call spend a phase's whole context on history.
const maxEpisodeLimit = 25

// diaryNoteMax bounds one written note, in BYTES.
//
// The store's own rule, not a second opinion about it: [learning.MaxContentBytes]
// is where it is stated, because the post-turn PersistDecider writes into the
// same table and the two used to disagree — this path refused an over-long note
// while that one stored it whole.
//
// BYTES is not what the name says and is what every guard on it counts: each
// one is len() over a Go string. So the refusals below say bytes, because a
// model told it wrote 3 000 characters of CJK when it wrote 1 000 has been
// handed a number it cannot reproduce by counting what it typed, and the
// refusal's whole job is to let it aim.
const diaryNoteMax = learning.MaxContentBytes

// --- use_skill ------------------------------------------------------------ //

type useSkill struct {
	skills SkillStore
	events Telemetry
}

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
	// ONE EVENT PER LOAD, which is the measurement skill induction has to
	// pass to be worth its cost: without it, "are the skills the
	// synthesizer drafts ever loaded again" is answerable only by diffing
	// a database column. Distinct from the per-OFFER stamp
	// internal/learning deliberately keeps silent — that one fires for
	// every skill the prompt merely listed.
	note(ctx, t.events, turn, skillUsed(turn, sk.Name, sk.ID, "",
		types.SkillSourceSynthesized))

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
	// EVERY skill. This message exists so the model can retry with a name
	// that works, and the name it wants is as likely to sort past a cap as
	// before it — a truncated list of options is a list that answers a
	// different question than the one the model asked.
	names := make([]string, 0, len(have))
	for _, s := range have {
		names = append(names, s.Name)
	}
	return fmt.Sprintf("You have no synthesized skill called %q. Yours are: %s.",
		clip(name), strings.Join(names, ", "))
}

// --- query_episodes ------------------------------------------------------- //

type queryEpisodes struct {
	episodes EpisodeStore

	// recall is the turn-start prefetch's own similarity search, re-run on
	// demand.
	// Nil leaves the tool on the recency and conversation paths, which is
	// what a company with no embeddings has.
	recall Recaller

	// limit is the company's configured default hit count. Bounded by
	// maxEpisodeLimit whatever it says, because the ceiling is about what
	// fits in a prompt rather than what an operator wants.
	limit int
}

var _ tools.SeatCallable = (*queryEpisodes)(nil)

func (t *queryEpisodes) Name() string { return QueryEpisodesTool }

func (t *queryEpisodes) Description() string {
	return "Recall your own past turns — what you were asked, what you " +
		"did, how it went. Pass `query` to search by MEANING once you know " +
		"what this task actually involves; that is the one to use after " +
		"recon on a thin trigger, when the block at the top of your prompt " +
		"said it found nothing. Pass `conversation` to narrow to one thread, " +
		"issue or pull request instead. With neither, you get your most " +
		"recent turns."
}

func (t *queryEpisodes) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type": "string",
				"description": "Optional: what this task is about, in your " +
					"own words. Searches by meaning rather than by recency",
			},
			"conversation": map[string]any{
				"type": "string",
				"description": "Optional: narrow to one conversation " +
					"(a thread, issue or PR key)",
			},
			"outcome_filter": map[string]any{
				"type": "string",
				"description": "Optional: keep only turns that ended this " +
					"way — done or failed. Those are the only two a turn " +
					"is remembered for: a turn that looped back on itself " +
					"is remembered as its own reattempt, and one where " +
					"nobody was asking writes no episode at all",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many turns to recall (default %d, max %d)",
					t.defaultLimit(), maxEpisodeLimit),
			},
		},
	}
}

// defaultLimit is the company's retrieval_limit, clamped to what a prompt can
// carry. A registry built without one falls back to the shipped default rather
// than to zero, which would be a tool that returns nothing.
func (t *queryEpisodes) defaultLimit() int {
	return clampInt(orDefault(t.limit, DefaultEpisodeLimit), 1, maxEpisodeLimit)
}

// similar runs the turn-start prefetch's own vector recall, or explains why it
// cannot.
//
// The REFUSAL is a message rather than an empty answer: "nothing resembles
// this" and "this deployment cannot search by meaning" send a model to
// opposite places, and the second one has a fallback it can still use.
func (t *queryEpisodes) similar(ctx context.Context, turn *turnctx.Turn, query string, limit int) ([]learning.Episode, error) {
	if t.recall == nil {
		return nil, errNoSimilarity
	}
	hits, err := t.recall.RecallEpisodes(ctx, turn.Seat, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]learning.Episode, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.Episode)
	}
	return out, nil
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
	limit := clampInt(argInt(args, "limit", t.defaultLimit()), 1, maxEpisodeLimit)
	outcome := strings.TrimSpace(argString(args, "outcome_filter"))

	var (
		found []learning.Episode
		err   error
		scope string
	)
	switch query := strings.TrimSpace(argString(args, "query")); {
	case query != "":
		// OVER-FETCHED when an outcome filter is set, because the search
		// ranks by similarity and the filter is applied to what came
		// back: asking for five and keeping only the failures would
		// otherwise return one. Bounded, so a filter that matches
		// nothing costs one wider search rather than the whole history.
		want := limit
		if outcome != "" {
			want = clampInt(limit*outcomeOverfetch, 1, maxEpisodeLimit)
		}
		found, err = t.similar(ctx, turn, query, want)
		scope = fmt.Sprintf(" like %s", clip(query))
	case argString(args, "conversation") != "":
		conversation := strings.TrimSpace(argString(args, "conversation"))
		found, err = t.episodes.ForConversation(ctx, handle, conversation, limit)
		scope = fmt.Sprintf(" in %s", clip(conversation))
	default:
		found, err = t.episodes.Recent(ctx, handle, limit)
	}
	if err != nil {
		return failed(fmt.Sprintf("Could not recall your turns: %v", err)), nil
	}
	if outcome != "" {
		found = keepOutcome(found, outcome, limit)
		scope += fmt.Sprintf(" that ended %s", clip(outcome))
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

type refreshMemory struct {
	diary  DiaryStore
	recall Recaller

	// maxHints is learning.personal_memory.max_refreshes_per_turn: how many
	// DISTINCT hints one turn may filter on. Zero takes
	// [DefaultRefreshesPerTurn].
	//
	// The cap exists because the filter is an auxiliary model call, so a
	// model that re-hints on every round spends a completion per round for
	// answers that converge after the second. Repeats of a hint it has
	// already used are free — see [hintLedger].
	maxHints int

	// hints remembers which hints each turn has already filtered on. It is
	// per TURN and bounded; see [hintLedger] for why it lives on the tool
	// rather than on the turn.
	hints hintLedger
}

var _ tools.SeatCallable = (*refreshMemory)(nil)

func (t *refreshMemory) Name() string { return RefreshMemoryTool }

func (t *refreshMemory) Description() string {
	return "Re-read your own durable notes — the facts you chose to keep " +
		"across turns. Pass `context_hint` describing what this task is " +
		"actually about, and the notes are re-filtered for relevance to it; " +
		"that is the one to use after recon on a thin trigger, when the " +
		"block at the top of your prompt said it found nothing. Without a " +
		"hint you get your most recent notes."
}

func (t *refreshMemory) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"context_hint": map[string]any{
				"type": "string",
				"description": "Optional: what this task is about, in your " +
					"own words. Re-filters your notes for relevance to it",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("How many notes (default %d, max %d)", noteLimit, maxEpisodeLimit),
			},
			"offset": map[string]any{
				"type": "integer",
				"description": "How many to skip: of the notes that bear on " +
					"`context_hint`, or of your notes newest first when there " +
					"is no hint. An answer that did not list them all names " +
					"the offset to pass for the rest.",
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
	limit := clampInt(argInt(args, "limit", noteLimit), 1, maxEpisodeLimit)
	offset := max(argInt(args, "offset", 0), 0)

	if hint := strings.TrimSpace(argString(args, "context_hint")); hint != "" {
		return t.filtered(ctx, turn, agentID, hint, limit, offset)
	}
	return t.recent(ctx, agentID, limit, offset)
}

// recent prints one page of the seat's notes, newest first: `limit` of them,
// starting `offset` in.
//
// A PAGE THAT STOPS SHORT OF THE OLDEST NOTE SAYS SO, and names the offset
// that reads on. A seat holding more notes than one page would otherwise read
// the page as everything it has kept.
//
// The store reads a seat's NEWEST n and takes no offset, so a page is read as
// the offset+limit newest plus ONE MORE — an evidence row, never printed,
// whose presence is what says older notes exist. What that reads is bounded by
// what the seat holds, whatever the offset. Positions count from the newest
// note at the time of each call, so a note kept or expired between two calls
// moves the next page by one.
func (t *refreshMemory) recent(ctx context.Context, agentID string, limit, offset int) (tools.Result, error) {
	want := offset + limit + 1
	if want < offset {
		// The sum overflowed. An offset that large is past the end of any
		// diary, and reading everything answers it as any such offset is
		// answered: with how many notes there are.
		want = math.MaxInt
	}
	entries, err := t.diary.Recent(ctx, agentID, time.Now().UTC(), want)
	if err != nil {
		return failed(fmt.Sprintf("Could not read your notes: %v", err)), nil
	}
	held := len(entries)
	switch {
	case held == 0:
		return tools.Result{Output: "You have no durable notes yet."}, nil
	case offset >= held:
		// Fewer rows came back than were asked for, so held is every note.
		return tools.Result{Output: fmt.Sprintf(
			"You have %d notes, so there are none from offset %d on.", held, offset)}, nil
	}
	end := min(offset+limit, held)
	older := held > end
	var b strings.Builder
	switch {
	case !older && offset == 0:
		fmt.Fprintf(&b, "All %d of your notes, newest first:\n\n", held)
	case !older:
		fmt.Fprintf(&b, "Your notes, newest first, %d to %d of %d:\n\n", offset+1, end, held)
	default:
		fmt.Fprintf(&b, "Your notes, newest first, %d to %d:\n\n", offset+1, end)
	}
	for _, e := range entries[offset:end] {
		fmt.Fprintf(&b, "- [%s] %s\n", e.Kind, e.Content)
	}
	if older {
		fmt.Fprintf(&b, "\nYou have older notes than these. Call %s again "+
			"with offset %d to read them.", RefreshMemoryTool, end)
	}
	return tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// filtered re-runs the personal-memory relevance filter against a hint.
//
// THE SAME FILTER the turn-start prefetch ran, which is why this is a pull rather than
// a second implementation: the block at the top of the prompt and this tool
// have to agree about what is relevant, and two answers to that question drift
// in the direction nobody looks.
func (t *refreshMemory) filtered(ctx context.Context, turn *turnctx.Turn,
	agentID, hint string, limit, offset int,
) (tools.Result, error) {
	if t.recall == nil {
		return failed("This deployment cannot re-filter your notes by " +
			"relevance — call refresh_memory without `context_hint` for your " +
			"most recent ones instead."), nil
	}
	take := t.hints.take(turn.RunID, hint, t.hintBudget())
	switch {
	case take.Hit:
		// ANSWERED FROM THE LEDGER, which is what makes a repeat free
		// rather than merely uncharged — see [hintLedger]. Re-rendered
		// rather than replayed, so a repeat asking for more notes than
		// the first call printed, or for the ones past them, gets them.
		return renderHintedNotes(take.Cached, hint, limit, offset), nil
	case !take.Allowed:
		// REFUSED with the count, so the model learns the shape of the
		// limit rather than that the tool became unreliable.
		return failed(fmt.Sprintf(
			"You have already re-filtered your notes on %d different hints this "+
				"turn, which is this company's limit. Re-use one of those hints "+
				"(that is free), or call refresh_memory without one.",
			take.Spent)), nil
	}

	entries, err := t.recall.RecallMemories(ctx, turn.Seat, agentID, hint)
	if err != nil {
		return failed(fmt.Sprintf("Could not re-filter your notes: %v", err)), nil
	}
	// Kept even when the filter found nothing: "nothing bears on this" is
	// an answer, and a repeat of the hint would otherwise cost another
	// completion to be told it again.
	t.hints.keep(turn.RunID, hint, entries)
	return renderHintedNotes(entries, hint, limit, offset), nil
}

// renderHintedNotes prints one page of a hint's filtered rows, in the order
// the filter ranked them: `limit` of them, starting `offset` in.
//
// A PAGE THAT IS NOT THE WHOLE ANSWER SAYS SO, with the count left and the
// offset that reads it. The heading claims these are the notes that bear on
// the hint, so a silent page reads as the whole set — a short answer and a
// short set would be the same text. The rest is reachable because the rows
// are the ledger's: a repeat of the hint is answered from [hintLedger] rather
// than re-filtered, so the next offset is into the same ranked answer.
//
// An empty result is NOT a fallback to recency. "The most recent eight" would
// put a note about one person in front of a turn about another, which is the
// failure the filter exists to prevent.
func renderHintedNotes(entries []learning.DiaryEntry, hint string, limit, offset int) tools.Result {
	total := len(entries)
	if total == 0 {
		return tools.Result{Output: fmt.Sprintf(
			"Nothing in your notes bears on %s.", clip(hint))}
	}
	if offset >= total {
		return tools.Result{Output: fmt.Sprintf(
			"%d of your notes bear on %s, so there are none from offset %d on.",
			total, clip(hint), offset)}
	}
	end := min(offset+limit, total)
	var b strings.Builder
	if offset == 0 && end == total {
		fmt.Fprintf(&b, "Your notes that bear on %s:\n\n", clip(hint))
	} else {
		fmt.Fprintf(&b, "Your notes that bear on %s, %d to %d of %d:\n\n",
			clip(hint), offset+1, end, total)
	}
	for _, e := range entries[offset:end] {
		fmt.Fprintf(&b, "- [%s] %s\n", e.Kind, e.Content)
	}
	if rest := total - end; rest > 0 {
		fmt.Fprintf(&b, "\n%d more bear on it. Call %s again with this "+
			"context_hint and offset %d to read them — repeating a hint costs "+
			"nothing.", rest, RefreshMemoryTool, end)
	}
	return tools.Result{Output: strings.TrimRight(b.String(), "\n")}
}

// hintBudget is the company's max_refreshes_per_turn, or the shipped default.
func (t *refreshMemory) hintBudget() int {
	return orDefault(t.maxHints, DefaultRefreshesPerTurn)
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
			"That note is %d bytes and the limit is %d. A note is re-read "+
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
		TurnID: turn.RunID, CreatedAt: time.Now().UTC(),
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
				"type": "string",
				"description": fmt.Sprintf(
					"Optional: what you learned while orienting. At most %d "+
						"bytes — longer is refused, not shortened.", diaryNoteMax),
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
	// REFUSED, not clipped — the same bound reflect_and_persist enforces on
	// the same store, by the same rule. This used to clip silently, so the
	// two writers into the diary disagreed about what happens to an
	// over-long note: one told the model to tighten it, the other stored
	// half of it and reported success.
	notes := strings.TrimSpace(argString(args, "notes"))
	if len(notes) > diaryNoteMax {
		return failed(fmt.Sprintf(
			"Those notes are %d bytes and a diary note is capped at %d. "+
				"Tighten them — what you keep is what you will read back.",
			len(notes), diaryNoteMax)), nil
	}
	err := t.onboarding.Mark(ctx, learning.Marker{
		AgentID: agentID, Handle: turn.Handle(), Role: turn.Role(),
		// The chain hash is what makes the marker specific to THIS
		// org shape: a company whose management chain changed is a
		// different orientation, and a marker that ignored it would
		// leave a seat permanently un-onboarded to its new context.
		ChainHash: learning.ChainHash(turn.Org, turn.Seat),
		Summary:   notes,
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

// clampInt bounds a model-supplied count into what a prompt can carry.
//
// The floor is 1 at every call site and stays a parameter rather than a
// constant, because the two bounds mean different things: the ceiling is about
// prompt weight and the floor is about a tool that returns nothing, and
// folding one into the function hides which of the two a caller is asking for.
func clampInt(v, lo, hi int) int { //nolint:unparam // see the doc comment
	return min(max(v, lo), hi)
}

// outcomeOverfetch widens a similarity search when an outcome filter is set.
//
// Four: the filter is applied to what the search returned, so asking for five
// and keeping only the failures would routinely return one. Wide enough that a
// filter matching a quarter of a seat's turns still fills the answer, bounded
// so one that matches none costs a single wider search rather than a scan.
const outcomeOverfetch = 4

// errNoSimilarity is what a deployment with no embeddings answers a `query`
// with. Its own sentinel so the tool can say which of two very different
// things happened.
var errNoSimilarity = errors.New("no embeddings are configured on this deployment")

// keepOutcome filters recalled turns by how they ended, preserving order.
//
// Case-insensitive on the operator's side of the comparison, because the
// outcomes are a closed set the model is told about in the parameter
// description and a model that capitalised one should not silently get an
// empty answer.
func keepOutcome(found []learning.Episode, outcome string, limit int) []learning.Episode {
	want := strings.ToLower(strings.TrimSpace(outcome))
	out := make([]learning.Episode, 0, min(len(found), limit))
	for _, ep := range found {
		if strings.ToLower(ep.ReviewOutcome) != want {
			continue
		}
		out = append(out, ep)
		if len(out) == limit {
			break
		}
	}
	return out
}
