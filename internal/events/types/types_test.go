package types

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

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
		AgentReassigned{}, RoleUpdated{},
		// config.go
		ConfigRevisionActivated{}, ConfigRevisionApplied{},
		// task.go
		TaskCreated{}, TaskAssigned{}, TaskStarted{}, TaskCompleted{},
		TaskFailed{}, TaskDelegated{},
		// schedule.go
		ScheduledTaskFired{},
		// notification.go
		MessageSent{}, ExternalNotification{}, TurnTriggerSkipped{},
		NotificationsCoalesced{}, NotificationSkipped{},
		// a2a.go
		A2AChannelOpened{}, A2AMessageSent{}, A2AMessageDelivered{},
		A2AChannelClosed{},
		// knowledge.go
		DocumentCreated{}, DocumentUpdated{},
		// budget.go
		BudgetExhausted{}, BudgetReported{},
		// provider.go
		LLMUnavailable{}, ProviderFallback{},
		// agent.go
		AgentTurnCompleted{}, TurnCompleted{}, AgentPhaseStarted{},
		AgentPhaseCompleted{}, AgentTurnProgress{}, SubagentBatched{},
		// learning.go
		EpisodeWritten{}, PersistDeciderCompleted{}, SkillUsed{},
		PlanPrefetchSummary{}, RelevantKnowledgeRefetched{},
		CounterpartyProfileUpdated{}, SkillSynthesized{}, SkillRefined{},
		SkillPromoted{}, SkillStaled{}, SkillArchived{}, SkillRevived{},
		SkillTelemetryWriteFailed{}, CompactionRequested{},
		CompactionCompleted{}, ReflectionCompleted{},
		// sandbox.go
		SandboxRunStarted{}, SandboxRunCompleted{},
		SandboxClarificationRequested{},
		// turn.go
		ExecuteMissingTool{}, PhaseToolActivated{}, ToolSkillGuardBlocked{},
		PromptSize{}, TurnGuardBreach{},
		// webhook.go
		RawWebhook{},
	}
}

// wireTypes is the wire contract: the exact set of type strings this build
// publishes and decodes. Sorted, because it is compared against
// events.RegisteredTypes.
var wireTypes = []string{
	"a2a_channel_closed",
	"a2a_channel_opened",
	"a2a_message_delivered",
	"a2a_message_sent",
	"agent_phase_completed",
	"agent_phase_started",
	"agent_reassigned",
	"agent_spawned",
	"agent_terminated",
	"agent_turn_completed",
	"agent_turn_progress",
	"budget_exhausted",
	"budget_reported",
	"compaction_completed",
	"compaction_requested",
	"config_revision_activated",
	"config_revision_applied",
	"counterparty_profile_updated",
	"document_created",
	"document_updated",
	"episode_written",
	"execute.missing_tool",
	"external_notification",
	"llm_unavailable",
	"message_sent",
	"notification_skipped",
	"notifications_coalesced",
	"org_started",
	"org_stopped",
	"persist_decider_completed",
	"phase.tool_activated",
	"phase.tool_skill_blocked",
	"plan_prefetch_summary",
	"prompt.size",
	"provider_fallback",
	"raw_webhook",
	"reflection_completed",
	"relevant_knowledge_refetched",
	"role_updated",
	"sandbox_clarification_requested",
	"sandbox_run_completed",
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
	"task_completed",
	"task_created",
	"task_delegated",
	"task_failed",
	"task_started",
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
					t.Errorf("field %q is not snake_case; Python spells it that way", key)
				}
			}
		})
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

func TestFailed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		eventType     string
		payloadFailed bool
		tagFailed     bool
		want          bool
	}{
		{"failure by type alone", "task_failed", false, false, true},
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
	event := events.New(TaskCreated{Title: "Build API", TargetRole: "Engineer"},
		events.TraceContext{})
	event.Source = "PM"

	trigger := DescribeTrigger(event)
	if trigger.ID != event.ID.String() {
		t.Errorf("id = %q, want %q", trigger.ID, event.ID.String())
	}
	if trigger.Type != "task_created" {
		t.Errorf("type = %q", trigger.Type)
	}
	if trigger.Summary != event.Summary() {
		t.Errorf("summary = %q, want the event's own %q", trigger.Summary, event.Summary())
	}
	// The actor falls back to the envelope's source, which is where a task
	// creator's identity lives.
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
	source := events.New(TaskCreated{Title: "Build API"}, events.TraceContext{})
	source.Source = "PM"
	trigger := DescribeTrigger(source)

	phase := events.New(AgentPhaseCompleted{Phase: PhasePlan, Trigger: trigger},
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
	raw := []byte(`{"id":"x","type":"task_created","timestamp":"last tuesday"}`)
	var trigger Trigger
	if err := json.Unmarshal(raw, &trigger); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if trigger.ID != "x" || trigger.Type != "task_created" {
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
