package wiregate

// THE INVENTORY ONLY SHRINKS. The gate arrived after the code, so what the tree
// held then is listed here, and the gate refuses an entry it no longer finds
// as well as a finding with no entry: a field sent always or marked leaves its
// line, and a new one never gets one. Each package's entries go as its fields
// are judged, sent always or marked, a package at a time.

// omittedInventory is, per package, every flag and number a client receives
// that its struct leaves out at zero with no marker, as struct.key: 601 when
// the gate arrived.
var omittedInventory = map[string][]string{
	"internal/api": {
		"GateDomainAnswer.unvouched",
	},
	"internal/api/artifactsapi": {
		"AdoptedBrand.newer_version", "Artifact.elevated", "CommentRemark.removed",
		"CommentThread.resolved_version", "ListItem.pinned", "ShareAccess.invalid",
		"movedEntry.truncated",
	},
	"internal/api/configapi": {
		"Reloaded.deferred", "Reloaded.epoch",
	},
	"internal/api/iamapi": {
		"NodeToken.company_writer", "SeatHolding.sealed", "SeatInvitation.sealed",
		"credentialView.spent", "invitationView.sealed", "personView.sealed",
		"sessionView.enrolment_only",
	},
	"internal/api/livestate": {
		"ExecutorEntry.body_withheld", "LiveCall.failed", "LiveCall.max_rounds",
		"LiveCall.round_ceiling", "LiveCall.running_calls_elided",
	},
	"internal/api/queries": {
		"AtlasWorkAnswer.capped", "BackupPointRow.bytes", "CheckLogPage.next",
		"CodeChangeFiles.since_revision", "CodeCommit.more", "CodeMergePreview.conflicts_omitted",
		"chatRail.default_private", "chatRail.truncated", "chatRail.unreadable", "chatRoom.muted",
		"chatSearchAnswer.building",
	},
	"internal/api/setupapi": {
		"SSHServer.containers", "SSHServer.inventory", "SeatState.enrolled",
		"ToolState.seats_required",
	},
	"internal/api/stream": {
		"ArtifactChange.head", "ArtifactData.gone", "ArtifactData.reread", "ArtifactData.resync",
		"ArtifactData.schema", "ArtifactData.through", "ChatChange.channel_seq",
		"ChatPresence.roster", "DocKey.deleted", "Envelope.id", "SpendRollup.days",
		"SpendRollup.entries_withheld",
	},
	"internal/artifactdata": {
		"Change.av", "Change.bv", "Doc.deleted", "DocDelta.deleted", "DocPage.resync",
		"HistoryEntry.elevated", "HistoryEntry.undoable", "HistoryEntry.undoes",
	},
	"internal/artifacts": {
		"Entry.carried", "ScriptPolicy.unsafe_eval", "ScriptPolicy.wasm_eval",
	},
	"internal/artifacts/commentanchor": {
		"Anchor.x", "Anchor.y",
	},
	"internal/artifacts/dashspec": {
		"EngineSource.windowed", "Freshness.every_seconds", "SourceParam.max_bytes",
		"Table.truncated",
	},
	"internal/artifacts/slideformat": {
		"OutlineSlide.hidden", "OutlineSlide.notes", "Slide.hidden",
	},
	"internal/artifactsdomain": {
		"HistoryEntry.elevated", "HistoryEntry.number", "HistorySummary.bytes",
		"HistorySummary.files", "HistorySummary.forced", "HistorySummary.head",
		"HistorySummary.limit", "HistorySummary.restored_from", "HistorySummary.usage",
		"InboxNotice.snoozed", "NoticeThread.resolved_version", "VersionSummary.forced",
		"VersionSummary.restored_from",
	},
	"internal/artifactupload/lint": {
		"Finding.line",
	},
	"internal/assistant/wire": {
		"Change.new", "Column.detail", "EntityCard.items_more", "EntityCard.items_total",
		"EntityItem.depth", "ErrorCard.retryable", "ErrorEvent.retry_after_seconds", "Fact.detail",
		"Fact.mono", "ProposalAnswer.after_seq", "ProposalCard.step_up", "StepEvent.duration_ms",
		"TableCard.more",
	},
	"internal/atlas": {
		"Claimed.denied", "Edge.suggested", "Entry.not_used_recently", "Entry.review",
		"FactView.suggested", "Value.private",
	},
	"internal/atlas/collect": {
		"Scope.unread",
	},
	"internal/backup": {
		"ObjectArtifact.replayed", "ObjectArtifact.reused",
	},
	"internal/capability": {
		"Box.memory_bytes", "Box.observed", "HostCommands.more", "Integration.scope_more",
		"Snapshot.compact", "Snapshot.mcp_more", "Snapshot.policies_more", "Snapshot.setup_more",
		"ToolServer.more",
	},
	"internal/chatlet": {
		"ActivityCounts.capped", "ActivityItem.collective", "ActivityItem.read_through",
		"ActivityItem.unread", "Channel.auto_join", "Channel.chart_epoch",
		"ChannelMember.follow_all", "ChannelSummary.follow_all", "ChannelSummary.unread_capped",
		"Directory.truncated", "Directory.unreadable", "DirectoryRoom.member", "Draft.collective",
		"Message.asks", "Message.collective", "MessagePreview.deleted", "Reach.truncated",
		"ReactionCount.mine", "RoomName.archived", "RoomNames.truncated", "SavedEntry.gone",
		"Thread.earlier", "Thread.unreadable", "Transcript.unreadable",
	},
	"internal/config": {
		"Problem.line", "Range.min_exclusive", "Range.operator_max", "Range.operator_min",
		"Range.or_zero",
	},
	"internal/coord": {
		"ChatReadState.revision", "InsightRecord.answer", "InsightRecord.attempts",
		"MaintenanceOperation.observed_max_bytes",
	},
	"internal/doclet": {
		"Container.chart_epoch", "PageChange.quiet", "Summary.children", "Summary.folder",
		"Summary.onboarding", "Summary.skill",
	},
	"internal/engine": {
		"FeedDeadLetter.delivered", "FeedDeadLetter.generation", "FeedDeadLetter.position",
	},
	"internal/events/types": {
		"AgentCapabilityTold.baseline", "AgentPhaseCompleted.calls_elided",
		"AgentPhaseCompleted.delivered_refs_elided", "AgentPhaseCompleted.empty_answer_rounds",
		"AgentPhaseCompleted.host_round", "AgentPhaseCompleted.max_rounds",
		"AgentPhaseCompleted.pushed_refs_elided", "AgentPhaseCompleted.round_ceiling",
		"AgentPhaseCompleted.rounds_elided", "AgentPhaseCompleted.tool_catalogue_elided",
		"AgentPhaseCompleted.tools_available_elided", "AgentTurnCompleted.cache_read_tokens",
		"AgentTurnCompleted.cache_write_tokens", "AgentTurnCompleted.calls_elided",
		"AgentTurnCompleted.stopped", "AgentTurnCompleted.subagent_input_tokens",
		"AgentTurnCompleted.subagent_output_tokens", "AgentTurnCompleted.suspended",
		"AgentTurnProgress.calls_elided", "AgentTurnProgress.max_rounds",
		"AgentTurnProgress.round_ceiling", "AgentTurnProgress.rounds_elided",
		"AgentTurnProgress.running_calls_elided", "AgentTurnSteered.iteration",
		"AgentTurnSteered.retold", "AgentTurnSteered.round", "AssistantAuditStep.bytes",
		"AssistantAuditStep.status", "AssistantTurnCompleted.failed", "BackupRequested.failed",
		"BackupRequested.streams", "CoalescedMessage.addressed", "CodeCheckFinished.duration_ms",
		"CodeCheckFinished.required", "CodeImported.mirrored", "CodeMirrorPushed.deleted_count",
		"CodeMirrorPushed.updated_count", "CodeMirrorPushed.waiting_count",
		"CodeTargetBroken.number", "DeadChange.draft", "ElidedCalls.failed",
		"ExecutorAnswerGiven.delegated_rule", "ExecutorAnswered.answer_bytes",
		"ExecutorAnswered.answer_max_bytes", "ExecutorAnswered.delegated_rule",
		"ExecutorCollected.activity_transcript_elided_bytes",
		"ExecutorCollected.activity_transcript_elided_lines",
		"ExecutorCollected.context_window_tokens", "ExecutorCollected.delivered_refs_elided",
		"ExecutorCollected.harness_session_continued", "ExecutorCollected.harness_turns",
		"ExecutorCollected.memory_bytes", "ExecutorCollected.memory_kills",
		"ExecutorCollected.pushed_refs_elided", "ExecutorCollected.session_taken_bytes",
		"ExternalNotification.addressed", "ExternalNotification.subject_is_label",
		"IAMLoginFailures.overflow", "IAMPasswordReset.first", "IAMPasswordResetIssued.first",
		"InboundDelivery.replayed", "KnowledgeReadPage.rank", "MCPServerActed.draft",
		"MCPServerActed.dry_run", "MCPServerActed.failed", "ModelActed.dry_run", "ModelActed.failed",
		"OperatorActed.failed", "PromptSection.headed", "ProviderFallback.from_subscription",
		"RawWebhook.first_party", "RawWebhook.replayed", "SeatPaused.stop_running",
		"SeatProviderParked.subscription", "SubagentBatched.round", "TaskAssigned.timeout_seconds",
		"ToolSkillPageChanged.walk", "ToolTally.failed", "ToolTally.refused", "ToolTally.total_ms",
		"ToolTally.unavailable", "TurnCompleted.acted", "TurnCompleted.suspended",
		"TurnTally.failed", "TurnTally.reviews", "TurnTally.sent_back", "TurnTally.unaccepted",
		"TurnTokens.aux_input", "TurnTokens.aux_output", "TurnTokens.cache_read",
		"TurnTokens.cache_write", "TurnTokens.input", "TurnTokens.output", "WorkPickup.progressed",
		"WorkPickup.ready_capped",
	},
	"internal/executorkeep": {
		"Ending.delivered", "Ending.delivery_unknown", "Ending.went_on",
	},
	"internal/github": {
		"Manifest.setup_on_update",
	},
	"internal/gitlet": {
		"BlameCommit.author_bytes", "BlameCommit.author_email_bytes", "BlameCommit.committer_bytes",
		"BlameCommit.committer_email_bytes", "BlameCommit.subject_bytes", "BlameLine.text_bytes",
		"BlobAnswer.binary", "BlobAnswer.continues", "BlobAnswer.end_line", "BlobAnswer.start_byte",
		"BlobAnswer.start_line", "BranchCount.more", "CardEjection.checks_more",
		"ChangeCard.reviewers_omitted", "ChangeDue.facts_due", "ChangeDue.merge_due",
		"ChangeDue.refresh_due", "ChangeRow.body_bytes", "ChangesText.building",
		"ChangesText.truncated", "CheckCheckout.none", "CheckCounts.cancelled",
		"CheckCounts.errored", "CheckCounts.failed", "CheckCounts.other", "CheckCounts.passed",
		"CheckCounts.queued", "CheckCounts.running", "CheckCounts.skipped", "CheckDetail.log_lines",
		"CheckDetail.tail_cut", "CheckDetail.tail_first_line", "CheckFailure.line",
		"CheckQueued.required", "CheckQueued.revision", "CheckQueued.skipped",
		"CheckRollup.failing_more", "CheckRollup.more", "CheckRow.duration_ms",
		"CheckRow.failures_omitted", "CheckRow.required", "CheckRow.revision",
		"CheckRow.timeout_seconds", "CheckTests.failed", "CheckTests.skipped", "CheckWork.dispatch",
		"ChecksFileAnswer.size", "ClearedBy.persons", "CodeFile.size", "CodeFilesAnswer.more",
		"Command.forced", "CommandCounts.created", "CommandCounts.deleted", "CommandCounts.forced",
		"CommandCounts.moved", "CommandCounts.refused", "CommandCounts.unchanged",
		"CommentRow.resolved", "CommitChanges.more", "CommitMark.checks_more", "CommitMark.more",
		"CommitTags.more", "Coverage.complete", "DiffFile.binary", "DiffFile.similarity",
		"EjectedRow.close_task", "EjectedRow.delete_source", "EjectedTotal.more", "Excerpt.binary",
		"Excerpt.more", "ExcerptLine.new", "ExcerptLine.old", "ExcerptLine.text_bytes",
		"ExpungeState.dropped_bytes", "ExpungeState.dropped_objects", "FailingCheck.required",
		"FilePatch.continues", "FilePatch.rest_omitted", "FindingPlace.end", "FindingPlace.gone",
		"FindingPlace.outdated", "FindingPlace.start", "GitAnswer.memo", "InboxNotice.fallback",
		"InboxNotice.snoozed", "IntroducedRow.count", "LandingGate.approvals_more",
		"LandingGate.gone", "LandingRow.broken", "LogAnswer.complete", "LogCommit.author_bytes",
		"LogCommit.author_email_bytes", "LogCommit.committer_bytes",
		"LogCommit.committer_email_bytes", "LogCommit.parent_count", "LogCommit.subject_bytes",
		"MergeGate.agents_edit_checks", "MergeGate.owners_approve", "MergeabilityRow.chart_epoch",
		"MergeabilityRow.conflicts_omitted", "MergeabilityRow.ready", "MergeabilityRow.waits_person",
		"NoticeChange.body_cut", "ParticipantList.omitted", "ParticipantRow.verdict_revision",
		"Precondition.blockers_omitted", "PushRow.atomic", "PushRow.changes_more",
		"PushRow.introduced_count", "PushRow.offered", "PushRow.pack_bytes", "PushRule.allow_delete",
		"PushRule.allow_force", "Pushers.omitted", "Pushers.omitted_pushes", "QueueCounts.more",
		"QueueRow.close_task", "QueueRow.delete_source", "Reanchored.end", "Reanchored.gone",
		"Reanchored.outdated", "Reanchored.start", "RefListing.may_delete", "RefMove.forced",
		"RefProtection.allow_delete", "RefProtection.allow_force", "RepoCard.push_mirrored",
		"RepoRef.drop_epoch", "RepoRef.mirrored", "ReviewerFace.asked",
		"ReviewerFace.owned_paths_omitted", "ReviewerFace.verdict_revision",
		"ReviewerRow.owned_paths_omitted", "RevisionClasses.complete",
		"RevisionFactsAnswer.head_in_target", "RevisionFactsAnswer.touches_all",
		"RevisionFactsRow.patch_id_omitted", "RevisionFactsRow.touches_all", "RevisionRow.forced",
		"RoutedRecipient.addressed", "RoutedRecipient.fallback", "RoutedRecipient.fallback_rank",
		"SearchAnswer.skipped_files", "SearchAnswer.truncated", "SearchHit.file_matches",
		"TagRow.date_unix", "Tip.counts_out_of_time", "TipDue.more", "TipLanding.broken",
		"TipLanding.last", "TipTag.subject_bytes", "TipTag.tagger_bytes",
		"TipTag.tagger_email_bytes", "TouchedPaths.touches_all", "TreeCommit.out_of_time",
		"TreeCommitsAnswer.out_of_time", "TreeEntry.size",
	},
	"internal/gitlet/cache": {
		"VerifyReport.rebuilt", "VerifyReport.repaired_in_place",
	},
	"internal/inbox": {
		"Notice.fallback", "Notice.snoozed", "Position.generation",
	},
	"internal/integration": {
		"Finding.seat",
	},
	"internal/objstore/migrate": {
		"UnreadableFile.damaged",
	},
	"internal/presence": {
		"Focus.version", "Member.queued", "Roster.truncated",
	},
	"internal/queue/jetstream": {
		"MetaPeer.leader", "MetaPeer.offline", "MetaPeer.self",
	},
	"internal/research": {
		"Page.next", "Results.dropped",
	},
	"internal/sandbox": {
		"BridgeCall.abandoned", "BridgeCall.appended", "BridgeCall.approved",
		"BridgeCall.args_bytes", "BridgeCall.args_unparsed", "BridgeCall.duration_ms",
		"BridgeCall.failed", "BridgeCall.held", "BridgeCall.late", "BridgeCall.output_bytes",
		"BridgeCall.seq", "ElidedCall.failed", "Output.front", "Output.held", "Output.reset",
	},
	"internal/sandbox/executor": {
		"TranscriptEntry.cut", "TranscriptEntry.failed",
	},
	"internal/search": {
		"TaskLink.key_collision",
	},
	"internal/seataccess": {
		"BoxLine.tool", "GrantEntry.max_hours", "GrantEntry.write", "LaneAnswer.administers",
		"LaneView.lookup_more", "Level.administers", "Level.custom", "Notice.replace_key",
		"Observed.credential_gone", "Options.multiple", "Options.releasable", "Options.rotatable",
		"Options.scope_live", "Options.scope_open", "Options.scope_required", "Options.scoped",
		"PermissionSpec.administers_on_write", "PermissionSpec.write_only", "PlanAnswer.step_up",
		"SeatAnswer.departed", "SeatPlan.step_up", "SeatReview.at_least", "Step.drift",
	},
	"internal/setup": {
		"Requirement.checkable", "Requirement.connect", "Requirement.hidden", "Requirement.mintable",
		"Requirement.shared",
	},
	"internal/statelog": {
		"DomainReport.evictions_unreadable", "DomainReport.max_bytes", "DomainReport.reserve_bytes",
		"MaintenanceReport.acks_unread", "MaintenanceReport.admissions_unread",
		"NodeDomainReport.deferred", "NodeDomainReport.log_diverged", "SnapshotReport.bytes",
	},
	"internal/store": {
		"EventRecord.calls", "EventRecord.tokens", "Spend.cache_read_tokens",
		"Spend.cache_write_tokens", "Spend.calls", "Spend.input_tokens", "Spend.iteration",
		"Spend.output_tokens", "Spend.total_tokens", "Turn.stopped",
		"UnsettledRow.cache_read_tokens", "UnsettledRow.cache_write_tokens",
		"UnsettledRow.duration_ms", "UnsettledRow.failed", "UnsettledRow.input_tokens",
		"UnsettledRow.output_tokens", "UnsettledRow.total_tokens",
	},
	"internal/tokens": {
		"AgentRow.person", "GroupRow.people", "GroupRow.seats",
	},
	"internal/worklet": {
		"ActivityRecord.late", "ActivityRecord.subject_key_collision", "Around.total_capped",
		"AskAnswer.resolved", "AskView.resolved", "Blocker.dead", "Blocker.key_collision",
		"Blocker.one_sided", "Blocker.one_sided_final", "ChecklistItem.done", "ChecklistItem.order",
		"ChecklistRow.task_key_collision", "ClaimTotal.capped", "Comment.removed",
		"Comment.resolved", "DeadWait.draft", "DetailLink.derived", "DetailLink.key_collision",
		"DetailLink.one_sided", "DetailLink.one_sided_final", "DueStanding.overdue",
		"FeedRow.key_collision", "FeedRow.reassignment_budget", "FieldConfig.multi",
		"FieldConfig.precision", "FieldConfig.time", "FieldDef.archived",
		"FieldDef.hide_from_agents", "FieldDef.pinned", "FieldDef.required",
		"FieldDef.required_in_subtasks", "FieldValue.foreign", "FieldValue.hidden",
		"FieldValue.undeclared", "Group.subgroups_dropped", "HistoryEntry.quiet",
		"HoldingAgents.capped", "HoldingRow.blocks_capped", "InboxNotice.fallback",
		"InboxNotice.snoozed", "InboxNotice.subject_key_collision", "Option.archived",
		"Option.order", "ProjectListing.truncated", "ProjectRow.archived", "Ranked.key_collision",
		"Relation.one_sided_final", "RoutingAnswer.truncated", "RoutingRecipient.fallback",
		"RoutingRecipient.fallback_rank", "Spend.cache_read", "Spend.cache_write", "Spend.input",
		"Spend.output", "Spend.rounds", "Spend.sent_back", "Spend.tokens", "Spend.turns",
		"Spend.wall_ms", "Spend.workers", "Tag.archived", "Task.archived", "Task.body_version",
		"Task.change_seq", "Task.depth", "Task.due_all_day", "Task.estimate_minutes",
		"Task.merge_reparent", "Task.merging", "Task.moving", "Task.points", "Task.reassignments",
		"Task.scoped_through", "TaskDetail.blocked",
		"TaskDetail.key_collision", "TaskRef.key_collision", "TaskRow.archived", "TaskRow.blocked",
		"TaskRow.depth", "TaskRow.estimate_min", "TaskRow.key_collision", "TaskRow.overdue",
		"TaskRow.points", "TaskTurn.sent_back", "TaskTurn.workers", "TaskTurns.key_collision",
		"TaskType.archived", "TaskType.builtin", "TurnPlace.key_collision", "ViewRow.count_capped",
		"ViewRow.default", "ViewRow.pinned", "ViewRow.protected", "WorkloadAnswer.applied_through",
		"WorkloadAnswer.log_seq", "WorkloadAnswer.truncated",
	},
}

// declaredOptionalInventory is, per interface of the dashboard's wire module,
// every flag and number member it declares optional that the engine always
// sends: 66 when the gate arrived.
var declaredOptionalInventory = map[string][]string{
	"AgentSpendRow": {
		"cache_read_tokens", "cache_write_tokens",
	},
	"Bucket": {
		"cache_read_tokens", "cache_write_tokens",
	},
	"ConfigWarning": {
		"introduced",
	},
	"EventBar": {
		"failed",
	},
	"EventSeries": {
		"failed",
	},
	"Frame": {
		"retry_after",
	},
	"LiveCall": {
		"body_withheld", "cache_read_tokens", "cache_write_tokens", "rounds_elided",
	},
	"ModelRow": {
		"cache_read_tokens", "cache_write_tokens",
	},
	"Overlay": {
		"current_iteration", "live_call_seq",
	},
	"PageActivityAnswer": {
		"applied_through", "complete", "log_seq",
	},
	"PageDetail": {
		"children_total", "folder",
	},
	"PhaseRow": {
		"cache_read_tokens", "cache_write_tokens",
	},
	"ProviderSpendRow": {
		"cache_read_tokens", "cache_write_tokens",
	},
	"ResetView": {
		"first",
	},
	"RetentionTerm": {
		"seq",
	},
	"SeriesBand": {
		"cache_read_tokens", "cache_write_tokens",
	},
	"SeriesPart": {
		"cache_read_tokens", "cache_write_tokens",
	},
	"SeriesPoint": {
		"cache_read_tokens", "cache_write_tokens",
	},
	"SetupSeatState": {
		"present",
	},
	"SetupToolState": {
		"can_provision",
	},
	"TurnRow": {
		"cache_read_tokens", "cache_write_tokens", "parked",
	},
	"TurnSpendRow": {
		"cache_read_tokens", "cache_write_tokens",
	},
	"WorkActivityAnswer": {
		"applied_through", "log_seq",
	},
	"WorkAskRow": {
		"open",
	},
	"WorkCatalogueAnswer": {
		"applied_through", "log_seq",
	},
	"WorkInboxAnswer": {
		"applied_through", "complete", "log_seq",
	},
	"WorkItemDetail": {
		"applied_through", "log_seq", "reassignment_budget",
	},
	"WorkItemTurnsAnswer": {
		"applied_through", "log_seq",
	},
	"WorkMyWork": {
		"applied_through", "log_seq",
	},
	"WorkPersonState": {
		"applied_through", "log_seq",
	},
	"WorkProjectDetail": {
		"applied_through", "log_seq",
	},
	"WorkProjectsAnswer": {
		"applied_through", "log_seq",
	},
	"WorkRoutingAnswer": {
		"applied_through", "complete", "log_seq",
	},
	"WorkerRow": {
		"cache_read_tokens", "cache_write_tokens",
	},
}
