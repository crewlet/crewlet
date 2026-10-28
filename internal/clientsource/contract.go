package clientsource

import (
	"fmt"
	"slices"
)

// Reader is how a declaration is read out of the dashboard's source.
type Reader string

const (
	// ReadLiteral reads a `const` initialised with an array or object
	// literal, through [Literal].
	ReadLiteral Reader = "literal"
	// ReadUnion reads a `type` alias that is a union of string literals,
	// through [Union].
	ReadUnion Reader = "union"
	// ReadInterface reads an `interface`'s members, through [Interface].
	ReadInterface Reader = "interface"
	// ReadScalar reads a `const` initialised with one number or string,
	// through [Scalar].
	ReadScalar Reader = "scalar"
)

// Valid reports whether r is a reader this package has.
func (r Reader) Valid() bool {
	switch r {
	case ReadLiteral, ReadUnion, ReadInterface, ReadScalar:
		return true
	}
	return false
}

// ContractDir is the directory, under [Tree], that holds every declaration in
// the contract and nothing else: `dashboard/src/contract/`.
//
// ONE HOME, because a declaration the engine owns that a screen keeps beside
// its own code is one a reader of that screen edits without knowing a Go gate
// holds it — and moves without knowing the move is invisible to the gate but
// not to the next person who greps for it. `contract_test.go` holds the
// directory both ways: every row is declared there, and everything the
// directory exports is a row.
const ContractDir = "contract"

// Entry is one row of the contract: a declaration the dashboard makes because
// the engine owns the value, how it is read, and the gate that holds it
// against the engine.
type Entry struct {
	// Name is the identifier the dashboard declares.
	Name string
	// Reader is the one reader that may read it.
	Reader Reader
	// Gate is the test that owns the comparison, as the package's directory
	// from the module root and the test's name:
	// `internal/events.TestCategoryChipsAreTheEngines`. The directory is
	// under internal/, or is cmd/crewlet for a declaration the CLI owns: the
	// commands are the binary's own package, and its tests are the only
	// place they run.
	Gate string
}

// contract is every declaration the engine's gates read out of the dashboard,
// grouped by the `dashboard/src/contract/` module that declares it.
//
// ONE OWNER PER DECLARATION. Two gates over one list is how the event
// categories came to be held twice, by two tests that agreed with each other
// and would have gone on agreeing while one of them was edited; a second
// question about a declaration belongs in the gate that already owns it.
//
// A declaration NO SCREEN IMPORTS is still a row when a gate reads it: the
// memory answer's row types are declared once and composed into the one
// interface a screen names, and each of them is a shape the engine sends.
var contract = []Entry{
	// categories.ts
	{"CATEGORIES", ReadLiteral, "internal/events.TestCategoryChipsAreTheEngines"},

	// turnbands.ts
	{"WENT_WRONG", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"GIVEN", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"DID", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"LEFT_BEHIND", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"TURN_STOP", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},
	{"ABSORBED", ReadLiteral, "internal/events/types.TestTurnBandsNameOnlyTurnScopedEvents"},

	// reasons.ts
	{"PHRASES", ReadLiteral, "internal/api/queries.TestEveryWakeReasonReadsAsEnglishOnTheClient"},

	// spend.ts
	{"GROUPS", ReadLiteral, "internal/api/queries.TestEveryCostDimensionTheScreenOffersIsOneTheEngineAccepts"},
	{"ADMIN_GROUPS", ReadLiteral, "internal/api/queries.TestTheScreenOffersAnAdminsSplitsToAnAdminAlone"},
	{"BANDS", ReadLiteral, "internal/api/queries.TestTheSpendChartDrawsExactlyTheEnginesPhaseBands"},
	{"PHASE_BANDS", ReadLiteral, "internal/api/queries.TestEveryPhaseIsDrawnInTheBandTheEngineFoldsItInto"},
	{"BudgetState", ReadUnion, "internal/events/types.TestTheDashboardKnowsExactlyTheBudgetStatesTheEngineSends"},

	// work.ts
	{"WorkViewShape", ReadUnion, "internal/worklet.TestEveryViewShapeTheEngineMintsHasARenderer"},
	{"COLUMN_SORT_KEYS", ReadLiteral, "internal/worklet.TestEveryGridSortKeyIsOneTheGrammarTakes"},
	{"PROJECT_SORT_KEYS", ReadLiteral,
		"internal/worklet.TestTheProjectsDirectorySortsOnExactlyTheOrderingsTheEngineTakes"},
	{"CHANGES", ReadLiteral, "internal/worklet.TestEveryChangeKindTheEngineWritesHasAMarkAndAPhrase"},
	{"GROUP_AXES", ReadLiteral, "internal/worklet.TestEveryGroupingTheDashboardOffersIsOneTheGrammarTakes"},
	{"TASK_TITLE_MAX_BYTES", ReadScalar, "internal/worklet.TestTheDashboardBoundsWorkletTextAtTheEnginesCaps"},
	{"TASK_BODY_MAX_BYTES", ReadScalar, "internal/worklet.TestTheDashboardBoundsWorkletTextAtTheEnginesCaps"},
	{"VIEW_NAME_MAX_BYTES", ReadScalar, "internal/worklet.TestTheDashboardBoundsWorkletTextAtTheEnginesCaps"},
	{"DECISION_QUESTION_MAX_BYTES", ReadScalar,
		"internal/worklet.TestTheDashboardBoundsWorkletTextAtTheEnginesCaps"},

	// backups.ts
	{"BackupsAnswer", ReadInterface, "internal/api/queries.TestTheBackupsScreenReadsWhatTheAnswerSends"},
	{"BackupPointRow", ReadInterface, "internal/api/queries.TestTheBackupsScreenReadsWhatTheAnswerSends"},
	{"BackupCover", ReadInterface, "internal/api/queries.TestTheBackupsScreenReadsWhatTheAnswerSends"},
	{"BackupRunRow", ReadInterface, "internal/api/queries.TestTheBackupsScreenReadsWhatTheAnswerSends"},
	{"BackupOwnerKind", ReadUnion, "internal/api/queries.TestTheBackupsScreenReadsWhatTheAnswerSends"},
	{"BackupOutcome", ReadUnion, "internal/api/queries.TestTheBackupsScreenReadsWhatTheAnswerSends"},
	{"BackupPolicy", ReadUnion, "internal/api/queries.TestTheBackupsScreenReadsWhatTheAnswerSends"},

	// audit.ts
	{"OPERATOR_SOURCE", ReadScalar, "internal/events/types.TestTheAuditLogReadsEveryRuntimeAuditType"},
	{"RUNTIME_AUDIT_TYPES", ReadLiteral, "internal/events/types.TestTheAuditLogReadsEveryRuntimeAuditType"},

	// mcp.ts
	{"McpServersStatusAnswer", ReadInterface, "internal/api/queries.TestTheToolsScreenReadsWhatTheServerStatusSends"},
	{"McpStatusNode", ReadInterface, "internal/api/queries.TestTheToolsScreenReadsWhatTheServerStatusSends"},
	{"McpServerStatus", ReadInterface, "internal/api/queries.TestTheToolsScreenReadsWhatTheServerStatusSends"},
	{"McpServerNode", ReadInterface, "internal/api/queries.TestTheToolsScreenReadsWhatTheServerStatusSends"},
	{"McpServerState", ReadUnion, "internal/api/queries.TestTheToolsScreenReadsWhatTheServerStatusSends"},

	// attribution.ts
	{"CHANGE_FIELDS", ReadLiteral, "internal/worklet.TestAttributionFieldsAreTheEngines"},

	// references.ts
	{"REFERENCE_NAME", ReadScalar, "internal/envref.TestTheDashboardReadsAReferenceByTheEnginesGrammar"},

	// avatar.ts
	{"AVATAR_CHARACTERS", ReadLiteral, "internal/config.TestTheAvatarSetsAreTheDashboards"},
	{"AVATAR_COLORS", ReadLiteral, "internal/config.TestTheAvatarSetsAreTheDashboards"},

	// providersteps.ts
	{"PROVIDER_STEPS", ReadScalar, "cmd/crewlet.TestTheFirstModelStepsRunOnACompanyOnlyTheStoreHolds"},

	// models.ts
	{"CLIAuthMode", ReadUnion, "internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes"},
	{"DEFAULT_CLI_AGENT", ReadScalar, "internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes"},
	{"EXECUTOR_CLI_AGENTS", ReadLiteral, "internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes"},
	{"DEFAULT_CLI_AUTH_MODE", ReadScalar, "internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes"},
	{"DEFAULT_CLI_TIMEOUT_SECONDS", ReadScalar, "internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes"},
	{"DEFAULT_CLI_MAX_CONCURRENT", ReadScalar, "internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes"},
	{"CLI_PRODUCTS", ReadLiteral, "internal/config.TestEveryBuiltInCLIHasAProductName"},

	// config.ts
	{"ENTITY_KINDS", ReadLiteral, "internal/api/configapi.TestEntityKindsMatchTheClient"},
	{"BUDGET_WINDOWS", ReadLiteral, "internal/config.TestTheBudgetEditorOffersExactlyTheEnginesWindows"},
	{"SeatHeldHolder", ReadInterface,
		"internal/api/configapi.TestTheDashboardReadsWhatASeatHeldRefusalSends"},
	{"Charter", ReadInterface, "internal/api/configapi.TestTheDashboardReadsTheCharterTheEngineSends"},
	{"CharterMoved", ReadInterface, "internal/api/configapi.TestTheDashboardReadsWhatACharterConflictSends"},
	{"MISSION_MAX_CHARS", ReadScalar, "internal/api/configapi.TestTheDashboardBoundsTheCharterAtTheEnginesCaps"},
	{"VISION_MAX_CHARS", ReadScalar, "internal/api/configapi.TestTheDashboardBoundsTheCharterAtTheEnginesCaps"},
	{"POLICY_MAX_CHARS", ReadScalar, "internal/api/configapi.TestTheDashboardBoundsTheCharterAtTheEnginesCaps"},
	{"POLICIES_MAX", ReadScalar, "internal/api/configapi.TestTheDashboardBoundsTheCharterAtTheEnginesCaps"},

	// errors.ts
	{"QueryErrorCode", ReadUnion,
		"internal/api/stream.TestTheDashboardKnowsExactlyTheQueryErrorCodesTheEngineSends"},
	{"ACT_ERRORS", ReadLiteral, "internal/api/operator.TestTheDashboardKnowsExactlyTheActRefusals"},

	// actions.ts
	{"ACTIONS", ReadLiteral, "internal/api/operator.TestEveryActionTheDashboardTakesIsOneTheActTransportServes"},

	// domains.ts
	{"SESSION_QUERIES", ReadLiteral, "internal/api/queries.TestEverySessionQueryTakesAFreshnessFloor"},

	// gate.ts
	{"GATE_ACTIONS", ReadLiteral, "internal/api.TestTheDashboardKnowsEveryGateAction"},
	{"GATE_ACTIONS_KEEPING_OPERATION", ReadLiteral,
		"internal/api.TestTheDashboardKeepsTheGesturesOwnIDWhereTheEngineDoes"},
	{"GATE_REQUEST_TIMEOUT_MS", ReadScalar,
		"internal/api.TestTheDashboardWaitsAsLongAsTheEngineSaysAClientShould"},

	// retry.ts
	{"UNAVAILABLE_RETRY_MS", ReadScalar, "internal/api.TestTheDashboardRetriesOnTheEnginesOwnHints"},
	{"RETRY_AFTER_MAX_MS", ReadScalar, "internal/api.TestTheDashboardRetriesOnTheEnginesOwnHints"},

	// positions.ts
	{"GENERATION_STRIDE", ReadScalar,
		"internal/api.TestTheDashboardUnpacksAPositionWithTheEnginesStride"},

	// closecodes.ts
	{"CLOSE_UNAUTHENTICATED", ReadScalar,
		"internal/api/stream.TestTheDashboardClosesOnTheEnginesCloseCodes"},
	{"CLOSE_FORBIDDEN", ReadScalar, "internal/api/stream.TestTheDashboardClosesOnTheEnginesCloseCodes"},

	// retention.ts
	{"RetentionTrimFloorState", ReadUnion, "internal/statelog.TestTheDashboardKnowsEveryRetentionReportState"},
	{"RetentionGenerationState", ReadUnion, "internal/statelog.TestTheDashboardKnowsEveryRetentionReportState"},
	{"RetentionIdentityCause", ReadUnion, "internal/statelog.TestTheDashboardKnowsEveryRetentionReportState"},
	{"RetentionTermName", ReadUnion, "internal/statelog.TestTheDashboardKnowsEveryRetentionReportState"},

	// coverage.ts
	{"Coverage", ReadInterface, "internal/eventfan.TestTheDashboardDeclaresExactlyTheCoverageTheEngineSends"},
	{"NodeCoverage", ReadInterface, "internal/eventfan.TestTheDashboardDeclaresExactlyTheCoverageTheEngineSends"},

	// integrations.ts
	{"IntegrationRow", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"IntegrationsAnswer", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"ReconcileStatus", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"ReconcileFinding", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"IntegrationTool", ReadInterface, "internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends"},
	{"IntegrationToolState", ReadUnion, "internal/integration.TestTheDashboardKnowsExactlyTheRollupStates"},
	{"INTEGRATION_TOOLS", ReadLiteral, "internal/integration.TestTheDashboardGroupsSurfacesAsTheEngineDoes"},

	// memory.ts
	{"AgentMemory", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"AgentEpisode", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"DiaryEntry", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"Episode", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"EpisodeCompaction", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"SynthesizedSkill", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"CounterpartyProfile", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"CounterpartySubject", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"MemoryOverview", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},
	{"MemoryOverviewSeat", ReadInterface, "internal/api/queries.TestTheMemoryScreenReadsWhatThisAnswerSends"},

	// identity.ts
	{"GRANTS", ReadLiteral, "internal/api/iamapi.TestTheDashboardOffersExactlyTheEnginesGrants"},
	{"APPS", ReadLiteral, "internal/api/iamapi.TestTheDashboardOffersExactlyTheEnginesGrants"},
	{"PERSON_LOGIN", ReadScalar, "internal/api/iamapi.TestTheDashboardChecksALoginByTheEnginesGrammar"},
	{"MACHINE_LOGIN", ReadScalar, "internal/api/iamapi.TestTheDashboardChecksALoginByTheEnginesGrammar"},
	{"MAX_LOGIN", ReadScalar, "internal/api/iamapi.TestTheDashboardChecksALoginByTheEnginesGrammar"},
	{"CREDENTIAL_CLASSES", ReadLiteral,
		"internal/api/iamapi.TestTheDashboardChecksALoginByTheEnginesGrammar"},
	{"ASSIST_CREDENTIAL_PREFIX", ReadScalar,
		"internal/api/iamapi.TestTheDashboardKnowsTheAssistantsCredentialClass"},
	{"TOKEN_DEFAULT_DAYS", ReadScalar,
		"internal/api/iamapi.TestTheDashboardMintsWithinTheEnginesTokenLifetimes"},
	{"TOKEN_MAX_DAYS", ReadScalar,
		"internal/api/iamapi.TestTheDashboardMintsWithinTheEnginesTokenLifetimes"},
	{"FINDING_KINDS", ReadLiteral,
		"internal/api/iamapi.TestTheDashboardWordsEveryFindingKindInTheEnginesOrder"},
	{"HumanSeatsAnswer", ReadInterface,
		"internal/api/iamapi.TestTheSeatSurfacesReadWhatTheSeatListingSends"},
	{"HumanSeat", ReadInterface, "internal/api/iamapi.TestTheSeatSurfacesReadWhatTheSeatListingSends"},
	{"SeatHolder", ReadInterface, "internal/api/iamapi.TestTheSeatSurfacesReadWhatTheSeatListingSends"},
	{"SeatInvitation", ReadInterface,
		"internal/api/iamapi.TestTheSeatSurfacesReadWhatTheSeatListingSends"},
	{"ResetView", ReadInterface, "internal/api/authapi.TestTheResetScreenReadsWhatTheResetViewSends"},

	// services.ts
	{"OrgServices", ReadInterface, "internal/api.TestTheDashboardKnowsExactlyTheServicesTheEngineSends"},
	{"TrackerBackend", ReadUnion, "internal/api.TestTheDashboardKnowsExactlyTheServicesTheEngineSends"},
	{"KnowledgeBackend", ReadUnion, "internal/api.TestTheDashboardKnowsExactlyTheServicesTheEngineSends"},

	// limits.ts
	{"LimitsAnswer", ReadInterface, "internal/api.TestTheDashboardReadsExactlyTheLimitsTheEngineSends"},
	{"LimitRow", ReadInterface, "internal/api.TestTheDashboardReadsExactlyTheLimitsTheEngineSends"},
	{"LimitGroup", ReadUnion, "internal/api.TestTheDashboardReadsExactlyTheLimitsTheEngineSends"},
	{"LimitUnit", ReadUnion, "internal/api.TestTheDashboardReadsExactlyTheLimitsTheEngineSends"},

	// signin.ts
	{"SignInPolicyAnswer", ReadInterface, "internal/api.TestTheDashboardReadsExactlyTheSignInPolicyTheEngineSends"},
	{"SignInPolicy", ReadInterface, "internal/api.TestTheDashboardReadsExactlyTheSignInPolicyTheEngineSends"},
	{"SecondFactorRule", ReadUnion, "internal/api.TestTheDashboardReadsExactlyTheSignInPolicyTheEngineSends"},

	// health.ts
	{"EngineHealth", ReadInterface, "internal/api.TestTheDashboardDeclaresExactlyTheHealthTheEngineReports"},
	{"HealthAlarms", ReadInterface, "internal/api.TestTheDashboardDeclaresExactlyTheHealthTheEngineReports"},
	{"HealthObjects", ReadInterface, "internal/api.TestTheDashboardDeclaresExactlyTheHealthTheEngineReports"},
	{"ConfigAttempt", ReadInterface, "internal/api.TestTheDashboardDeclaresExactlyTheHealthTheEngineReports"},

	// wire.ts
	{"PushKind", ReadUnion, "internal/api/stream.TestTheDashboardKnowsExactlyThePushKindsTheEngineSends"},
	{"QUERY_TIMEOUT_MS", ReadScalar, "internal/api.TestTheDashboardWaitsPastAReadOfTheGroup"},
	{"MAX_EVENTS", ReadScalar, "internal/api/livestate.TestTheDashboardKeepsTheFeedTheEngineKeeps"},
	{"LIVE_CALL_DETAIL", ReadLiteral, "internal/api/livestate.TestTheDashboardMergesEveryVersionedField"},
	{"COLLEAGUE_QUERY_MAX", ReadScalar, "internal/api/queries.TestTheDashboardSendsNoNameTheEngineWouldRefuse"},
	{"SEARCH_QUERY_MAX", ReadScalar, "internal/api/queries.TestTheDashboardSendsNoSearchTheEngineWouldRefuse"},
	{"MAX_ARTIFACT_ROOMS", ReadScalar, "internal/api/stream.TestTheDashboardJoinsNoMoreRoomsThanTheEngineHolds"},
	{"SeatActivity", ReadUnion, "internal/api/livestate.TestTheDashboardKnowsExactlyTheSeatStatesTheEngineSends"},
	{"StoppedReason", ReadUnion, "internal/api/livestate.TestTheDashboardKnowsExactlyTheSeatStatesTheEngineSends"},

	// steer.ts
	{"STEER_NOTE_MAX_RUNES", ReadScalar, "internal/agent/steer.TestTheDashboardBoundsANoteAtTheEnginesCap"},
	{"STEER_READS", ReadLiteral, "internal/agent/steer.TestTheDashboardKnowsWhenANoteIsRead"},
	{"STEER_BOUNDARIES", ReadLiteral, "internal/events/types.TestTheDashboardNamesEverySteerBoundary"},

	// sandbox.ts
	{"SANDBOX_TAIL_OUTCOMES", ReadLiteral, "internal/sandbox.TestTheDashboardKnowsEveryTailOutcome"},
	{"LIVE_OUTPUT_MAX_BYTES", ReadScalar, "internal/sandbox.TestTheDashboardHoldsWhatTheRecordWillHold"},
	{"SANDBOX_TAIL_POLL_MS", ReadScalar, "internal/sandbox.TestTheDashboardPollsPastTheOwnersReuse"},

	// assistant.ts
	{"AssistantEventKind", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantTurnState", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantStatusState", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantStepState", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantCardKind", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantProposalState", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantProposalScope", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantProposalRisk", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantChangeKind", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantWordOp", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantColumnType", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantAlign", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantNoticeTone", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantErrorCode", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantResourceKind", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantViewerReason", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantActionConfirm", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantActionState", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantMessageRole", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantOpenReason", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantDraftKind", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantDraftFieldKind", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantRecordNoun", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantTurnMode", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantBriefTone", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantBriefActionKind", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"ASSISTANT_PROTOCOL_VERSION", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_INPUT_MAX_RUNES", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_PAGE_CONTEXT_MAX_BYTES", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_PAGE_DRAFT_MAX_FIELDS", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_PAGE_DRAFT_HELP_MAX_RUNES", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_PAGE_DRAFT_SECRET_PATH", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_AUDIT_REASON_MAX_RUNES", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_KEEPALIVE_SECONDS", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_ACTION_CONTEXT_INVALIDATE", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_DIGEST_PREFIX", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"AssistantFrame", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantConversationEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantPageContextEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantPageChip", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantStatusEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantDeltaEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantStepEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantStepRequest", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantRefusal", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantCardEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantActionEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantInvalidate", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantDoneEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantErrorEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantSnapshotEvent", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantTableCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantColumn", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantExports", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantLink", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantEntityCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantFact", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantEntityItem", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantNoun", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantProposalCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantChange", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantWord", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantReceipt", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantPageActionRef", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantTechnicalRequest", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantFailure", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantOpenInAppCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantErrorCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantNoticeCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantOversizeCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantPageContext", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantActionArgs", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantResource", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantResourceRef", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantPageDraft", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantDraftField", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantDraftFileRef", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantVersioned", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantTurnRequest", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantTurnInput", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantTurnAccepted", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantProposalDecision", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantProposalAnswer", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantProposalRef", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantActionReport", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantActionAnswer", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantStopAnswer", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantPurgeAnswer", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantAuditReadRequest", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantConversationList", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantConversationCreated", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantConversationSummary", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantTranscript", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantMessage", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantOpenTurn", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantAuditRead", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantTurnOutcome", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantViewerAssistant", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantModelNotice", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	// assistant.ts (deep work, im-deep)
	{"AssistantDeepState", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"AssistantAttachmentMediaType", ReadUnion, "internal/assistant/wire.TestTheDashboardKnowsEveryAssistantClosedSet"},
	{"ASSISTANT_DEEP_ACTIVITY_LINES", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_FILE_MAX_BYTES", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_ATTACHMENTS_PER_TURN", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"ASSISTANT_DELIVERED_FILES_PER_RUN", ReadScalar, "internal/assistant/wire.TestTheDashboardBoundsTheAssistantAtTheEnginesLimits"},
	{"AssistantDeepProgress", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantFileRef", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantFileCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantBriefCard", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantBriefScope", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantBriefNeed", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantBriefAction", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	{"AssistantFileStored", ReadInterface, "internal/assistant/wire.TestTheDashboardSpeaksTheAssistantProtocol"},
	// assistant.ts (page actions)
	{"ASSISTANT_PAGE_ACTIONS", ReadLiteral, "internal/assistant/catalog.TestEveryPageActionIsTheDashboardsToo"},

	// failures.ts
	{"ProviderErrorKind", ReadUnion, "internal/providers/llm.TestTheDashboardKnowsEveryErrorKind"},

	// stops.ts
	{"StopReason", ReadUnion, "internal/providers/llm.TestTheDashboardKnowsEveryStopReason"},
	{"InvocationOutcome", ReadUnion, "internal/agent/toolloop.TestTheDashboardKnowsEveryInvocationOutcome"},

	// fleet.ts
	{"BROKER_KINDS", ReadLiteral, "internal/api.TestTheDashboardKnowsEveryBrokerKind"},
	{"BROKER_FINDING_KINDS", ReadLiteral, "internal/api.TestTheDashboardKnowsEveryBrokerFinding"},
	{"BROKER_REMOVE_TIMEOUT_MS", ReadScalar, "internal/api.TestTheDashboardWaitsPastABrokerRemoval"},
	{"OBJECTS_STATES", ReadLiteral, "internal/api.TestTheDashboardKnowsEveryObjectsState"},

	// links.ts
	{"PAGE_ADDRESS_PREFIX", ReadScalar, "internal/doclet.TestTheDashboardAddressesAPageTheWayTheBacklinksReadIt"},

	// pages.ts
	{"PageReadsAnswer", ReadInterface, "internal/api/queries.TestThePageScreenReadsWhatTheseAnswersSend"},
	{"PageReadRow", ReadInterface, "internal/api/queries.TestThePageScreenReadsWhatTheseAnswersSend"},
	{"TurnPlace", ReadInterface, "internal/api/queries.TestThePageScreenReadsWhatTheseAnswersSend"},
	{"SkillLoad", ReadInterface, "internal/api/queries.TestThePageScreenReadsWhatTheseAnswersSend"},
	{"PageBacklinks", ReadInterface, "internal/api/queries.TestThePageScreenReadsWhatTheseAnswersSend"},
	{"PageLink", ReadInterface, "internal/api/queries.TestThePageScreenReadsWhatTheseAnswersSend"},
	{"TaskLink", ReadInterface, "internal/api/queries.TestThePageScreenReadsWhatTheseAnswersSend"},
	{"LinkedFromStatus", ReadUnion, "internal/api/queries.TestTheDashboardKnowsExactlyTheBacklinkStatuses"},
	{"PAGE_TITLE_MAX_BYTES", ReadScalar, "internal/doclet.TestTheDashboardBoundsPageTextAtTheEnginesCaps"},
	{"PAGE_MESSAGE_MAX_BYTES", ReadScalar, "internal/doclet.TestTheDashboardBoundsPageTextAtTheEnginesCaps"},
	{"PAGE_BODY_MAX_BYTES", ReadScalar, "internal/doclet.TestTheDashboardBoundsPageTextAtTheEnginesCaps"},
	{"PAGE_LABELS_MAX", ReadScalar, "internal/doclet.TestTheDashboardBoundsPageTextAtTheEnginesCaps"},
	{"PAGE_LABEL_MAX_BYTES", ReadScalar, "internal/doclet.TestTheDashboardBoundsPageTextAtTheEnginesCaps"},
	{"PAGE_CHANGES", ReadLiteral, "internal/doclet.TestEveryPageChangeTheEngineWritesReadsAsASentence"},

	// artifacts.ts
	{"ARTIFACT_KINDS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"ARTIFACT_KIND_SPECS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsEachKindAsTheKindTableSays"},
	{"SLIDE_ELEMENTS", ReadLiteral, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"SLIDE_STYLE_PROPERTIES", ReadLiteral, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"SLIDE_SVG_ELEMENTS", ReadLiteral, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"SLIDE_SVG_ATTRIBUTES", ReadLiteral, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"SLIDE_TRANSITIONS", ReadLiteral, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"SLIDE_SHAPES", ReadLiteral, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"SLIDE_ICONS", ReadLiteral, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"SLIDE_STAGE_WIDTH", ReadScalar, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"SLIDE_STAGE_HEIGHT", ReadScalar, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"SLIDE_ICON_GLYPH_PERCENT", ReadScalar, "internal/api/artifactsapi.TestTheRendererDrawsTheSlideSubset"},
	{"DASHBOARD_WIDGET_TYPES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_SOURCE_KINDS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_REFRESH_MODES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_DATASET_STATUSES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_AGGS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_NUMBER_FORMATS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_COLUMN_TYPES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_COLUMN_WORDS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_COLUMNS", ReadScalar, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_MIN_REFRESH_SECONDS", ReadScalar, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_MAX_WIDGETS", ReadScalar, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_MAX_DATASETS", ReadScalar, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"DASHBOARD_MAX_TABLE_COLUMNS", ReadScalar, "internal/api/artifactsapi.TestTheDashboardDrawsTheBoardFormat"},
	{"ARTIFACT_CAPABILITIES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"ARTIFACT_ROLES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"SHAREABLE_ROLES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"AUDIENCE_KINDS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"ORIGIN_MODES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"PRESENCE_ACTIVITIES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"ARTIFACT_LIST_SCOPES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"ARTIFACT_LIST_SORTS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"ARTIFACT_HISTORY_OPS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"ARTIFACT_TITLE_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_DESCRIPTION_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_ICON_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_LABEL_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"PRESENCE_FRAMES_PER_SECOND", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_CHECK_MAX_FILES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_CHECK_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"DOC_MAX_TABS", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"DOC_TAB_TITLE_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"DECK_PLAN_MAX_SLIDES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"DECK_SLIDE_TITLE_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_TRASH_RETENTION_DAYS", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_FOLDER_VISIBILITIES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"ARTIFACT_FOLDER_NAME_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_FOLDER_MAX_DEPTH", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_MOVE_MAX_ITEMS", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"ARTIFACT_LIST_DELIMITER", ReadScalar, "internal/api/artifactsapi.TestTheDashboardListsWithTheEnginesDelimiter"},

	// brand.ts
	{"BRAND_NAME_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_TAGLINE_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_VOICE_SUMMARY_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_VOICE_MAX_EXAMPLES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_VOICE_EXAMPLE_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_KIT_FACES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"BRAND_SYSTEM_STACKS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"BRAND_FACE_FAMILIES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardDrawsTheFacesTheBrandStylesheetNames"},
	{"BRAND_COLOR_FALLBACKS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardJudgesBrandContrastAsTheAdoptionDoes"},
	{"BRAND_CONTRAST_PAIRS", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardJudgesBrandContrastAsTheAdoptionDoes"},
	{"BRAND_FAMILY_NAME_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_FONT_FILES_MAX", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_FONT_FILE_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_SVG_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_IMAGE_MAX_BYTES", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_IMAGE_MAX_SIDE", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_FONT_WEIGHT_MIN", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_FONT_WEIGHT_MAX", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_FONT_WEIGHT_STEP", ReadScalar, "internal/api/artifactsapi.TestTheDashboardBoundsArtifactTextAtTheEnginesCaps"},
	{"BRAND_FAMILY_NAME_PATTERN", ReadScalar, "internal/api/artifactsapi.TestTheDashboardNamesAFamilyByTheEnginesGrammar"},
	{"BRAND_FONT_STYLES", ReadLiteral, "internal/api/artifactsapi.TestTheDashboardSpeaksTheArtifactsVocabulary"},
	{"COLUMN_TYPES", ReadLiteral, "internal/api/artifactdataapi.TestTheDashboardSpeaksTheDatastoreVocabulary"},
	{"READ_RULES", ReadLiteral, "internal/api/artifactdataapi.TestTheDashboardSpeaksTheDatastoreVocabulary"},
	{"COLLECTION_PRESETS", ReadLiteral, "internal/api/artifactdataapi.TestTheDashboardSpeaksTheDatastoreVocabulary"},
	{"FIELD_CONDITIONS", ReadLiteral, "internal/api/artifactdataapi.TestTheDashboardSpeaksTheDatastoreVocabulary"},
	{"STATUS_CATEGORIES", ReadLiteral, "internal/api/artifactdataapi.TestTheDashboardSpeaksTheDatastoreVocabulary"},
	{"DATA_OP_KINDS", ReadLiteral, "internal/api/artifactdataapi.TestTheDashboardSpeaksTheDatastoreVocabulary"},
	{"DATA_VERDICTS", ReadLiteral, "internal/api/artifactdataapi.TestTheDashboardSpeaksTheDatastoreVocabulary"},
	{"STATE_COLLECTION", ReadScalar, "internal/api/artifactdataapi.TestTheDashboardSpeaksTheDatastoreVocabulary"},
	{"DATA_BATCH_MAX_OPS", ReadScalar, "internal/api/artifactdataapi.TestTheDashboardBoundsTheDatastoreAtTheEnginesCaps"},
	{"DATA_COLLECTIONS_MAX", ReadScalar, "internal/api/artifactdataapi.TestTheDashboardBoundsTheDatastoreAtTheEnginesCaps"},
	{"DATA_COLUMNS_MAX", ReadScalar, "internal/api/artifactdataapi.TestTheDashboardBoundsTheDatastoreAtTheEnginesCaps"},
	{"COLLECTION_NAME_PATTERN", ReadScalar, "internal/api/artifactdataapi.TestTheDashboardNamesCollectionsAndFieldsByTheEnginesGrammar"},
	{"RESERVED_COLLECTIONS", ReadLiteral, "internal/api/artifactdataapi.TestTheDashboardNamesCollectionsAndFieldsByTheEnginesGrammar"},
	{"FIELD_NAME_PATTERN", ReadScalar, "internal/api/artifactdataapi.TestTheDashboardNamesCollectionsAndFieldsByTheEnginesGrammar"},
	{"ARTIFACT_PUBLISH_TOOLS", ReadLiteral, "internal/agent/builtin.TestATranscriptLinksTheArtifactAPublishWrote"},
	{"ARTIFACT_ANSWER_ID", ReadScalar, "internal/agent/builtin.TestATranscriptLinksTheArtifactAPublishWrote"},
	{"ARTIFACT_ANSWER_VERSION", ReadScalar, "internal/agent/builtin.TestATranscriptLinksTheArtifactAPublishWrote"},
	{"RUNTIME_CONTRACT", ReadScalar, "internal/artifactupload.TestTheEmbeddedArtifactTreeServesWhatTheDashboardBuilds"},
	{"CAVEAT_ACTIONS", ReadLiteral, "internal/caveat.TestTheDashboardReadsWhatACaveatAsks"},
	{"LIMIT_SETTINGS", ReadLiteral, "internal/caveat.TestTheDashboardKnowsEverySettingARemedyNames"},
	{"Remedy", ReadInterface, "internal/caveat.TestTheDashboardReadsARemedyAsTheEngineWritesIt"},
}

// Contract is every declaration the engine's gates read out of the
// dashboard's source, with its reader and its owning gate.
func Contract() []Entry { return slices.Clone(contract) }

// registered refuses a declaration the contract does not carry, or carries
// under another reader.
//
// THIS IS THE DIRECTION `contract_test.go` CANNOT WALK: a gate holding a
// declaration nobody registered would be a comparison no table knows about,
// and the table would stop describing what the engine checks. Refused at the
// read, the gate fails the first time it runs.
func registered(name string, reader Reader) error {
	i := slices.IndexFunc(contract, func(e Entry) bool { return e.Name == name })
	switch {
	case i < 0:
		return fmt.Errorf("clientsource: %s is not in the contract "+
			"(internal/clientsource/contract.go) — a declaration a gate holds "+
			"against the engine is registered there with its reader and its "+
			"owning gate before anything reads it", name)
	case contract[i].Reader != reader:
		return fmt.Errorf("clientsource: the contract reads %s as a %s, not a %s",
			name, contract[i].Reader, reader)
	}
	return nil
}
