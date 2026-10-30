package catalog

import (
	"errors"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// The read tools: each one request, to one named read route the dashboard
// itself reads, answered as the person and carded from the route's own JSON.
// Every field a card reads is the answer's own key (internal/api/queries'
// answers), held to the answer's type by the catalogue's card walk where the
// answer is a typed value.

// The column types, short.
const (
	tText     = wire.ColumnText
	tNumber   = wire.ColumnNumber
	tTokens   = wire.ColumnTokens
	tDuration = wire.ColumnDuration
	tDatetime = wire.ColumnDatetime
	tLabel    = wire.ColumnLabel
)

// col is a table column of engine data; prose is one a person or an agent
// wrote.
func col(key, label, path string, typ wire.ColumnType) cards.Column {
	return cards.Column{Key: key, Label: label, Path: path, Type: typ}
}

func prose(key, label, path string) cards.Column {
	return cards.Column{Key: key, Label: label, Path: path, Type: tText, Prose: true}
}

// seat is a column of seat handles, drawn by each seat's name ([cards.Column.Seat]).
func seat(key, label, path string) cards.Column {
	return cards.Column{Key: key, Label: label, Path: path, Type: tText, Seat: true}
}

func fact(label, path string, typ wire.ColumnType) cards.Fact {
	return cards.Fact{Label: label, Path: path, Type: typ}
}

// one is a tool's single card spec.
func one(spec cards.Spec) map[string]cards.Spec { return map[string]cards.Spec{"": spec} }

// by is a tool's card variant, read from one of its enum arguments.
func by(arg, fallback string) func(Args) string {
	return func(a Args) string {
		if v := a.String(arg); v != "" {
			return v
		}
		return fallback
	}
}

// The task columns every task table carries.
var taskColumns = []cards.Column{
	col("key", "Key", "key", tText),
	prose("title", "Title", "title"),
	col("status", "Status", "status", tLabel),
	col("priority", "Priority", "priority", tLabel),
	seat("assignee", "Assignee", "assignee"),
	col("due", "Due", "due", tDatetime),
	col("project", "Project", "project", tText),
}

func taskTable(title, rows string) cards.Spec {
	return cards.Spec{Table: &cards.Table{Title: title, Rows: rows, Columns: taskColumns,
		// A LIST PAGES BY ITS CURSOR, present only while more lie past it.
		More: "next_cursor",
		// THE KEY IS DRAWN: it is what the answer and the person name a task
		// by, and every other column stays on the card for the downloads and
		// the model.
		Brief: []string{"key", "title", "status", "assignee"},
		Open:  &cards.Open{Screen: "worklet-tasks"}}}
}

var workletApp = []iam.Grant{iam.GrantWorklet}
var docletApp = []iam.Grant{iam.GrantDoclet}
var adminOnly = []iam.Grant{iam.GrantAdmin}

// statuses and priorities are Worklet's own closed sets (internal/worklet),
// which Worklet refuses anything outside.
var (
	statuses   = []string{"todo", "in_progress", "in_review", "done", "cancelled", "closed"}
	priorities = []string{"none", "low", "normal", "high", "urgent"}
)

// readTools are the read tools, in the order a model meets them.
func readTools() []Tool {
	return []Tool{
		{
			Name: "search_work", Phrase: "search tasks",
			Narrow: []string{"q"},
			Description: "Find tasks by what they are about, ranked. Search before proposing a " +
				"new task, so you do not file a duplicate.",
			schema: object([]string{"q"}, map[string]*schema{
				"q":     text("What the work is about, in plain words.", queryBytes),
				"limit": positive("How many results, at most.", 25),
			}),
			Routes: []string{"GET /worklet/search"}, Apps: workletApp, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /worklet/search", nil,
					query(a).str("q", "q").num("limit", "limit", 10).values())
			},
			// A SEARCH THAT COULD NOT RUN SAYS WHY in its own `note` (an index
			// still building), as search_knowledge's does, rather than reading
			// to the model as "nothing matched".
			Cards: one(cards.Spec{Note: "note", Table: &cards.Table{Title: "Matching tasks", Rows: "hits",
				Brief: []string{"key", "title", "status", "assignee"},
				Columns: []cards.Column{col("key", "Key", "key", tText),
					prose("title", "Title", "title"), col("status", "Status", "status", tLabel),
					seat("assignee", "Assignee", "assignee"),
					col("project", "Project", "project", tText),
					prose("snippet", "Matched", "snippet")},
				Open: &cards.Open{Screen: "worklet-search"}}}),
			Label: func(map[string]any) string { return "Searching tasks" },
			Scope: quoted,
		},
		{
			Name: "list_tasks", Phrase: "list tasks",
			Narrow: []string{"q", "assignee", "project", "status", "priority", "type", "tag", "due"},
			Description: "List tasks by their fields: what is open, overdue or due soon, assigned to " +
				"somebody, in a status, of a priority or a type, in a project. Lists are filtered " +
				"to what the person may see.",
			schema: object(nil, map[string]*schema{
				"project":     text("A project's key or name, ENG for example.", nameBytes),
				"assignee":    text("A seat's handle or name.", nameBytes),
				"status":      {kind: "array", description: "Statuses to keep.", maxItems: len(statuses), items: oneOf("", statuses...)},
				"priority":    oneOf("A priority to keep.", priorities...),
				"type":        text("A task type from the workspace's catalogue.", nameBytes),
				"tag":         text("A tag the project declares.", nameBytes),
				"due":         oneOf("When the work is due.", "overdue", "next7", "thisweek", "thismonth", "last7", "earlier"),
				"q":           text("Words the title must contain.", queryBytes),
				"show_closed": flag("Include finished work, which is left out unless asked."),
				"limit":       positive("How many tasks, at most.", 50),
			}),
			Routes: []string{"GET /worklet"}, Apps: workletApp, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				args := Args(a)
				q := query(args).str("assignee", "assignee").list("status", "status").
					str("priority", "priority").str("type", "type").str("tag", "tag").
					str("due", "due").str("q", "q").truth("show_closed", "show_closed").
					num("limit", "limit", 25)
				if p := args.String("project"); p != "" {
					q.fixed("container", "project:"+p)
				}
				return get("GET /worklet", nil, q.values())
			},
			Cards: one(taskTable("Tasks", "items")),
			Label: func(map[string]any) string { return "Listing tasks" },
			Scope: taskScope,
			Names: map[string]Directory{"project": DirProject, "assignee": DirSeat},
		},
		{
			Name: "get_task", Phrase: "read this task",
			Description: "Read one task: its description, status, people, dates and the version " +
				"an update proposes against. Its comments are get_task_thread.",
			schema: object([]string{"item"}, map[string]*schema{
				"item": text("The task's key (ENG-42) or id.", nameBytes),
			}),
			Routes: []string{"GET /worklet/{id}"}, Apps: workletApp, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /worklet/{id}", map[string]string{"id": Args(a).String("item")}, nil)
			},
			Cards: one(cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceTask, Root: "task",
				ID: "key", Key: "key", Title: "title", Subtitle: "key", Prose: true, Text: "body",
				Facts: []cards.Fact{fact("Status", "status", tLabel),
					fact("Priority", "priority", tLabel), fact("Assignee", "assignee", tText),
					{Label: "Reporter", Path: "reporter", Type: tText, Detail: true},
					fact("Project", "project", tText),
					fact("Type", "type", tLabel), fact("Due", "due_at", tDatetime),
					fact("Updated", "updated_at", tDatetime),
					{Label: "Version", Path: "version", Type: tNumber, Mono: true, Detail: true}},
				Open: &cards.Open{Screen: "item", Params: map[string]string{"id": "id"},
					LabelFrom: "key"}}}),
			Label: func(a map[string]any) string { return "Reading " + Args(a).String("item") },
		},
		{
			Name: "get_task_thread", Phrase: "read this task's comments",
			Description: "Read a task's comments, newest last, with any question somebody asked " +
				"on it.",
			schema: object([]string{"item"}, map[string]*schema{
				"item":  text("The task's key or id.", nameBytes),
				"limit": positive("How many comments, at most.", 50),
			}),
			Routes: []string{"GET /worklet/comments"}, Apps: workletApp, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /worklet/comments", nil,
					query(a).str("item", "item").num("limit", "limit", 20).values())
			},
			Cards: one(cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceTask, ID: "key",
				Key: "key", Title: "title", Prose: true,
				Items: &cards.Items{Path: "comments", Kind: wire.ResourceTask, ID: "id",
					Label: "body", Fact: "author", Prose: true, More: "next_cursor",
					Noun: wire.Noun{One: "comment", Many: "comments"}},
				Open: &cards.Open{Screen: "item", Params: map[string]string{"id": "item"},
					LabelFrom: "key"}}}),
			Label: func(a map[string]any) string {
				return "Reading the comments on " + Args(a).String("item")
			},
		},
		{
			Name: "get_task_cost", Phrase: "read what this task cost",
			Description: "The agent turns spent on one task: who ran each, its outcome, tokens " +
				"and time.",
			schema: object([]string{"item"}, map[string]*schema{
				"item":  text("The task's key or id.", nameBytes),
				"limit": positive("How many turns, at most.", 50),
			}),
			Routes: []string{"GET /worklet/turns"}, Apps: workletApp, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /worklet/turns", nil,
					query(a).str("item", "id").num("limit", "limit", 20).values())
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Turns on the task", Rows: "turns",
				More: "next_cursor", Brief: []string{"seat", "outcome", "tokens"},
				Columns: []cards.Column{col("seat", "Seat", "seat", tText),
					col("outcome", "Outcome", "outcome", tLabel),
					col("tokens", "Tokens", "tokens", tTokens),
					col("rounds", "Rounds", "rounds", tNumber),
					col("wall", "Time", "wall_ms", tDuration),
					col("at", "When", "at", tDatetime), prose("summary", "Summary", "summary")}}}),
			Label: func(map[string]any) string { return "Reading what a task cost" },
			// THE TASK, IN THE TITLE: the turns on two tasks are two reads, never
			// one table the drawer keeps once.
			Scope: func(a Args) string { return a.String("item") },
		},
		{
			Name: "get_my_work", Phrase: "read your work",
			Narrow: []string{"unread"},
			Description: "The person's own work: what is assigned to them, their priority order, " +
				"the questions asked of them, or their task notices (inbox: mentions, assignments " +
				"and comments, the Inbox screen's Updates). The Inbox screen's Needs you is not " +
				"here: read list_decisions and list_agents for it.",
			schema: object(nil, map[string]*schema{
				"section": oneOf("Which part. Default assigned. inbox is the task notices.", "assigned", "priorities", "asked", "inbox"),
				"unread":  flag("Inbox only: just what is unread."),
			}),
			Routes: []string{"GET /worklet/my-work", "GET /worklet/inbox"}, Apps: workletApp, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				if Args(a).String("section") == "inbox" {
					// ONE PAGE OF THE INBOX, its cursor saying more lie past it:
					// a person's notices grow with the company, and read whole
					// they pass what one step reads.
					return get("GET /worklet/inbox", nil,
						query(a).truth("unread", "unread").num("limit", "limit", 25).values())
				}
				return get("GET /worklet/my-work", nil, nil)
			},
			Variant: by("section", "assigned"),
			Cards: map[string]cards.Spec{
				"assigned":   taskTable("Assigned to you", "assigned"),
				"priorities": taskTable("Your priorities", "priorities"),
				"asked": {Table: &cards.Table{Title: "Asked of you", Rows: "asked_of_me",
					Brief: []string{"title", "asked_by", "asked_at"},
					Columns: []cards.Column{col("key", "Key", "key", tText),
						prose("title", "Title", "title"),
						seat("asked_by", "Asked by", "asked_by"),
						col("asked_at", "Asked", "asked_at", tDatetime),
						prose("question", "Question", "body")},
					Open: &cards.Open{Screen: "me"}}},
				"inbox": {Table: &cards.Table{Title: "Inbox", Rows: "notices",
					Brief: []string{"excerpt", "reason", "at"},
					Columns: []cards.Column{col("subject", "Item", "subject_key", tText),
						col("reason", "Why", "reason", tLabel),
						seat("actor", "By", "actor"), prose("excerpt", "What", "excerpt"),
						col("at", "When", "at", tDatetime), col("read", "Read", "read", tText),
						col("record", "Record", "record_id", tText)},
					More: "next_cursor", Open: &cards.Open{Screen: "inbox"}}},
			},
			Label: func(map[string]any) string { return "Reading your work" },
			Scope: func(a Args) string {
				if a.String("section") == "inbox" && a.Bool("unread") {
					return "unread only"
				}
				return ""
			},
		},
		{
			Name: listProjectsTool, Phrase: "list projects",
			Description: "The projects: each one's lead, unit, target date and open counts, or " +
				"one project with its key.",
			schema: object(nil, map[string]*schema{
				"key": text("One project's key or name, for that project alone.", nameBytes),
			}),
			Routes: []string{"GET /worklet/projects", "GET /worklet/projects/{key}"}, Apps: workletApp,
			Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				if key := Args(a).String("key"); key != "" {
					return get("GET /worklet/projects/{key}", map[string]string{"key": key}, nil)
				}
				// ONE PAGE OF PROJECTS, `truncated` saying more lie past it.
				return get("GET /worklet/projects", nil, url.Values{"limit": {"25"}})
			},
			Variant: func(a Args) string {
				if a.String("key") != "" {
					return "one"
				}
				return ""
			},
			Cards: map[string]cards.Spec{
				"": {Table: &cards.Table{Title: "Projects", Rows: "projects", More: "truncated",
					Brief: []string{"name", "active", "todo", "done"},
					Columns: []cards.Column{col("key", "Key", "key", tText),
						prose("name", "Name", "name"), seat("lead", "Lead", "lead.handle"),
						col("unit", "Unit", "unit.name", tText),
						col("target", "Target", "target_date", tText),
						col("todo", "To do", "task_counts.todo", tNumber),
						col("active", "Active", "task_counts.active", tNumber),
						col("done", "Done", "task_counts.done", tNumber)},
					Open: &cards.Open{Screen: "worklet-projects"}}},
				"one": {Entity: &cards.Entity{Kind: wire.ResourceProject, ID: "key",
					Key: "key", Title: "name", Subtitle: "purpose", Prose: true,
					Facts: []cards.Fact{fact("Lead", "lead.handle", tText),
						fact("Unit", "unit.name", tText), fact("Target", "target_date", tText),
						fact("To do", "task_counts.todo", tNumber),
						fact("Active", "task_counts.active", tNumber),
						fact("Done", "task_counts.done", tNumber)},
					Open: &cards.Open{Screen: "project", Params: map[string]string{"key": "key"},
						Label: "Open the project"}}},
			},
			Label: func(map[string]any) string { return "Listing projects" },
			Names: map[string]Directory{"key": DirProject},
		},
		{
			Name: "get_workload", Phrase: "read the workload",
			Narrow: []string{"unit"},
			Description: "Open work per assignee, agents and people alike: how much each holds, " +
				"blocked and overdue.",
			schema: object(nil, map[string]*schema{
				"unit": text("A unit's key or name, for that team alone.", nameBytes),
			}),
			Routes: []string{"GET /worklet/workload"}, Apps: workletApp,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /worklet/workload", nil, query(a).str("unit", "unit").values())
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Workload", Rows: "rows",
				Brief: []string{"handle", "open", "blocked", "overdue"},
				Columns: []cards.Column{seat("handle", "Assignee", "handle"),
					col("open", "Open", "open", tNumber), col("blocked", "Blocked", "blocked", tNumber),
					col("overdue", "Overdue", "overdue", tNumber),
					col("points", "Points", "points", tNumber),
					col("unscheduled", "Unscheduled", "unscheduled", tNumber)},
				Open: &cards.Open{Screen: "roster"}}}),
			Label: func(map[string]any) string { return "Reading the workload" },
			Scope: func(a Args) string { return labelled(a, "unit", "for") },
			Names: map[string]Directory{"unit": DirUnit},
		},
		{
			Name: "list_decisions", Phrase: "list what waits on you",
			Description: "What is waiting on the person: questions asked of them on tasks, and " +
				"coding runs parked on a question for them. With list_agents' stopped seats, this " +
				"is the Inbox screen's Needs you.",
			schema: object(nil, map[string]*schema{}),
			Routes: []string{"GET /worklet/decisions"}, Fence: true,
			// An ask is a colleague's; a coding run's is not, and is read by
			// the one rule for a coding run's words (provenance.CodingRun).
			TrustOf: outsideWhere(decisionRows, runAskField),
			Build: func(map[string]any) (exec.Request, error) {
				return get("GET /worklet/decisions", nil, nil)
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Waiting on you", Rows: "items",
				Brief: []string{"title", "kind", "at"},
				Columns: []cards.Column{col("kind", "Kind", "kind", tLabel),
					col("at", "Since", "at", tDatetime), col("key", "Task", "ask.key", tText),
					prose("title", "Title", "ask.title"), prose("question", "Question", "ask.body"),
					seat("asked_by", "Asked by", "ask.asked_by"),
					seat("agent", "Coding run", "run.agent_handle"),
					prose("run_question", "Run question", "run.question")},
				Open: &cards.Open{Screen: "inbox"}}}),
			Label: func(map[string]any) string { return "Reading what waits on you" },
		},
		{
			Name: "list_agents", Phrase: "list the agents",
			Description: "Every seat with what it is doing now: working, idle, or stopped and " +
				"why (paused, and by whom and why; out of budget; no node; its model unavailable), " +
				"its current phase, its last turn's outcome and the last error it hit. A stopped " +
				"seat is listed in the Inbox screen's Needs you.",
			schema: object(nil, map[string]*schema{}),
			// FENCED: a pause's reason is its author's words, and a seat's
			// last error can carry a provider's.
			Routes: []string{"GET /agents"}, Fence: true,
			Build: func(map[string]any) (exec.Request, error) {
				return get("GET /agents", nil, nil)
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Agents",
				Brief: []string{"role", "activity", "task"},
				Columns: []cards.Column{col("handle", "Handle", "handle", tText),
					col("role", "Seat", "role", tText), col("activity", "Activity", "activity", tLabel),
					col("phase", "Phase", "current_phase", tLabel),
					col("task", "Task", "turn.work_item.key", tText),
					col("last", "Last turn", "last_turn.outcome", tLabel),
					col("ended", "Ended", "last_turn.ended_at", tDatetime),
					// WHY A SEAT IS STOPPED, and what it last hit: "all four
					// seats are stopped" with no reason was once the answer.
					col("stopped", "Why stopped", "stopped_reason", tLabel),
					seat("paused_by", "Paused by", "paused.by"),
					prose("paused_reason", "Pause reason", "paused.reason"),
					// BY ITS KIND: a failure's own text names the
					// company's model entries, which only an admin reads.
					col("last_error", "Last error", "last_error.kind", tLabel)},
				Open: &cards.Open{Screen: "roster"}}}),
			Label: func(map[string]any) string { return "Listing the agents" },
		},
		{
			Name: "get_agent", Phrase: "read this agent",
			Narrow: []string{"days"},
			Description: "One seat now (its activity, phase and last turn), or every seat's turns " +
				"per day over a window.",
			schema: object(nil, map[string]*schema{
				"handle": text("The seat's handle or name. Required for the seat's detail.", nameBytes),
				"view":   oneOf("detail (the default) or activity.", "detail", "activity"),
				"days":   positive("Activity only: the window in days.", 90),
			}),
			Routes: []string{"GET /agents/{id}", "GET /agents/activity"},
			Build: func(a map[string]any) (exec.Request, error) {
				args := Args(a)
				if args.String("view") == "activity" {
					return get("GET /agents/activity", nil,
						query(args).str("handle", "seat").num("days", "days", 7).values())
				}
				if args.String("handle") == "" {
					return exec.Request{}, errors.New("a seat's detail names the seat's handle")
				}
				// THE SEAT ALONE: its call history is whole transcripts,
				// past what a step reads, and a detail card reads none of it.
				return get("GET /agents/{id}", map[string]string{"id": args.String("handle")}, seatAlone)
			},
			Variant: by("view", "detail"),
			Cards: map[string]cards.Spec{
				"detail": {Entity: &cards.Entity{Kind: wire.ResourceSeat, ID: "handle",
					Title: "role", Subtitle: "handle",
					Facts: []cards.Fact{fact("Activity", "live.activity", tLabel),
						fact("Why stopped", "live.stopped_reason", tLabel),
						fact("Phase", "live.current_phase", tLabel),
						fact("Last turn", "live.last_turn.outcome", tLabel),
						fact("Last ended", "live.last_turn.ended_at", tDatetime),
						fact("Last error", "live.last_error.kind", tLabel)},
					// LABELLED, NOT BY ITS HANDLE: the first parameter's value
					// was the link's text, a handle the person does not know.
					Open: &cards.Open{Screen: "seat", Params: map[string]string{"handle": "handle"},
						Label: "Open the seat"}}},
				"activity": {Table: &cards.Table{Title: "Turns per seat", Rows: "seats",
					Brief: []string{"role", "turns", "failed", "tokens"},
					Columns: []cards.Column{col("handle", "Handle", "handle", tText),
						col("role", "Seat", "role", tText), col("turns", "Turns", "turns", tNumber),
						col("failed", "Failed", "failed", tNumber),
						col("first_pass", "First pass %", "first_pass_pct", tNumber),
						col("p50", "Median time", "p50_ms", tDuration),
						col("tokens", "Tokens", "tokens", tTokens)},
					Open: &cards.Open{Screen: "roster"}}},
			},
			Label: func(a map[string]any) string {
				if Args(a).String("view") == "activity" {
					return "Reading turns per seat"
				}
				return "Reading " + Args(a).String("handle")
			},
			Scope: func(a Args) string {
				if a.String("view") != "activity" {
					return ""
				}
				return scopeOf(lastDays(a, 7), labelled(a, "handle", "for"))
			},
			Names: map[string]Directory{"handle": DirSeat},
		},
		{
			Name: "get_org_chart", Phrase: "read the org chart",
			Description: "The company's units with their purposes and leads, or every human " +
				"seat with whoever holds it.",
			schema: object(nil, map[string]*schema{
				"view": oneOf("chart (the default) or seats.", "chart", "seats"),
			}),
			Routes: []string{"GET /org", "GET /iam/seats"}, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				if Args(a).String("view") == "seats" {
					return get("GET /iam/seats", nil, nil)
				}
				return get("GET /org", nil, nil)
			},
			Variant: by("view", "chart"),
			Cards: map[string]cards.Spec{
				"chart": {Entity: &cards.Entity{Kind: wire.ResourceUnit, Title: "name",
					Subtitle: "mission", Prose: true,
					Items: &cards.Items{Path: "units", Kind: wire.ResourceUnit, ID: "id",
						Label: "name", Fact: "lead", Prose: true,
						Noun: wire.Noun{One: "unit", Many: "units"}},
					Open: &cards.Open{Screen: "org-chart"}}},
				"seats": {Table: &cards.Table{Title: "Human seats", Rows: "seats",
					Brief: []string{"name", "holder", "unit"},
					Columns: []cards.Column{col("handle", "Handle", "handle", tText),
						col("name", "Seat", "name", tText), col("unit", "Unit", "unit", tText),
						col("holder", "Held by", "holder.name", tText),
						col("kind", "Kind", "holder.kind", tLabel)},
					Open: &cards.Open{Screen: "org-chart"}}},
			},
			Label: func(map[string]any) string { return "Reading the org chart" },
		},
		{
			Name: "list_turns", Phrase: "list this seat's turns",
			Narrow: []string{"handle", "failed", "days"},
			Description: "One seat's recent turns: outcome, model, tokens and duration, newest " +
				"first, and for a failed turn why it failed. Rows the person may not read are " +
				"left out.",
			schema: object([]string{"handle"}, map[string]*schema{
				"handle": text("The seat's handle or name.", nameBytes),
				"failed": flag("Only the turns that failed."),
				"days":   positive("The window in days.", 30),
				"limit":  positive("How many turns, at most.", 50),
			}),
			Routes: []string{"GET /turns"}, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /turns", nil, query(a).str("handle", "seat").
					truth("failed", "failed").num("days", "days", 0).num("limit", "limit", 25).values())
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Turns", Rows: "turns", More: "next",
				Brief: []string{"started", "task", "tokens", "error"},
				Columns: []cards.Column{col("role", "Seat", "role", tText),
					col("started", "Started", "started_at", tDatetime),
					col("duration", "Duration", "duration_ms", tDuration),
					col("tokens", "Tokens", "total_tokens", tTokens),
					col("failed", "Failed", "failed", tText),
					// WHY IT FAILED, off the row (store.Turn.Error): the one
					// fact a question about a failed turn is asking.
					prose("error", "Why it failed", "error"),
					col("error_kind", "Failure kind", "error_kind", tLabel),
					col("models", "Models", "models", tText),
					col("task", "Task", "work_item.key", tText),
					prose("summary", "Summary", "summary"),
					col("turn", "Turn", "turn_id", tText)},
				Open: &cards.Open{Screen: "turns"}}}),
			Label: func(map[string]any) string { return "Listing the turns" },
			// THE SEAT, IN THE TITLE: two seats' turns are two reads, never one
			// table the drawer keeps once.
			Scope: func(a Args) string {
				failedOnly := ""
				if a.Bool("failed") {
					failedOnly = "failed only"
				}
				return scopeOf(labelled(a, "handle", "for"), failedOnly, lastDays(a, 0))
			},
			Names: map[string]Directory{"handle": DirSeat},
		},
		{
			Name: "list_coding_runs", Phrase: "list coding runs",
			Description: "Coding runs in flight and recent, including any parked on a question.",
			schema:      object(nil, map[string]*schema{}),
			Routes:      []string{"GET /sandbox-runs"}, Fence: true,
			// A RUN'S QUESTION IS THE CODING CLI'S, a third party's model
			// working from a brief that may be an outside chat message or a
			// webhook's payload, read by the one rule for a coding run's
			// words (provenance.CodingRun) wherever it is read.
			TrustOf: outsideWhere("runs", "question"),
			Build: func(map[string]any) (exec.Request, error) {
				return get("GET /sandbox-runs", nil, nil)
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Coding runs", Rows: "runs",
				Brief: []string{"agent", "status", "task", "started"},
				Columns: []cards.Column{seat("agent", "Seat", "agent_handle"),
					// THE TASK IS THE ITEM THE LAUNCHING TURN WAS CHARGED TO
					// (`work_item`); `work_key` is the turn's delivery identity
					// (internal/workkey), which no person reads as a task.
					col("status", "Status", "status", tLabel),
					col("task", "Task", "work_item.key", tText),
					col("coding_agent", "Agent", "coding_agent", tText),
					col("started", "Started", "started_at", tDatetime),
					col("updated", "Updated", "updated_at", tDatetime),
					prose("question", "Question", "question"), col("turn", "Turn", "turn_id", tText)},
				Open: &cards.Open{Screen: "live"}}}),
			Label: func(map[string]any) string { return "Listing coding runs" },
		},
		{
			Name: "get_spend", Phrase: "read token spend",
			Narrow: []string{"handle", "days"},
			Description: "Token spend over a window of company days, by agent, by model or by " +
				"phase. By agent also lists each person's own Ask Crewlet use, marked as a " +
				"person's: say so, and leave it out when ranking the agents.",
			schema: object(nil, map[string]*schema{
				"days":   positive("The window in company days. Default 7.", 90),
				"by":     oneOf("How to break it down. Default agent.", "agent", "model", "phase"),
				"handle": text("One seat's handle or name, for that seat alone.", nameBytes),
			}),
			Routes: []string{"GET /tokens/breakdown"},
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /tokens/breakdown", nil,
					query(a).num("days", "days", 7).str("handle", "seat").values())
			},
			Variant: by("by", "agent"),
			Cards: map[string]cards.Spec{
				// THE SEAT BY ITS NAME OFF ITS HANDLE, not the name the
				// usage rows recorded (which can be empty, and stale after a
				// rename), and A PERSON'S OWN USE MARKED: what the assistant
				// spent for a person is on this list beside the agents, and
				// was ranked as one.
				"agent": spendTable("Spend by agent", "by_agent",
					seat("seat", "Seat", "handle"),
					col("person", "A person's own use", "person", tText),
					col("handle", "Handle", "handle", tText)),
				"model": spendTable("Spend by model", "by_model", col("model", "Model", "model", tText)),
				"phase": spendTable("Spend by phase", "by_phase", col("phase", "Phase", "phase", tLabel)),
			},
			Label: func(map[string]any) string { return "Reading token spend" },
			Scope: func(a Args) string {
				return scopeOf(lastDays(a, 7), labelled(a, "handle", "for"))
			},
			Names: map[string]Directory{"handle": DirSeat},
		},
		{
			Name: "get_spend_series", Phrase: "read spend over time",
			Narrow: []string{"days"},
			Description: "Token spend per day or week over a window, the company's whole, or its " +
				"largest groups. Use bucket week to compare this week with last week in one read.",
			schema: object(nil, map[string]*schema{
				"days":   positive("The window in company days. Default 30.", 90),
				"bucket": oneOf("day (the default) or week.", "day", "week"),
				"view":   oneOf("over_time (the default) or by_group.", "over_time", "by_group"),
				"group": oneOf("by_group only: what to group by. provider (the company's model entries) "+
					"is answered to an admin alone and refused to anyone else.",
					"phase", "model", "provider", "seat", "unit", "worker"),
			}),
			Routes: []string{"GET /tokens/series"},
			// THE GROUP ONLY WHERE THE VIEW IS ONE: it groups by_group alone,
			// and sent beside the series it asks nothing of the rows drawn,
			// and refuses an over-time read by provider to anyone but an
			// administrator.
			Build: func(a map[string]any) (exec.Request, error) {
				q := query(a).num("days", "days", 30).str("bucket", "bucket")
				if Args(a).String("view") == "by_group" {
					q = q.str("group", "group")
				}
				return get("GET /tokens/series", nil, q.values())
			},
			Variant: by("view", "over_time"),
			Cards: map[string]cards.Spec{
				"over_time": {Table: &cards.Table{Title: "Spend over time", Rows: "series",
					Brief: []string{"at", "total", "calls"},
					Columns: []cards.Column{col("at", "From", "at", tText),
						col("total", "Tokens", "total_tokens", tTokens),
						col("input", "Input", "input_tokens", tTokens),
						col("output", "Output", "output_tokens", tTokens),
						col("calls", "Calls", "calls", tNumber)},
					Open: &cards.Open{Screen: "spend"}}},
				"by_group": {Table: &cards.Table{Title: "Spend by group", Rows: "by_group",
					Brief: []string{"label", "total", "calls"},
					Columns: []cards.Column{col("group", "Group", "group", tText),
						col("label", "Name", "label", tText),
						col("total", "Tokens", "total_tokens", tTokens),
						col("calls", "Calls", "calls", tNumber)},
					Open: &cards.Open{Screen: "spend"}}},
			},
			Label: func(map[string]any) string { return "Reading spend over time" },
			Scope: func(a Args) string {
				bucket := ""
				if a.String("bucket") == "week" {
					bucket = "by week"
				}
				group := ""
				if a.String("view") == "by_group" {
					group = labelled(a, "group", "by")
				}
				return scopeOf(lastDays(a, 30), bucket, group)
			},
		},
		{
			Name: "list_budgets", Phrase: "read the budgets",
			Description: "The company's and each agent seat's budget windows: the limit, what is " +
				"used, and when each resets.",
			schema: object(nil, map[string]*schema{
				"scope": oneOf("agents (the default) or company.", "agents", "company"),
			}),
			Routes: []string{"GET /budgets"},
			Build: func(map[string]any) (exec.Request, error) {
				return get("GET /budgets", nil, nil)
			},
			Variant: by("scope", "agents"),
			Cards: map[string]cards.Spec{
				"agents": {Table: &cards.Table{Title: "Agent budgets", Rows: "seats",
					Expand: "windows",
					Brief:  []string{"role", "period", "used", "limit"},
					Columns: []cards.Column{col("handle", "Handle", "parent.handle", tText),
						col("role", "Seat", "parent.role", tText),
						col("period", "Window", "period", tLabel), col("used", "Used", "used", tTokens),
						col("limit", "Limit", "limit", tTokens), col("state", "State", "state", tLabel),
						col("resets", "Resets", "resets_at", tDatetime)},
					Open: &cards.Open{Screen: "budgets"}}},
				"company": {Table: &cards.Table{Title: "Company budget", Rows: "org.windows",
					Brief: []string{"period", "used", "limit", "state"},
					Columns: []cards.Column{col("period", "Window", "period", tLabel),
						col("used", "Used", "used", tTokens), col("limit", "Limit", "limit", tTokens),
						col("state", "State", "state", tLabel),
						col("resets", "Resets", "resets_at", tDatetime)},
					Open: &cards.Open{Screen: "budgets"}}},
			},
			Label: func(map[string]any) string { return "Reading the budgets" },
		},
		{
			Name: searchKnowledgeTool, Phrase: "search the knowledge base",
			Narrow:      []string{"q"},
			Description: "Search the company's pages, ranked, with the passage each matched on.",
			schema: object([]string{"q"}, map[string]*schema{
				"q": text("What to find, in plain words.", queryBytes),
			}),
			// OFFERED AS THE KNOWLEDGE QUESTION ANSWERS: to a reader who
			// sees pages or tasks (it searches both), not on one app.
			Routes: []string{"GET /query/{what}"}, Fence: true,
			// THE NATIVE KNOWLEDGE BASE IS WRITTEN BY THE COMPANY; a connected
			// one (Confluence) by whoever its own users are.
			TrustOf: func(_ Args, answer any) provenance.Trust { return knowledgeTrust(answer) },
			Shows:   []authz.ObjectKind{authz.KindPage, authz.KindTask},
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /query/{what}", map[string]string{"what": "knowledge"},
					query(a).str("q", "q").values())
			},
			// A SEARCH THAT COULD NOT RUN SAYS WHY in its own `note` (no
			// access to the pages, no backend, an index still building), which
			// an empty answer hands the model rather than "no rows".
			Cards: one(cards.Spec{Note: "note", Table: &cards.Table{Title: "Pages", Rows: "hits",
				Brief: []string{"title", "container"},
				Columns: []cards.Column{prose("title", "Title", "title"),
					col("container", "Space", "container", tText),
					prose("snippet", "Matched", "snippet"), col("id", "Page", "id", tText)},
				Open: &cards.Open{Screen: "doclet"}}}),
			Label: func(map[string]any) string { return "Searching the knowledge base" },
			Scope: quoted,
		},
		{
			Name: "list_spaces", Phrase: "list the knowledge spaces",
			Description: "The knowledge base's spaces, each with its key and its name. A page " +
				"lives in one; create_page takes either.",
			schema: object(nil, map[string]*schema{}),
			Routes: []string{"GET /containers"}, Apps: docletApp, Fence: true,
			Build: func(map[string]any) (exec.Request, error) {
				return get("GET /containers", nil, nil)
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Spaces", Rows: "containers",
				Brief: []string{"name", "key", "purpose"},
				Columns: []cards.Column{prose("name", "Name", "name"),
					col("key", "Key", "key", tText), prose("purpose", "Purpose", "purpose")},
				Open: &cards.Open{Screen: "doclet"}}}),
			Label: func(map[string]any) string { return "Listing the knowledge spaces" },
		},
		{
			Name: getPageTool, Phrase: "read this page",
			Description: "Read one knowledge page whole: its body, labels, comments and the " +
				"version an edit proposes against.",
			schema: object([]string{"page"}, map[string]*schema{
				"page": text("The page's id.", nameBytes),
			}),
			Routes: []string{"GET /doclet/pages/{id}"}, Apps: docletApp, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /doclet/pages/{id}", map[string]string{"id": Args(a).String("page")}, nil)
			},
			Cards: one(cards.Spec{Entity: &cards.Entity{Kind: wire.ResourcePage, Root: "page",
				ID: "id", Title: "title", Subtitle: "container", Prose: true, Text: "body",
				Facts: []cards.Fact{fact("Status", "status", tLabel), fact("Author", "author", tText),
					fact("Labels", "labels", tText), fact("Updated", "updated_at", tDatetime),
					{Label: "Version", Path: "version", Type: tNumber, Mono: true, Detail: true}},
				Items: &cards.Items{Path: "comments", Kind: wire.ResourcePage, ID: "id",
					Label: "body", Fact: "author", Prose: true,
					Noun: wire.Noun{One: "comment", Many: "comments"}},
				// THE LINK SAYS WHERE IT GOES: labelled by the title, it
				// drew the page's title twice, once as the card's heading.
				Open: &cards.Open{Screen: "page", Params: map[string]string{"id": "id"},
					Label: "Open the page"}}}),
			Label: func(map[string]any) string { return "Reading a page" },
		},
		{
			Name: "get_company_feed", Phrase: "read the company's activity",
			Description: "What the company did recently, across tasks, pages and schedules, " +
				"newest first. Rows the person may not see are left out.",
			schema: object(nil, map[string]*schema{
				"limit": positive("How many entries, at most.", 50),
			}),
			Routes: []string{"GET /feed"}, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /feed", nil, query(a).num("limit", "limit", 20).values())
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Recent activity", Rows: "rows",
				More: "next_cursor", Brief: []string{"kind", "title", "page", "at"},
				Columns: []cards.Column{col("kind", "Kind", "kind", tLabel),
					col("at", "When", "at", tDatetime), col("task", "Task", "work.key", tText),
					prose("title", "Task title", "work.title"), seat("by", "By", "work.actor"),
					prose("page", "Page", "page.title"), seat("page_by", "Page by", "page.actor"),
					col("schedule", "Schedule", "schedule.name", tText),
					col("outcome", "Outcome", "schedule.outcome", tLabel)},
				Open: &cards.Open{Screen: "home"}}}),
			Label: func(map[string]any) string { return "Reading the company's activity" },
		},
		{
			Name: listArtifactsTool, Phrase: "list artifacts",
			Narrow: []string{"kind", "q"},
			Description: "The artifacts the person can open: documents, decks, sheets and " +
				"dashboards, newest first.",
			schema: object(nil, map[string]*schema{
				"q":     text("Words the title must contain.", queryBytes),
				"kind":  oneOf("Only one kind.", artifactKinds()...),
				"limit": positive("How many, at most.", 50),
			}),
			Routes: []string{"GET /artifacts"}, Fence: true,
			// THE LISTING SPEAKS S3'S ListObjectsV2 (artifactsapi.ListPage): its
			// page size is `max-keys` and its rows are `contents`.
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /artifacts", nil, query(a).str("q", "q").str("kind", "kind").
					num("limit", "max-keys", 25).values())
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Artifacts", Rows: "contents",
				More:  "is_truncated",
				Brief: []string{"title", "kind", "owner", "updated"},
				Columns: []cards.Column{prose("title", "Title", "title"),
					col("kind", "Kind", "kind", tLabel), col("owner", "Owner", "owner", tText),
					col("role", "Your role", "role", tText),
					col("updated", "Updated", "updated_at", tDatetime), col("id", "Artifact", "id", tText)},
				Open: &cards.Open{Screen: "artifacts"}}}),
			Label: func(map[string]any) string { return "Listing artifacts" },
			Scope: func(a Args) string { return scopeOf(a.String("kind"), quoted(a)) },
		},
		{
			Name: getArtifactTool, Phrase: "read this artifact",
			Description: "One artifact: its kind, owner, the person's role on it, its version and " +
				"how widely it is shared.",
			schema: object([]string{"artifact"}, map[string]*schema{
				"artifact": text("The artifact's id.", nameBytes),
			}),
			Routes: []string{"GET /artifacts/{id}"}, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /artifacts/{id}", map[string]string{"id": Args(a).String("artifact")}, nil)
			},
			Cards: one(cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceArtifact, ID: "id",
				Title: "title", Subtitle: "kind", Prose: true, Text: "description",
				Facts: []cards.Fact{fact("Owner", "owner", tText), fact("Your role", "role", tLabel),
					fact("Status", "status", tLabel), {Label: "Version", Path: "head_number", Type: tNumber, Detail: true},
					fact("Shares", "share_count", tNumber), fact("Updated", "updated_at", tDatetime)},
				Open: &cards.Open{Screen: "artifact", Params: map[string]string{"id": "id"},
					Label: "Open the artifact"}}}),
			Label: func(map[string]any) string { return "Reading an artifact" },
		},
		{
			Name: "list_integrations", Phrase: "read integration health",
			Description: "Administrators: every integration's state, its traffic and the last " +
				"thing its reconcile said.",
			schema: object(nil, map[string]*schema{}),
			Routes: []string{"GET /integrations"}, Apps: adminOnly,
			Build: func(map[string]any) (exec.Request, error) {
				return get("GET /integrations", nil, nil)
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Integrations", Rows: "integrations",
				Brief: []string{"key", "enabled", "phase", "last"},
				Columns: []cards.Column{col("key", "Integration", "key", tText),
					col("enabled", "Enabled", "enabled", tText),
					col("inbound", "Deliveries", "inbound", tNumber),
					col("last", "Last delivery", "last_at", tDatetime),
					col("phase", "State", "reconcile.phase_label", tText)},
				Open: &cards.Open{Screen: "integrations"}}}),
			Label: func(map[string]any) string { return "Reading integration health" },
		},
		{
			Name: "get_fleet_health", Phrase: "read the fleet's health",
			Description: "Administrators: the nodes and their configuration rollout, each MCP " +
				"server's state, or the backups taken.",
			schema: object(nil, map[string]*schema{
				"view": oneOf("nodes (the default), mcp_servers or backups.", "nodes", "mcp_servers", "backups"),
			}),
			Routes: []string{"GET /fleet", "GET /mcp-servers", "GET /backups"}, Apps: adminOnly,
			Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				switch Args(a).String("view") {
				case "mcp_servers":
					return get("GET /mcp-servers", nil, nil)
				case "backups":
					return get("GET /backups", nil, nil)
				}
				return get("GET /fleet", nil, nil)
			},
			Variant: by("view", "nodes"),
			Cards: map[string]cards.Spec{
				"nodes": {Table: &cards.Table{Title: "Nodes", Rows: "nodes",
					Brief: []string{"id", "posture", "config", "seats"},
					Columns: []cards.Column{col("id", "Node", "id", tText),
						col("roles", "Roles", "roles", tText), col("seats", "Seats", "seats", tNumber),
						col("posture", "Posture", "posture", tLabel),
						col("config", "Configuration", "config_status", tLabel),
						col("lease", "Lease (s)", "expires_in", tNumber)},
					Open: &cards.Open{Screen: "nodes"}}},
				"mcp_servers": {Table: &cards.Table{Title: "MCP servers", Rows: "servers",
					Brief: []string{"name", "state", "tools"},
					Columns: []cards.Column{col("name", "Server", "name", tText),
						col("state", "State", "state", tLabel), col("tools", "Tools", "tools", tNumber),
						col("started", "Started", "started", tNumber),
						col("failed", "Failed", "failed", tNumber)},
					Open: &cards.Open{Screen: "nodes"}}},
				"backups": {Table: &cards.Table{Title: "Backups", Rows: "history",
					Brief: []string{"at", "outcome", "by"},
					Columns: []cards.Column{col("at", "When", "at", tDatetime),
						col("node", "Node", "node", tText), col("by", "By", "actor", tText),
						col("outcome", "Outcome", "outcome", tLabel),
						prose("summary", "Summary", "summary")},
					Open: &cards.Open{Screen: "backups"}}},
			},
			Label: func(map[string]any) string { return "Reading the fleet's health" },
		},
		{
			Name: "list_config_changes", Phrase: "read the configuration history",
			Description: "Administrators: who changed the company document and when, newest " +
				"first, with each change's summary.",
			schema: object(nil, map[string]*schema{
				"limit": positive("How many revisions, at most.", 50),
			}),
			Routes: []string{"GET /query/{what}"}, Apps: adminOnly, Fence: true,
			Build: func(a map[string]any) (exec.Request, error) {
				return get("GET /query/{what}", map[string]string{"what": "config_audit"},
					query(a).num("limit", "limit", 20).values())
			},
			Cards: one(cards.Spec{Table: &cards.Table{Title: "Configuration changes",
				Brief: []string{"summary", "by", "at"},
				Columns: []cards.Column{col("at", "When", "created_at", tDatetime),
					col("by", "By", "created_by", tText), col("source", "Source", "source", tText),
					prose("summary", "Summary", "summary"), col("active", "Active", "is_active", tText),
					col("revision", "Revision", "revision_id", tText)},
				Open: &cards.Open{Screen: "config"}}}),
			Label: func(map[string]any) string { return "Reading the configuration history" },
		},
		principlesRead(),
	}
}

// spendTable is a spend breakdown over rows, led by the columns that name a
// row.
func spendTable(title, rows string, lead ...cards.Column) cards.Spec {
	return cards.Spec{Table: &cards.Table{Title: title, Rows: rows,
		Brief: []string{lead[0].Key, "total", "calls"},
		Columns: append(lead, col("total", "Tokens", "total_tokens", tTokens),
			col("input", "Input", "input_tokens", tTokens),
			col("output", "Output", "output_tokens", tTokens),
			col("calls", "Calls", "calls", tNumber), col("cost", "Cost (USD)", "cost_usd", tNumber)),
		Open: &cards.Open{Screen: "spend"}}}
}

// artifactKinds are the artifact kinds this build serves (internal/artifacts),
// the closed set the gallery filters on.
func artifactKinds() []string {
	out := make([]string, 0, len(artifacts.Kinds))
	for _, k := range artifacts.Kinds {
		out = append(out, string(k))
	}
	return out
}

// outsideWhere is the trust of an answer whose rows (at rows) may carry a
// coding run's question at field: the one trust decided for a coding run's
// words (provenance.CodingRun) where any row carries one, and where none does
// the engine's own records, a colleague's by the one rule for a surface the
// engine hosts.
func outsideWhere(rows, field string) func(Args, any) provenance.Trust {
	return func(_ Args, answer any) provenance.Trust {
		if carriesRunAsk(answer, rows, field) {
			return provenance.CodingRun("").Trust
		}
		return provenance.TrustForHosted("", nil, false)
	}
}

// carriesRunAsk reports whether any row of answer at rows carries a coding
// run's question at field.
func carriesRunAsk(answer any, rows, field string) bool {
	list, _ := cards.Lookup(answer, rows).([]any)
	for _, row := range list {
		if strings.TrimSpace(cards.Text(cards.Lookup(row, field))) != "" {
			return true
		}
	}
	return false
}

// knowledgeTrust is a knowledge search's trust, as each backend labels its own
// hits: Doclet's (whose answers carry the stored backend name "native",
// doclet.Backend) by the one rule for a surface the engine hosts, a
// colleague's with nobody named; a connected knowledge base's external, since
// people outside the company may write its pages and the answer names nobody.
func knowledgeTrust(answer any) provenance.Trust {
	switch cards.Text(cards.Lookup(answer, "backend")) {
	case "", "native":
		return provenance.TrustForHosted("", nil, false)
	}
	return provenance.TrustExternal
}
