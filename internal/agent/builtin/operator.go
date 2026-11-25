package builtin

import (
	"slices"

	"github.com/crewlet/crewlet/internal/tools"

	"github.com/crewlet/crewlet/internal/org"
)

// The OPERATOR catalogue: the same tools a seat holds, offered to a person's
// own AI assistant and to the dashboard, which makes every change through it
// (ADR-0024). Each entry is the value [Register] files into a seat's registry;
// only who a write is attributed to differs ([WorkDeps.Actor]), since a second
// copy drifts on trims, defaults and refusals, and only one copy gets tested.
//
// Deliberately absent: the tools that need a turn. The memory tools (a diary is
// a seat's), the skill tools (loaded into a phase), `a2a_ask` (nobody here for
// the answer to come back to) and `lookup_colleague` (resolves against the
// turn's org). A run a seat parked on a question is a person's to answer,
// though: `answer_executor`.

// OperatorDeps are the halves an operator surface can serve.
type OperatorDeps struct {
	Work      WorkDeps
	Pages     PageDeps
	Knowledge KnowledgeSearcher

	// Artifacts and ArtifactData are the artifacts and datastore tools'
	// dependencies on this surface. Each zero value omits its tools.
	Artifacts    ArtifactDeps
	ArtifactData ArtifactDataDeps

	// Code is the code tools' half: the SAME [CodeTools] list a seat's registry
	// takes, its Actor the request's principal, writes only where its Writer
	// is set. The zero value (a company hosting no code) omits them.
	Code CodeDeps

	// Atlas is the atlas tools' half: the settings this node runs, read per
	// call. Gitlet is read through Code, as the caller. The zero value omits
	// the tools.
	Atlas AtlasDeps

	// AtlasWrite is the atlas's write tools' half: its log, who writes and
	// the chart an owner is named from. Each write is decided on the atlas
	// as Atlas reads it. The zero value omits the tools.
	AtlasWrite AtlasWriteDeps

	// AtlasRefresh is what a person asks for a read of one atlas source
	// through. The zero value omits the tool.
	AtlasRefresh AtlasRefreshDeps

	// HomeBoard is what a person saves their Home board through. The zero
	// value omits the tool.
	HomeBoard HomeBoardDeps

	// Instructions is what a person saves their own Ask Crewlet instructions
	// through. The zero value omits the tool.
	Instructions InstructionsDeps

	// Readers is `code_repo_readers`' half beside Code — who read a
	// `visibility: unit` repository — served only where Code serves the
	// code reads ([CodeDeps.serves]). A nil reader omits the tool.
	Readers ReadersDeps

	// ScheduleRuns is what a person runs one schedule now through, off its
	// cron. The zero value omits the tool.
	ScheduleRuns ScheduleRunDeps

	// Health is the health tools' — the SAME [HealthTools] list a seat's
	// registry takes, its code half's Actor the request's principal. The
	// zero value omits both.
	Health HealthDeps

	// Chat is the company's own chat, its [ChatDeps.Actor] the request's
	// principal ([PrincipalChatActor]): a person writes as the `kind: human`
	// seat their credential is bound to, the credential beside it, since a
	// room renders a speaker by seat. A credential acting as itself is refused
	// every chat verb, reads included, by the table (`unseated`) and the tools.
	Chat ChatDeps

	// Org is the company this surface is about, resolved per call because a
	// config apply replaces it. With no turn to read one from, search_knowledge
	// takes it from here, and refuses every call while it is nil.
	Org func() *org.Organization

	// Authorize decides whether the party behind a call may make it: the SAME
	// seam a seat's registry and the HTTP surface use, so a permission an HTTP
	// route honours is honoured here too.
	//
	// NIL REFUSES EVERYTHING, on [Deps.Authorize]'s rule. [OperatorTools]
	// pushes it into Work, Pages and Chat, so a caller cannot set them apart.
	Authorize Authorizer

	// Runs answers a parked executor by its turn — see answerexecutor.go.
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
// company on Jira has no Worklet, and an assistant shown a tool that
// always fails learns to distrust the whole catalogue.
//
// The order is the order an operator's assistant meets them in: read before
// write, work before knowledge. Nothing depends on it, and a stable one means
// two boots of one config advertise the same list.
func OperatorTools(deps OperatorDeps) []tools.Callable {
	deps.Work.Authorize, deps.Pages.Authorize = deps.Authorize, deps.Authorize
	deps.Chat.Authorize = deps.Authorize
	// A CALL HERE IS AN OPERATION ITS CALLER HOLDS, having no turn to derive
	// one from ([operations]). Set here alone, for every write family built
	// below, as a fact about this surface; the artifacts mint on their own clock.
	deps.Work.ops = operations{callerOperations: true}
	deps.Artifacts.ops = operations{callerOperations: true, now: deps.Artifacts.Now}
	deps.Code.ops = operations{callerOperations: true}
	deps.AtlasWrite.ops = operations{callerOperations: true}
	deps.Work.entries = atlasReader{deps: deps.Atlas, code: deps.Code, authorize: deps.Authorize}
	work, pages, talk := deps.Work, deps.Pages, deps.Chat
	candidates := []candidate{
		{&listWorkItems{deps: work}, work.Reader != nil},
		{&getWorkItem{deps: work}, work.Reader != nil},
		{&createWorkItem{deps: work, pages: pages.Reader}, work.Writer != nil},
		{&updateWorkItem{deps: work},
			work.Writer != nil && work.Reader != nil},
		{&commentOnWorkItem{deps: work, pages: pages.Reader},
			work.Writer != nil && work.Reader != nil},
		{&mergeWorkItem{deps: work}, work.Merges != nil && work.Reader != nil},
		{&moveWorkItem{deps: work},
			work.Moves != nil && work.Reader != nil},
		{&searchWorkItems{deps: work}, work.Search != nil},
		// THE VIEW TOOLS ARE HERE AND IN NO SEAT'S REGISTRY. A saved
		// view is furniture a person arranges; see the file head of
		// workviews.go for why a seat is not given them.
		{&listWorkViews{deps: work}, work.Reader != nil},
		{&saveWorkView{deps: work}, work.ViewWriter != nil},
		{&deleteWorkView{deps: work}, work.ViewWriter != nil},
		{&getWorkCatalogue{deps: work}, work.Reader != nil},
		{&writeWorkCatalogue{deps: work}, work.CatalogueWriter != nil},
		{&listProjects{deps: work}, projectReads(work)},
		{&describeProject{deps: work}, projectReads(work)},
		{&taskActivity{deps: work}, feedReads(work)},
		{&myWork{deps: work}, feedReads(work)},
		{&getPerson{deps: work}, work.Reader != nil},
		{&setPriorities{deps: work}, work.PersonWriter != nil},
		{&setPins{deps: work}, work.PersonWriter != nil},
		{&markInbox{deps: work}, work.PersonWriter != nil},
		{&workInbox{deps: work}, work.Inbox != nil},
		{&writeProject{deps: work}, work.ProjectWriter != nil},
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
		// THE ATLAS, read as the caller: every entry to a holder of the app,
		// and the repositories in it only where they use Gitlet (ADR-0127).
		{&atlasSearch{atlasReader{deps: deps.Atlas, code: deps.Code, authorize: deps.Authorize}},
			deps.Atlas.Settings != nil},
		{&atlasGet{atlasReader{deps: deps.Atlas, code: deps.Code, authorize: deps.Authorize}},
			deps.Atlas.Settings != nil},
		{&atlasTrace{atlasReader{deps: deps.Atlas, code: deps.Code, authorize: deps.Authorize}},
			deps.Atlas.Settings != nil},
		// NOR IS A SEAT GIVEN THE ASK THAT A SOURCE BE READ NOW: a read of
		// a vendor is spent against the company's rate limit, so it is a
		// person's to ask for (see atlasrefresh.go).
		{&atlasRefresh{deps: deps.AtlasRefresh}, deps.AtlasRefresh.serves()},
		// NOR A HOME BOARD: a seat has no dashboard to arrange (see homeboard.go).
		{&saveHomeBoard{deps: deps.HomeBoard}, deps.HomeBoard.serves()},
		{&saveInstructions{deps: deps.Instructions}, deps.Instructions.serves()},
		// A SEAT IS NEVER GIVEN IT: a question an executor asked is a
		// person's to answer, and a seat that could answer its own
		// executor's question would be one guessing on its own behalf.
		{&answerExecutor{deps: deps.Runs, authorize: deps.Authorize},
			deps.Runs.Desk != nil},
		// NOR THESE: whether a seat works is a person's decision about it,
		// and a seat that could pause a colleague — or resume itself —
		// would be one overruling the people who run the company.
		{&pauseSeat{deps: deps.Pauses, authorize: deps.Authorize},
			deps.Pauses.Pauses != nil},
		{&resumeSeat{deps: deps.Pauses, authorize: deps.Authorize},
			deps.Pauses.Pauses != nil},
		// NOR THIS: when a seat's scheduled work runs is a person's to
		// decide, and a seat that could run a schedule would be starting
		// its colleagues' turns, or its own, outside the times they set.
		{&runSchedule{deps: deps.ScheduleRuns, authorize: deps.Authorize},
			deps.ScheduleRuns.serves()},
		// NOR THIS: a note to a running turn is a person redirecting the
		// work, and a seat that could steer a colleague's turn would be
		// directing it past the person who asked for the work.
		{&steerTurn{deps: deps.Steer, authorize: deps.Authorize, org: deps.Org},
			deps.Steer.Asker != nil},
		// NOR THIS: it is a person's question answered by a model at the
		// company's expense, and a seat has search_knowledge and a model
		// of its own — see answerknowledge.go.
		{newAnswerKnowledge(deps), answerable(deps)},
		// NOR THIS: who read a unit's repository is an audit, admin's
		// (authz.ActionCodeRepoReaders), and a seat asking it would be a model
		// watching the people it works for (codereaders.go). Served on the code
		// reads' own terms ([CodeDeps.serves]), never Gitlet's alone: an audit
		// of reads not offered would be a code tool served where the rest are not.
		{&codeRepoReaders{code: deps.Code, deps: deps.Readers},
			deps.Code.serves() && deps.Readers.Readers != nil},
		// NOR THIS: a person's marks over their own code notices — a seat
		// is woken, never shown an inbox, so it has none to mark. See
		// codemarks.go. Beside the code writes, on their terms: a surface
		// that changes none of the company's code marks none of it either.
		{&codeMarkInbox{deps: deps.Code}, deps.Code.serves() && deps.Code.Writer != nil},
		// Nor a person's viewed files of a change: a seat reviews a change whole and keeps none.
		{&codeMarkViewed{deps: deps.Code}, deps.Code.serves() && deps.Code.Writer != nil},
		// EVERY CHAT GESTURE A PERSON MAKES, since the dashboard makes them here
		// (ADR-0024): the nine a seat holds, plus the furniture only a person
		// arranges (a room made or changed, its whole membership stated, a
		// message rewritten or taken back, a conversation opened before its
		// first word, how far one has read). chatoperator.go says why.
		{&listChannels{deps: talk}, talk.Reader != nil},
		{&readChannel{deps: talk}, talk.Reader != nil},
		{&searchMessages{deps: talk}, talk.Search != nil},
		{&postMessage{deps: talk}, talk.Writer != nil},
		{&replyInThread{deps: talk}, talk.Writer != nil},
		{&sendDM{deps: talk}, talk.Writer != nil},
		{&reactToMessage{deps: talk}, talk.Writer != nil},
		{&joinChannel{deps: talk}, talk.Writer != nil},
		{&leaveChannel{deps: talk}, talk.Writer != nil},
		// THE THREE DECIDED AFTER A READ need the reader as well: whose
		// words a message is, and who made a room and which unit holds
		// it, are on the rows ([ChatDeps.remarkAuthority],
		// [ChatDeps.archiveAuthority]).
		{&editMessage{deps: talk}, talk.Writer != nil && talk.Reader != nil},
		{&deleteMessage{deps: talk}, talk.Writer != nil && talk.Reader != nil},
		{&createChannel{deps: talk}, talk.Writer != nil},
		{&openDM{deps: talk}, talk.Writer != nil},
		{&updateChannel{deps: talk}, talk.Writer != nil && talk.Reader != nil},
		{&setChannelMembers{deps: talk}, talk.Writer != nil},
		{&addChannelMembers{deps: talk}, talk.Writer != nil},
		{&markChatRead{deps: talk}, talk.Cursors != nil},
	}
	candidates = slices.Concat(candidates,
		artifactOperatorCandidates(deps), artifactDataOperatorCandidates(deps),
		// AND THE ATLAS'S WRITES, which no seat's registry holds: what the
		// company's map says is a person's to correct (see atlaswrite.go).
		atlasWriteCandidates(deps),
		// THE CODE TOOLS, the list a seat's registry takes whole: a person's
		// assistant reads every repository and changes code as the human seat
		// the person holds.
		// An operator holds no turn, so it holds nothing from a scratch
		// repository's expiry.
		codeCandidates(deps.Code),
		// AND THE HEALTH TOOLS, the list a granted seat's registry takes:
		// the deployment as an administrator reads it, which the table
		// keeps an administrator's on this surface.
		healthCandidates(deps.Health))
	var out []tools.Callable
	for _, c := range candidates {
		if c.on {
			// GATED HERE TOO, with the decision a seat's registry is gated
			// with: a catalogue that skipped the wrapper would be the one
			// surface of three that decided nothing.
			out = append(out, gate(c.tool, deps.Authorize, nil))
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

// newAnswerKnowledge builds the answer tool over the node's ONE cache, which
// [AnswerDeps.Cache] carries in: the catalogue is built per request, so a cache
// built here would be one request's. A company that moves to another
// knowledge backend never reads the last one's answers from it, because an
// answer is keyed on the corpus position and only Doclet has one
// ([answerKnowledge.cacheKey]).
func newAnswerKnowledge(deps OperatorDeps) *answerKnowledge {
	return &answerKnowledge{
		search: deps.Knowledge, items: deps.Work.Search, org: deps.Org,
		pages: deps.Pages.Reader, tasks: deps.Work.Reader, deps: deps.Answer,
	}
}
