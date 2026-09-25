package types

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
)

// catalogue is every payload this package registers, one prototype each. It is
// written by hand and checked against the registry below: a type added to a
// domain file and forgotten in an init, or registered and never listed here,
// fails the build rather than becoming an event nobody can decode.
func catalogue() []events.Payload {
	return []events.Payload{
		// org.go
		OrgStarted{}, OrgStopped{}, AgentSpawned{}, AgentTerminated{},
		// config.go
		ConfigRevisionActivated{}, ConfigRevisionApplied{},
		// task.go
		TaskAssigned{},
		// schedule.go
		ScheduledTaskFired{},
		// notification.go
		ExternalNotification{}, TurnTriggerSkipped{},
		NotificationsCoalesced{}, NotificationSkipped{},
		// a2a.go
		A2ARequest{}, A2AMessage{},
		A2AChannelOpened{}, A2AMessageSent{}, A2AChannelClosed{},
		// budget.go
		BudgetExhausted{}, BudgetMeters{},
		// provider.go
		LLMUnavailable{}, ProviderFallback{},
		// agent.go
		AgentTurnStarted{}, AgentTurnCompleted{}, TurnCompleted{}, AgentPhaseStarted{},
		AgentPhaseCompleted{}, AgentTurnProgress{}, SubagentBatched{},
		// learning.go
		EpisodeWritten{}, PersistDeciderCompleted{}, SkillUsed{},
		PrefetchSummary{},
		CounterpartyProfileUpdated{}, SkillSynthesized{}, SkillRefined{},
		SkillPromoted{}, SkillStaled{}, SkillArchived{}, SkillRevived{},
		SkillTelemetryWriteFailed{}, CompactionRequested{},
		CompactionCompleted{}, ReflectionCompleted{},
		// sandbox.go
		SandboxRunStarted{}, SandboxRunCompleted{}, SandboxRunFailed{},
		SandboxClarificationRequested{},
		// turn.go
		ToolSkillGuardBlocked{}, PromptSize{}, TurnGuardBreach{},
		// toolskill.go
		ToolSkillPageChanged{},
		// knowledge.go
		KnowledgeRead{},
		// webhook.go
		RawWebhook{},
		// operator.go
		OperatorActed{}, BackupRequested{},
	}
}

// wireTypes is the wire contract: the exact set of type strings this build
// publishes and decodes. Sorted, because it is compared against
// events.RegisteredTypes.
var wireTypes = []string{
	"a2a_channel_closed",
	"a2a_channel_opened",
	"a2a_message",
	"a2a_message_sent",
	"a2a_request",
	"agent_phase_completed",
	"agent_phase_started",
	"agent_spawned",
	"agent_terminated",
	"agent_turn_completed",
	"agent_turn_progress",
	"agent_turn_started",
	"backup_requested",
	"budget_exhausted",
	"budget_meters",
	"compaction_completed",
	"compaction_requested",
	"config_revision_activated",
	"config_revision_applied",
	"counterparty_profile_updated",
	"episode_written",
	"external_notification",
	"knowledge_read",
	"llm_unavailable",
	"notification_skipped",
	"notifications_coalesced",
	"operator_acted",
	"org_started",
	"org_stopped",
	"persist_decider_completed",
	"phase.tool_skill_blocked",
	"prefetch_summary",
	"prompt.size",
	"provider_fallback",
	"raw_webhook",
	"reflection_completed",
	"sandbox_clarification_requested",
	"sandbox_run_completed",
	"sandbox_run_failed",
	"sandbox_run_started",
	"scheduled_task_fired",
	"skill_archived",
	"skill_promoted",
	"skill_refined",
	"skill_revived",
	"skill_staled",
	"skill_synthesized",
	"skill_telemetry_write_failed",
	"skill_used",
	"subagent_batched",
	"task_assigned",
	"tool_skill_page_changed",
	"turn.guard_breach",
	"turn_completed",
	"turn_trigger_skipped",
}

func TestRegistryMatchesWireTypes(t *testing.T) {
	t.Parallel()
	got := events.RegisteredTypes()
	want := slices.Clone(wireTypes)
	slices.Sort(want)
	if slices.Equal(got, want) {
		return
	}
	t.Errorf("registry and the declared wire contract disagree\n"+
		"declared but not registered (a forgotten Register call): %v\n"+
		"registered but not declared (an undeclared type on the wire): %v",
		missing(want, got), missing(got, want))
}

func TestCatalogueCoversRegistry(t *testing.T) {
	t.Parallel()
	seen := map[string]int{}
	for _, payload := range catalogue() {
		seen[payload.EventType()]++
	}
	for typeName, count := range seen {
		if count > 1 {
			t.Errorf("catalogue lists %s %d times", typeName, count)
		}
	}
	for _, typeName := range events.RegisteredTypes() {
		if _, ok := seen[typeName]; !ok {
			t.Errorf("registered type %s is not in the test catalogue", typeName)
		}
	}
	if len(seen) != len(events.RegisteredTypes()) {
		t.Errorf("catalogue has %d types, registry has %d",
			len(seen), len(events.RegisteredTypes()))
	}
}

// envelopeOwnedKeys mirrors the key set event.go reserves for the envelope. A
// payload field colliding with one of these is dropped on the way out with no
// error anywhere, so the collision has to be caught here.
var envelopeOwnedKeys = map[string]struct{}{
	"id": {}, "type": {}, "timestamp": {}, "source": {}, "payload": {},
	"trace_id": {}, "span_id": {}, "parent_span_id": {},
	"delegation_depth": {}, "parent_turn_id": {}, "delegation_chain": {},
	"node": {},
}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func TestPayloadRoundTripsThroughEnvelope(t *testing.T) {
	t.Parallel()
	for _, prototype := range catalogue() {
		t.Run(prototype.EventType(), func(t *testing.T) {
			t.Parallel()
			payload := filled(prototype)

			original := &events.Event{
				ID:              uuid.New(),
				Type:            payload.EventType(),
				Timestamp:       sampleTime,
				Source:          "engine",
				TraceID:         "abcd1234abcd1234abcd1234abcd1234",
				SpanID:          "1234abcd1234abcd",
				DelegationDepth: 3,
				ParentTurnID:    "turn-1",
				DelegationChain: []string{"cto", "eng"},
				Data:            payload,
			}
			raw, err := json.Marshal(original)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var restored events.Event
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if restored.Data == nil {
				t.Fatalf("typed body did not decode; the type is not registered")
			}
			if !reflect.DeepEqual(restored.Data, payload) {
				t.Errorf("payload changed across the wire\n got: %+v\nwant: %+v",
					restored.Data, payload)
			}
			// A known type leaves nothing for Extra: every key it wrote is one
			// it can read back.
			if len(restored.Extra) != 0 {
				t.Errorf("known type left %v in Extra", restored.Extra)
			}
		})
	}
}

func TestPayloadTagsAreDistinctAndSnakeCase(t *testing.T) {
	t.Parallel()
	for _, prototype := range catalogue() {
		t.Run(prototype.EventType(), func(t *testing.T) {
			t.Parallel()
			payload := filled(prototype)
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			keys := map[string]json.RawMessage{}
			if err := json.Unmarshal(raw, &keys); err != nil {
				t.Fatalf("remap: %v", err)
			}
			// Every field is populated, so omitempty drops nothing: a key count
			// below the field count means two fields share a tag, which
			// encoding/json resolves by silently emitting neither.
			fields := reflect.TypeOf(prototype).NumField()
			if len(keys) != fields {
				t.Errorf("%d JSON keys for %d fields — a duplicate or missing tag",
					len(keys), fields)
			}
			for key := range keys {
				if _, reserved := envelopeOwnedKeys[key]; reserved {
					t.Errorf("field %q collides with an envelope key and would be dropped", key)
				}
				if !snakeCase.MatchString(key) {
					t.Errorf("field %q is not snake_case, which every wire field here is", key)
				}
			}
		})
	}
}

// wireTags is the OTHER HALF OF THE WIRE CONTRACT: the exact JSON keys each
// registered type publishes. `wireTypes` above pins the type strings, and
// nothing pinned the keys — so a renamed tag was as green as a comment
// change, which is exactly how `prompt.size` lost `system_chars`/`user_chars`
// to a tidier spelling for the length of one review cycle.
//
// A KEY IS A PEER CONTRACT, AND A RENAME IS A SILENT DELETE. ADR-0006 is
// additive-only and binds before any release, because a rolling upgrade runs
// in both directions at once: a renamed tag is a dropped field on whichever
// half has not upgraded, and every row already written keeps the old key
// forever. The failure has no symptom a reviewer can see — the build is
// green, the tests pass, and a reader of the dashboard gets a confidently
// wrong `0` where the payload used to answer.
//
// GOLDEN, NOT DERIVED. Computing the expected keys from the struct is a test
// that asserts the code equals itself; the whole value here is that the
// literal strings live in a file a rename has to edit, in a diff a reviewer
// reads. An ADDITIVE change — a genuinely new field — edits one line here and
// that friction is the feature. A rename edits one too, and then has to argue
// for it.
//
// Sorted per type, because the comparison is against sorted marshalled keys.
var wireTags = map[string][]string{
	"org_started":               {"org_name"},
	"org_stopped":               {"org_name"},
	"agent_spawned":             {"agent_id", "role"},
	"agent_terminated":          {"agent_id", "reason", "role"},
	"config_revision_activated": {"created_by", "revision_id", "revision_summary"},
	"operator_acted": {"actor_seat", "failed", "operator_id", "outcome", "position",
		"refusal", "request_id", "tool", "transport"},
	"backup_requested": {"actor_seat", "dir", "failed", "operator_id",
		"outcome", "streams"},
	"config_revision_applied":         {"applied_subsystems", "error", "revision_id", "status"},
	"task_assigned":                   {"agent_id", "description", "role", "schedule", "task_id", "timeout_seconds"},
	"scheduled_task_fired":            {"schedule_name", "scheduled_at", "scope_id", "scope_type", "target_handle"},
	"external_notification":           {"addressed", "agent_id", "body", "context_requires_recon", "messages", "metadata", "notification_source", "owes", "recipient_email", "salient_body", "sender", "source_event_type", "subject"},
	"turn_trigger_skipped":            {"agent_handle", "agent_id", "reason", "trigger_id", "trigger_type"},
	"notifications_coalesced":         {"agent_handle", "count", "first_at", "last_at", "notification_source", "partition_key"},
	"notification_skipped":            {"handle", "notification_source", "reason"},
	"a2a_request":                     {"channel_id", "content", "requester", "sender_role", "work_item", "work_item_basis"},
	"a2a_message":                     {"channel_id", "content", "question", "sender", "sender_role"},
	"a2a_channel_opened":              {"channel_id", "participants", "requester", "target", "turn_id", "work_key"},
	"a2a_message_sent":                {"channel_id", "content", "message_id", "recipient", "sender", "sender_role", "turn_id", "work_key"},
	"a2a_channel_closed":              {"channel_id", "closed_by", "duration_ms", "message_count", "participants", "turn_id", "work_key"},
	"budget_exhausted":                {"agent_id", "budget_type", "max_tokens", "period", "resets_at", "role", "turn_id", "used_tokens", "window", "work_key"},
	"budget_meters":                   {"meter_id", "org", "seats", "seq", "timezone"},
	"llm_unavailable":                 {"agent_id", "attempt_count", "last_error", "last_error_kind", "provider_chain", "role", "turn_id", "work_key"},
	"provider_fallback":               {"agent_id", "error_kind", "from_provider_key", "iteration", "phase", "role", "to_provider_key", "turn_id", "work_key"},
	"agent_turn_started":              {"agent_handle", "agent_id", "conversation_key", "resumed", "role", "started_at", "trigger", "turn_id", "work_item", "work_item_basis", "work_key"},
	"agent_turn_completed":            {"a2a_context", "agent_id", "cache_read_tokens", "cache_write_tokens", "conversation_key", "decision", "error", "error_kind", "execute_model", "failed", "input_tokens", "iterations", "model", "output_tokens", "plan_model", "prompt", "prompt_messages", "response", "review_model", "role", "subagent_count", "subagent_input_tokens", "subagent_output_tokens", "subagent_tokens", "suspended", "tool_executions", "total_tokens", "trigger", "turn_id", "work_item", "work_item_basis", "work_key"},
	"turn_completed":                  {"agent_handle", "agent_id", "all_tool_names", "conversation_key", "duration_ms", "ended_at", "interactions", "iterations", "outcome", "plan_decision", "plan_summary", "plan_tool_sequence", "review_outcome", "role", "skills_used", "started_at", "suspended", "task_summary", "tool_sequence", "turn_id", "work_item", "work_item_basis", "work_key"},
	"agent_phase_started":             {"agent_id", "iteration", "phase", "role", "trigger", "turn_id", "work_item", "work_key"},
	"agent_phase_completed":           {"activity_transcript", "agent_id", "backend", "cache_read_tokens", "cache_write_tokens", "coding_agent", "conversation_key", "cost_usd", "decision", "delivered_refs", "duration_ms", "empty_answer_rounds", "error", "error_kind", "exhausted_rounds", "failed", "host_iteration", "host_phase", "host_round", "input_tokens", "iteration", "launch_id", "max_rounds", "model", "notes", "output_tokens", "phase", "provider_key", "rescue_fired", "response", "role", "round_ceiling", "round_narration", "rounds", "rounds_used", "sandbox_id", "started_at", "system_prompt", "task_id", "tool_catalogue", "tool_executions", "tools_available", "total_tokens", "trigger", "turn_id", "user_prompt", "work_item", "work_key", "worker"},
	"agent_turn_progress":             {"a2a_context", "agent_id", "cache_read_tokens", "cache_write_tokens", "input_tokens", "iteration", "max_rounds", "model", "output_tokens", "partial_round", "phase", "prompt", "prompt_messages", "response", "role", "round_ceiling", "round_narration", "round_num", "round_started_at", "rounds", "running_call", "tool_executions", "total_tokens", "trigger", "turn_id", "work_item", "work_key"},
	"subagent_batched":                {"failures", "graph", "parent_handle", "round", "started_at", "statuses", "successes", "task_count", "total_tokens", "turn_id", "work_key"},
	"episode_written":                 {"agent_handle", "agent_id", "duration_ms", "review_outcome", "role", "tool_count", "turn_id", "work_key"},
	"persist_decider_completed":       {"agent_handle", "agent_id", "classification", "doc_id", "persisted", "review_outcome", "role", "scope", "ttl_until", "turn_id", "work_key"},
	"skill_used":                      {"agent_handle", "agent_id", "file_loaded", "role", "skill_id", "skill_name", "source_container", "source_kind", "source_page_id", "turn_id", "work_key"},
	"knowledge_read":                  {"agent_handle", "agent_id", "backend", "pages", "phase", "query", "role", "turn_id", "via", "work_key"},
	"prefetch_summary":                {"agent_handle", "agent_id", "counterparty_bytes", "counterparty_hit", "duration_ms", "episode_recall_bytes", "episode_recall_hit", "onboarding_hint_bytes", "onboarding_hint_hit", "personal_memory_bytes", "personal_memory_hit", "relevant_knowledge_bytes", "relevant_knowledge_hit", "relevant_knowledge_selection_count", "role", "started_at", "synthesized_skills_bytes", "synthesized_skills_hit", "thread_context_bytes", "thread_context_hit", "thread_context_posts", "thread_context_read", "thread_context_stopped_short", "trigger_requires_recon", "turn_id", "work_key"},
	"counterparty_profile_updated":    {"observer_handle", "role", "subject_external_id", "subject_handle", "subject_name", "subject_platform", "traits_patched", "turn_id", "work_key"},
	"skill_synthesized":               {"agent_handle", "agent_id", "cluster_size", "role", "skill_id", "skill_name", "tool_count", "trigger", "turn_id", "work_key"},
	"skill_refined":                   {"agent_handle", "agent_id", "refinement_kind", "role", "skill_id", "skill_name", "skill_version", "turn_id", "work_key"},
	"skill_promoted":                  {"container_key", "distinct_agents", "page_id", "page_title", "role", "sibling_count", "skill_name", "unit_id"},
	"skill_staled":                    {"agent_handle", "last_used_at", "skill_id", "skill_name", "transitioned_at"},
	"skill_archived":                  {"agent_handle", "last_used_at", "skill_id", "skill_name", "transitioned_at"},
	"skill_revived":                   {"agent_handle", "prior_state", "skill_id", "skill_name", "transitioned_at", "turn_id", "work_key"},
	"skill_telemetry_write_failed":    {"agent_handle", "error", "kind", "skill_id", "skill_name", "turn_id", "work_key"},
	"compaction_requested":            {"agent_handle", "raw_count", "threshold"},
	"compaction_completed":            {"agent_handle", "clusters_compacted", "compacted_evicted", "consolidated_dropped", "non_terminal_dropped", "raw_replaced_by_compaction", "skipped_reason"},
	"reflection_completed":            {"agent_handle", "agent_id", "review_outcome", "role", "turn_id", "work_key", "workers_run"},
	"sandbox_run_started":             {"agent_handle", "agent_id", "coding_agent", "conversation_key", "role", "sandbox_id", "task", "turn_id", "work_item", "work_key"},
	"sandbox_run_completed":           {"agent_handle", "agent_id", "coding_agent", "launch_id", "role", "sandbox_id", "turn_id", "work_key"},
	"sandbox_run_failed":              {"agent_handle", "agent_id", "coding_agent", "detail", "reason", "role", "sandbox_id", "turn_id", "work_key"},
	"sandbox_clarification_requested": {"agent_handle", "agent_id", "audience", "conversation_key", "question", "role", "sandbox_id", "turn_id", "work_item", "work_key"},
	"phase.tool_skill_blocked":        {"agent_id", "iteration", "phase", "role", "skill_keys", "tool_name", "turn_id", "work_key"},
	"prompt.size":                     {"agent_id", "approximate_tokens", "iteration", "message_chars", "phase", "role", "system_chars", "tool_chars", "tool_count", "turn_id", "user_chars", "work_key"},
	"turn.guard_breach":               {"agent_id", "detail", "kind", "role", "turn_id", "work_key"},
	"tool_skill_page_changed":         {"backend", "container", "page_id"},
	"raw_webhook":                     {"body", "body_raw", "forge_atlassian_id", "handle", "headers"},
}

// TestPayloadTagsMatchTheWireContract pins every payload's keys, both ways: a
// type whose keys moved fails, and so does a type missing from the golden map
// or listed in it after being retired.
//
// This is ADR-0006's field-level clause, which had no gate until now — both
// of the records it named cover unknown-TYPE preservation, so the rule that
// existing fields are never removed or repurposed was held by prose alone.
func TestPayloadTagsMatchTheWireContract(t *testing.T) {
	t.Parallel()
	declared := make(map[string]bool, len(wireTags))
	for name := range wireTags {
		declared[name] = true
	}
	for _, prototype := range catalogue() {
		t.Run(prototype.EventType(), func(t *testing.T) {
			t.Parallel()
			want, ok := wireTags[prototype.EventType()]
			if !ok {
				t.Fatalf("no declared wire keys: a new type adds its keys to " +
					"wireTags, which is what puts them in front of a reviewer")
			}
			// `filled` populates every field, so omitempty drops nothing and
			// the key set is the whole contract rather than whatever a zero
			// value happened to emit.
			raw, err := json.Marshal(filled(prototype))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			keys := map[string]json.RawMessage{}
			if err := json.Unmarshal(raw, &keys); err != nil {
				t.Fatalf("remap: %v", err)
			}
			got := make([]string, 0, len(keys))
			for key := range keys {
				got = append(got, key)
			}
			slices.Sort(got)
			sorted := slices.Clone(want)
			slices.Sort(sorted)
			if slices.Equal(got, sorted) {
				return
			}
			t.Errorf("wire keys moved — a renamed key is a DROPPED FIELD on "+
				"whichever half of a rolling upgrade has not upgraded, and a "+
				"silent one on every row already stored (ADR-0006)\n"+
				"declared but not published (a rename or a removal): %v\n"+
				"published but not declared (an additive field, or a rename): %v",
				missing(sorted, got), missing(got, sorted))
		})
		delete(declared, prototype.EventType())
	}
	// THE OTHER DIRECTION: an entry whose type left the catalogue is stale,
	// and a stale entry is how a golden list stops describing the build.
	for name := range declared {
		t.Errorf("wireTags declares %q, which the catalogue no longer carries", name)
	}
}

// TestUnknownTypeSurvivesIntact is the rolling-upgrade invariant: an event this
// build has never heard of must decode into the envelope with every unknown
// field preserved and re-encode identically. Dropping or erroring on one would
// make every upgrade an outage, since the newer half of a fleet publishes types
// the older half does not know.
func TestUnknownTypeSurvivesIntact(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"id":"6f1c3d2e-0000-4000-8000-000000000001",
		"type":"quantum_turn_entangled",
		"timestamp":"2031-04-01T09:08:07.654321Z",
		"source":"engine",
		"payload":{"free":"form"},
		"trace_id":"aaaa1111aaaa1111aaaa1111aaaa1111",
		"span_id":"bbbb2222bbbb2222",
		"parent_span_id":"cccc3333cccc3333",
		"delegation_depth":2,
		"parent_turn_id":"turn-9",
		"delegation_chain":["cto","eng"],
		"entangled_with":["turn-7","turn-8"],
		"coherence":0.75,
		"observer":{"handle":"dana","collapsed":false}
	}`)

	var event events.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("an unknown type must never fail to decode: %v", err)
	}
	if event.Data != nil {
		t.Fatalf("unknown type decoded a typed body: %#v", event.Data)
	}
	if event.Type != "quantum_turn_entangled" || event.DelegationDepth != 2 {
		t.Fatalf("envelope did not decode: %+v", event)
	}
	for _, key := range []string{"entangled_with", "coherence", "observer"} {
		if _, ok := event.Extra[key]; !ok {
			t.Errorf("unknown field %q was dropped", key)
		}
	}

	out, err := json.Marshal(&event)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var before, after map[string]any
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatalf("decode source: %v", err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("round trip through an older build was lossy\n got: %v\nwant: %v", after, before)
	}
}

// retiredTypes are wire types an earlier build registered and this one does
// not. Each one had no publisher and was removed from the registry, and every
// one of them can still be on the wire: a node of that build publishes it
// during a rolling upgrade, and a row an older build wrote is read back from
// the store for as long as retention keeps it.
var retiredTypes = []string{
	"agent_reassigned", "role_updated",
	"task_created", "task_started", "task_completed", "task_failed", "task_delegated",
	"message_sent", "a2a_message_delivered",
	"document_created", "document_updated",
}

// A RETIRED TYPE IS AN UNKNOWN TYPE, AND STAYS A LOSSLESS ONE.
//
// Removing a type from the registry is only safe because the envelope treats a
// type this build does not know as data rather than as an error, which is the
// invariant [TestUnknownTypeSurvivesIntact] pins with an invented name. This
// pins it for the names that actually left, and pins the other half of the
// retirement: none of them may come back registered or placed under a
// category. A name reused for a different payload would try to decode every
// row the old build wrote into a shape it never had.
func TestARetiredTypeStillSurvivesAPeerThatStillPublishesIt(t *testing.T) {
	t.Parallel()
	for _, retired := range retiredTypes {
		t.Run(retired, func(t *testing.T) {
			t.Parallel()
			if _, ok := events.PayloadFor(retired); ok {
				t.Fatalf("%q is registered again: rows an older build wrote under "+
					"it would decode into a payload they were never written as", retired)
			}
			if category, ok := events.Category(retired); ok {
				t.Errorf("%q is placed under %q, so a filter offers a value nothing "+
					"in this build publishes", retired, category)
			}
			raw := []byte(`{
				"id":"6f1c3d2e-0000-4000-8000-0000000000aa",
				"type":"` + retired + `",
				"timestamp":"2026-08-01T09:08:07.654321Z",
				"source":"engine",
				"trace_id":"aaaa1111aaaa1111aaaa1111aaaa1111",
				"span_id":"bbbb2222bbbb2222","parent_span_id":"",
				"delegation_depth":0,"parent_turn_id":"",
				"task_id":"T-1","agent_id":"a-1","role":"Engineer",
				"detail":{"reason":"written by the build that published it"}
			}`)
			var event events.Event
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatalf("a retired type failed to decode, so a mixed fleet drops it: %v", err)
			}
			if event.Data != nil || event.Type != retired {
				t.Fatalf("decoded %q with a typed body %#v, want the envelope alone", event.Type, event.Data)
			}
			out, err := json.Marshal(&event)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			var before, after map[string]any
			if err := json.Unmarshal(raw, &before); err != nil {
				t.Fatalf("decode source: %v", err)
			}
			if err := json.Unmarshal(out, &after); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("re-publishing %q was lossy\n got: %v\nwant: %v", retired, after, before)
			}
		})
	}
}

func TestFailed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		eventType     string
		payloadFailed bool
		tagFailed     bool
		want          bool
	}{
		{"failure by type alone", "sandbox_run_failed", false, false, true},
		{"llm chain exhausted", "llm_unavailable", false, false, true},
		{"budget refused a charge", "budget_exhausted", false, false, true},
		{"guard breach", "turn.guard_breach", false, false, true},
		{"live payload flag", "agent_phase_completed", true, false, true},
		{"history tag, payload unread", "agent_phase_completed", false, true, true},
		{"both", "agent_turn_completed", true, true, true},
		{"ordinary event", "agent_turn_completed", false, false, false},
		{"unknown type from a newer build", "quantum_turn_entangled", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Failed(tc.eventType, tc.payloadFailed, tc.tagFailed); got != tc.want {
				t.Errorf("Failed(%q, %v, %v) = %v, want %v",
					tc.eventType, tc.payloadFailed, tc.tagFailed, got, tc.want)
			}
		})
	}
}

func TestDescribeTriggerWithNoTrigger(t *testing.T) {
	t.Parallel()
	trigger := DescribeTrigger(nil)
	if !trigger.IsZero() {
		t.Errorf("a nil event must describe no trigger, got %+v", trigger)
	}
	if got := trigger.Map(); len(got) != 0 {
		t.Errorf("the zero trigger renders as {}, got %v", got)
	}
	raw, err := json.Marshal(trigger)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "{}" {
		t.Errorf("the zero trigger marshals to {}, got %s", raw)
	}
}

func TestDescribeTriggerCompactDescriptor(t *testing.T) {
	t.Parallel()
	// AN EVENT WITH NO ROLE OF ITS OWN, so the actor falls back to the
	// envelope's source — which is the link this case is about.
	event := events.New(OrgStarted{OrgName: "Acme"}, events.TraceContext{})
	event.Source = "PM"

	trigger := DescribeTrigger(event)
	if trigger.ID != event.ID.String() {
		t.Errorf("id = %q, want %q", trigger.ID, event.ID.String())
	}
	if trigger.Type != "org_started" {
		t.Errorf("type = %q", trigger.Type)
	}
	if trigger.Summary != event.Summary() {
		t.Errorf("summary = %q, want the event's own %q", trigger.Summary, event.Summary())
	}
	if trigger.Actor != "PM" {
		t.Errorf("actor = %q, want PM", trigger.Actor)
	}
	if !trigger.Timestamp.Equal(event.Timestamp) {
		t.Errorf("timestamp = %v, want %v", trigger.Timestamp, event.Timestamp)
	}

	// Only an integration trigger carries these; their ABSENCE is what tells a
	// dashboard to use its plain type label instead of a branded badge.
	descriptor := trigger.Map()
	for _, key := range []string{"integration", "sender", "source_event_type"} {
		if _, present := descriptor[key]; present {
			t.Errorf("non-integration trigger carries %q", key)
		}
	}
	for _, key := range []string{"id", "type", "summary", "actor", "timestamp"} {
		if _, present := descriptor[key]; !present {
			t.Errorf("descriptor is missing %q", key)
		}
	}
}

func TestDescribeTriggerNamesTheIntegration(t *testing.T) {
	t.Parallel()
	event := events.New(ExternalNotification{
		NotificationSource: "slack",
		SourceEventType:    "message",
		Sender:             "alice",
		Subject:            "Need a hand",
	}, events.TraceContext{})

	descriptor := DescribeTrigger(event).Map()
	want := map[string]any{
		"type": "external_notification", "integration": "slack",
		"sender": "alice", "source_event_type": "message",
	}
	for key, value := range want {
		if descriptor[key] != value {
			t.Errorf("descriptor[%q] = %v, want %v", key, descriptor[key], value)
		}
	}
}

// An integration that named itself but no human must not invent one: the sender
// and source-event-type keys stay absent rather than arriving blank.
func TestDescribeTriggerOmitsUnnamedSender(t *testing.T) {
	t.Parallel()
	event := events.New(NotificationsCoalesced{
		AgentHandle:        "eng",
		NotificationSource: "jira",
		Count:              3,
	}, events.TraceContext{})

	descriptor := DescribeTrigger(event).Map()
	if descriptor["integration"] != "jira" {
		t.Errorf("integration = %v, want jira", descriptor["integration"])
	}
	for _, key := range []string{"sender", "source_event_type"} {
		if _, present := descriptor[key]; present {
			t.Errorf("descriptor carries an empty %q", key)
		}
	}
}

// A descriptor built from a trigger slots onto the phase events a dashboard
// reads as the turn's source, and survives the wire path to get there.
func TestTriggerRidesOnPhaseEvents(t *testing.T) {
	t.Parallel()
	source := events.New(TaskAssigned{Description: "Build API"}, events.TraceContext{})
	source.Source = "PM"
	trigger := DescribeTrigger(source)

	phase := events.New(AgentPhaseCompleted{Phase: PhaseExecute, Trigger: trigger},
		events.TraceContext{})
	raw, err := json.Marshal(phase)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored events.Event
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	completed, ok := events.DataAs[*AgentPhaseCompleted](&restored)
	if !ok {
		t.Fatalf("phase event lost its typed body")
	}
	if !reflect.DeepEqual(completed.Trigger, trigger) {
		t.Errorf("trigger changed across the wire\n got: %+v\nwant: %+v",
			completed.Trigger, trigger)
	}
}

func TestPhaseEventsDefaultToNoTrigger(t *testing.T) {
	t.Parallel()
	carriers := []events.Payload{
		AgentPhaseStarted{}, AgentPhaseCompleted{}, AgentTurnProgress{},
		AgentTurnCompleted{},
	}
	for _, carrier := range carriers {
		t.Run(carrier.EventType(), func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(carrier)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("remap: %v", err)
			}
			if string(fields["trigger"]) != "{}" {
				t.Errorf("trigger = %s, want {} — the no-source contract",
					fields["trigger"])
			}
		})
	}
}

// A descriptor whose timestamp cannot be parsed must not take the rest of the
// event down with it: the envelope drops the WHOLE typed body when a payload
// fails to decode.
func TestTriggerToleratesAnUnparseableTimestamp(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"id":"x","type":"org_started","timestamp":"last tuesday"}`)
	var trigger Trigger
	if err := json.Unmarshal(raw, &trigger); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if trigger.ID != "x" || trigger.Type != "org_started" {
		t.Errorf("the other fields were lost: %+v", trigger)
	}
	if !trigger.Timestamp.IsZero() {
		t.Errorf("timestamp = %v, want the zero time", trigger.Timestamp)
	}
}

// --- fixtures --------------------------------------------------------------

// sampleTime has nanosecond precision and no monotonic reading, so it survives
// RFC3339Nano exactly and compares by value.
var sampleTime = time.Date(2026, 8, 22, 12, 34, 56, 789012345, time.UTC)

// filled returns a pointer to a copy of the prototype with every field set to a
// distinct non-zero value, which is what makes a round trip prove that every
// field has a working tag rather than that the zero values matched.
func filled(prototype events.Payload) events.Payload {
	value := reflect.New(reflect.TypeOf(prototype))
	f := &filler{}
	f.fill(value.Elem(), reflect.TypeOf(prototype).Name())
	payload, ok := value.Interface().(events.Payload)
	if !ok {
		panic("filled: *T does not implement Payload for " + prototype.EventType())
	}
	return payload
}

type filler struct{ n int }

func (f *filler) next() int { f.n++; return f.n }

func (f *filler) fill(v reflect.Value, name string) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(name + "-" + strconv.Itoa(f.next()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(f.next()))
	case reflect.Uint8:
		// Only reached as the element of a []byte, which JSON carries as
		// base64. Filling it like any other field is what makes the round
		// trip prove the tag rather than prove that two empty slices match.
		v.SetUint(uint64(f.next()))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(f.next()) + 0.5)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		f.fill(p.Elem(), name)
		v.Set(p)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(sampleTime.Add(time.Duration(f.next()) * time.Second)))
			return
		}
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if field.IsExported() {
				f.fill(v.Field(i), field.Name)
			}
		}
	case reflect.Slice:
		slice := reflect.MakeSlice(v.Type(), 2, 2)
		for i := range 2 {
			f.fill(slice.Index(i), name)
		}
		v.Set(slice)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		for range 2 {
			key := reflect.ValueOf(name + "-key-" + strconv.Itoa(f.next()))
			element := reflect.New(v.Type().Elem()).Elem()
			f.fill(element, name)
			m.SetMapIndex(key.Convert(v.Type().Key()), element)
		}
		v.Set(m)
	case reflect.Interface:
		// Strings only: a number would come back from JSON as a float64 and
		// compare unequal for reasons that have nothing to do with the tag
		// under test.
		v.Set(reflect.ValueOf(name + "-any-" + strconv.Itoa(f.next())))
	default:
		panic("filler: unhandled kind " + v.Kind().String() + " for " + name)
	}
}

// missing returns the members of want that are absent from got, for a readable
// diff on the registry comparison.
func missing(want, got []string) []string {
	var out []string
	for _, w := range want {
		if !slices.Contains(got, w) {
			out = append(out, w)
		}
	}
	return out
}

// A summary's leading rune is upper-cased, not its leading BYTE. s[:1] on a
// multi-byte lead hands ToUpper an invalid UTF-8 fragment, which it
// substitutes — so an "Éditeur" phrase would reach the event feed and the
// store's summary column as "\uFFFD\x89diteur". Every phrase is ASCII-led
// today, which makes this latent rather than live, and one variable-led
// phrase is all it takes.
func TestASummaryLeadIsUpperCasedByRuneNotByte(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"éditeur plan (done)", "Éditeur plan (done)"},
		{"über alles", "Über alles"},
		{"日本語", "日本語"},
		{"completed a turn", "Completed a turn"},
		{"", ""},
	} {
		got := lead("", tc.in)
		if got != tc.want {
			t.Errorf("lead(\"\", %q) = %q, want %q", tc.in, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("lead(\"\", %q) produced invalid UTF-8", tc.in)
		}
	}
}

// A PROMOTED TAG MEANS ONE THING, AND THE CATALOGUE IS WHERE THAT IS HELD.
//
// internal/store promotes the flat wire field `conversation_key` out of every
// event into one column of the event log, and the value a query gets back has
// to mean the same thing whichever row it matched. It did not: every event a
// turn publishes puts the conversation IDENTITY there, while the coalescing
// record put the inbox PARTITION there — two values that differ exactly where
// it matters, since a direct message's identity is the bare channel and its
// partition can be a thread inside it. So a filter on that tag silently mixed
// the thread a seat is talking on with the batch a wake arrived in.
//
// The rule this holds is the naming one, because that is the half a reviewer
// can check: a field spelled `conversation_key` on the wire is the
// conversation and is called ConversationKey, and a field holding a partition
// is called PartitionKey and spells itself `partition_key`. A new event
// stamping its batch into the shared tag now has to rename a field to do it.
func TestNoPayloadPutsAPartitionInTheConversationField(t *testing.T) {
	t.Parallel()
	for _, payload := range catalogue() {
		typ := reflect.TypeOf(payload)
		for i := range typ.NumField() {
			field := typ.Field(i)
			wire, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			switch {
			case wire == "conversation_key" && field.Name != "ConversationKey":
				t.Errorf("%s.%s is `conversation_key` on the wire: that tag is "+
					"the conversation identity on every event, so a field "+
					"holding anything else must not be promoted through it",
					typ.Name(), field.Name)
			case field.Name == "PartitionKey" && wire != "partition_key":
				t.Errorf("%s.PartitionKey is `%s` on the wire, want partition_key: "+
					"the partition has its own tag so the conversation's keeps "+
					"one meaning", typ.Name(), wire)
			}
		}
	}
}
