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
	candidates := []struct {
		tool tools.Callable
		on   bool
	}{
		{&listWorkItems{deps: work}, work.Reader != nil},
		{&getWorkItem{deps: work}, work.Reader != nil},
		{&createWorkItem{deps: work}, work.Writer != nil},
		{&updateWorkItem{deps: work}, work.Writer != nil && work.Reader != nil},
		{&commentOnWorkItem{deps: work}, work.Writer != nil && work.Reader != nil},
		// THE VIEW TOOLS ARE HERE AND IN NO SEAT'S REGISTRY. A saved
		// view is furniture a person arranges; see the file head of
		// workviews.go for why a seat is not given them.
		{&listWorkViews{deps: work}, work.Reader != nil},
		{&saveWorkView{deps: work}, work.ViewWriter != nil},
		{&listWorkGoals{deps: work}, work.Reader != nil},
		{&writeWorkGoal{deps: work}, work.GoalWriter != nil},
		{&getWorkCatalogue{deps: work}, work.Reader != nil},
		{&writeWorkCatalogue{deps: work}, work.CatalogueWriter != nil},
		{&listProjects{deps: work}, projectReads(work)},
		{&describeProject{deps: work}, projectReads(work)},
		{&sprintReport{deps: work}, projectReads(work)},
		{&taskActivity{deps: work}, feedReads(work)},
		{&myWork{deps: work}, feedReads(work)},
		{&getPerson{deps: work}, work.Reader != nil},
		{&setPriorities{deps: work, leads: deps.Leads}, work.PersonWriter != nil},
		{&setPins{deps: work}, work.PersonWriter != nil},
		{&markInbox{deps: work}, work.PersonWriter != nil},
		{&writeProject{deps: work, leads: deps.LeadsProject},
			work.ProjectWriter != nil},
		{&manageSprint{deps: work, leads: deps.LeadsProject},
			work.SprintWriter != nil},
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
			out = append(out, c.tool)
		}
	}
	return out
}
