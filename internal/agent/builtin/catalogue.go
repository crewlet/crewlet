package builtin

import (
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE CATALOGUE: where each built-in is filed (the group it is shown in) and
// the gate its authority row decides, since holding a tool is not being
// allowed to call it ([gate] asks internal/authz of the tool's own row).
//
// The gate is written down, not read off [authz.ClassOf] at run time: a
// surface deciding from a class would be the second implementation
// internal/authz exists to remove. It decides nothing; this package's tests
// walk every gate against its row's class, app and artifact role, and the
// groups against the registry, both ways. Listed: what a seat's registry
// holds ([Register]) and the discovery pair (internal/toolname); not the
// operator's verbs ([OperatorTools]), never an agent's, nor a tool whose row
// bars every agent ([Withheld]).

// Group is one shelf of the built-in catalogue.
//
// A closed set: the dashboard draws one rail entry per group, so an unknown
// value is a value rather than a panic ([Group.Valid]).
type Group string

const (
	// GroupWork is Worklet's work items: reading, filing, moving and
	// discussing them.
	GroupWork Group = "work"
	// GroupProjects is each project's settings and the files kept in it.
	GroupProjects Group = "projects"
	// GroupKnowledge is the company's written knowledge: Doclet's pages and
	// the knowledge search.
	GroupKnowledge Group = "knowledge"
	// GroupChat is Chatlet's rooms and direct messages.
	GroupChat Group = "chat"
	// GroupArtifacts is the artifacts agents make and share, their
	// folders, the company's brand and each artifact's data.
	GroupArtifacts Group = "artifacts"
	// GroupCode is Gitlet, the engine's own git host: its repositories, their
	// branches and history, the changes proposed to them, their reviews
	// and their checks.
	GroupCode Group = "code"
	// GroupHealth is the deployment's health: every live node and the Gitlet
	// nodes' caches. A shelf of its own rather than the code's, because it
	// answers about the fleet, not about one repository.
	GroupHealth Group = "health"
	// GroupAtlas is the company's atlas: what it runs on, how sure the
	// atlas is of each entry, and what depends on what.
	GroupAtlas Group = "atlas"
	// GroupServers is the company's servers, reached over SSH.
	GroupServers Group = "servers"
	// GroupColleagues is finding a colleague and asking another agent.
	GroupColleagues Group = "colleagues"
	// GroupMemory is the agent's own diary, past turns and sessions, and
	// skills.
	GroupMemory Group = "memory"
	// GroupDiscovery is the pair an agent reaches MCP tools through.
	GroupDiscovery Group = "discovery"
)

// Groups are the twelve, in the order the catalogue shows them.
var Groups = []Group{
	GroupWork, GroupProjects, GroupKnowledge, GroupChat, GroupArtifacts,
	GroupCode, GroupHealth, GroupAtlas, GroupServers, GroupColleagues, GroupMemory, GroupDiscovery,
}

// Valid reports whether g is a group this build files tools under.
func (g Group) Valid() bool { return slices.Contains(Groups, g) }

// groupWords are each group's name and one line about it, as a reader sees
// them.
var groupWords = map[Group]struct{ label, description string }{
	GroupWork: {"Work items",
		"Worklet, the company's tracker: read, file, discuss and move work."},
	GroupProjects: {"Projects",
		"Each project's settings, and the files kept in it."},
	GroupKnowledge: {"Knowledge",
		"The company's pages in Doclet, and the search over its knowledge."},
	GroupChat: {"Chat",
		"Chatlet rooms and direct messages."},
	GroupArtifacts: {"Artifacts",
		"Documents, slides, sheets and dashboards agents make and share, and the data behind them."},
	GroupCode: {"Code",
		"The company's repositories on Gitlet: their files and history, branches, changes, reviews and checks."},
	GroupHealth: {"Health",
		"The deployment's health: every live node, and the Gitlet nodes' caches."},
	GroupAtlas: {"Atlas",
		"The company's atlas: what it runs on, how sure the atlas is of each entry, and what depends on what."},
	GroupServers: {"Servers",
		"The company's servers over SSH: allowlisted reads, and commands a person or a delegate approves first."},
	GroupColleagues: {"Colleagues",
		"Finding a colleague, and asking another agent a question."},
	GroupMemory: {"Memory and skills",
		"The agent's own notes, past turns and sessions, and skills."},
	GroupDiscovery: {"MCP discovery",
		"How an agent reaches MCP tools: it lists a server's tools, then activates the ones it needs."},
}

// Label is the group's name as a reader sees it, or the group itself for one
// this build does not know.
func (g Group) Label() string {
	if w, ok := groupWords[g]; ok {
		return w.label
	}
	return string(g)
}

// Description is one line about what the group's tools are for, or empty for
// a group this build does not know.
func (g Group) Description() string { return groupWords[g].description }

// Gate is who may call a built-in: what its authority row asks of the caller,
// named rather than decided.
//
// Flat, one word per row shape, answering "could this agent call it?". The
// walk in this package's tests holds each to its rows' class, app and
// artifact role, so a row of a shape no gate names fails until one does.
type Gate string

const (
	// GateEverybody names any agent; the row asks nothing
	// ([authz.ClassEverybody]). Also the discovery pair's, which no row
	// decides: every phase that discovers is handed it.
	GateEverybody Gate = "everybody"
	// GateSelf names the calling agent, about its own state and nobody else's
	// ([authz.ClassSelf]).
	GateSelf Gate = "self"
	// GateWorklet names an agent that uses Worklet ([authz.ClassApp] on
	// [iam.GrantWorklet]), including a verb that is about the caller's own
	// record and takes no name for anybody else's.
	GateWorklet Gate = "worklet"
	// GateWorkletAsked names an agent that uses Worklet, or one an open ask on
	// the item names ([authz.ClassAppOrAsked] on Worklet).
	GateWorkletAsked Gate = "worklet_asked"
	// GateProjectLead names the lead of the project the work or the file is in,
	// who uses Worklet ([authz.ClassDestructive] on Worklet).
	GateProjectLead Gate = "project_lead"
	// GateDoclet names an agent that uses Doclet ([authz.ClassApp] on
	// [iam.GrantDoclet]).
	GateDoclet Gate = "doclet"
	// GateChatlet names an agent that uses Chatlet ([authz.ClassApp] on
	// [iam.GrantChatlet]).
	GateChatlet Gate = "chatlet"
	// GateArtifacts names an agent that authors artifacts ([authz.ClassApp] on
	// [iam.GrantArtifacts]).
	GateArtifacts Gate = "artifacts"
	// GateGitlet names an agent that uses Gitlet ([authz.ClassApp] on
	// [iam.GrantGitlet]), which every seat does as it uses every app: what it
	// may do to a repository is its grant there, which the code domain
	// decides, and never this gate.
	GateGitlet Gate = "gitlet"
	// GateAtlas names an agent that uses the atlas ([authz.ClassApp] on
	// [iam.GrantAtlas]), which every seat does as it uses every app: a
	// private address it reads only within its own reach (ADR-0109).
	GateAtlas Gate = "atlas"
	// GateArtifactViewer names anybody the artifact is shared with, as a viewer
	// or more ([authz.ClassShared], least role viewer, no app).
	GateArtifactViewer Gate = "artifact_viewer"
	// GateArtifactEditor names the artifact's editors who author artifacts
	// ([authz.ClassShared], least role editor, beside
	// [iam.GrantArtifacts]).
	GateArtifactEditor Gate = "artifact_editor"
	// GateArtifactOwner names the artifact's owner ([authz.ClassShared], least
	// role owner, no app).
	GateArtifactOwner Gate = "artifact_owner"
	// GateArtifactCommenter names anybody the artifact is shared with as a
	// commenter or more ([authz.ClassShared], least role commenter, no app).
	GateArtifactCommenter Gate = "artifact_commenter"
	// GateHealthTools names an agent whose role carries the health grant,
	// `role.health_tools` ([authz.ClassAdmin] with [authz.SeatGrantHealth]):
	// the grant alone decides, no app or relation.
	GateHealthTools Gate = "health_tools"
	// GateSSHRead names an agent whose SSH access is on with a server
	// ([authz.ClassAdmin] with [authz.SeatGrantSSHRead]).
	GateSSHRead Gate = "ssh_read"
	// GateSSHOperate names an agent whose SSH access is at operate
	// ([authz.ClassAdmin] with [authz.SeatGrantSSHOperate]).
	GateSSHOperate Gate = "ssh_operate"
	// GateDelegate names an agent an enabled delegation rule names
	// ([authz.ClassAdmin] with [authz.SeatGrantDelegate]).
	GateDelegate Gate = "delegated_decisions"
)

// Gates are the eighteen, in declaration order.
var Gates = []Gate{
	GateEverybody, GateSelf, GateWorklet, GateWorkletAsked, GateProjectLead,
	GateDoclet, GateChatlet, GateArtifacts, GateGitlet, GateAtlas, GateArtifactViewer,
	GateArtifactEditor, GateArtifactOwner, GateArtifactCommenter, GateHealthTools, GateSSHRead,
	GateSSHOperate, GateDelegate,
}

// Valid reports whether g is a gate this build names.
func (g Gate) Valid() bool { return slices.Contains(Gates, g) }

// Filing is where one built-in sits in the catalogue, and who may call it.
type Filing struct {
	Group Group
	Gate  Gate
	// Vendor is the vendor permission the tool needs, the zero value for one
	// the engine's own estates serve.
	Vendor VendorNeed
	// Rank orders the tools of one group: reads before writes, the
	// everyday before the rare.
	Rank int
}

// filed is every built-in the catalogue lists, each group's tools in the
// order it shows them.
//
// Keyed on the tool's name, as the authority table and [annotationsFor] are:
// one tool type serving two surfaces shares it.
var filed = []struct {
	tool   string
	group  Group
	gate   Gate
	vendor VendorNeed
}{
	{ListWorkItemsTool, GroupWork, GateWorklet, reachesNoVendor},
	{GetWorkItemTool, GroupWork, GateWorkletAsked, reachesNoVendor},
	{SearchWorkItemsTool, GroupWork, GateWorklet, reachesNoVendor},
	{worklet.GetWorkCatalogueTool, GroupWork, GateWorklet, reachesNoVendor},
	{worklet.TaskActivityTool, GroupWork, GateWorklet, reachesNoVendor},
	{worklet.MyWorkTool, GroupWork, GateWorklet, reachesNoVendor},
	{CreateWorkItemTool, GroupWork, GateWorklet, reachesNoVendor},
	{UpdateWorkItemTool, GroupWork, GateWorklet, reachesNoVendor},
	{CommentOnWorkTool, GroupWork, GateWorkletAsked, reachesNoVendor},
	// A seat's own question, as its sessions are its own.
	{WithdrawAskTool, GroupWork, GateSelf, reachesNoVendor},
	{RequestAccessTool, GroupWork, GateSelf, reachesNoVendor},
	{ReviewDelegatedTool, GroupWork, GateDelegate, reachesNoVendor},
	{MergeWorkItemTool, GroupWork, GateWorklet, reachesNoVendor},
	{worklet.MoveWorkItemTool, GroupWork, GateProjectLead, reachesNoVendor},

	{worklet.ListProjectsTool, GroupProjects, GateWorklet, reachesNoVendor},
	{worklet.DescribeProjectTool, GroupProjects, GateWorklet, reachesNoVendor},
	{worklet.WriteProjectTool, GroupProjects, GateWorklet, reachesNoVendor},
	{worklet.ListProjectFilesTool, GroupProjects, GateWorklet, reachesNoVendor},
	{worklet.ReadProjectFileTool, GroupProjects, GateWorklet, reachesNoVendor},
	{worklet.WriteProjectFileTool, GroupProjects, GateWorklet, reachesNoVendor},
	{worklet.RemoveProjectFileTool, GroupProjects, GateProjectLead, reachesNoVendor},

	{SearchKnowledgeTool, GroupKnowledge, GateDoclet, searchesConfluence},
	{ListPagesTool, GroupKnowledge, GateDoclet, reachesNoVendor},
	{GetPageTool, GroupKnowledge, GateDoclet, reachesNoVendor},
	{WritePageTool, GroupKnowledge, GateDoclet, reachesNoVendor},
	{SavePageTool, GroupKnowledge, GateDoclet, reachesNoVendor},
	{CommentOnPageTool, GroupKnowledge, GateDoclet, reachesNoVendor},

	{ListChannelsTool, GroupChat, GateChatlet, reachesNoVendor},
	{ReadChannelTool, GroupChat, GateChatlet, reachesNoVendor},
	{SearchMessagesTool, GroupChat, GateChatlet, reachesNoVendor},
	{PostMessageTool, GroupChat, GateChatlet, reachesNoVendor},
	{ReplyInThreadTool, GroupChat, GateChatlet, reachesNoVendor},
	{SendDMTool, GroupChat, GateChatlet, reachesNoVendor},
	{ReactToMessageTool, GroupChat, GateChatlet, reachesNoVendor},
	{JoinChannelTool, GroupChat, GateChatlet, reachesNoVendor},
	{LeaveChannelTool, GroupChat, GateChatlet, reachesNoVendor},

	{ListArtifactsTool, GroupArtifacts, GateEverybody, reachesNoVendor},
	{GetArtifactTool, GroupArtifacts, GateArtifactViewer, reachesNoVendor},
	{RenderArtifactTool, GroupArtifacts, GateArtifactViewer, reachesNoVendor},
	{ArtifactFormatTool, GroupArtifacts, GateEverybody, reachesNoVendor},
	{CompanyBrandTool, GroupArtifacts, GateEverybody, reachesNoVendor},
	{DashboardSourcesTool, GroupArtifacts, GateEverybody, reachesNoVendor},
	{ReadArtifactDataTool, GroupArtifacts, GateArtifactViewer, reachesNoVendor},
	{QueryArtifactDataTool, GroupArtifacts, GateArtifactViewer, reachesNoVendor},
	{CreateArtifactTool, GroupArtifacts, GateArtifacts, reachesNoVendor},
	{PublishArtifactTool, GroupArtifacts, GateArtifactEditor, reachesNoVendor},
	{UpdateArtifactTool, GroupArtifacts, GateArtifactEditor, reachesNoVendor},
	{ShareArtifactTool, GroupArtifacts, GateArtifactOwner, reachesNoVendor},
	{UnshareArtifactTool, GroupArtifacts, GateArtifactOwner, reachesNoVendor},
	{UploadArtifactAssetTool, GroupArtifacts, GateArtifactEditor, reachesNoVendor},
	{CreateArtifactFolderTool, GroupArtifacts, GateArtifacts, reachesNoVendor},
	{MoveArtifactTool, GroupArtifacts, GateArtifactEditor, reachesNoVendor},
	{ListArtifactCommentsTool, GroupArtifacts, GateArtifactViewer, reachesNoVendor},
	{CommentOnArtifactTool, GroupArtifacts, GateArtifactCommenter, reachesNoVendor},
	{ResolveArtifactCommentTool, GroupArtifacts, GateArtifactCommenter, reachesNoVendor},
	{EditArtifactCommentTool, GroupArtifacts, GateArtifactCommenter, reachesNoVendor},
	{RemoveArtifactCommentTool, GroupArtifacts, GateArtifactCommenter, reachesNoVendor},
	{WriteArtifactDataTool, GroupArtifacts, GateArtifactViewer, reachesNoVendor},
	{SetArtifactSchemaTool, GroupArtifacts, GateArtifactEditor, reachesNoVendor},

	// Gitlet's tools take the app (ADR-0110), which every seat holds and a
	// woken turn holds to its requester's (ADR-0109); what a seat may do to a
	// repository is the code domain's, by the repository's grant for the seat
	// and its requester, never a role's permission. Reads first, then writes.
	// Not the three a person alone makes (code_report_check,
	// code_purge_unreachable, code_import_repo): their rows bar every agent.
	{CodeListReposTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeGetRepoTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeResolveTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeReadTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeSearchTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeDiffTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeCompareTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeLandedInTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeLogTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeBlameTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeListChangesTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeGetChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeGetCheckFailuresTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeCommitFilesTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeRevertTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeCherryPickTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeCreateBranchTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeDeleteBranchTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeCreateRepoTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeUpdateRepoTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeDeleteRepoTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeRestoreRepoTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeProtectBranchTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeOpenChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeUpdateChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeCloseChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeRequestReviewTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeReviewChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeCommentOnChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeDeleteCommentTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeResolveFindingTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeApplySuggestionsTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeRebaseChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeMergeChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeDequeueChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeRevertChangeTool, GroupCode, GateGitlet, reachesNoVendor},
	{CodeRerunChecksTool, GroupCode, GateGitlet, reachesNoVendor},
	// Held for a delegate or a person: any agent may ask; carrying it out is
	// that approver's credential's (release.go).
	{AskToMergeTool, GroupCode, GateEverybody, reachesNoVendor},
	{AskToDeployTool, GroupCode, GateEverybody, reachesNoVendor},

	{FleetHealthTool, GroupHealth, GateHealthTools, reachesNoVendor},
	{CodeStatusTool, GroupHealth, GateHealthTools, reachesNoVendor},

	{AtlasSearchTool, GroupAtlas, GateAtlas, reachesNoVendor},
	{AtlasGetTool, GroupAtlas, GateAtlas, reachesNoVendor},
	{AtlasTraceTool, GroupAtlas, GateAtlas, reachesNoVendor},
	{AtlasRecordTool, GroupAtlas, GateAtlas, reachesNoVendor},
	{AtlasDisputeTool, GroupAtlas, GateAtlas, reachesNoVendor},

	{SSHReadTool, GroupServers, GateSSHRead, reachesNoVendor},
	{SSHOperateTool, GroupServers, GateSSHOperate, reachesNoVendor},
	{SSHAskOperateTool, GroupServers, GateSSHOperate, reachesNoVendor},

	{LookupColleagueTool, GroupColleagues, GateEverybody, reachesNoVendor},
	{A2AAskTool, GroupColleagues, GateEverybody, reachesNoVendor},

	{RefreshMemoryTool, GroupMemory, GateSelf, reachesNoVendor},
	{QueryEpisodesTool, GroupMemory, GateSelf, reachesNoVendor},
	{ListSessionsTool, GroupMemory, GateSelf, reachesNoVendor},
	{ReadSessionTool, GroupMemory, GateSelf, reachesNoVendor},
	{ReflectAndPersistTool, GroupMemory, GateSelf, reachesNoVendor},
	{UseSkillTool, GroupMemory, GateSelf, reachesNoVendor},
	{RefineSkillTool, GroupMemory, GateSelf, reachesNoVendor},
	{LoadToolSkillTool, GroupMemory, GateSelf, reachesNoVendor},
	{MarkOnboardedTool, GroupMemory, GateSelf, reachesNoVendor},

	// No authority row decides these two (internal/toolname says why): the
	// runner hands them to every phase that discovers.
	{toolname.ListMCPServerTools, GroupDiscovery, GateEverybody, reachesNoVendor},
	{toolname.Activate, GroupDiscovery, GateEverybody, reachesNoVendor},
}

// filings indexes [filed] by tool, once.
var filings = sync.OnceValue(func() map[string]Filing {
	out := make(map[string]Filing, len(filed))
	ranks := map[Group]int{}
	for _, f := range filed {
		out[f.tool] = Filing{Group: f.group, Gate: f.gate, Rank: ranks[f.group], Vendor: f.vendor}
		ranks[f.group]++
	}
	return out
})

// FilingOf is where a built-in sits in the catalogue and who may call it, or
// false for a tool the catalogue does not list: an MCP server's, or a verb
// only the operator's surface offers.
func FilingOf(tool string) (Filing, bool) {
	f, ok := filings()[tool]
	return f, ok
}

// Catalogued is every tool the catalogue lists, in its order: group by group,
// each group's tools by rank.
func Catalogued() []string {
	out := make([]string, 0, len(filed))
	for _, g := range Groups {
		for _, f := range filed {
			if f.group == g {
				out = append(out, f.tool)
			}
		}
	}
	return out
}
