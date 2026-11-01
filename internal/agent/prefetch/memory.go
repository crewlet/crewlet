package prefetch

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/provenance"
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
	// memoryVectorLimit and memoryRecencyLimit are the two halves of the
	// candidate pool, UNIONED. Their sum is what bounds the pool — there is
	// no third cap over the union, and the one that used to sit there was
	// unreachable arithmetic: a dedup of two 50-row halves cannot exceed
	// 100, so a `> 100` trim never fired.
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

	// memoryMarkBudget bounds the detached write that records which
	// memories the filter picked.
	//
	// ONE indexed UPDATE over at most memoryMaxSelected ids against a
	// local file, so the work is sub-millisecond and this is a guard
	// against a wedged store rather than a real allowance. It has to be
	// finite because the write deliberately outlives the turn's context:
	// an unbounded detached write on a store that never answers would
	// park a goroutine per turn for the life of the process.
	memoryMarkBudget = 5 * time.Second
)

// EmptyMemoryHint is what the block says when the seat HAS memories and none
// surfaced.
//
// Without it a context-thin trigger and a brand-new agent render the same
// empty block, leaving the executor no signal that looking again would help.
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
// exactly the memories that matter most. The prompt states that once, in
// rules 1 and 3, rather than as a preamble the rules then repeat.
//
// "Most relevant first" is the half of the cap the model holds:
// [memoryMaxSelected] keeps the head of the array, so the order decides what
// survives it, and the number itself is the code's to enforce.
const memoryFilterSystemPrompt = `You are a memory-relevance filter for an AI agent.

Given the agent's current task and a numbered list of its stored personal memories, answer with a JSON array of the integer indices of the memories relevant to the task, most relevant first, and nothing else; [] if none is.

A memory is relevant if any one of these holds:

1. It is operational context bearing on the task's topic: the state of the world that affects how to handle this kind of task now (routing and coverage, someone away, a freeze window, incident mode, a deployment lock, a capacity limit). It applies whoever is asking, even when it names somebody else.

2. It is a general rule, convention or preference for an action the task requires. It applies to every task of that kind.

3. It is a preference for engaging with a person the task involves: the person on the "Current sender:" line, the recipient, or someone named in the task. This is the one rule that turns on who is involved: a preference about somebody not party to the task does not apply.

A memory describing a past state that no longer holds is not relevant.`

// personalMemory renders the block.
func (f *Fetcher) personalMemory(ctx context.Context, r Request, vector turnVector) string {
	if f.src.Diary == nil || r.AgentID == "" || strings.TrimSpace(r.Task) == "" {
		return ""
	}
	if !r.judgeable() {
		// THE THIN-TRIGGER GATE, and it gates BOTH halves of the pool. The
		// trigger is a pointer — "PR #42 got a comment" — or asked
		// nothing, so asking a model which memories bear on it spends a
		// call to judge relevance against a sentence with no content, and
		// a vector of it is a similarity search keyed on nothing. Only the
		// cheap recency read runs, so the hint renders only for a seat that
		// has memories to look through again — which is worth doing
		// precisely because recon will make the trigger real.
		if len(f.recentMemories(ctx, r)) == 0 {
			return ""
		}
		return EmptyMemoryHint
	}
	candidates := f.memoryCandidates(ctx, r, vector)
	if len(candidates) == 0 {
		// Nothing stored. The hint would be a lie: there is no filter to
		// re-run and nothing for it to find.
		return ""
	}
	selected := f.filterMemories(ctx, r, candidates)
	if len(selected) == 0 {
		return EmptyMemoryHint
	}
	bullets := make([]string, 0, len(selected))
	for _, entry := range selected {
		bullets = append(bullets, renderMemory(r.Boundary, entry))
	}
	return joinBullets(bullets)
}

// memoryCandidates gathers the pool the filter judges.
//
// SIMILARITY UNION RECENCY, in that order, deduplicated by id. The union is
// the point: similarity alone misses a standing rule that matches no
// particular task, and recency alone misses the one relevant memory written
// six months ago.
//
// The similarity half ranks against the turn's ONE vector, the embedding of
// what it was asked — shared with episode recall rather than computed again.
func (f *Fetcher) memoryCandidates(ctx context.Context, r Request, vector turnVector) []learning.DiaryEntry {
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

	if v, ok := vector(); ok {
		hits, err := f.src.Diary.Recall(ctx, r.AgentID, learning.RecallQuery{
			Handle: r.AgentID, Embedding: v.Values, Model: v.Model,
			Limit: memoryVectorLimit,
		}, now)
		if err != nil {
			log.WarnContext(ctx, "memory_recall_failed", "agent_id", r.AgentID, "error", err.Error())
		}
		for _, hit := range hits {
			add(hit.Entry)
		}
	}
	for _, entry := range f.recentMemories(ctx, r) {
		add(entry)
	}
	return out
}

// recentMemories is the recency half of the pool: the seat's newest live
// notes, a database read with no embedding and no model call.
func (f *Fetcher) recentMemories(ctx context.Context, r Request) []learning.DiaryEntry {
	recent, err := f.src.Diary.Recent(ctx, r.AgentID, f.now(), memoryRecencyLimit)
	if err != nil {
		log.WarnContext(ctx, "memory_recent_failed", "agent_id", r.AgentID, "error", err.Error())
	}
	return recent
}

// filterMemories asks the auxiliary model which candidates bear on the task.
func (f *Fetcher) filterMemories(ctx context.Context, r Request, candidates []learning.DiaryEntry) []learning.DiaryEntry {
	answer, ok := f.auxCall(ctx, r, types.AuxMemoryFilter, memoryFilterSystemPrompt,
		memoryFilterPrompt(r, candidates))
	if !ok {
		return nil
	}
	picked := parseIndices(answer, len(candidates))
	if len(picked) > memoryMaxSelected {
		picked = picked[:memoryMaxSelected]
	}
	out := make([]learning.DiaryEntry, 0, len(picked))
	ids := make([]string, 0, len(picked))
	for _, i := range picked {
		out = append(out, candidates[i])
		ids = append(ids, candidates[i].ID)
	}
	f.markRetrieved(ctx, ids)
	return out
}

// markRetrieved records that the filter SELECTED these entries.
//
// HERE, and only here, because this is where both recall paths meet, the
// turn-start block and refresh_memory's hinted re-filter, and because what
// counts as a use is what the filter picked. A candidate is not a use: the
// pool is similarity union recency, so counting candidates would move the
// counter for every entry a seat owns on every turn and make the trim's
// ordering meaningless in the other direction.
//
// The trim that bounds a seat's durable memory orders by retrieval count,
// then last retrieval, then age — so with nothing marking a use it evicted
// the OLDEST durable facts, which is precisely what a cap on worth rather
// than age exists to avoid. Nothing called MarkRetrieved before this.
//
// DETACHED and bounded: the turn's context is often cancelled the moment the
// phase ends, and this write happens after the seat already had the benefit
// of the recall. Inheriting the cancellation would drop the count on exactly
// the turns that ran longest.
func (f *Fetcher) markRetrieved(ctx context.Context, ids []string) {
	if f.src.Diary == nil || len(ids) == 0 {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memoryMarkBudget)
	defer cancel()
	f.src.Diary.MarkRetrieved(writeCtx, ids, f.now())
}

// memoryFilterPrompt renders the task, the sender and the numbered pool.
//
// THE TASK IS WHAT THE TURN WAS ASKED ([Request.Ask]), not the brief the
// executor is handed. The brief wraps a chat message in scaffolding that is
// the same on every turn of that surface — the seat's handle and marker, the
// collectives, a role group in the triage — and the filter's rule 3 admits a
// per-subject memory about "someone named in the task", so read against
// the brief, a memory about whoever the scaffolding names is relevant to every
// chat turn it wraps. It was found that way: while the triage carried worked
// examples ("@PM open a ticket for @SWE"), a preference about the PM matched
// every chat turn. What the brief adds that the filter needs is the surface,
// and the ask carries it in the notification's subject ("Slack message", an
// issue's key and title).
//
// THE TASK ARRIVES FENCED, under a token minted for this call ([Ask]): the
// filter's answer decides which of the seat's own memories the seat is shown,
// so a task that addresses the filter must read as somebody's words. The
// sender line is a display name somebody chose and is neutralised under the
// same token.
func memoryFilterPrompt(r Request, candidates []learning.DiaryEntry) string {
	token := provenance.NewBoundary()
	var b strings.Builder
	b.WriteString("Agent's current task:\n" + r.Ask.For(token) + "\n")
	// THE SENDER, STRUCTURED. Without it the filter has only whatever
	// platform ids appear in the task body to reason from, and rule 3 —
	// the one hard subject filter — becomes unenforceable: it cannot tell
	// a preference about the person asking from a preference about
	// somebody else entirely.
	if line := senderLine(r.Senders); line != "" {
		b.WriteString(provenance.Neutralise(token, line) + "\n")
	}
	b.WriteString("\nStored memories:\n")
	for i, entry := range candidates {
		// COLLAPSED, NOT CUT. One line per index is what keeps the indices
		// unambiguous; a length cap on top of that decided which memories
		// the filter could SEE, so a rule whose scope clause sat past the
		// cut was judged on its opening and excluded — and the seat then
		// behaved as if it had never been written.
		//
		// NEUTRALISED under the call's token: a memory is what a model
		// wrote after reading other people's words, and no line of it may
		// open or close the task's fence.
		b.WriteString(strconv.Itoa(i) + ". " + memoryKindLabel(entry) +
			provenance.Neutralise(token, collapse(entry.Content)) + "\n")
	}
	b.WriteString("\nRelevant indices (JSON array):")
	return b.String()
}

// senderLine names who triggered the turn, for the filter's rule 3.
//
// IN [learning.Subject.Label]'s FORM, which is the form the post-turn
// classifier attributes a stored fact to its requester in: rule 3 asks the
// model to match the one against the other, and a sender line that renders
// a person differently from the memories about them is a match left to
// guesswork.
func senderLine(senders []learning.Subject) string {
	labels := make([]string, 0, len(senders))
	for _, s := range senders {
		if label := s.Label(); label != "" && !slices.Contains(labels, label) {
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

// renderMemory renders one selected memory as a bullet, neutralised under b,
// the turn's token (ADR-0040): a memory is what a post-turn model concluded
// from other people's words, or what the seat noted mid-turn, and it is
// shown beside the turn's fences, which nothing in it may open or close.
func renderMemory(b provenance.Boundary, entry learning.DiaryEntry) string {
	content := provenance.Neutralise(b, collapse(entry.Content))
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
		log.Warn("memory_filter_unparseable", "answer", text)
		return nil
	}
	var raw []int
	if err := json.Unmarshal([]byte(array), &raw); err != nil {
		log.Warn("memory_filter_unparseable", "answer", text,
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

// embed turns text into a vector, or says why it cannot — and the two reasons
// are different answers: an error wrapping [learning.ErrNoEmbeddings] is a
// company with none (a nil seam, or the engine's current epoch configuring
// none), which is how that company is set up and is not logged; any other is
// an embedder that was asked and did not give a usable vector, which is.
func (f *Fetcher) embed(ctx context.Context, text string) (learning.Vector, error) {
	if f.src.Embed == nil {
		return learning.Vector{}, learning.ErrNoEmbeddings
	}
	vector, err := f.src.Embed(ctx, text)
	switch {
	case errors.Is(err, learning.ErrNoEmbeddings):
		return learning.Vector{}, err
	case err == nil && (len(vector.Values) == 0 || vector.Model == ""):
		// A vector with no model is one no recall can compare: it names no
		// space, and every stored vector is filtered on its space.
		err = errors.New("prefetch: the embedder answered without a vector or the model it came from")
	}
	if err != nil {
		log.WarnContext(ctx, "prefetch_embedding_failed", "error", err.Error())
		return learning.Vector{}, err
	}
	return vector, nil
}

// collapse folds a memory's internal newlines so one entry stays one bullet.
func collapse(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}
