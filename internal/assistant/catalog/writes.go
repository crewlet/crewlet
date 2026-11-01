package catalog

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// The write tools. Every call is a PROPOSAL and never an effect (ADR-0051):
// the turn builds the one request the write would send, the person sees a
// preview of it and approves, and only the approval sends it, as them. Each
// executes as one operator-catalogue tool through the act route, or (the two
// artifact shares) through the artifacts surface the dashboard's own control
// sends, or (the company's principles) through the route Company › Principles
// saves through, so a write the assistant prepared and a write the person
// clicked are one code path.

// DeriveTier is a write's risk from the authority table and the act tool's own
// hint: R3 (consequential) for a verb only a person takes, a verb that asks a
// recent proof, or a tool that destroys or replaces what is there; R2
// (reversible) for every other write. A row may state a higher tier and never
// a lower one.
func DeriveTier(act authz.Action, destructive bool) Tier {
	humanOnly, _ := authz.HumanOnlyOf(act)
	recency, _ := authz.RecencyOf(act)
	if humanOnly || recency == iam.RecencyStepUp || destructive {
		return TierConsequential
	}
	return TierReversible
}

// write is a write tool executing as verb's tool on the act route, the call's
// arguments sent as that tool's own, under the same names.
//
// THE ACT TOOL IS NAMED BY ITS VERB. Where a tool exists the authority table
// names its verb after it, and internal/agent/builtin's walks hold the two
// equal in both directions, so the verb is at once the tool's name and what
// the write's tier and apps are derived from. It is also the one spelling of
// the name this package can read: the tool's owner (the tracker, the pages,
// the builtins) is a package internal/assistant's import walk keeps the
// assistant from reaching, and a literal here would be a copy that a rename
// of the tool leaves behind.
func write(name string, verb authz.Action, destructive bool, phrase, description string,
	s *schema) Tool {
	tool := string(verb)
	return Tool{
		Name: name, Act: tool, Kind: KindWrite, Phrase: phrase, Description: description,
		schema: s, Routes: []string{ActRoute}, Destructive: destructive,
		Tier: DeriveTier(verb, destructive),
		Apps: appsOf(verb),
		Build: func(a map[string]any) (exec.Request, error) {
			return act(tool, Args(a))
		},
	}
}

// answersRun is a write whose request answers with a coding run's question
// beside its receipt: what the resumed turn reads of it is fenced by the one
// rule for a coding run's words (provenance.Executor), as every read of that
// question is.
func answersRun(t Tool) Tool {
	t.Trust = provenance.Executor("").Trust
	return t
}

// appsOf is the app a verb's row asks, as a list.
func appsOf(a authz.Action) []iam.Grant {
	if app, _ := authz.AppOf(a); app != "" {
		return []iam.Grant{app}
	}
	return nil
}

// writeTools are the write tools, in the order a model meets them.
func writeTools() []Tool {
	out := []Tool{
		write("create_task", authz.ActionWorkCreate, false, "file tasks",
			"Propose filing a task, or a question a colleague owes an answer to (ask). Search "+
				"first. The person approves before anything is filed.",
			object([]string{"title"}, map[string]*schema{
				"title":    text("What the work is, in one line.", lineBytes),
				"body":     text("The description, in Markdown.", bodyBytes),
				"project":  text("The project's key or name. Default the person's team's.", nameBytes),
				"assignee": text("A seat's handle or name. Default the project's default assignee.", nameBytes),
				"ask":      text("A colleague's handle or seat name: the task IS a question they owe an answer to.", nameBytes),
				"parent":   text("The parent task's key or id, for a subtask.", nameBytes),
				"type":     text("A task type from the workspace's catalogue.", nameBytes),
				"status":   oneOf("Where it starts. Default todo.", statuses...),
				"priority": oneOf("Its priority. Default none.", priorities...),
				"due":      text("When it is due: a date (2031-04-16) or today, tomorrow, eow, eom, +7d.", nameBytes),
				"labels":   names("Tags the project declares.", 20),
			})),
		write("update_task", authz.ActionWorkUpdate, true, "change tasks",
			"Propose changing a task's fields. Read it with get_task first and pass its version "+
				"as if_match, so the change applies to what the person saw.",
			object([]string{"item", "if_match"}, map[string]*schema{
				"item":     text("The task's key or id.", nameBytes),
				"if_match": positive("The task's version from get_task.", 1<<30),
				"title":    text("A new title.", lineBytes),
				"body":     text("A new description, replacing the old one.", bodyBytes),
				"status":   oneOf("A new status.", statuses...),
				"priority": oneOf("A new priority.", priorities...),
				"assignee": text("A seat's handle or name, or \"\" to unassign.", nameBytes),
				"reason":   text("With assignee: one line on why the work is theirs now.", lineBytes),
				"due":      text("A new due date (2031-04-16, today, eow, +7d).", nameBytes),
				"labels":   names("The task's tags, replacing them.", 20),
			})),
		write("comment_on_task", authz.ActionWorkComment, false, "comment on tasks",
			"Propose a comment on a task, or a reply to one.",
			object([]string{"item", "body"}, map[string]*schema{
				"item":     text("The task's key or id.", nameBytes),
				"body":     text("The comment, in Markdown.", bodyBytes),
				"reply_to": text("The id of the comment this replies to.", nameBytes),
			})),
		write("answer_decision", authz.ActionWorkComment, false, "answer questions on tasks",
			"Propose answering a question asked of the person on a task, choosing one of its "+
				"options where it carries a decision.",
			object([]string{"item"}, map[string]*schema{
				"item":    text("The task's key or id.", nameBytes),
				"answers": text("The comment id of the question this answers.", nameBytes),
				"choice":  text("The id of the option chosen, where the question offers options.", nameBytes),
				"body":    text("The answer, or why this option, in Markdown.", bodyBytes),
			})),
		write("set_my_priorities", authz.ActionPrioritiesSet, false, "reorder priorities",
			"Propose the person's priority order, most important first, or a report's by "+
				"their handle.",
			object([]string{"items"}, map[string]*schema{
				"items":  names("Task keys or ids, most important first.", 100),
				"handle": text("A report's handle or seat name. Default the person's own list.", nameBytes),
			})),
		write(markInboxTool, authz.ActionInboxMark, false, "mark the inbox",
			"Propose marking inbox notices read or unread, by their record ids.",
			object(nil, map[string]*schema{
				"read":   names("Record ids to mark read.", 200),
				"unread": names("Record ids to mark unread.", 200),
			})),
		write("pause_agent", authz.ActionSeatPause, true, "pause agents",
			"Propose pausing an agent seat, optionally stopping the turn it is running.",
			object([]string{"handle", "reason"}, map[string]*schema{
				"handle":       text("The seat's handle or name.", nameBytes),
				"reason":       text("Why, in one line.", lineBytes),
				"stop_running": flag("Stop the turn it is running now as well."),
			})),
		write("resume_agent", authz.ActionSeatResume, false, "resume agents",
			"Propose resuming a paused agent seat.",
			object([]string{"handle"}, map[string]*schema{
				"handle": text("The seat's handle or name.", nameBytes),
			})),
		write(steerTurnTool, authz.ActionTurnSteer, false, "steer running turns",
			"Propose a note to a turn that is running now; it becomes that agent's input.",
			object([]string{"turn_id", "note"}, map[string]*schema{
				"turn_id": text("The running turn's id.", nameBytes),
				"note":    text("The note, verbatim.", 8<<10),
			})),
		answersRun(write(answerExecutorTool, authz.ActionExecutorAnswer, false, "answer coding runs",
			"Propose an answer to a coding run parked on a question for the person.",
			object([]string{"turn_id", "launch_id"}, map[string]*schema{
				"turn_id": text("The run's turn id.", nameBytes),
				"launch_id": text("The question being answered: the run's launch_id, read with "+
					"its question.", nameBytes),
				"choice": text("The id of the option chosen, where the question offers "+
					"options (its decision).", nameBytes),
				// The bound answer_executor itself refuses past
				// (builtin.MaxRunAnswerBytes).
				"answer": text("The answer in words, verbatim: the whole answer to an open "+
					"question, or what to add to a choice.", 16<<10),
			}))),
		write("create_page", authz.ActionPageCreate, false, "write pages",
			"Propose a new knowledge page.",
			object([]string{"title", "body"}, map[string]*schema{
				"title":     text("The page's title.", lineBytes),
				"body":      text("The page, in Markdown.", bodyBytes),
				"container": text("The space's key or name (list_spaces lists them). Default the person's team's.", nameBytes),
				"parent":    text("The id of the page it belongs under.", nameBytes),
				"labels":    names("Labels.", 20),
				"status":    oneOf("published (the default) or draft.", "published", "draft"),
				"message":   text("One line on why the page exists.", lineBytes),
			})),
		write("edit_page", authz.ActionPageSave, true, "edit pages",
			"Propose a new version of a page. Read it with "+getPageTool+" first and pass "+
				"its version as base_version.",
			object([]string{"page", "base_version", "body"}, map[string]*schema{
				"page":         text("The page's id.", nameBytes),
				"base_version": positive("The page's version from "+getPageTool+".", 1<<30),
				"body":         text("The whole new body, in Markdown.", bodyBytes),
				"title":        text("A new title.", lineBytes),
				"message":      text("One line on what changed and why.", lineBytes),
			})),
	}
	// AN ANSWER UNBLOCKS SOMEBODY ELSE: the agent that asked resumes on
	// it, so it is R3 above the comment verb's own derivation.
	for i := range out {
		if out[i].Name == "answer_decision" {
			out[i].Tier = TierConsequential
		}
		out[i].Names = writeNames[out[i].Name]
	}
	out = append(out, principlesWrite(), budgetWrite(), seatAccessWrite())
	out = append(out, shareTools()...)
	return append(out, chatWrites()...)
}

// writeNames are the arguments of each write that name a seat, a project or a
// space, which the turn resolves from a name to the key before the preview is
// read ([Resolver.Resolve]). A share's audience is not one: `unit:<key>` and
// `company` are an audience's own grammar, which its route reads.
var writeNames = map[string]map[string]Directory{
	"create_task":       {"project": DirProject, "assignee": DirSeat, "ask": DirSeat},
	"update_task":       {"assignee": DirSeat},
	"set_my_priorities": {"handle": DirSeat},
	"pause_agent":       {"handle": DirSeat},
	"resume_agent":      {"handle": DirSeat},
	"create_page":       {"container": DirSpace},
}

// The artifacts surface's share routes (internal/api/artifactsapi), the ones
// the dashboard's own share control sends.
const (
	ShareRoute   = "PUT /artifacts/{id}/shares"
	UnshareRoute = "DELETE /artifacts/{id}/shares/{audience...}"
)

// shareTools are the two writes that do not execute through the act route:
// the act transport does not serve the artifact tools, so a share executes on
// the surface the dashboard's control uses.
func shareTools() []Tool {
	audience := text("A seat's handle, unit:<key> for a team and everybody under it, or "+
		"company.", nameBytes)
	share := Tool{
		Name: shareArtifactTool, Act: string(authz.ActionArtifactShare), Kind: KindWrite,
		Phrase:      "share artifacts",
		Description: "Propose sharing an artifact the person owns with a seat, a team or the company.",
		schema: object([]string{"artifact", "to", "role"}, map[string]*schema{
			"artifact": text("The artifact's id.", nameBytes),
			"to":       audience,
			"role":     oneOf("What they may do.", "viewer", "commenter", "contributor", "editor"),
		}),
		Routes: []string{ShareRoute},
		Build: func(a map[string]any) (exec.Request, error) {
			args := Args(a)
			to, err := audienceOf(args.String("to"))
			if err != nil {
				return exec.Request{}, err
			}
			req, err := at(ShareRoute, map[string]string{"id": args.String("artifact")}, nil)
			if err != nil {
				return exec.Request{}, err
			}
			req.Body, err = json.Marshal(map[string]any{"audience": to,
				"role": args.String("role")})
			return req, err
		},
		// THE WHOLE COMPANY READING IT IS A STEP-UP (D09): a share to
		// everybody is the widest visibility change the assistant can
		// prepare, so the approver confirms it is them.
		StepUp: func(a Args) bool { return a.String("to") == string(artifacts.AudienceCompany) },
	}
	unshare := Tool{
		Name: unshareArtifactTool, Act: string(authz.ActionArtifactUnshare), Kind: KindWrite,
		Phrase:      "withdraw shares",
		Description: "Propose withdrawing one audience's share of an artifact the person owns.",
		schema: object([]string{"artifact", "from"}, map[string]*schema{
			"artifact": text("The artifact's id.", nameBytes),
			"from":     audience,
		}),
		Routes: []string{UnshareRoute},
		Build: func(a map[string]any) (exec.Request, error) {
			args := Args(a)
			from, err := audienceOf(args.String("from"))
			if err != nil {
				return exec.Request{}, err
			}
			req, err := at(UnshareRoute, map[string]string{"id": args.String("artifact"),
				"audience": from.String()}, nil)
			return req, err
		},
	}
	// A VISIBILITY CHANGE REACHES OTHER PEOPLE, so both are R3 above what
	// their rows derive (DeriveTier gives R2): who will see the result is a
	// consequence the person reads before approving.
	share.Tier, unshare.Tier = TierConsequential, TierConsequential
	share.ChangesVisibility, unshare.ChangesVisibility = true, true
	share.Apps, unshare.Apps = appsOf(authz.ActionArtifactShare), appsOf(authz.ActionArtifactUnshare)
	return []Tool{share, unshare}
}

// audienceOf reads an audience as the share tools take it: a bare seat handle,
// `unit:<key>`, or `company`.
func audienceOf(s string) (artifacts.Audience, error) {
	if s != string(artifacts.AudienceCompany) && !strings.Contains(s, ":") {
		s = string(artifacts.AudienceSeat) + ":" + s
	}
	a, err := artifacts.ParseAudience(s)
	if err != nil {
		return artifacts.Audience{}, fmt.Errorf("catalog: %w", err)
	}
	return a, a.Validate()
}
