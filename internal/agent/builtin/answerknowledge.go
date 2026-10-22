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
//
// # A person's question, not a seat's
//
// Nobody's seat runs this and no turn exists: a person typed a question into
// the dashboard and is waiting for a paragraph. So it is an OPERATOR tool, and
// one only a PERSON BOUND TO A SEAT may call ([authz.ActionKnowledgeAnswer]
// admits whoever uses either app, and is human-only and SEATED): it runs on
// the auxiliary model of the seat the identity directory binds them to — the
// human seat every person holds (ADR-0032). A principal no seat binds — a
// pipeline's token or a service account, never a person (ADR-0032) — has no
// seat whose model to run on, and the table refuses it `unseated` rather than
// letting it be answered on the company's default model, because "a CI token"
// is not somebody the spend can be attributed to; a refusal there is also what
// keeps a screen from offering the control to such a caller at all. The tool
// asks the same of the actor it is handed, as the arm a surface wired without
// the table would otherwise reach. A seat is never given it: a seat has
// `search_knowledge` and its own model, and an answer written by a second
// model into its context would be one it could not check.
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
// The AUXILIARY model of the person's own seat, resolved through the engine's
// one auxiliary seam like every auxiliary pass — so a company that pointed its
// cheap work at a small model answers questions on it too — under the
// OPERATOR stage, which the seam charges to the COMPANY's windows alone: a
// person has no seat budget (`token_budget` is on the company and on roles
// that run turns), so the company's day, week and month are what an answer is
// judged against. It is GATED here before the call — a company whose window
// has no room is refused `budget_exhausted` and spends nothing — and CHARGED by
// the seam after, because an answer's size is known only from its reply (see
// [coord.Budgets.PostChargeOrg]). The seam also RECORDS it, as
// `auxiliary_spend` for the person, so the company's spend history holds what
// its people's questions cost — the answer, and every source condensed for it,
// each under its own purpose.
//
// ONE CHARGE SITE, the seam's. The answer used to charge its own completion and
// wrap the condensing calls in a second charger of its own, so two copies of
// "record what a person's question cost" existed beside the seam every other
// auxiliary call went through — and neither reached an event.
//
// # Cached, and on what
//
// A question asked again — by the same person pressing Enter twice, or by a
// colleague on the same node — gets the same answer while nothing it could
// have been written from has changed. The cache is the NODE's
// ([AnswerDeps.Cache]), handed in rather than built with the catalogue: a
// surface builds its catalogue per request, and a cache built with it would
// hold one request's answers. The key is the normalised question, the APPS
// the asker uses and the CORPUS POSITION: where this node's pages, tracker and
// vector logs have been applied through. A cache hit spends nothing and says
// `cached: true`. A company whose knowledge base is not native has no position
// to key on — an external wiki can change under the answer without this node
// hearing — so its answers are never cached.
//
// # Each corpus only for an asker who uses its app
//
// The pages are the knowledge base's and the work items the tracker's, and an
// answer QUOTES them: an answer written from a work item, read by somebody
// kept off the tracker, is the work item read. So the pages are searched only
// for an asker holding Doclet and the items only for one holding
// the tracker ([authz.Shows], the one per-app filter every cross-app answer
// goes through), and the table refuses an asker holding neither before
// anything is searched ([authz.ClassAnyApp]). The apps are in the cache key for
// the same reason: two askers who may read different corpora asked different
// questions, however alike they typed them, and one's answer handed to the
// other would carry what the second may not read.

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

	// AnswerSourceBytes is the most of one source the answering model is
	// handed — and a source longer than it is CONDENSED to it, for the
	// question, never cut.
	//
	// Four kilobytes, about a thousand tokens: a task's whole description,
	// most runbooks whole, and at eight sources a prompt of about eight
	// thousand tokens — small enough for any auxiliary model's context and
	// for a person waiting on the answer. It used to be where every page
	// was cut, unmarked, so an answer written from the first four
	// kilobytes of a runbook could only conclude that it had no step four.
	// A page past it is now rewritten by the same auxiliary model, told the
	// question, keeping what bears on it — so the step four the question
	// is about survives wherever on the page it is.
	AnswerSourceBytes = 4 << 10

	// AnswerTimeout bounds the one model call.
	//
	// A minute: eight thousand tokens in and up to a thousand out on a
	// small model is ten to thirty seconds at the tail, and a person is
	// waiting — past a minute the answer is not worth waiting for.
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
	// A YES IS A REFUSAL, not an observation: it is asked immediately
	// before the answer's first call, and the question is refused on yes,
	// so an implementation records it as the gate turning that call away
	// (the window's refusal stamp). Never ask it only to look.
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
	// the tool: an answer that could spend tokens against no counter at
	// all is the one shape this is built to refuse.
	Budget AnswerBudget

	// Corpus is where this node's knowledge corpus is applied through, and
	// false where it cannot say — the cache key's second half. Nil caches
	// nothing.
	Corpus func() (string, bool)

	// Actor is who is asking, off the caller's own credential
	// ([PrincipalActor]): a person bound to a seat is named by that seat,
	// kind human, which is the one asker this tool answers. Nil omits the
	// tool.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Cache is this node's answers, kept across every catalogue the
	// surface builds — see [NewAnswerCache]. Nil caches nothing.
	Cache *AnswerCache

	// Rewrites is the cache a source condensed for a question is kept in,
	// so the same question at the same corpus pays for each rewrite once.
	// Nil keeps none; every rewrite is still charged and recorded by the
	// seam Models resolves through.
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
	Title string `json:"title"`
	// URL is a page's own link where its backend has one — an external
	// wiki's page, which the dashboard has no route for.
	URL string `json:"url,omitempty"`

	// Written is what the source was read as — the label the door that found
	// it gave it, which the model writing the answer read it under — and
	// what the answer's envelope says of it ([answerKnowledge.answered]).
	// Not on the wire: the envelope says it once for the whole answer.
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
	// A PERSON BOUND TO A SEAT, and nobody else: [iam.ActorFor] names
	// exactly that asker kind human, by the seat. The table's seated row
	// refuses everybody else first; this is the arm that keeps the rule
	// where the actor is read, so a surface wired without the table is not
	// the one that answers an unbound credential. See the file head.
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
		// REFUSED, NOT CUT. The question IS the search text, so it takes
		// the search's own bound ([knowledge.MaxQueryBytes]) — a longer one
		// reaches no ranker usefully — and an answer to the first four
		// hundred bytes of a question is an answer to a different one. It
		// used to be cut to fit, silently, so the answer a person read was
		// to a question they had not quite asked and nothing said so.
		return failed(fmt.Sprintf("The question is %d bytes and %s takes at most %d. "+
			"Ask it in a sentence or two — the search reads keywords, not a document.",
			len(question), AnswerKnowledgeTool, knowledge.MaxQueryBytes)), nil
	}

	company := t.org()
	if company == nil {
		return refused(tools.RefusalUnavailable,
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
		// THE TABLE REFUSES THIS before the tool runs; refused here too,
		// because an answer from nothing would be one written from what
		// the model already believes rather than from the company.
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

	// THE GATE BEFORE ANY SPEND, and fail closed: tokens leave the
	// building on the call, so a counter nobody can read does not let one
	// through.
	refusal, refusing, err := t.deps.Budget.Refusing(ctx)
	if err != nil {
		log.WarnContext(ctx, "knowledge_answer_budget_unreadable", "seat", actor.Handle,
			"error", err.Error())
		return refused(tools.RefusalUnavailable, "The company's token counter could not "+
			"be read, so no answer was written and nothing was spent. Try again shortly."), nil
	}
	if refusing {
		// THE COMPANY'S CEILING, the one this gate reads, named to a person
		// as where it is raised (ADR-0060's remedy).
		return toPerson(refused(tools.RefusalBudgetExhausted, budgetSentence(refusal)),
			caveat.TokenBudgetSpent(caveat.CompanyTokenBudget())), nil
	}

	sources, excerpts, searched := t.retrieve(ctx, question, company, role, reach)
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
			t.deps.Cache.put(key, answer)
		}
		return t.answered(ctx, turn, actor, answer)
	}

	// THE OPERATOR STAGE, which the seam charges to the company's windows
	// and records for the person — whatever becomes of the answer, since
	// the tokens are spent at the vendor the moment it replies.
	member, err := t.deps.Models.Auxiliary(role, answerUse.For(types.AuxAnswerKnowledge))
	if err != nil {
		return refused(tools.RefusalUnavailable, fmt.Sprintf("No model is configured "+
			"to answer with (%v). Nothing was spent.", err)), nil
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
	if err != nil || completion == nil {
		reason := "it answered nothing"
		if err != nil {
			reason = err.Error()
		}
		log.WarnContext(ctx, "knowledge_answer_failed", "seat", actor.Handle,
			"model", member.Key, "error", reason)
		return refused(tools.RefusalUnavailable, fmt.Sprintf("The model could not "+
			"answer (%s). Try again shortly.", reason)), nil
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
