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
// [WorkDeps.Actor] — a single field — rather than a second copy of ten tools.
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
// exist), and `lookup_colleague` — which would be useful, but resolves
// against the turn's own org and has no other source.
//
// What is left is the surface an operator's assistant actually needs: read
// the board, file and move work, read and write the wiki, search what the
// company knows.

// OperatorDeps are the halves an operator surface can serve.
type OperatorDeps struct {
	Work      WorkDeps
	Pages     PageDeps
	Knowledge KnowledgeSearcher

	// Org is the company this surface is about, resolved per call because
	// a config apply replaces it. An operator has no turn and therefore no
	// org to read from one, so search_knowledge takes it from here — and
	// with it nil that tool refuses every call it is registered for.
	Org func() *org.Organization

	// Authorize decides whether the party behind a call may make it, and
	// it is the SAME seam a seat's registry and the HTTP surface use.
	//
	// It replaced `Leads` and `LeadsProject`, which asked the chart the
	// two relation questions [authz.ClassOwnOrLead] and
	// [authz.ClassContainer] already ask — from the tool rather than from
	// the table, two-valued, and consulting no grant at all, so an
	// operator holding fleet:operate was refused here while every HTTP
	// route allowed.
	//
	// NIL REFUSES EVERYTHING, on [Deps.Authorize]'s rule. Pushed down
	// into [OperatorDeps.Work] and [OperatorDeps.Pages] by
	// [OperatorTools], so a caller cannot set the three differently.
	Authorize Authorizer
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
	deps.Work.Authorize, deps.Pages.Authorize = deps.Authorize, deps.Authorize
	work, pages := deps.Work, deps.Pages
	candidates := []struct {
		tool tools.Callable
		on   bool
	}{
		{&listWorkItems{deps: work}, work.Reader != nil},
		{&getWorkItem{deps: work}, work.Reader != nil},
		{&createWorkItem{deps: work}, work.Writer != nil},
		{&updateWorkItem{deps: work},
			work.Writer != nil && work.Reader != nil},
		{&commentOnWorkItem{deps: work}, work.Writer != nil && work.Reader != nil},
		{&mergeWorkItem{deps: work}, work.Merges != nil && work.Reader != nil},
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
		{&setPriorities{deps: work}, work.PersonWriter != nil},
		{&setPins{deps: work}, work.PersonWriter != nil},
		{&markInbox{deps: work}, work.PersonWriter != nil},
		{&workInbox{deps: work}, work.Inbox != nil},
		{&writeProject{deps: work}, work.ProjectWriter != nil},
		{&removeWorkItem{deps: work}, work.TrashWriter != nil && work.Reader != nil},
		{&restoreWorkItem{deps: work}, work.TrashWriter != nil && work.Reader != nil},
		{&listPages{deps: pages}, pages.Reader != nil},
		{&getPage{deps: pages}, pages.Reader != nil},
		{&writePage{deps: pages}, pages.Writer != nil},
		{&savePage{deps: pages}, pages.Writer != nil && pages.Reader != nil},
		{&commentOnPage{deps: pages}, pages.Writer != nil && pages.Reader != nil},
		{&searchKnowledge{search: deps.Knowledge, org: deps.Org},
			deps.Knowledge != nil && deps.Org != nil},
	}
	var out []tools.Callable
	for _, c := range candidates {
		if c.on {
			// GATED HERE TOO, and with the same decision a seat's
			// registry is gated with. An operator's assistant is a
			// caller like any other: the tools it holds are the same
			// values, and a catalogue that skipped the wrapper would
			// be the one surface of three that decided nothing.
			out = append(out, gate(c.tool, deps.Authorize))
		}
	}
	return out
}
