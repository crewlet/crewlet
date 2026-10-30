package catalog

import (
	"fmt"
	"slices"
)

// The exposure table: which surface each CURATED tool is built on, one row per
// question, operator tool or route a curated tool reads or proposes, held to
// the engine's own metadata by the catalogue walks (internal/api/queries,
// internal/agent/builtin and internal/api each walk what they own). A curated
// tool is a better-shaped shortcut onto a surface; every other surface the
// person's grant reaches is reached through the generic API tools and decided
// by its own route (ADR-0052), so a surface needs a row only when a curated
// tool is built on it.

func asked(question, tool string) Exposure {
	return Exposure{Question: question, As: AsReadTool, Name: tool}
}

func proposed(tool, name string) Exposure {
	return Exposure{Tool: tool, As: AsWriteTool, Name: name}
}

func routed(route, tool string) Exposure {
	return Exposure{Route: route, As: AsReadTool, Name: tool}
}

func routeProposed(route, tool string) Exposure {
	return Exposure{Route: route, As: AsWriteTool, Name: tool}
}

// questionExposures are the registry questions a curated tool reads.
var questionExposures = []Exposure{
	asked("agent", "get_agent"),
	asked("tokens", "get_spend"),
	asked("turns", "list_turns"),
	asked("token_series", "get_spend_series"),
	asked("seat_activity", "get_agent"),
	asked("fleet", "get_fleet_health"),
	asked("mcp_servers_status", "get_fleet_health"),
	asked("backups", "get_fleet_health"),
	asked("budgets", "list_budgets"),
	asked("integrations", "list_integrations"),
	asked("knowledge", "search_knowledge"),
	asked("containers", "list_spaces"),
	asked("sandbox_runs", "list_coding_runs"),
	asked("work_items", "list_tasks"),
	asked("work_item", "get_task"),
	asked("work_comments", "get_task_thread"),
	asked("work_item_turns", "get_task_cost"),
	asked("work_projects", "list_projects"),
	asked("work_project", "list_projects"),
	asked("work_workload", "get_workload"),
	asked("work_my_work", "get_my_work"),
	asked("work_inbox", "get_my_work"),
	asked("company_feed", "get_company_feed"),
	asked("decisions", "list_decisions"),
	asked("page", "get_page"),
	asked("work_search", "search_work"),
	asked("config_audit", "list_config_changes"),
	asked("artifacts", "list_artifacts"),
	asked("artifact", "get_artifact"),
	// READ BY A PREVIEW ONLY: a priority change is bound to the person's
	// record at the version its preview read.
	{Question: "work_person", As: AsPreviewRead, Name: "set_my_priorities"},
}

// toolExposures are the operator catalogue tools a curated write executes as.
var toolExposures = []Exposure{
	proposed("create_work_item", "create_task"),
	proposed("update_work_item", "update_task"),
	// ONE ROW, TWO TOOLS: comment_on_task and answer_decision both
	// execute as this one, an answer being a comment that closes a
	// question.
	proposed("comment_on_work_item", "comment_on_task"),
	proposed("set_priorities", "set_my_priorities"),
	proposed("mark_inbox", "mark_inbox"),
	proposed("write_page", "create_page"),
	proposed("save_page", "edit_page"),
	proposed("answer_run", "answer_coding_run"),
	proposed("pause_seat", "pause_agent"),
	proposed("resume_seat", "resume_agent"),
	proposed("steer_turn", "steer_turn"),
	proposed("share_artifact", "share_artifact"),
	proposed("unshare_artifact", "unshare_artifact"),
}

// routeExposures are the routes a curated tool reads or proposes on directly,
// rather than through a registry question or the act route: the overview
// reads the stream service serves, the org chart's people, the shares and the
// principles.
var routeExposures = []Exposure{
	routed("GET /agents", "list_agents"),
	routed("GET /org", "get_org_chart"),
	routed("GET /iam/seats", "get_org_chart"),
	routeProposed(ShareRoute, "share_artifact"),
	routeProposed(UnshareRoute, "unshare_artifact"),
	routed(PrinciplesRoute, "get_company_principles"),
	routeProposed(PrinciplesWriteRoute, "update_company_principles"),
	// A CEILING IS WRITTEN AS BUDGETS WRITES IT: a seat's through the seat,
	// the company's as a merge patch of the document.
	routeProposed(seatWriteRoute, "set_token_budget"),
	routeProposed(companyWriteRoute, "set_token_budget"),
	// AN AGENT'S ACCESS AT A VENDOR IS SAVED AS ITS AGENTS TAB SAVES IT.
	routeProposed(accessWriteRoute, "set_seat_access"),
}

// Exposures are every exposure row, questions first, then tools, then routes.
func Exposures() []Exposure {
	return slices.Concat(questionExposures, toolExposures, routeExposures)
}

// Check reports what is wrong with the table as written, the walks aside: a
// row naming no surface or two, a decision this build does not know, and a row
// naming no curated tool of its kind.
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
		}
		t, ok := byName[e.Name]
		want := map[As]Kind{AsReadTool: KindRead, AsWriteTool: KindWrite,
			AsPreviewRead: KindWrite}[e.As]
		if !ok || t.Kind != want {
			return fmt.Errorf("catalog: exposure %+v names no %s tool %q", e, want, e.Name)
		}
	}
	return nil
}
