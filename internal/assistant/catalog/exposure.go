package catalog

import (
	"fmt"
	"slices"
)

// The exposure table: one decision per registry question, per operator
// catalogue tool and per guarded route, held complete in both directions by the
// catalogue walks (internal/api/queries, internal/agent/builtin and
// internal/api each walk the metadata they own). A surface added without a row
// fails the build until somebody decides what the assistant does with it.

func asked(question, tool string) Exposure {
	return Exposure{Question: question, As: AsReadTool, Name: tool}
}

func linked(question, why string) Exposure {
	return Exposure{Question: question, As: AsNavigateOnly, Why: why}
}

func refused(question, why string) Exposure {
	return Exposure{Question: question, As: AsNever, Why: why}
}

func proposed(tool, name string) Exposure {
	return Exposure{Tool: tool, As: AsWriteTool, Name: name}
}

func toolNever(tool, why string) Exposure {
	return Exposure{Tool: tool, As: AsNever, Why: why}
}

func routed(route, tool string) Exposure {
	return Exposure{Route: route, As: AsReadTool, Name: tool}
}

func routeProposed(route, tool string) Exposure {
	return Exposure{Route: route, As: AsWriteTool, Name: tool}
}

func routeLinked(route, why string) Exposure {
	return Exposure{Route: route, As: AsNavigateOnly, Why: why}
}

func routeNever(route, why string) Exposure {
	return Exposure{Route: route, As: AsNever, Why: why}
}

// The reasons the table repeats, each stated once.
const (
	whyTranscript = "raw model transcripts and event payloads are the widest prompt " +
		"injection and secret surface the engine has; the screen shows them"
	whyActRead = "a read on the act transport, which serves writes only; the assistant " +
		"reads the same answer through the question registry's named route"
	whyLater  = "not in the first catalogue: a person opens it on its screen"
	whyAuthor = "authoring needs the artifact kit and a file bundle, which the editors " +
		"are for"
	whyIrreversible = "irreversible or company-wide: a person makes this change on its own " +
		"screen, with its own confirmation"
	whyWriteSurface = "the human write surface for scripts and the CLI; the assistant's " +
		"writes execute through the act route the dashboard's controls send"
	whySelfOnly = "covered: the assistant opens the screen and the screen reads it"
)

// questionExposures are the registry questions'.
var questionExposures = []Exposure{
	refused("viewer", "the turn reads the person itself for the prompt's person block; a tool "+
		"would be a second answer to who is asking"),
	asked("agent", "get_agent"),
	refused("live_call", whyTranscript),
	asked("tokens", "get_spend"),
	linked("events", whyTranscript),
	linked("event_series", whyTranscript),
	linked("event", whyTranscript),
	linked("trace", whyTranscript),
	linked("turn", whyTranscript),
	asked("turns", "list_turns"),
	refused("phases", "phase timings cost more tokens than any question they answer; list_turns "+
		"carries each turn's duration"),
	asked("token_series", "get_spend_series"),
	asked("seat_activity", "get_agent"),
	linked("page_reads", whyLater),
	asked("fleet", "get_fleet_health"),
	asked("mcp_servers_status", "get_fleet_health"),
	asked("backups", "get_fleet_health"),
	linked("fleet_broker", whyLater),
	linked("schedule_runs", whyLater),
	asked("budgets", "list_budgets"),
	linked("limits", whyLater),
	linked("schedules", whyLater),
	asked("integrations", "list_integrations"),
	asked("knowledge", "search_knowledge"),
	refused("colleague", "a seat's role card; get_agent and the org chart answer who a seat is"),
	asked("sandbox_runs", "list_coding_runs"),
	linked("sandbox_tail", "a run's live output belongs on the Runs screen, which streams it"),
	linked("retention", whyLater),
	linked("sign_in_policy", "the deployment's security posture; nothing to ask beyond the screen"),
	asked("work_items", "list_tasks"),
	asked("work_item", "get_task"),
	asked("work_comments", "get_task_thread"),
	asked("work_item_turns", "get_task_cost"),
	linked("work_views", whyLater),
	linked("work_saved_views", whyLater),
	asked("work_projects", "list_projects"),
	asked("work_project", "list_projects"),
	asked("work_workload", "get_workload"),
	linked("work_activity", whyLater),
	asked("work_my_work", "get_my_work"),
	asked("work_inbox", "get_my_work"),
	linked("work_routing", whyLater),
	refused("work_catalogue", "the write tools' own schemas name the statuses and priorities "+
		"a write may set"),
	// READ BY A PREVIEW ONLY: a priority change is bound to the person's
	// record at the version its preview read.
	{Question: "work_person", As: AsPreviewRead, Name: "set_my_priorities"},
	linked("work_flow", whyLater),
	asked("company_feed", "get_company_feed"),
	asked("decisions", "list_decisions"),
	linked("pages", whyLater),
	asked("page", "get_page"),
	linked("containers", whyLater),
	linked("page_activity", whyLater),
	linked("page_revision", whyLater),
	refused("agent_memory", "a seat's diary is its own words about the company's work, fenced "+
		"reads of it are a later decision"),
	refused("agent_episode", "a seat's diary is its own words about the company's work, fenced "+
		"reads of it are a later decision"),
	refused("memory_overview", "a seat's diary is its own words about the company's work, "+
		"fenced reads of it are a later decision"),
	linked("conversations", "outside parties' own words, which the conversations screen shows"),
	linked("a2a_channels", whyLater),
	refused("work_files", "a project's file bytes; a person opens a file from the project"),
	asked("work_search", "search_work"),
	refused("config", "the whole company document names every secret reference; the specific "+
		"reads answer its facts"),
	asked("config_audit", "list_config_changes"),
	refused("config_diff", "a revision's document diff carries what the document carries, "+
		"secret references included"),
	refused("config_entities", "configuration entities carry model endpoints, tool commands "+
		"and their credential references"),
	asked("artifacts", "list_artifacts"),
	asked("artifact", "get_artifact"),
	linked("artifact_versions", whySelfOnly),
	linked("artifact_version", whySelfOnly),
	linked("artifact_history", whySelfOnly),
	linked("artifact_assets", whySelfOnly),
	linked("artifact_seen_by", whySelfOnly),
	linked("artifact_access", whySelfOnly),
	refused("artifact_usage", "the deployment's storage against its plan is an "+
		"administrator's reading, not a question the assistant answers"),
	refused("artifact_trail", "the company-wide trail is never the assistant's (audit.read)"),
	refused("artifact_data", "datastore rows a page script writes; fenced reads of them are a "+
		"later decision"),
	refused("artifact_data_doc", "datastore rows a page script writes; fenced reads of them "+
		"are a later decision"),
	refused("artifact_data_query", "datastore rows a page script writes; fenced reads of them "+
		"are a later decision"),
	refused("artifact_data_history", "datastore rows a page script writes; fenced reads of "+
		"them are a later decision"),
	refused("artifact_data_schema", "datastore rows a page script writes; fenced reads of "+
		"them are a later decision"),
	refused("artifact_data_state", "datastore rows a page script writes; fenced reads of them "+
		"are a later decision"),
	refused("artifact_data_usage", "the deployment's storage against its plan is an "+
		"administrator's reading, not a question the assistant answers"),
}

// toolExposures are the operator catalogue's tools'.
var toolExposures = []Exposure{
	toolNever("list_work_items", whyActRead),
	toolNever("get_work_item", whyActRead),
	proposed("create_work_item", "create_task"),
	proposed("update_work_item", "update_task"),
	// ONE ROW, TWO TOOLS: comment_on_task and answer_decision both
	// execute as this one, an answer being a comment that closes a
	// question.
	proposed("comment_on_work_item", "comment_on_task"),
	toolNever("merge_work_item", whyLater),
	toolNever("move_work_item", whyLater),
	toolNever("search_work_items", whyActRead),
	toolNever("list_work_views", whyActRead),
	toolNever("save_work_view", whyLater),
	toolNever("get_work_catalogue", whyActRead),
	toolNever("write_work_catalogue", "rewrites every seat's tracker vocabulary; the catalogue "+
		"screen is the place"),
	toolNever("list_projects", whyActRead),
	toolNever("describe_project", whyActRead),
	toolNever("task_activity", whyActRead),
	toolNever("my_work", whyActRead),
	toolNever("get_person", whyActRead),
	proposed("set_priorities", "set_my_priorities"),
	toolNever("set_pins", whyLater),
	proposed("mark_inbox", "mark_inbox"),
	toolNever("work_inbox", whyActRead),
	toolNever("write_project", whyLater),
	toolNever("list_project_files", whyActRead),
	toolNever("read_project_file", whyActRead),
	toolNever("write_project_file", "file bytes are uploaded on the project's Files lens"),
	toolNever("remove_project_file", whyIrreversible),
	toolNever("remove_work_item", whyLater),
	toolNever("restore_work_item", whyLater),
	toolNever("place_work_item", "a card's place on a board is dragged by the person"),
	toolNever("list_pages", whyActRead),
	toolNever("get_page", whyActRead),
	proposed("write_page", "create_page"),
	proposed("save_page", "edit_page"),
	toolNever("comment_on_page", whyLater),
	toolNever("search_knowledge", whyActRead),
	proposed("answer_run", "answer_coding_run"),
	proposed("pause_seat", "pause_agent"),
	proposed("resume_seat", "resume_agent"),
	proposed("steer_turn", "steer_turn"),
	toolNever("answer_knowledge", "the assistant is itself a model answering from the "+
		"knowledge reads; a model calling a model doubles the spend and hides provenance"),
	toolNever("list_artifacts", whyActRead),
	toolNever("get_artifact", whyActRead),
	toolNever("list_dashboard_sources", whyActRead),
	toolNever("create_artifact", whyAuthor),
	toolNever("publish_artifact", whyAuthor),
	toolNever("update_artifact", whyAuthor),
	toolNever("upload_artifact_asset", whyAuthor),
	proposed("share_artifact", "share_artifact"),
	proposed("unshare_artifact", "unshare_artifact"),
	toolNever("read_artifact_data", whyActRead),
	toolNever("query_artifact_data", whyActRead),
	toolNever("write_artifact_data", whyLater),
	toolNever("set_artifact_schema", "a schema change rewrites every row of a store"),
}

// Exposures are every exposure decision, questions first, then tools, then
// routes.
func Exposures() []Exposure {
	return slices.Concat(questionExposures, toolExposures, routeExposures)
}

// ReadsQuestion reports whether the catalogue reads question, as a tool or
// in a write's preview: the one set of questions an assisted principal may ask
// the registry (the seam internal/api/queries enforces with it).
func ReadsQuestion(question string) bool {
	return slices.ContainsFunc(questionExposures, func(e Exposure) bool {
		return e.Question == question && (e.As == AsReadTool || e.As == AsPreviewRead)
	})
}

// ReadsRoute reports whether the catalogue reads the route, "METHOD
// /pattern", as a tool: for the reads served from the stream service rather
// than the registry (`GET /agents`, `GET /org`), which an assisted principal
// may reach only as the catalogue exposes them.
func ReadsRoute(route string) bool {
	return slices.ContainsFunc(routeExposures, func(e Exposure) bool {
		return e.Route == route && e.As == AsReadTool
	})
}

// Check reports what is wrong with the table as written, the walks aside: a
// row naming no surface or two, a decision this build does not know, a read
// or write row naming no tool or a tool of another kind, and a never or
// navigate row with no reason.
func Check() error {
	byName := map[string]Tool{}
	for _, t := range Tools() {
		byName[t.Name] = t
	}
	for _, e := range Exposures() {
		named := 0
		for _, s := range []string{e.Question, e.Tool, e.Route} {
			if s != "" {
				named++
			}
		}
		switch {
		case named != 1:
			return fmt.Errorf("catalog: exposure %+v names %d surfaces, not one", e, named)
		case !e.As.Valid():
			return fmt.Errorf("catalog: exposure %+v decides %q", e, e.As)
		case (e.As == AsNever || e.As == AsNavigateOnly) && e.Why == "":
			return fmt.Errorf("catalog: exposure %+v gives no reason", e)
		}
		switch e.As {
		case AsReadTool, AsWriteTool, AsPreviewRead:
			t, ok := byName[e.Name]
			want := map[As]Kind{AsReadTool: KindRead, AsWriteTool: KindWrite,
				AsPreviewRead: KindWrite}[e.As]
			if !ok || t.Kind != want {
				return fmt.Errorf("catalog: exposure %+v names no %s tool %q", e, want, e.Name)
			}
		}
	}
	return nil
}
