package builtin

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Answering a person's question from the company's knowledge: the command
// palette's answer block.
//
// # A person's question, not a seat's
//
// Nobody's seat runs this and no turn exists: a person typed a question into
// the dashboard and is waiting for a paragraph. So it is an OPERATOR tool, and
// one only a token bound to a person may call — the act transport admits
// nobody else, and this tool refuses an unbound credential on the MCP
// transport too, because the spend is attributed to somebody and "a CI token"
// is not somebody. A seat is never given it: a seat has `search_knowledge` and
// its own model, and an answer written by a second model into its context
// would be one it could not check.
//
// # Why it is a write
//
// It reads, but it SPENDS — one model call per question — and the dashboard's
// reads are refetched on focus and on reconnect. Served as a read it would be
// asked again every time a person tabbed back to the window, each time for
// tokens. As a write it is sent once, when a person asks.
//
// # What it spends, and against what
//
// The AUXILIARY model of the person's own seat, resolved the way every
// auxiliary pass resolves one — so a company that pointed its cheap work at a
// small model answers questions on it too — and charged to the COMPANY's
// windows alone: a person has no seat budget (`token_budget` is on the company
// and on roles that run turns), so the company's day, week and month are what
// an answer is judged against. It is GATED before the call — a company whose
// window has no room is refused `budget_exhausted` and spends nothing — and
// CHARGED after, because an answer's size is known only from its reply (see
// [coord.Budgets.PostChargeOrg]).
//
// # Cached, and on what
//
// A question asked again — by the same person pressing Enter twice, or by a
// colleague on the same node — gets the same answer while nothing it could
// have been written from has changed. The key is the normalised question and
// the CORPUS POSITION: where this node's pages, tracker and vector logs have
// been applied through. A cache hit spends nothing and says `cached: true`. A
// company whose knowledge base is not native has no position to key on — an
// external wiki can change under the answer without this node hearing — so
// its answers are never cached.

// AnswerKnowledgeTool is the tool's wire name.
const AnswerKnowledgeTool = "answer_knowledge"

// The answer's knobs, each tied to what it bounds.
const (
	// AnswerCacheEntries is how many answers one node keeps.
	//
	// 256: the palette asks at most once per settled question and a
	// company's people ask the same few dozen things, so a hit is a
	// repeated question at an unchanged corpus — and any write to a page,
	// a task or a vector changes the position and makes every older entry
	// unreachable, which is what bounds how long one lives. Each is a few
	// kilobytes of Markdown and a source list, so the cache is a megabyte
	// at most.
	AnswerCacheEntries = 256

	// AnswerQuestionMax bounds the question, in bytes.
	//
	// The question IS the search text, so it takes the search's own cap
	// ([searchQueryMax]): a longer one reaches no ranker usefully and would
	// only be cut there instead.
	AnswerQuestionMax = searchQueryMax

	// AnswerPageSources and AnswerTaskSources are how many pages and work
	// items one answer is written from.
	//
	// Five pages and three items: the page search's own turn-start depth
	// is six ([searchHits]) and an answer reads each source whole rather
	// than a title and a snippet, so it takes fewer; items are the
	// secondary corpus — a knowledge answer is about what is WRITTEN
	// DOWN, and a work item is usually a pointer to the work rather than
	// an account of it.
	AnswerPageSources = 5
	AnswerTaskSources = 3

	// AnswerExcerptBytes is how much of one source the model reads.
	//
	// Four kilobytes, about a thousand tokens: the opening of a runbook
	// or a task's whole description, and at eight sources a prompt of
	// about eight thousand tokens — small enough for any auxiliary model's
	// context and for a person waiting on the answer.
	AnswerExcerptBytes = 4 << 10

	// AnswerMaxTokens caps the answer the model writes.
	//
	// 1024: the contract asks for a few short paragraphs, which is a few
	// hundred tokens, and the cap is the room a thinking model spends
	// reasoning before it writes them — the empty answer at the cap is the
	// failure a smaller one produces.
	AnswerMaxTokens = 1024

	// AnswerTimeout bounds the one model call.
	//
	// A minute: eight thousand tokens in and up to a thousand out on a
	// small model is ten to thirty seconds at the tail, and a person is
	// waiting — past a minute the answer is not worth waiting for.
	AnswerTimeout = time.Minute
)

// answerTemperature keeps an answer to the same question at the same corpus
// close to the one cached for it. Not zero, for the reason the auxiliary
// passes give: greedy decoding on a small model loops.
const answerTemperature = 0.2

// The source kinds an answer lists.
const (
	AnswerSourcePage = "page"
	AnswerSourceTask = "task"
)

// AnswerModels resolves the model an answer runs on.
type AnswerModels interface {
	Head(role *org.Role, ph phase.Phase) (chain.Member, error)
}

// BudgetRefusal is the company window that turns an answer away.
type BudgetRefusal struct {
	// Period is `day`, `week` or `month`, and Window its label
	// (`2026-09-25`, `2026-W39`, `2026-09`).
	Period string
	Window string
	// ResetsAt is when the window turns over and the company has room
	// again without a ceiling being raised.
	ResetsAt    time.Time
	Used, Limit int
}

// AnswerBudget gates and charges an answer against the company's windows.
type AnswerBudget interface {
	// Refusing reports the company window with no room left, if any.
	// THREE-VALUED: an unreadable counter is an error, never "room".
	Refusing(ctx context.Context) (BudgetRefusal, bool, error)
	// Charge records what an answer cost. Called after the model answered,
	// on a context that outlives the caller's.
	Charge(ctx context.Context, tokens int) error
}

// TaskReader reads one work item whole, for the description an answer is
// written from.
type TaskReader interface {
	Task(ctx context.Context, idOrKey string, want tracker.DetailWants,
		fresh statelog.Freshness) (tracker.TaskDetail, error)
}

// AnswerDeps are what answering needs beyond the search seams the operator
// surface already has.
type AnswerDeps struct {
	// Models resolves the person's seat's auxiliary model. Nil omits the
	// tool.
	Models AnswerModels

	// Budget is the company's windows. Nil omits the tool: an answer that
	// could spend tokens against no counter at all is the one shape this
	// is built to refuse.
	Budget AnswerBudget

	// Corpus is where this node's knowledge corpus is applied through, and
	// false where it cannot say — the cache key's second half. Nil caches
	// nothing.
	Corpus func() (string, bool)

	// Actor is who is asking, off the caller's own credential. Nil omits
	// the tool.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)
}

// AnswerSource is one document an answer was written from, in the order the
// model was given them: source [n] is the n-th.
type AnswerSource struct {
	// Kind is [AnswerSourcePage] or [AnswerSourceTask].
	Kind string `json:"kind"`
	// Ref addresses it: a page's id, a work item's key.
	Ref   string `json:"ref"`
	Title string `json:"title"`
	// URL is a page's own link where its backend has one — an external
	// wiki's page, which the dashboard has no route for.
	URL string `json:"url,omitempty"`
}

// AnswerTokens is what THIS call spent. Zero on a cached answer.
type AnswerTokens struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// KnowledgeAnswer is the tool's answer.
//
// TOKENS, NEVER A PRICE: what an answer cost is reported in the unit the
// company's budget counts, and nothing here converts it.
type KnowledgeAnswer struct {
	AnswerMD string         `json:"answer_md"`
	Sources  []AnswerSource `json:"sources"`
	Tokens   AnswerTokens   `json:"tokens"`
	// Model is the model that wrote it — empty for an answer no model
	// wrote, which is the one where nothing matched.
	Model  string `json:"model"`
	Cached bool   `json:"cached"`
}

// answerKnowledge answers a question from the company's pages and work items.
type answerKnowledge struct {
	search KnowledgeSearcher
	items  WorkSearcher
	org    func() *org.Organization
	pages  PageReader
	tasks  TaskReader
	deps   AnswerDeps
	cache  *answerCache
}

var _ tools.SeatCallable = (*answerKnowledge)(nil)

func (t *answerKnowledge) Name() string { return AnswerKnowledgeTool }

func (t *answerKnowledge) Description() string {
	return "Answer a question from what the company has written down: its " +
		"knowledge-base pages and its work items. Returns a short Markdown " +
		"answer that cites its sources, the sources themselves, and the " +
		"tokens it spent — charged to the company's budget. Only a token " +
		"bound to a person may ask. The same question asked again while " +
		"nothing it was written from has changed is answered from a cache " +
		"and spends nothing."
}

func (t *answerKnowledge) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("The question, in plain words. At most %d "+
					"bytes.", AnswerQuestionMax),
			},
		},
		"required": []any{"q"},
	}
}

func (t *answerKnowledge) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *answerKnowledge) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return refused(tools.RefusalForbidden, AnswerKnowledgeTool+
			" answers the person whose credential calls it, and this call carries none."), nil
	}
	// A PERSON, and nobody else. See the file head.
	if strings.TrimSpace(actor.Seat) == "" {
		return refused(tools.RefusalForbidden, fmt.Sprintf("%s spends the company's "+
			"tokens on a person's behalf, and the token %q is bound to no seat. Bind it "+
			"with contact.crewlet_operator_id on the person's seat.",
			AnswerKnowledgeTool, actor.Handle)), nil
	}
	question := strings.TrimSpace(argString(args, "q"))
	if question == "" {
		return failed(AnswerKnowledgeTool + " needs `q` — the question, in plain words."), nil
	}
	// textcut, not a byte slice, for search_knowledge's reason: a cut
	// through a multi-byte rune is invalid UTF-8 at every reader after it.
	question = textcut.Bytes(question, AnswerQuestionMax)

	company := t.org()
	if company == nil {
		return refused(tools.RefusalUnavailable,
			"No organization is loaded, so there is no knowledge to answer from."), nil
	}
	role := company.SeatByHandle(actor.Seat)
	if role == nil {
		// The token names a seat the chart no longer has: a config apply
		// removed it between the binding and this call.
		return refused(tools.RefusalForbidden, fmt.Sprintf("The seat %q this token "+
			"is bound to is not in the company chart.", actor.Seat)), nil
	}

	key, cacheable := t.cacheKey(question)
	if cacheable {
		if hit, ok := t.cache.get(key); ok {
			hit.Cached, hit.Tokens = true, AnswerTokens{}
			log.InfoContext(ctx, "knowledge_answered", "seat", actor.Seat,
				"cached", true, "sources", len(hit.Sources))
			return jsonResult(hit)
		}
	}

	// THE GATE BEFORE ANY SPEND, and fail closed: tokens leave the
	// building on the call, so a counter nobody can read does not let one
	// through.
	refusal, refusing, err := t.deps.Budget.Refusing(ctx)
	if err != nil {
		log.WarnContext(ctx, "knowledge_answer_budget_unreadable", "seat", actor.Seat,
			"error", err.Error())
		return refused(tools.RefusalUnavailable, "The company's token counter could not "+
			"be read, so no answer was written and nothing was spent. Try again shortly."), nil
	}
	if refusing {
		return refused(tools.RefusalBudgetExhausted, budgetSentence(refusal)), nil
	}

	sources, excerpts, searched := t.retrieve(ctx, question, company)
	if !searched {
		return refused(tools.RefusalUnavailable, "The company's knowledge could not be "+
			"searched just now, so this says nothing about whether it is written down. "+
			"Nothing was spent; try again shortly."), nil
	}
	if len(sources) == 0 {
		// NOTHING TO ANSWER FROM, so no model is asked: a model given no
		// sources can only say it has none, and saying so costs nothing.
		answer := KnowledgeAnswer{
			AnswerMD: "Nothing the company has written down matches this question.",
			Sources:  []AnswerSource{},
		}
		if cacheable {
			t.cache.put(key, answer)
		}
		return jsonResult(answer)
	}

	member, err := t.deps.Models.Head(role, phase.Auxiliary)
	if err != nil {
		return refused(tools.RefusalUnavailable, fmt.Sprintf("No model is configured "+
			"to answer with (%v). Nothing was spent.", err)), nil
	}
	call, cancel := context.WithTimeout(ctx, AnswerTimeout)
	defer cancel()
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: prompts.KnowledgeAnswerSystem},
			{Role: llm.RoleUser, Content: prompts.BuildKnowledgeAnswer(question, excerpts)},
		},
		// No tools: the answer is the content, and a tool on the surface
		// invites a model to call it and write nothing.
		Temperature: llm.Temp(answerTemperature),
		MaxTokens:   AnswerMaxTokens,
	})
	if completion != nil {
		// CHARGED WHATEVER BECOMES OF THE ANSWER: the tokens are spent at
		// the vendor the moment it replies, and on a context that
		// outlives the caller's, because a person closing the palette
		// between the reply and the write is spend the counter would
		// otherwise never hear about.
		if spent := completion.TotalTokens(); spent > 0 {
			if chargeErr := t.deps.Budget.Charge(context.WithoutCancel(ctx), spent); chargeErr != nil {
				log.WarnContext(ctx, "knowledge_answer_spend_uncounted", "seat", actor.Seat,
					"tokens", spent, "model", completion.Model, "error", chargeErr.Error(),
					"detail", "the company's counter now understates its spend")
			}
		}
	}
	if err != nil || completion == nil {
		reason := "it answered nothing"
		if err != nil {
			reason = err.Error()
		}
		log.WarnContext(ctx, "knowledge_answer_failed", "seat", actor.Seat,
			"model", member.Key, "error", reason)
		return refused(tools.RefusalUnavailable, fmt.Sprintf("The model could not "+
			"answer (%s). Try again shortly.", reason)), nil
	}
	text := strings.TrimSpace(completion.Content)
	if text == "" {
		// The cap spent reasoning, usually. Reported with both numbers,
		// because the fix is a bigger cap and nothing else says so.
		log.WarnContext(ctx, "knowledge_answer_empty", "seat", actor.Seat,
			"model", completion.Model, "output_tokens", completion.OutputTokens,
			"max_tokens", AnswerMaxTokens)
		return refused(tools.RefusalUnavailable, "The model wrote no answer. Try again, "+
			"or ask the question more narrowly."), nil
	}
	answer := KnowledgeAnswer{
		AnswerMD: text,
		Sources:  sources,
		Tokens:   AnswerTokens{Input: completion.InputTokens, Output: completion.OutputTokens},
		Model:    completion.Model,
	}
	if cacheable {
		t.cache.put(key, answer)
	}
	log.InfoContext(ctx, "knowledge_answered", "seat", actor.Seat, "cached", false,
		"model", answer.Model, "input_tokens", answer.Tokens.Input,
		"output_tokens", answer.Tokens.Output, "sources", len(sources))
	return jsonResult(answer)
}

// cacheKey is the question's cache key at this node's corpus position, and
// false where the corpus cannot say where it is.
func (t *answerKnowledge) cacheKey(question string) (string, bool) {
	if t.cache == nil || t.deps.Corpus == nil {
		return "", false
	}
	position, ok := t.deps.Corpus()
	if !ok || position == "" {
		return "", false
	}
	return NormalizeQuestion(question) + "\x00" + position, true
}

// NormalizeQuestion is the form two questions that are one question share:
// case folded and whitespace collapsed. Nothing more — "deploy" and
// "deploys" are different searches, and an answer to one is not the other's.
func NormalizeQuestion(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

// retrieve runs both searches and reads each hit's text. searched is false
// when NEITHER corpus could be searched — which is not "nothing matched".
func (t *answerKnowledge) retrieve(ctx context.Context, question string,
	company *org.Organization) ([]AnswerSource, []prompts.KnowledgeSource, bool) {

	sources := []AnswerSource{}
	var excerpts []prompts.KnowledgeSource
	searched := false

	// THE COMPANY'S OWN ACCOUNT: a nil seat, which is how the operator
	// surface's search_knowledge reads — the person holds the company's
	// credential, and the search is not narrowed to their seat's scope.
	if t.search.CanSearch(nil, company) {
		result := t.search.Search(ctx, knowledge.Query{
			Text: question, Org: company, Limit: AnswerPageSources,
			Mode: knowledge.ModeHybrid,
			// Drafts hidden, as every search a reader acts on hides
			// them: an unreviewed proposal is not what the company
			// knows.
			ExcludeAncestors: []string{knowledge.AutoDraftedParent},
		})
		if result.ServedMode != "" || len(result.Hits) > 0 {
			searched = true
		}
		for _, hit := range result.Hits {
			title := strings.TrimSpace(hit.Title)
			if title == "" {
				title = "(untitled)"
			}
			sources = append(sources, AnswerSource{
				Kind: AnswerSourcePage, Ref: hit.PageID, Title: title, URL: hit.URL,
			})
			excerpts = append(excerpts, prompts.KnowledgeSource{
				Kind: "page", Label: title, Text: t.pageText(ctx, hit),
			})
		}
	}

	if t.items != nil {
		answer, err := t.items.Search(ctx, tracker.SearchQuery{
			Text: question, Limit: AnswerTaskSources, Mode: knowledge.ModeHybrid,
		})
		switch {
		case errors.Is(err, tracker.ErrIndexBuilding):
			// This node's index is behind; the pages still answer.
			log.DebugContext(ctx, "knowledge_answer_items_building")
		case err != nil:
			log.WarnContext(ctx, "knowledge_answer_items_unsearchable", "error", err.Error())
		default:
			searched = true
			for _, hit := range answer.Hits {
				ref := hit.Key
				if ref == "" {
					ref = hit.ID
				}
				sources = append(sources, AnswerSource{
					Kind: AnswerSourceTask, Ref: ref, Title: hit.Title,
				})
				excerpts = append(excerpts, prompts.KnowledgeSource{
					Kind: "work item", Label: strings.TrimSpace(ref + " " + hit.Title),
					Text: t.taskText(ctx, hit),
				})
			}
		}
	}
	return sources, excerpts, searched
}

// pageText is the page's own body where this node holds it, else the search's
// snippet. An external wiki's body is not this node's to read, so its snippet
// is what the model reads — and says less.
func (t *answerKnowledge) pageText(ctx context.Context, hit knowledge.Hit) string {
	if t.pages != nil && hit.Backend == pages.Backend && hit.PageID != "" {
		detail, err := t.pages.Get(ctx, hit.PageID, seatRead)
		if err == nil {
			return textcut.Bytes(detail.Page.Body, AnswerExcerptBytes)
		}
		log.DebugContext(ctx, "knowledge_answer_page_unread", "page", hit.PageID,
			"error", err.Error())
	}
	return hit.Snippet
}

// taskText is the item's description, else the index's excerpt.
func (t *answerKnowledge) taskText(ctx context.Context, hit tracker.Ranked) string {
	if t.tasks != nil && hit.ID != "" {
		detail, err := t.tasks.Task(ctx, hit.ID, tracker.DetailWants{}, seatRead)
		if err == nil {
			return textcut.Bytes(detail.Task.Body, AnswerExcerptBytes)
		}
		log.DebugContext(ctx, "knowledge_answer_task_unread", "task", hit.ID,
			"error", err.Error())
	}
	return hit.Snippet
}

// budgetSentence says which window is out and when it comes back.
func budgetSentence(r BudgetRefusal) string {
	when := ""
	if !r.ResetsAt.IsZero() {
		when = " It resets at " + r.ResetsAt.UTC().Format(time.RFC3339) + "."
	}
	return fmt.Sprintf("The company's %s token budget (%s) is spent — %d of %d — so no "+
		"answer was written and nothing was spent.%s Raise the ceiling to ask sooner.",
		r.Period, r.Window, r.Used, r.Limit, when)
}

// answerCache is one node's answers, least recently used out first.
type answerCache struct {
	mu      sync.Mutex
	limit   int
	order   *list.List
	entries map[string]*list.Element
}

type cachedAnswer struct {
	key    string
	answer KnowledgeAnswer
}

func newAnswerCache(limit int) *answerCache {
	return &answerCache{limit: limit, order: list.New(), entries: map[string]*list.Element{}}
}

// get is a copy of the entry, so a caller stamping `cached` on it changes
// nothing stored.
func (c *answerCache) get(key string) (KnowledgeAnswer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return KnowledgeAnswer{}, false
	}
	c.order.MoveToFront(el)
	answer := el.Value.(*cachedAnswer).answer
	answer.Sources = append([]AnswerSource{}, answer.Sources...)
	return answer, true
}

func (c *answerCache) put(key string, answer KnowledgeAnswer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	answer.Sources = append([]AnswerSource{}, answer.Sources...)
	if el, ok := c.entries[key]; ok {
		el.Value.(*cachedAnswer).answer = answer
		c.order.MoveToFront(el)
		return
	}
	c.entries[key] = c.order.PushFront(&cachedAnswer{key: key, answer: answer})
	for c.order.Len() > c.limit {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cachedAnswer).key)
	}
}

// len is how many answers the cache holds.
func (c *answerCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
