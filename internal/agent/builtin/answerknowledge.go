package builtin

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Answering a person's question from the company's knowledge: the command
// palette's answer block.

// # A person's question, not a seat's
//
// No turn exists: a person at the dashboard is waiting for a paragraph. So it
// is an OPERATOR tool only a PERSON BOUND TO A SEAT may call
// ([authz.ActionKnowledgeAnswer], human-only and SEATED), run on the auxiliary
// model of the human seat the identity directory binds them to (ADR-0032). A
// principal no seat binds (a pipeline's token, a service account) is refused
// `unseated` rather than answered on the default model: nobody to attribute the
// spend to, and the refusal keeps a screen from offering it the control. The
// tool checks its actor too, for a surface wired without the table. A seat
// never gets it: it has `search_knowledge` and its own model, and a second
// model's answer in its context is one it could not check.

// # Why it is a write
//
// It reads, but it SPENDS a model call per question, and the dashboard refetches
// reads on focus and on reconnect: as a read it would spend again every time a
// person tabbed back. As a write it is sent once, when a person asks.

// # What it spends, and against what
//
// The person's seat's AUXILIARY model, through the engine's one auxiliary seam
// (so a company's cheap model answers questions too), under the OPERATOR stage,
// which the seam charges to the COMPANY's windows alone: a person has no seat
// budget. It is GATED here before the call (no room: `budget_exhausted`, nothing
// spent) and CHARGED by the seam after, since an answer's size is known only from
// its reply ([coord.Budgets.PostChargeOrg]). The seam is the ONE charge site: it
// also records the answer and every source condensed for it as
// `auxiliary_spend` for the person, each under its own purpose.

// # Cached, and on what
//
// A repeated question gets the same answer while nothing it could be written
// from has changed. The cache is the NODE's ([AnswerDeps.Cache]), not built with
// the catalogue, which a surface builds per request. The key is the normalised
// question, the asker's APPS and the CORPUS POSITION ([AnswerDeps.Corpus]),
// which a write moves and a read never does. A hit spends nothing and says
// `cached: true`. A knowledge base that is not native has no position (an
// external wiki changes unheard), so its answers are never cached.

// # Each corpus only for an asker who uses its app
//
// An answer QUOTES its sources: one written from a work item, read by somebody
// kept off the tracker, is the work item read. So pages are searched only for an
// asker holding Doclet and items only for one holding the tracker
// ([authz.Shows]), and the table refuses an asker holding neither
// ([authz.ClassAnyApp]). The apps are in the cache key for the same reason: one
// asker's answer handed to another could carry what the second may not read.

// AnswerKnowledgeTool is the tool's wire name.
const AnswerKnowledgeTool = "answer_knowledge"

// The answer's knobs, each tied to what it bounds.
const (
	// AnswerCacheEntries is how many answers one node keeps.
	//
	// 256: people ask the same few dozen things, and any write to a page, a
	// task or a vector moves the corpus position and strands every older
	// entry, which bounds how long one lives. At a few kilobytes each, the
	// cache is a megabyte at most.
	AnswerCacheEntries = 256

	// AnswerPageSources and AnswerTaskSources are how many pages and work
	// items one answer is written from.
	//
	// Five pages, under the search's turn-start depth of six ([searchHits]),
	// because an answer reads each source whole; three items, the secondary
	// corpus: an answer is about what is WRITTEN DOWN, and a work item usually
	// points at the work rather than giving an account of it.
	AnswerPageSources = 5
	AnswerTaskSources = 3

	// AnswerSourceBytes is the most of one source the answering model is
	// handed. A longer source is CONDENSED to it for the question, never cut:
	// a cut runbook loses the step four the question may be about.
	//
	// Four kilobytes, about a thousand tokens: a task's whole description,
	// most runbooks whole, and at eight sources a prompt of about eight
	// thousand tokens, small enough for any auxiliary model and a waiting
	// person.
	AnswerSourceBytes = 4 << 10

	// AnswerTimeout bounds the one model call.
	//
	// A minute: eight thousand tokens in and a thousand out on a small model
	// is ten to thirty seconds at the tail, and a person is waiting.
	AnswerTimeout = time.Minute
)

// The source kinds an answer lists.
const (
	AnswerSourcePage = "page"
	AnswerSourceTask = "task"
)

// AnswerModels resolves the model an answer runs on: the engine's auxiliary
// seam, which charges and records every completion made through what it hands
// back under the attribution the call states — here always the operator stage.
type AnswerModels interface {
	Auxiliary(role *org.Role, use auxspend.Use) (chain.Member, error)
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

// AnswerBudget gates an answer against the company's windows. The CHARGE is
// not here: the auxiliary seam [AnswerModels] resolves through makes it, for
// every call an answer makes.
type AnswerBudget interface {
	// Refusing reports the company window with no room left, if any.
	// THREE-VALUED: an unreadable counter is an error, never "room".
	//
	// A YES IS A REFUSAL, not an observation: it is asked just before the
	// answer's first call, so an implementation records it as the gate
	// turning that call away (the window's refusal stamp). Never ask it only
	// to look.
	Refusing(ctx context.Context) (BudgetRefusal, bool, error)
}

// TaskReader reads one work item whole, for the description an answer is
// written from.
type TaskReader interface {
	Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
		fresh statelog.Freshness) (worklet.TaskDetail, error)
}

// AnswerDeps are what answering needs beyond the search seams the operator
// surface already has.
type AnswerDeps struct {
	// Models resolves the person's seat's auxiliary model through the
	// seam that charges and records it. Nil omits the tool.
	Models AnswerModels

	// Budget is the company's windows, as the gate reads them. Nil omits
	// the tool: an answer spending against no counter is what the gate
	// exists to refuse.
	Budget AnswerBudget

	// Corpus is where this node's knowledge corpus last changed (a token a
	// write moves and a read never does), false where it cannot say: the
	// cache key's corpus position. Nil caches nothing.
	Corpus func() (string, bool)

	// Actor is who is asking, off the caller's own credential
	// ([PrincipalActor]): a person bound to a seat is named by that seat,
	// kind human, which is the one asker this tool answers. Nil omits the
	// tool.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Cache is this node's answers, kept across every catalogue the
	// surface builds — see [NewAnswerCache]. Nil caches nothing.
	Cache *AnswerCache

	// Rewrites keeps each source condensed for a question, so the same
	// question at the same corpus pays for each rewrite once. Nil keeps
	// none; every rewrite is still charged and recorded through Models.
	Rewrites *compact.Cache
}

// AnswerSource is one document an answer was written from, in the order the
// model was given them: source [n] is the n-th.
type AnswerSource struct {
	// Kind is [AnswerSourcePage] or [AnswerSourceTask].
	Kind string `json:"kind"`
	// Ref addresses it: a page's id, a work item's key.
	Ref string `json:"ref"`
	// Title is its writers' words, as every source's text is.
	Title string `json:"title" untrusted:"name"`
	// URL is a page's own link where its backend has one — an external
	// wiki's page, which the dashboard has no route for.
	URL string `json:"url,omitempty"`

	// Written is the label the door that found the source gave it: what the
	// answering model read it under and the envelope reports
	// ([answerKnowledge.answered]). Not on the wire: the envelope says it once.
	Written provenance.Label `json:"-"`
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
}

var _ tools.SeatCallable = (*answerKnowledge)(nil)

func (t *answerKnowledge) Name() string { return AnswerKnowledgeTool }

func (t *answerKnowledge) Description() string {
	return "Answer a question from what the company has written down: its " +
		"knowledge-base pages and its work items. Returns a short Markdown " +
		"answer that cites its sources, the sources themselves, and the " +
		"tokens it spent — charged to the company's budget. Only a person " +
		"bound to a seat may ask. The same question asked again while " +
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
					"bytes.", knowledge.MaxQueryBytes),
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
	// A PERSON BOUND TO A SEAT only: [iam.ActorFor] names that asker kind
	// human, by the seat. The table's seated row refuses everybody else
	// first; this arm holds for a surface wired without the table.
	if actor.Kind != worklet.AuthorHuman || strings.TrimSpace(actor.Handle) == "" {
		return refused(tools.RefusalForbidden, fmt.Sprintf("%s runs on the auxiliary "+
			"model of the seat its asker is bound to, and %q is bound to no seat, so "+
			"there is no model to answer on and nobody the spend is charged for. A "+
			"person bound to a seat in the identity directory may ask.",
			AnswerKnowledgeTool, actor.Handle)), nil
	}
	question := strings.TrimSpace(argString(args, "q"))
	if question == "" {
		return failed(AnswerKnowledgeTool + " needs `q` — the question, in plain words."), nil
	}
	if knowledge.CheckQuery(question) != nil {
		// REFUSED, NOT CUT: the question IS the search text, so it takes the
		// search's bound ([knowledge.MaxQueryBytes]), and an answer to its
		// first four hundred bytes answers a different question.
		return failed(fmt.Sprintf("The question is %d bytes and %s takes at most %d. "+
			"Ask it in a sentence or two — the search reads keywords, not a document.",
			len(question), AnswerKnowledgeTool, knowledge.MaxQueryBytes)), nil
	}

	company := t.org()
	if company == nil {
		return refusedBy(tools.RefusalUnavailable, waits(),
			"No organization is loaded, so there is no knowledge to answer from."), nil
	}
	role := company.SeatByHandle(actor.Handle)
	if role == nil {
		// The binding names a seat the company no longer has: an apply
		// removed it between the binding and this call.
		return refused(tools.RefusalForbidden, fmt.Sprintf("The seat %q you are "+
			"bound to is not in the company chart.", actor.Handle)), nil
	}

	// WHAT THIS ASKER MAY BE ANSWERED FROM, decided once and used for the
	// search and the cache alike — see the file head.
	principal, _ := iam.From(ctx)
	reach := answerReachOf(principal)
	if !reach.pages && !reach.tasks {
		// The table refuses this first; refused here too, since an answer
		// from nothing is what the model believes, not what the company wrote.
		return refused(tools.RefusalForbidden, AnswerKnowledgeTool+" answers from "+
			"Doclet and Worklet, and your access includes neither. An admin can "+
			"give it to you."), nil
	}
	key, cacheable := t.cacheKey(question, reach)
	if cacheable {
		if hit, ok := t.deps.Cache.get(key); ok {
			hit.Cached, hit.Tokens = true, AnswerTokens{}
			log.InfoContext(ctx, "knowledge_answered", "seat", actor.Handle,
				"cached", true, "sources", len(hit.Sources))
			return t.answered(ctx, turn, actor, hit)
		}
	}

	// THE GATE BEFORE ANY SPEND, failing closed: a counter nobody can read
	// lets no call through.
	refusal, refusing, err := t.deps.Budget.Refusing(ctx)
	if err != nil {
		// A CONDITION IS WRITTEN HERE, with the seat, since [nodeFailure]
		// logs only a fault: a counter the coordination store did not
		// answer for is a wait, and one this node cannot read is its fault.
		if _, condition := Condition(err); condition {
			log.WarnContext(ctx, "knowledge_answer_budget_unreadable", "seat", actor.Handle,
				"error", err.Error())
		}
		return nodeFailure(ctx, AnswerKnowledgeTool, err, "Could not read the company's "+
			"token counter", "No answer was written and nothing was spent."), nil
	}
	if refusing {
		// THE COMPANY'S CEILING, the one this gate reads, named to a person
		// as where it is raised (ADR-0060's remedy).
		return toPerson(refused(tools.RefusalBudgetExhausted, budgetSentence(refusal)),
			caveat.TokenBudgetSpent(caveat.CompanyTokenBudget())), nil
	}

	sources, excerpts, searched := t.retrieve(ctx, question, company, role, reach)
	if !searched {
		return refusedBy(tools.RefusalUnavailable, waits(), "The company's knowledge could not be "+
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
			t.deps.Cache.put(key, answer)
		}
		return t.answered(ctx, turn, actor, answer)
	}

	// THE OPERATOR STAGE, which the seam charges to the company's windows
	// and records for the person — whatever becomes of the answer, since
	// the tokens are spent at the vendor the moment it replies.
	member, err := t.deps.Models.Auxiliary(role, answerUse.For(types.AuxAnswerKnowledge))
	if err != nil {
		// WHY IS THE LOG'S: the seam's error names the chain it resolved and
		// the configuration it read, nothing the person asking acts on.
		log.WarnContext(ctx, "knowledge_answer_no_model", "seat", actor.Handle,
			"error", err.Error())
		return refusedBy(tools.RefusalUnavailable, waitless(), "No model is set up to "+
			"answer questions, so nothing was spent. An administrator needs to set one up."), nil
	}
	call, cancel := context.WithTimeout(ctx, AnswerTimeout)
	defer cancel()
	// ONE TOKEN FOR THE CALL, never a turn's: every source is fenced under
	// it as who wrote it ([prompts.BuildKnowledgeAnswer]).
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: prompts.KnowledgeAnswerSystem},
			{Role: llm.RoleUser, Content: prompts.BuildKnowledgeAnswer(provenance.NewBoundary(),
				question, excerpts)},
		},
		// No tools: the answer is the content, and a tool on the surface
		// invites a model to call it and write nothing.
	})
	if retryAt, resetsAt, limited := llm.LimitedUntil(err); limited {
		// A LIMIT ALREADY REPORTED IS A WAIT, NOT A FAILURE, as every
		// auxiliary caller reads one: the backend sat the entry out and
		// launched nothing. So no warn line per press, and the person is told
		// when it lifts rather than sent back under a two-second busy hint.
		log.InfoContext(ctx, "knowledge_answer_limited", "seat", actor.Handle,
			"model", member.Key, "retry_at", instantOrNone(retryAt),
			"resets_at", instantOrNone(resetsAt))
		return toPerson(refusedBy(tools.RefusalUnavailable, waitsUntil(retryAt),
			limitedSentence(retryAt, resetsAt)), caveat.ModelAtLimit()), nil
	}
	if err != nil || completion == nil {
		reason := "it answered nothing"
		if err != nil {
			reason = err.Error()
		}
		log.WarnContext(ctx, "knowledge_answer_failed", "seat", actor.Handle,
			"model", member.Key, "error", reason)
		words, waiting := modelFailed(err)
		if !waiting {
			return refusedBy(tools.RefusalUnavailable, waitless(), words), nil
		}
		return refusedBy(tools.RefusalUnavailable, waits(), words), nil
	}
	// THE ANSWER IS DEFUSED under the caller's token: it is a model's
	// rewording of pages somebody else wrote, read next by a person or by
	// their assistant, and nothing in it may open or close a fence.
	text := provenance.Neutralise(turn.FenceBoundary(), strings.TrimSpace(completion.Content))
	if text == "" {
		// Output spent reasoning, usually. Reported with the count,
		// because nothing else says the model worked.
		log.WarnContext(ctx, "knowledge_answer_empty", "seat", actor.Handle,
			"model", completion.Model, "output_tokens", completion.OutputTokens)
		return refusedBy(tools.RefusalUnavailable, waits(), "The model wrote no answer. Try again, "+
			"or ask the question more narrowly."), nil
	}
	answer := KnowledgeAnswer{
		AnswerMD: text,
		Sources:  sources,
		Tokens:   AnswerTokens{Input: completion.InputTokens, Output: completion.OutputTokens},
		Model:    completion.Model,
	}
	if cacheable {
		t.deps.Cache.put(key, answer)
	}
	log.InfoContext(ctx, "knowledge_answered", "seat", actor.Handle, "cached", false,
		"model", answer.Model, "input_tokens", answer.Tokens.Input,
		"output_tokens", answer.Tokens.Output, "sources", len(sources))
	return t.answered(ctx, turn, actor, answer)
}

// answered is the tool's answer, labelled with whose words it carries: each
// source's title is its writers', and the answer itself a model's account of
// them — so it goes out through a [foreignRead], every string defused and the
// envelope saying what the sources were read as, each by the label the door
// that found it gave it (ADR-0040). A person asking at the dashboard is the
// usual caller, and the reader of this answer is often their assistant.
func (t *answerKnowledge) answered(ctx context.Context, turn *turnctx.Turn, actor Actor,
	answer KnowledgeAnswer) (tools.Result, error) {

	read := newForeignRead(turn, actor.Handle)
	for _, source := range answer.Sources {
		read.adopt(source.Written)
	}
	return read.answer(ctx, AnswerKnowledgeTool, answer, "")
}

// answerReach is which of the two corpora one asker may be answered from.
type answerReach struct{ pages, tasks bool }

// answerReachOf is what p may be answered from, through [authz.Shows] — the one
// per-app filter every cross-app answer asks.
func answerReachOf(p iam.Principal) answerReach {
	return answerReach{
		pages: authz.Shows(p, authz.KindPage),
		tasks: authz.Shows(p, authz.KindTask),
	}
}

// key is the reach as the cache key spells it: the apps it reads from, in a
// fixed order, so two askers who may read the same corpora share answers and
// two who may not never do.
func (r answerReach) key() string {
	var apps []string
	if r.tasks {
		apps = append(apps, string(iam.GrantWorklet))
	}
	if r.pages {
		apps = append(apps, string(iam.GrantDoclet))
	}
	slices.Sort(apps)
	return strings.Join(apps, ",")
}

// cacheKey is the question's cache key for one asker's reach at this node's
// corpus position, and false where the corpus cannot say where it is.
func (t *answerKnowledge) cacheKey(question string, reach answerReach) (string, bool) {
	if t.deps.Cache == nil || t.deps.Corpus == nil {
		return "", false
	}
	position, ok := t.deps.Corpus()
	if !ok || position == "" {
		return "", false
	}
	return NormalizeQuestion(question) + "\x00" + reach.key() + "\x00" + position, true
}

// NormalizeQuestion is the form two questions that are one question share:
// case folded and whitespace collapsed. Nothing more — "deploy" and
// "deploys" are different searches, and an answer to one is not the other's.
func NormalizeQuestion(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

// retrieve runs the searches the asker's reach allows and reads each hit's
// text. searched is false when no corpus that was asked could be searched —
// which is not "nothing matched" — and a corpus the asker may not read is
// never asked, so it is neither a source nor a reason the search failed.
func (t *answerKnowledge) retrieve(ctx context.Context, question string,
	company *org.Organization, asker *org.Role,
	reach answerReach) ([]AnswerSource, []prompts.KnowledgeSource, bool) {

	sources := []AnswerSource{}
	var excerpts []prompts.KnowledgeSource
	searched := false

	// THE COMPANY'S OWN ACCOUNT: a nil seat, which is how the operator
	// surface's search_knowledge reads — an asker who uses the knowledge
	// base reads the whole of it, and the search is not narrowed to their
	// seat's scope.
	if reach.pages && t.search.CanSearch(nil, company) {
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
				Written: hit.Provenance,
			})
			excerpts = append(excerpts, prompts.KnowledgeSource{
				Kind: "page", Label: title, Text: t.pageText(ctx, hit),
				// AS THE BACKEND THAT FOUND IT LABELS IT, the label the
				// seat's own search reads the same page under.
				Written: hit.Provenance,
			})
		}
	}

	if reach.tasks && t.items != nil {
		answer, err := t.items.Search(ctx, worklet.SearchQuery{
			Text: question, Limit: AnswerTaskSources, Mode: knowledge.ModeHybrid,
		})
		switch {
		case errors.Is(err, worklet.ErrIndexBuilding):
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
					Written: workDescription,
				})
				excerpts = append(excerpts, prompts.KnowledgeSource{
					Kind: "work item", Label: strings.TrimSpace(ref + " " + hit.Title),
					Text: t.taskText(ctx, hit), Written: workDescription,
				})
			}
		}
	}
	sources, excerpts = t.condense(ctx, question, asker, sources, excerpts)
	return sources, excerpts, searched
}

// condense makes every source fit [AnswerSourceBytes]: a source within it is
// handed over whole, and one past it is REWRITTEN for the question by the
// asker's auxiliary model — through the same seam as the answer, under the
// operator stage, so it is charged to the company and recorded for the person
// like the answer is — and marked as a rewrite.
//
// A SOURCE THAT CANNOT BE CONDENSED IS DROPPED, from the excerpts and from the
// list of sources alike. It is neither cut, which would hand the model the
// opening of a page as the page, nor carried whole, which on a half-megabyte
// page is a prompt the answering model cannot hold — and it is not listed,
// because an answer must never cite a source it was not shown.
func (t *answerKnowledge) condense(ctx context.Context, question string, asker *org.Role,
	sources []AnswerSource, excerpts []prompts.KnowledgeSource,
) ([]AnswerSource, []prompts.KnowledgeSource) {
	fit := compact.New(t.deps.Models, t.deps.Rewrites).For(asker, answerUse)
	keep := make([]bool, len(excerpts))
	var group errgroup.Group
	group.SetLimit(compact.Parallel)
	for i := range excerpts {
		group.Go(func() error {
			text := excerpts[i].Text
			if len(text) <= AnswerSourceBytes {
				keep[i] = true
				return nil
			}
			res, err := fit.Labelled(excerpts[i].Written).Focused(ctx, compact.KindSource, text,
				AnswerSourceBytes, question)
			if err != nil {
				log.WarnContext(ctx, "knowledge_answer_source_dropped", "source", sources[i].Ref,
					"bytes", len(text), "error", err.Error(),
					"detail", "the source is longer than an answer can read and could not be condensed")
				return nil
			}
			excerpts[i].Text = res.Note() + "\n" + res.Text
			keep[i] = true
			return nil
		})
	}
	_ = group.Wait()
	var keptSources []AnswerSource
	var keptExcerpts []prompts.KnowledgeSource
	for i := range excerpts {
		if keep[i] {
			keptSources = append(keptSources, sources[i])
			keptExcerpts = append(keptExcerpts, excerpts[i])
		}
	}
	if keptSources == nil {
		keptSources = []AnswerSource{}
	}
	return keptSources, keptExcerpts
}

// SnippetOnly is the line a source the answer could not read in full is
// introduced by, so the model weighs a search snippet as the pointer it is.
const SnippetOnly = "(only the search snippet — the source itself could not be read here)\n"

// pageText is the page's own body WHOLE where this node holds it, else the
// search's snippet, SAID to be one. An external wiki's body is not this
// node's to read, so its snippet is what the model reads — and a model told
// it is reading a snippet does not conclude the page says nothing more.
func (t *answerKnowledge) pageText(ctx context.Context, hit knowledge.Hit) string {
	if t.pages != nil && hit.Backend == doclet.Backend && hit.PageID != "" {
		detail, err := t.pages.Get(ctx, hit.PageID, seatRead)
		if err == nil {
			return detail.Page.Body
		}
		log.DebugContext(ctx, "knowledge_answer_page_unread", "page", hit.PageID,
			"error", err.Error())
	}
	return snippetOnly(hit.Snippet)
}

// taskText is the item's description WHOLE, else the index's snippet, said to
// be one.
func (t *answerKnowledge) taskText(ctx context.Context, hit worklet.Ranked) string {
	if t.tasks != nil && hit.ID != "" {
		detail, err := t.tasks.Task(ctx, hit.ID, worklet.DetailWants{}, seatRead)
		if err == nil {
			return detail.Task.Body
		}
		log.DebugContext(ctx, "knowledge_answer_task_unread", "task", hit.ID,
			"error", err.Error())
	}
	return snippetOnly(hit.Snippet)
}

// workDescription is the label a work item's description is read under here:
// a colleague's words with nobody named, as `search_work_items` reads the same
// text — the engine hosts the tracker, so only the company's principals write
// it, and neither the index nor the search says which of them wrote what it
// holds ([provenance.LabelForHosted]).
var workDescription = provenance.LabelForHosted("", provenance.SourceWorkDescription, nil, false)

func snippetOnly(snippet string) string {
	if strings.TrimSpace(snippet) == "" {
		return ""
	}
	return SnippetOnly + snippet
}

// answerUse is the attribution every call an answer makes states: the
// OPERATOR stage — a person's spend, on the company's windows alone — with no
// turn, since none exists. Each call adds its own purpose: the answer
// [types.AuxAnswerKnowledge], a condensed source `condense_source`.
var answerUse = auxspend.Use{Stage: types.AuxStageOperator}

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

// AnswerCache is one node's answers, least recently used out first.
//
// BUILT ONCE PER NODE and handed to every catalogue the operator surface
// builds ([AnswerDeps.Cache]), safe for concurrent use.
type AnswerCache struct {
	mu      sync.Mutex
	limit   int
	order   *list.List
	entries map[string]*list.Element
}

type cachedAnswer struct {
	key    string
	answer KnowledgeAnswer
}

// NewAnswerCache is a cache holding at most limit answers; a limit below one
// holds [AnswerCacheEntries].
func NewAnswerCache(limit int) *AnswerCache {
	if limit < 1 {
		limit = AnswerCacheEntries
	}
	return &AnswerCache{limit: limit, order: list.New(), entries: map[string]*list.Element{}}
}

// get is a copy of the entry, so a caller stamping `cached` on it changes
// nothing stored.
func (c *AnswerCache) get(key string) (KnowledgeAnswer, bool) {
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

func (c *AnswerCache) put(key string, answer KnowledgeAnswer) {
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
func (c *AnswerCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// limitedSentence is a question no model was asked because the model that
// answers it is at a usage limit, as the model reading the answer is told it:
// when the limit lifts where its vendor said, and otherwise when the model is
// tried again, each as an instant ([budgetSentence]'s form).
func limitedSentence(retryAt, resetsAt time.Time) string {
	const said = "The model that answers questions has reached its usage limit, so " +
		"no answer was written and nothing was spent."
	switch {
	case !resetsAt.IsZero():
		return said + " The limit lifts at " + resetsAt.UTC().Format(time.RFC3339) + "."
	case !retryAt.IsZero():
		return said + " Its vendor named no time it lifts; the model is tried again at " +
			retryAt.UTC().Format(time.RFC3339) + "."
	}
	return said + " Its vendor named no time it lifts."
}

// instantOrNone is an instant for a log line, or "" where it is unknown.
func instantOrNone(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// modelFailed is a failed answer as the person who asked reads it, by the
// provider layer's failure kind ([llm.KindOf]), and whether a wait can clear it,
// which the answer's Retry-After follows: a timeout or a harness that gave no
// usable answer may answer next time; no retry gets past an expired or missing
// login, or a call the engine cannot make ([llm.KindFatal]). A usage limit is
// not one of these: it is the wait [limitedSentence] tells.
//
// NEVER THE ERROR ITSELF, which went to the log: it names the provider entry,
// the model and the harness's words, nothing a person acts on.
func modelFailed(err error) (string, bool) {
	if err == nil {
		return "The model answered nothing. Try again shortly.", true
	}
	switch llm.KindOf(err) {
	case llm.KindTimeout:
		return "The model took too long to answer. Try again shortly.", true
	case llm.KindAuth:
		return "The model's service refused the company's credentials. An " +
			"administrator needs to check them.", false
	case llm.KindServer:
		return "The model's service failed. Try again shortly.", true
	case llm.KindProfileDrift:
		return "The engine could not read this release of the model's coding tool. An " +
			"administrator needs to check it.", false
	}
	return "The model couldn't answer this question.", false
}
