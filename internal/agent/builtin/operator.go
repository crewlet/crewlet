package builtin

import (
	"github.com/crewlet/crewlet/internal/tools"

	"github.com/crewlet/crewlet/internal/org"
)

// The OPERATOR catalogue: the same tools a seat holds, offered to a person's
// own AI assistant.
//
// # One implementation, two writers
//
// Every entry here is the same value [Register] files into a seat's registry.
// The only thing that differs is who a write is attributed to, and that is
// [WorkDeps.Actor] — a single field — rather than a second copy of the tools.
// Two copies drift on exactly the parts nobody looks at: which field is
// trimmed, which default applies, what a refusal says. Only one of the two is
// ever tested, and it is never the copy that broke.
//
// # What is deliberately NOT here
//
// The tools that only make sense INSIDE a turn: the memory tools (a diary
// belongs to a seat, and an operator has none), the skill tools (a skill is
// loaded into a phase), `a2a_ask` (a colleague ask is answered by waking a
// seat, and there is nobody here for the answer to come back to),
// `run_sandbox` (a detached run resumes a suspended phase that does not
// exist — though a run a seat parked on a question IS a person's to answer,
// which is `answer_run`), and `lookup_colleague` — which would be useful, but resolves
// against the turn's own org and has no other source.
//
// What is left is the surface an operator's assistant actually needs — and
// the dashboard, which makes every change it makes through this catalogue
// (ADR-0024): read the board, file and move work, read and write the wiki,
// search what the company knows, and talk in the company's own chat.

// OperatorDeps are the halves an operator surface can serve.
type OperatorDeps struct {
	Work      WorkDeps
	Pages     PageDeps
	Knowledge KnowledgeSearcher

	// Chat is the company's own chat. Its [ChatDeps.Actor] resolves the
	// credential to the `kind: human` seat it is bound to — a room renders
	// a speaker by seat, so that is the handle every message carries —
	// and records the write as an operator's, the token beside the seat,
	// as every write on this surface is (ADR-0024). A token bound to none
	// is refused every call, the reads included.
	Chat ChatDeps

	// Org is the company this surface is about, resolved per call because
	// a config apply replaces it. An operator has no turn and therefore no
	// org to read from one, so search_knowledge takes it from here — and
	// with it nil that tool refuses every call it is registered for.
	Org func() *org.Organization

	// Leads answers whether one handle leads another, which is the one
	// authority over a person's record that reaches across people. Nil
	// resolves nothing, which degrades to "your own only" rather than to
	// a hole — see [Leads].
	Leads Leads

	// LeadsProject answers whether a handle leads the unit that owns a
	// project, which is the authority over that project's PLAN. A
	// separate seam from [Leads] because it is a different question: one
	// is about a person, the other about a container, and a company can
	// answer either without answering the other. Nil REFUSES rather than
	// degrading — see [LeadsProject].
	LeadsProject LeadsProject

	// Runs answers a parked coding run by its turn — see answerrun.go.
	// A nil desk omits the tool.
	Runs RunDeps

	// Pauses pauses and resumes a seat — see seatpause.go. A nil record
	// omits both tools.
	Pauses SeatPauseDeps

	// Steer sends a note to a running turn — see steer.go. A nil asker
	// omits the tool.
	Steer SteerDeps

	// Answer answers a person's question from the company's knowledge —
	// see answerknowledge.go. It searches through Knowledge and the work
	// search above, so it is served only with Knowledge and Org; a nil
	// model, budget or actor omits it.
	Answer AnswerDeps
}

// OperatorTools is the catalogue for one operator surface.
//
// A tool whose dependency is absent is OMITTED, on [Register]'s own rule: a
// company on Jira has no native tracker, and an assistant shown a tool that
// always fails learns to distrust the whole catalogue.
//
// The order is the order an operator's assistant meets them in: read before
// write, work before knowledge. Nothing depends on it, and a stable one means
// two boots of one config advertise the same list.
func OperatorTools(deps OperatorDeps) []tools.Callable {
	work, pages := deps.Work, deps.Pages
	// A CALL HERE IS AN OPERATION ITS CALLER HOLDS, because there is no
	// turn to derive one from: every write answers with its `op_id`, and
	// the call brought back with it is that operation again. See
	// [WorkDeps.bindOperation] — and this is the only place that says so,
	// because it is a fact about this surface rather than a setting.
	work.callerOperations = true
	candidates := []struct {
		tool tools.Callable
		on   bool
	}{
		{&listWorkItems{deps: work}, work.Reader != nil},
		{&getWorkItem{deps: work}, work.Reader != nil},
		{&createWorkItem{deps: work, pages: pages.Reader}, work.Writer != nil},
		{&updateWorkItem{deps: work, leads: deps.LeadsProject},
			work.Writer != nil && work.Reader != nil},
		{&commentOnWorkItem{deps: work, pages: pages.Reader},
			work.Writer != nil && work.Reader != nil},
		{&mergeWorkItem{deps: work}, work.Merges != nil && work.Reader != nil},
		{&moveWorkItem{deps: work, leads: deps.LeadsProject},
			work.Moves != nil && work.Reader != nil},
		{&searchWorkItems{deps: work}, work.Search != nil},
		// THE VIEW TOOLS ARE HERE AND IN NO SEAT'S REGISTRY. A saved
		// view is furniture a person arranges; see the file head of
		// workviews.go for why a seat is not given them.
		{&listWorkViews{deps: work}, work.Reader != nil},
		{&saveWorkView{deps: work}, work.ViewWriter != nil},
		{&getWorkCatalogue{deps: work}, work.Reader != nil},
		{&writeWorkCatalogue{deps: work}, work.CatalogueWriter != nil},
		{&listProjects{deps: work}, projectReads(work)},
		{&describeProject{deps: work}, projectReads(work)},
		{&taskActivity{deps: work}, feedReads(work)},
		{&myWork{deps: work}, feedReads(work)},
		{&getPerson{deps: work}, work.Reader != nil},
		{&setPriorities{deps: work, leads: deps.Leads}, work.PersonWriter != nil},
		{&setPins{deps: work}, work.PersonWriter != nil},
		{&markInbox{deps: work}, work.PersonWriter != nil},
		{&workInbox{deps: work}, work.Inbox != nil},
		{&writeProject{deps: work, leads: deps.LeadsProject},
			work.ProjectWriter != nil},
		{&listProjectFiles{deps: work}, work.Files != nil},
		{&readProjectFile{deps: work}, work.Files != nil && work.Objects != nil},
		{&writeProjectFile{deps: work}, work.FileWriter != nil && work.Objects != nil},
		{&removeProjectFile{deps: work}, work.FileWriter != nil},
		{&removeWorkItem{deps: work}, work.TrashWriter != nil && work.Reader != nil},
		{&restoreWorkItem{deps: work}, work.TrashWriter != nil && work.Reader != nil},
		// AND THE BOARD DRAG: the order is a person's arrangement, and a
		// seat moves a card between lanes with update_work_item instead.
		{&placeWorkItem{deps: work}, work.Placer != nil && work.Reader != nil},
		{&listPages{deps: pages}, pages.Reader != nil},
		{&getPage{deps: pages}, pages.Reader != nil},
		{&writePage{deps: pages}, pages.Writer != nil},
		{&savePage{deps: pages}, pages.Writer != nil && pages.Reader != nil},
		{&commentOnPage{deps: pages}, pages.Writer != nil && pages.Reader != nil},
		{&searchKnowledge{search: deps.Knowledge, org: deps.Org},
			deps.Knowledge != nil && deps.Org != nil},
		// A SEAT IS NEVER GIVEN IT: a question a coding run asked is a
		// person's to answer, and a seat that could answer its own run's
		// question would be one guessing on its own behalf.
		{&answerRun{deps: deps.Runs}, deps.Runs.Desk != nil},
		// NOR THESE: whether a seat works is a person's decision about it,
		// and a seat that could pause a colleague — or resume itself —
		// would be one overruling the people who run the company.
		{&pauseSeat{deps: deps.Pauses}, deps.Pauses.Pauses != nil},
		{&resumeSeat{deps: deps.Pauses}, deps.Pauses.Pauses != nil},
		// NOR THIS: a note to a running turn is a person redirecting the
		// work, and a seat that could steer a colleague's turn would be
		// directing it past the person who asked for the work.
		{&steerTurn{deps: deps.Steer}, deps.Steer.Asker != nil},
		// NOR THIS: it is a person's question answered by a model at the
		// company's expense, and a seat has search_knowledge and a model
		// of its own — see answerknowledge.go.
		{newAnswerKnowledge(deps), answerable(deps)},
		// EVERY CHAT GESTURE A PERSON MAKES, because the dashboard makes
		// them through this catalogue (ADR-0024): read the rooms, find
		// what was said, say something, react, move one's own membership
		// — the nine a seat holds — and the furniture only a person
		// arranges, which no seat is given: a room made or its settings
		// changed, its whole membership stated, a message rewritten or
		// taken back, a conversation opened before its first word, and
		// how far one has read. See chatoperator.go for why each is a
		// person's.
		{&listChannels{deps: deps.Chat}, deps.Chat.Reader != nil},
		{&readChannel{deps: deps.Chat}, deps.Chat.Reader != nil},
		{&searchMessages{deps: deps.Chat}, deps.Chat.Search != nil},
		{&postMessage{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&replyInThread{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&sendDM{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&reactToMessage{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&joinChannel{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&leaveChannel{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&editMessage{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&deleteMessage{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&createChannel{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&openDM{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&updateChannel{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&setChannelMembers{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&markChatRead{deps: deps.Chat}, deps.Chat.Cursors != nil},
	}
	var out []tools.Callable
	for _, c := range candidates {
		if c.on {
			out = append(out, c.tool)
		}
	}
	return out
}

// answerable reports whether the operator surface can serve answer_knowledge:
// a search to answer from, a chart to scope it and resolve the asker's seat,
// and a model, a budget and an actor to ask, charge and attribute it.
func answerable(deps OperatorDeps) bool {
	a := deps.Answer
	return deps.Knowledge != nil && deps.Org != nil &&
		a.Models != nil && a.Budget != nil && a.Actor != nil
}

// newAnswerKnowledge builds the answer tool with its ONE cache: the catalogue
// is built once per surface, so this is one cache per node.
func newAnswerKnowledge(deps OperatorDeps) *answerKnowledge {
	return &answerKnowledge{
		search: deps.Knowledge, items: deps.Work.Search, org: deps.Org,
		pages: deps.Pages.Reader, tasks: deps.Work.Reader, deps: deps.Answer,
		cache: newAnswerCache(AnswerCacheEntries),
	}
}
