package catalog

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/authz"
)

// The exposure table: which surface each CURATED write is built on, one row per
// question its preview reads, operator tool it executes as or route it
// proposes on, held to the engine's own metadata by the catalogue walks
// (internal/api/queries, internal/agent/builtin and internal/api each walk what
// they own). Every read, and every change no curated write makes, goes through
// the generic API tools and is decided by its own route (ADR-0052), so a
// surface needs a row only when a curated write is built on it.

// proposed is a curated write executing as verb's operator tool, which the
// row names by the verb, as [write] does.
func proposed(verb authz.Action, name string) Exposure {
	return Exposure{Tool: string(verb), As: AsWriteTool, Name: name}
}

// previewed is a question read only by the named write's preview.
func previewed(question, tool string) Exposure {
	return Exposure{Question: question, As: AsPreviewRead, Name: tool}
}

func routeProposed(route, tool string) Exposure {
	return Exposure{Route: route, As: AsWriteTool, Name: tool}
}

// questionExposures are the registry questions a curated write's preview
// reads: what it changes, read as it is now, so the card shows the change and
// binds the revision it read (ADR-0051). Every other read is [ReadAPITool]'s,
// decided by its own route.
var questionExposures = []Exposure{
	previewed("work_item", "update_task"),
	previewed("work_comments", "comment_on_task"),
	previewed("decisions", "answer_decision"),
	previewed("work_inbox", markInboxTool),
	previewed("work_my_work", "set_my_priorities"),
	// A priority change is bound to the person's record at the version its
	// preview read.
	previewed("work_person", "set_my_priorities"),
	previewed("page", "edit_page"),
	previewed("artifact", shareArtifactTool),
	previewed("agent", "pause_agent"),
	previewed("executors", AnswerExecutorTool),
	previewed("atlas_entry", addAtlasEntryTool),
	previewed("chat_channel", "post_chat_message"),
	previewed("chat_thread", "reply_in_chat_thread"),
	// WHOM A POST OR A REPLY WOULD REACH: the post's own decision run dry, so
	// the card says what the send will do.
	previewed("chat_reach", "post_chat_message"),
	previewed("chat_reach", "reply_in_chat_thread"),
}

// toolExposures are the operator catalogue tools a curated write executes as.
var toolExposures = []Exposure{
	proposed(authz.ActionWorkCreate, "create_task"),
	proposed(authz.ActionWorkUpdate, "update_task"),
	// ONE ROW, TWO TOOLS: comment_on_task and answer_decision both
	// execute as this one, an answer being a comment that closes a
	// question.
	proposed(authz.ActionWorkComment, "comment_on_task"),
	proposed(authz.ActionPrioritiesSet, "set_my_priorities"),
	proposed(authz.ActionInboxMark, markInboxTool),
	proposed(authz.ActionPageCreate, "create_page"),
	proposed(authz.ActionPageSave, "edit_page"),
	proposed(authz.ActionExecutorAnswer, AnswerExecutorTool),
	proposed(authz.ActionSeatPause, "pause_agent"),
	proposed(authz.ActionAtlasAddEntry, addAtlasEntryTool),
	proposed(authz.ActionAtlasCorrect, correctAtlasFactTool),
	proposed(authz.ActionAtlasConfirm, confirmAtlasEntryTool),
	proposed(authz.ActionAtlasRelate, relateAtlasEntriesTool),
	proposed(authz.ActionAtlasUnrelate, unrelateAtlasEntriesTool),
	proposed(authz.ActionAtlasSetOwner, setAtlasOwnerTool),
	proposed(authz.ActionAtlasRetire, retireAtlasEntryTool),
	proposed(authz.ActionAtlasRestore, restoreAtlasEntryTool),
	proposed(authz.ActionAtlasMerge, mergeAtlasEntriesTool),
	proposed(authz.ActionAtlasSplit, splitAtlasEntriesTool),
	proposed(authz.ActionAtlasPurge, purgeAtlasEntryTool),
	proposed(authz.ActionSeatResume, "resume_agent"),
	proposed(authz.ActionTurnSteer, steerTurnTool),
	proposed(authz.ActionArtifactShare, shareArtifactTool),
	proposed(authz.ActionArtifactUnshare, unshareArtifactTool),
	proposed(authz.ActionChatPost, "post_chat_message"),
	proposed(authz.ActionChatReply, "reply_in_chat_thread"),
	proposed(authz.ActionChatDirect, "send_direct_message"),
	proposed(authz.ActionChatEdit, "edit_chat_message"),
	proposed(authz.ActionChatDelete, "delete_chat_message"),
	proposed(authz.ActionChatUpdate, "update_chat_room"),
	proposed(authz.ActionChatMembers, "set_chat_room_members"),
	proposed(authz.ActionChatLeave, "leave_chat_room"),
}

// routeExposures are the routes a curated write proposes on directly, rather
// than through the act route: the shares, the principles, the ceilings and an
// agent's access.
var routeExposures = []Exposure{
	routeProposed(ShareRoute, shareArtifactTool),
	routeProposed(UnshareRoute, unshareArtifactTool),
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
