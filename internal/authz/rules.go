package authz

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// The verbs this engine authorizes.
//
// NAMED AFTER THE VERB, NOT THE SURFACE. `PATCH /work/items/{key}` and the
// `update_work_item` tool are one action asked two ways, so they are one
// constant — which is the whole of what stops the operator MCP and a seat's
// own tools answering differently about one write. Where a tool exists its
// name IS the action, so a reader grepping either finds the other.
const (
	// --- reading what the company is doing -------------------------- //
	ActionWorkRead      Action = "get_work_item"
	ActionWorkList      Action = "list_work_items"
	ActionWorkSearch    Action = "search_work_items"
	ActionWorkActivity  Action = "task_activity"
	ActionProjectRead   Action = "describe_project"
	ActionProjectList   Action = "list_projects"
	ActionGoalList      Action = "list_work_goals"
	ActionCatalogueRead Action = "get_work_catalogue"
	ActionViewList      Action = "list_work_views"
	ActionPageRead      Action = "get_page"
	ActionPageList      Action = "list_pages"
	ActionKnowledgeRead Action = "search_knowledge"
	ActionColleagueRead Action = "lookup_colleague"

	// --- a colleague's ordinary work -------------------------------- //
	ActionWorkCreate  Action = "create_work_item"
	ActionWorkUpdate  Action = "update_work_item"
	ActionWorkComment Action = "comment_on_work_item"
	ActionWorkMerge   Action = "merge_work_item"
	ActionGoalWrite   Action = "write_work_goal"
	ActionPageCreate  Action = "write_page"
	ActionPageSave    Action = "save_page"
	ActionPageComment Action = "comment_on_page"

	// --- somebody's own record -------------------------------------- //
	ActionPersonRead    Action = "get_person"
	ActionMyWork        Action = "my_work"
	ActionInboxRead     Action = "work_inbox"
	ActionInboxMark     Action = "mark_inbox"
	ActionPinsSet       Action = "set_pins"
	ActionViewSave      Action = "save_work_view"
	ActionPrioritiesSet Action = "set_priorities"
	ActionOnboardedMark Action = "mark_onboarded"

	// --- a container's own policy ----------------------------------- //
	ActionProjectWrite   Action = "write_project"
	ActionCatalogueWrite Action = "write_work_catalogue"
	ActionPageRename     Action = "pages.rename"
	ActionContainerWrite Action = "pages.container"

	// --- taking something out of circulation ------------------------ //
	ActionWorkRemove  Action = "remove_work_item"
	ActionWorkRestore Action = "restore_work_item"
	ActionPageTrash   Action = "pages.trash"
	ActionPageRestore Action = "pages.restore"

	// --- beyond recovery -------------------------------------------- //
	ActionWorkPurge Action = "work.purge"
	ActionPagePurge Action = "pages.purge"

	// --- what somebody wrote ---------------------------------------- //
	ActionPageCommentEdit   Action = "pages.comment.edit"
	ActionPageCommentRemove Action = "pages.comment.remove"

	// --- the company's own controls --------------------------------- //
	ActionConfigRead     Action = "config.read"
	ActionConfigWrite    Action = "config.write"
	ActionSecretList     Action = "secrets.list"
	ActionSecretReveal   Action = "secrets.reveal"
	ActionSecretWrite    Action = "secrets.write"
	ActionSetupRead      Action = "setup.read"
	ActionSetupConnect   Action = "setup.connect"
	ActionFleetOperate   Action = "fleet.operate"
	ActionTranscriptRead Action = "transcripts.read"
	ActionSandboxRun     Action = "run_sandbox"
)

// rule is one verb's row: which class decides it, and — for [ClassOperator]
// alone — which capability that class asks for.
//
// ONE TABLE RATHER THAN TWO. A class map beside a grant map is two things
// that must agree about which verbs are operator surfaces, and the day they
// disagree one verb is decided by a class that reads a grant nobody set.
type rule struct {
	class Class
	// grant is what [ClassOperator] requires. Empty on every other class,
	// which decides from the principal's relation to the object instead —
	// and empty on an operator row is a gate somebody forgot to fill in,
	// which [granted] refuses.
	grant iam.Grant
}

// rules is THE authority table: every verb this engine knows, and how it is
// decided.
//
// A VERB MISSING FROM HERE IS REFUSED, and the completeness walk in this
// package's tests is what turns that into a build failure rather than a
// production one. The alternative — a default class — is exactly how a new
// verb ships ungated, and it ships looking correct.
var rules = map[Action]rule{
	ActionWorkRead:      {class: ClassRead},
	ActionWorkList:      {class: ClassRead},
	ActionWorkSearch:    {class: ClassRead},
	ActionWorkActivity:  {class: ClassRead},
	ActionProjectRead:   {class: ClassRead},
	ActionProjectList:   {class: ClassRead},
	ActionGoalList:      {class: ClassRead},
	ActionCatalogueRead: {class: ClassRead},
	ActionViewList:      {class: ClassRead},
	ActionPageRead:      {class: ClassRead},
	ActionPageList:      {class: ClassRead},
	ActionKnowledgeRead: {class: ClassRead},
	ActionColleagueRead: {class: ClassRead},

	ActionWorkCreate:  {class: ClassColleagueWrite},
	ActionWorkUpdate:  {class: ClassColleagueWrite},
	ActionWorkComment: {class: ClassColleagueWrite},
	ActionWorkMerge:   {class: ClassColleagueWrite},
	ActionGoalWrite:   {class: ClassColleagueWrite},
	ActionPageCreate:  {class: ClassColleagueWrite},
	ActionPageSave:    {class: ClassColleagueWrite},
	ActionPageComment: {class: ClassColleagueWrite},

	// READING SOMEBODY'S DAY IS THE LEAD RELATION, and MARKING it is not.
	// A lead re-orders what their report works on and says what is
	// important; marking somebody's mail read and rearranging their
	// pinned views is a gesture nobody asked a lead to make.
	ActionPersonRead:    {class: ClassOwnOrLead},
	ActionMyWork:        {class: ClassOwnOrLead},
	ActionInboxRead:     {class: ClassOwnOrLead},
	ActionPrioritiesSet: {class: ClassOwnOrLead},
	ActionInboxMark:     {class: ClassOwnRecord},
	ActionPinsSet:       {class: ClassOwnRecord},
	ActionViewSave:      {class: ClassOwnRecord},
	ActionOnboardedMark: {class: ClassOwnRecord},

	ActionProjectWrite:   {class: ClassContainer},
	ActionCatalogueWrite: {class: ClassContainer},
	ActionPageRename:     {class: ClassContainer},
	ActionContainerWrite: {class: ClassContainer},

	ActionWorkRemove:  {class: ClassDestructive},
	ActionWorkRestore: {class: ClassDestructive},
	ActionPageTrash:   {class: ClassDestructive},
	ActionPageRestore: {class: ClassDestructive},

	ActionWorkPurge: {class: ClassPurge},
	ActionPagePurge: {class: ClassPurge},

	ActionPageCommentEdit:   {class: ClassAuthored},
	ActionPageCommentRemove: {class: ClassAuthored},

	ActionConfigRead:     {class: ClassOperator, grant: iam.GrantConfigRead},
	ActionConfigWrite:    {class: ClassOperator, grant: iam.GrantConfigWrite},
	ActionSecretList:     {class: ClassOperator, grant: iam.GrantConfigRead},
	ActionSecretReveal:   {class: ClassOperator, grant: iam.GrantSecretRead},
	ActionSecretWrite:    {class: ClassOperator, grant: iam.GrantSecretWrite},
	ActionSetupRead:      {class: ClassOperator, grant: iam.GrantConfigRead},
	ActionSetupConnect:   {class: ClassOperator, grant: iam.GrantConfigWrite},
	ActionFleetOperate:   {class: ClassOperator, grant: iam.GrantFleetOperate},
	ActionTranscriptRead: {class: ClassOperator, grant: iam.GrantTranscriptRead},
	ActionSandboxRun:     {class: ClassOperator, grant: iam.GrantSandboxRun},
}

// Actions is every verb this build authorizes, sorted, for the walks that ask
// whether a surface covered all of them.
func Actions() []Action { return slices.Sorted(maps.Keys(rules)) }

// ClassOf reports which rule governs a verb.
//
// EXPORTED FOR THE WALKS rather than for a caller to branch on: a surface that
// read the class and decided for itself would be the second implementation
// this package exists to remove. Use [Decide].
func ClassOf(a Action) (Class, bool) {
	r, ok := rules[a]
	return r.class, ok
}
