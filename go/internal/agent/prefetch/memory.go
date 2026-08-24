package prefetch

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
)

// Personal memory: what this seat has learned and kept.
//
// # An auxiliary model filters, not cosine similarity
//
// The relationship that matters here is "rule X applies to task Y", and
// similarity misses it constantly: a memory reading "use semantic commit
// messages" and a task reading "fix the search indexing bug" share almost no
// vocabulary and are exactly relevant to each other. So the candidates are
// gathered cheaply — by similarity AND by recency — and a small model
// decides which of them bear on the task.
//
// # There is no unfiltered fallback, deliberately
//
// When the filter is unavailable or its answer is unparseable, the block is
// empty. Falling back to "the most recent eight" would leak a memory ABOUT
// ONE PERSON into a turn triggered by another — "Sam prefers short replies"
// surfacing on a turn where Miles is asking — which is the failure the
// filter exists to prevent, reappearing on exactly the transient outages
// nobody is watching for.

const (
	// MemoryCharBudget caps the rendered block.
	MemoryCharBudget = 1500

	// memoryCandidatePool caps what the filter is shown.
	//
	// The signal saturates well before this: the filter needs to
	// recognise a relevant memory, not to survey the seat's whole
	// history, and an agent whose diary has grown to hundreds of rows
	// would otherwise pay for all of them on every turn.
	memoryCandidatePool = 100

	// memoryVectorLimit and memoryRecencyLimit are the two halves of the
	// candidate pool, UNIONED.
	//
	// Similarity finds what the task is about; recency finds the
	// broadly-applicable rules that match no particular task and apply to
	// all of them. Either alone misses a whole category — 50 and 50 leave
	// the cap biting only on heavily-overlapping pools.
	memoryVectorLimit  = 50
	memoryRecencyLimit = 50

	// memoryMaxSelected caps what the filter may pick. Eight memories is
	// already a paragraph of standing instructions in front of the task.
	memoryMaxSelected = 8

	// memoryFilterTokens is headroom for the filter's answer.
	//
	// The visible output is a JSON array of at most eight integers — ten
	// tokens. The cap is two thousand because it covers THINKING as well:
	// on an extended-thinking model a tight cap is spent reasoning, the
	// call returns with output at the cap and content empty, and every
	// turn then renders an empty block with no error anywhere to say why.
	memoryFilterTokens = 2000
)

// EmptyMemoryHint is what the block says when the seat HAS memories and none
// surfaced.
//
// Without it a context-thin trigger and a brand-new agent render the same
// empty block, leaving the planner no signal that looking again would help.
// The wording points at the tool that re-runs the filter, because after
// recon the trigger is no longer thin and the answer may genuinely differ.
const EmptyMemoryHint = "(no stored memories surfaced at turn start — re-run " +
	"the filter with your memory-refresh tool once you have gathered more " +
	"context about what this task actually needs)"

// memoryFilterSystemPrompt is the filter's whole instruction.
//
// THE THREE RULES ARE THE POINT, and the distinction underneath them is what
// a memory is ABOUT. An operational memory is about a situation and tells
// the seat what to do; a per-subject memory is about a person and tells it
// how to interact with them. Subject matching applies to the second kind
// only — an operational rule may name somebody incidentally (whoever is on
// leave, whoever the routing target is) while its meaning is about the
// situation, and a filter that excluded it on a name mismatch would drop
// exactly the memories that matter most.
const memoryFilterSystemPrompt = `You are a memory-relevance filter for an AI agent.

Given the agent's current task and a numbered list of stored personal memories, return the indices of the memories genuinely relevant to this task.

Output format (strict):
- A single JSON array of integer indices, most relevant first.
- No prose before or after. Examples: [3, 0, 7] or []
- At most 8 indices.

The user prompt may include a "Current sender:" line identifying who triggered this turn. Use it when judging per-subject relevance (rule 3), but it is NOT a hard filter on its own — an operational rule that mentions a different person can still apply.

A memory is RELEVANT if ANY ONE of the following holds. Evaluate all three before deciding to exclude.

The distinction underlying these rules is what each memory is ABOUT. An operational memory is about a situation — a state, a condition, a workflow rule — and tells you what to do. A per-subject memory is about how to engage with a named person and tells you how to interact with them. The subject-match filter applies to the second kind only; an operational memory may name a person incidentally while its meaning is about the situation.

1. Operational context bearing on the task topic. Anything describing the state of the world that affects how to handle this kind of task right now — routing and coverage, someone being away, freeze windows, incident mode, deployment locks, capacity limits. May name people; the rule still applies regardless of who is asking. Subject mismatch alone is NOT grounds for exclusion here.

2. General rules, conventions or preferences for actions the task requires. Workflow-wide patterns ("use semantic commit messages", "review every backend change before merge"). No subject filter — they apply to every instance of the task type.

3. Per-subject preferences for engaging with someone the task explicitly involves. The memory describes how to interact with a named person who IS the current sender, OR is the recipient, OR is named in the task body. Subject mismatch IS a hard filter for this rule: a preference about somebody not party to the task does not apply.

EXCLUDE only when none of the three apply — a per-subject preference about somebody not party to the task, or a memory describing a past state that no longer holds.

If nothing applies, return [].`

// personalMemory renders the block.
func (f *Fetcher) personalMemory(ctx context.Context, r Request) string {
	if f.src.Diary == nil || r.AgentID == "" || strings.TrimSpace(r.Task) == "" {
		return ""
	}
	candidates := f.memoryCandidates(ctx, r)
	if len(candidates) == 0 {
		// Nothing stored. The hint would be a lie: there is no filter to
		// re-run and nothing for it to find.
		return ""
	}
	if r.RequiresRecon {
		// THE THIN-TRIGGER GATE. The task is a pointer — "PR #42 got a
		// comment" — so asking a model which memories bear on it spends
		// a call to judge relevance against a sentence with no content.
		// The hint says looking again later is worth it, which is true
		// precisely because recon will make the trigger real.
		return EmptyMemoryHint
	}
	selected := f.filterMemories(ctx, r, candidates)
	if len(selected) == 0 {
		return EmptyMemoryHint
	}
	bullets := make([]string, 0, len(selected))
	for _, entry := range selected {
		bullets = append(bullets, renderMemory(entry))
	}
	return budget(bullets, MemoryCharBudget)
}

// memoryCandidates gathers the pool the filter judges.
//
// SIMILARITY UNION RECENCY, in that order, deduplicated by id. The union is
// the point: similarity alone misses a standing rule that matches no
// particular task, and recency alone misses the one relevant memory written
// six months ago.
func (f *Fetcher) memoryCandidates(ctx context.Context, r Request) []learning.DiaryEntry {
	now := f.now()
	var (
		out  []learning.DiaryEntry
		seen = map[string]bool{}
	)
	add := func(entry learning.DiaryEntry) {
		if entry.ID == "" || seen[entry.ID] || entry.Expired(now) {
			return
		}
		seen[entry.ID] = true
		out = append(out, entry)
	}

	if vector, ok := f.embed(ctx, r.Task); ok {
		hits, err := f.src.Diary.Recall(ctx, r.AgentID, learning.RecallQuery{
			Handle: r.AgentID, Embedding: vector, Limit: memoryVectorLimit,
		}, now)
		if err != nil {
			log.Warn("memory_recall_failed", "agent_id", r.AgentID, "error", err.Error())
		}
		for _, hit := range hits {
			add(hit.Entry)
		}
	}
	recent, err := f.src.Diary.Recent(ctx, r.AgentID, now, memoryRecencyLimit)
	if err != nil {
		log.Warn("memory_recent_failed", "agent_id", r.AgentID, "error", err.Error())
	}
	for _, entry := range recent {
		add(entry)
	}
	if len(out) > memoryCandidatePool {
		out = out[:memoryCandidatePool]
	}
	return out
}

// filterMemories asks the auxiliary model which candidates bear on the task.
func (f *Fetcher) filterMemories(ctx context.Context, r Request, candidates []learning.DiaryEntry) []learning.DiaryEntry {
	answer, ok := f.auxCall(ctx, r.Seat, memoryFilterSystemPrompt,
		memoryFilterPrompt(r, candidates), memoryFilterTokens)
	if !ok {
		return nil
	}
	picked := parseIndices(answer, len(candidates))
	if len(picked) > memoryMaxSelected {
		picked = picked[:memoryMaxSelected]
	}
	out := make([]learning.DiaryEntry, 0, len(picked))
	for _, i := range picked {
		out = append(out, candidates[i])
	}
	return out
}

// memoryFilterPrompt renders the task, the sender and the numbered pool.
func memoryFilterPrompt(r Request, candidates []learning.DiaryEntry) string {
	var b strings.Builder
	b.WriteString("Agent's current task:\n" + truncate(r.Task, 1200) + "\n")
	// THE SENDER, STRUCTURED. Without it the filter has only whatever
	// platform ids appear in the task body to reason from, and rule 3 —
	// the one hard subject filter — becomes unenforceable: it cannot tell
	// a preference about the person asking from a preference about
	// somebody else entirely.
	if line := senderLine(r.Senders); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString("\nStored memories:\n")
	for i, entry := range candidates {
		b.WriteString(strconv.Itoa(i) + ". " + memoryKindLabel(entry) +
			truncate(collapse(entry.Content), 400) + "\n")
	}
	b.WriteString("\nRelevant indices (JSON array):")
	return b.String()
}

// senderLine names who triggered the turn, for the filter's rule 3.
func senderLine(senders []learning.Subject) string {
	labels := make([]string, 0, len(senders))
	for _, s := range senders {
		if label := subjectLabel(s); label != "" && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return ""
	}
	return "Current sender: " + strings.Join(labels, ", ")
}

// memoryKindLabel marks a short-lived memory as such.
//
// The filter is told the memory has an expiry because it changes what the
// memory MEANS: "we are in a deploy freeze" read as a standing rule is a
// company that never ships again.
func memoryKindLabel(entry learning.DiaryEntry) string {
	if entry.Kind == learning.DiaryShort {
		return "[short-lived] "
	}
	return ""
}

// renderMemory renders one selected memory as a bullet.
func renderMemory(entry learning.DiaryEntry) string {
	content := collapse(entry.Content)
	if content == "" {
		return ""
	}
	if entry.Kind == learning.DiaryShort && !entry.TTLUntil.IsZero() {
		// The EXPIRY, rendered. A short memory read as permanent is
		// worse than not having it: an agent that keeps honouring last
		// quarter's freeze window is confidently wrong.
		return "- " + content + " _(short-lived; expires " +
			entry.TTLUntil.UTC().Format(time.DateOnly) + ")_"
	}
	return "- " + content
}

// parseIndices reads the filter's JSON array, tolerating a chatty model.
//
// PERMISSIVE about the wrapper and STRICT about the contents: a model that
// wraps its array in a code fence or a sentence has still answered, while
// one that returns an index outside the candidate list has not — and acting
// on that index would surface an unrelated memory or panic, both of which
// are worse than an empty block.
func parseIndices(text string, pool int) []int {
	array := jsonArray(text)
	if array == "" {
		log.Warn("memory_filter_unparseable", "answer", truncate(text, 200))
		return nil
	}
	var raw []int
	if err := json.Unmarshal([]byte(array), &raw); err != nil {
		log.Warn("memory_filter_unparseable", "answer", truncate(text, 200),
			"error", err.Error())
		return nil
	}
	var (
		out  []int
		seen = map[int]bool{}
	)
	for _, i := range raw {
		if i < 0 || i >= pool || seen[i] {
			continue
		}
		seen[i] = true
		out = append(out, i)
	}
	return out
}

// jsonArray finds the outermost bracketed array in a model's answer.
func jsonArray(text string) string {
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start < 0 || end < start {
		return ""
	}
	return text[start : end+1]
}

// embed turns text into a vector, or reports that it cannot.
func (f *Fetcher) embed(ctx context.Context, text string) ([]float32, bool) {
	if f.src.Embed == nil {
		return nil, false
	}
	vector, err := f.src.Embed(ctx, text)
	if err != nil || len(vector) == 0 {
		if err != nil {
			log.Warn("prefetch_embedding_failed", "error", err.Error())
		}
		return nil, false
	}
	return vector, true
}

// subjectLabel renders a counterparty as the filter and the profile block
// name them.
func subjectLabel(s learning.Subject) string {
	if name := strings.TrimSpace(s.Name); name != "" {
		if handle := strings.TrimSpace(s.Handle); handle != "" {
			return fmt.Sprintf("%s (%s)", name, handle)
		}
		return name
	}
	if handle := strings.TrimSpace(s.Handle); handle != "" {
		return handle
	}
	return strings.TrimSpace(s.ExternalID)
}

// collapse folds a memory's internal newlines so one entry stays one bullet.
func collapse(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
