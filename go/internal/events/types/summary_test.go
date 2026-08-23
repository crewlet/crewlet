package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// The summary and actor assertions ported from tests/test_events/test_types.py.
// They are stated through the envelope rather than against the payload methods,
// because the envelope is what every consumer calls and where the fallbacks
// live.

func summaryOf(payload events.Payload, source string) string {
	event := events.New(payload, events.TraceContext{})
	event.Source = source
	return event.Summary()
}

func actorOf(payload events.Payload, source string) string {
	event := events.New(payload, events.TraceContext{})
	event.Source = source
	return event.Actor()
}

func TestSummaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload events.Payload
		source  string
		want    string
	}{{
		name:    "org names itself",
		payload: OrgStarted{OrgName: "Acme"},
		want:    "Organization 'Acme' started",
	}, {
		name:    "a spawned seat is named by its role",
		payload: AgentSpawned{RoleName: "Engineer"},
		source:  "pool",
		want:    "Engineer joined the organization",
	}, {
		name:    "a termination carries its reason",
		payload: AgentTerminated{RoleName: "Dev", Reason: "shutdown"},
		want:    "Dev was terminated: shutdown",
	}, {
		name:    "a completed task names the seat and the task",
		payload: TaskCompleted{RoleName: "Dev", TaskID: "T-42"},
		want:    "Dev completed task T-42",
	}, {
		name:    "a failed task appends the error",
		payload: TaskFailed{RoleName: "Dev", TaskID: "T-42", Error: "timeout"},
		want:    "Dev failed task T-42: timeout",
	}, {
		name:    "a message names its sender and channel",
		payload: MessageSent{Sender: "PM", Channel: "#backend"},
		want:    "PM sent a message to #backend",
	}, {
		name:    "a turn reports its model and token total",
		payload: AgentTurnCompleted{RoleName: "CTO", Model: "gpt-4o", TotalTokens: 500},
		want:    "CTO completed LLM turn (gpt-4o, 500 tokens)",
	}, {
		name: "an A2A turn is tagged with its channel",
		payload: AgentTurnCompleted{
			RoleName: "CTO", Model: "gpt-4o", TotalTokens: 500,
			A2AContext: map[string]any{"channel_id": "chan-1"},
		},
		want: "CTO completed LLM turn (gpt-4o, 500 tokens) [A2A:chan-1]",
	}, {
		name: "a failed turn says why it stopped",
		payload: AgentTurnCompleted{
			RoleName: "CTO", Failed: true, ErrorKind: "rate_limit",
		},
		want: "CTO turn failed (rate_limit)",
	}, {
		name:    "an exhausted chain counts what it tried",
		payload: LLMUnavailable{RoleName: "Engineer", ProviderChain: []string{"openai", "anthropic"}},
		want:    "LLM unavailable for Engineer (2 providers tried)",
	}, {
		// The dashboard builds a branded badge from notification_source, so the
		// summary leads with the human and never repeats the integration name.
		name:    "an inbound message leads with sender and subject",
		payload: ExternalNotification{NotificationSource: "slack", Sender: "alice", Subject: "Need a hand"},
		want:    "Message from alice: Need a hand",
	}, {
		name:    "a senderless notification still renders",
		payload: ExternalNotification{NotificationSource: "jira"},
		want:    "Notification",
	}, {
		name:    "a subject with no sender",
		payload: ExternalNotification{NotificationSource: "jira", Subject: "ACME-1"},
		want:    "Notification: ACME-1",
	}, {
		name: "a coalesced digest counts its constituents",
		payload: ExternalNotification{
			NotificationSource: "slack", Sender: "alice", Subject: "Thread",
			Messages: []CoalescedMessage{{Sender: "alice"}, {Sender: "bob"}},
		},
		want: "2 messages from alice: Thread",
	}, {
		name:    "a sandbox phase is badged with its coding agent",
		payload: AgentPhaseCompleted{RoleName: "Dev", Phase: PhaseExecute, Backend: BackendSandbox, CodingAgent: "claude-code"},
		want:    "Dev execute [sandbox:claude-code]",
	}, {
		name:    "a phase reports its structured decision",
		payload: AgentPhaseCompleted{RoleName: "Dev", Phase: PhasePlan, Decision: "direct", Model: "gpt-4o", TotalTokens: 12},
		want:    "Dev plan → direct (gpt-4o, 12 tokens)",
	}, {
		name:    "a guard breach names the invariant",
		payload: TurnGuardBreach{RoleName: "Dev", Kind: GuardStall, Detail: "unchanged artifact"},
		want:    "Dev guard stall: unchanged artifact",
	}, {
		name:    "a prefetch summary counts its blocks",
		payload: PlanPrefetchSummary{RoleName: "Dev", CounterpartyHit: true, PersonalMemoryHit: true},
		want:    "Dev plan prefetch: 2/6 hits",
	}, {
		name:    "a gated prefetch says it was gated",
		payload: PlanPrefetchSummary{RoleName: "Dev", TriggerRequiresRecon: true},
		want:    "Dev plan prefetch: 0/6 hits (thin trigger — filters gated)",
	}, {
		name:    "a no-op persist decision",
		payload: PersistDeciderCompleted{RoleName: "Dev", Classification: PersistNOOP},
		want:    "Dev reviewed turn — nothing to persist",
	}, {
		name:    "a config revision names its summary",
		payload: ConfigRevisionActivated{RevisionID: "r-2", RevisionSummary: "add engineer"},
		want:    "Config revision activated: add engineer",
	}, {
		name:    "an applied revision",
		payload: ConfigRevisionApplied{RevisionID: "r-2", Status: ApplyOK},
		want:    "Config revision r-2 applied",
	}, {
		name:    "a failed apply names the error",
		payload: ConfigRevisionApplied{RevisionID: "r-2", Status: ApplyError, Error: "bad dsn"},
		want:    "Config revision r-2 failed: bad dsn",
	}, {
		name:    "a compaction pass names the seat it ran for",
		payload: CompactionCompleted{SkippedReason: CompactionAlreadyRunning},
		source:  "eng",
		want:    "eng compaction skipped (already_running)",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := summaryOf(tc.payload, tc.source); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

// An unregistered type still summarises: the envelope title-cases its type
// string, which is what a node one version behind shows for an event it has
// never heard of.
func TestUnknownTypeSummarisesFromItsTypeString(t *testing.T) {
	t.Parallel()
	event := &events.Event{Type: "some_custom_event"}
	if got := event.Summary(); got != "Some Custom Event" {
		t.Errorf("summary = %q, want %q", got, "Some Custom Event")
	}
}

// No summary may say nothing, and none may open with a blank actor slot.
//
// A leading space is the specific failure mode of porting an actor-led
// f-string: " completed task T-42" is what interpolating an unresolved actor
// produces. An empty line is the other one — "" is the contract's
// defer-to-default signal, so an accidental one silently replaces a real
// summary with the title-cased type string.
//
// Asserted through the envelope, twice: with a publisher (the ordinary case)
// and with nothing at all, where the chain bottoms out at the engine itself.
// Both must produce a line, for a zero payload and a populated one alike.
func TestEverySummaryIsSpoken(t *testing.T) {
	t.Parallel()
	for _, prototype := range catalogue() {
		t.Run(prototype.EventType(), func(t *testing.T) {
			t.Parallel()
			for _, source := range []string{"engine", ""} {
				for _, payload := range []events.Payload{prototype, filled(prototype)} {
					got := summaryOf(payload, source)
					if strings.TrimSpace(got) == "" {
						t.Errorf("%T with source %q summarises to nothing", payload, source)
					}
					if strings.HasPrefix(got, " ") {
						t.Errorf("%T with source %q opens with a blank slot: %q",
							payload, source, got)
					}
				}
			}
		})
	}
}

// Every payload that KNOWS a link of the chain must contribute it.
//
// Derived from the wire tags rather than from a list, because the failure it
// guards is a new event carrying a role and not implementing Roler: the seat
// would be invisible to the chain, its turns would be attributed to whatever
// published them, and nothing would fail — the summary would just quietly name
// the wrong party.
func TestPayloadsContributeWhatTheyKnow(t *testing.T) {
	t.Parallel()
	for _, prototype := range catalogue() {
		t.Run(prototype.EventType(), func(t *testing.T) {
			t.Parallel()
			payload := filled(prototype)
			fields := map[string]json.RawMessage{}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("remap: %v", err)
			}

			if value, carries := fields["role"]; carries {
				roler, ok := payload.(events.Roler)
				if !ok {
					t.Fatalf("%T carries a role but does not implement Roler", payload)
				}
				if quoted(roler.Role()) != string(value) {
					t.Errorf("Role() = %s, want the role field %s",
						quoted(roler.Role()), value)
				}
			}
			if value, carries := fields["agent_id"]; carries {
				identified, ok := payload.(events.AgentIdentified)
				if !ok {
					t.Fatalf("%T carries an agent id but does not implement AgentIdentified",
						payload)
				}
				if quoted(identified.AgentID()) != string(value) {
					t.Errorf("AgentID() = %s, want the agent_id field %s",
						quoted(identified.AgentID()), value)
				}
			}
		})
	}
}

func quoted(s string) string {
	out, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// The actor chain, pinned link by link. It is one order resolved in one place,
// and these are the questions it answers.
func TestActorChain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload events.Payload
		source  string
		want    string
	}{{
		name: "an explicit override beats everything below it",
		// An inbound message's actor is the human who sent it. Without the
		// override the envelope reports a Slack message's actor as
		// "notification_service.slack" — a machine name in a column of human
		// ones, beside a badge already built from the same string.
		payload: ExternalNotification{
			NotificationSource: "slack", Sender: "Dana", Agent: "a1",
		},
		source: "notification_service.slack",
		want:   "Dana",
	}, {
		name:    "the payload's role beats the publisher",
		payload: TaskCompleted{RoleName: "Engineer", Agent: "a1"},
		source:  "task_engine",
		want:    "Engineer",
	}, {
		name:    "the publisher beats the agent id",
		payload: TaskCompleted{Agent: "a1"},
		source:  "task_engine",
		want:    "task_engine",
	}, {
		// The tail. A seat whose role and publisher are both unset is still
		// attributable, which is the whole reason the chain has a fourth link.
		name:    "the agent id answers when role and publisher are both empty",
		payload: TaskCompleted{Agent: "a1"},
		want:    "a1",
	}, {
		name:    "an override that names nobody defers to the chain",
		payload: ExternalNotification{NotificationSource: "slack", Agent: "a1"},
		source:  "notification_service.slack",
		want:    "notification_service.slack",
	}, {
		name:    "an override, no publisher, and only an agent id",
		payload: ExternalNotification{NotificationSource: "slack", Agent: "a1"},
		want:    "a1",
	}, {
		name:    "nothing at all is the engine itself",
		payload: TaskCreated{Title: "x"},
		want:    "system",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := actorOf(tc.payload, tc.source); got != tc.want {
				t.Errorf("actor = %q, want %q", got, tc.want)
			}
		})
	}
}

// The resolved actor is what a summary leads with, whichever link produced it.
func TestSummaryLeadsWithTheResolvedActor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload events.Payload
		source  string
		want    string
	}{{
		name:    "from the payload's role",
		payload: TaskCompleted{RoleName: "Dev", TaskID: "T-42"},
		source:  "task_engine",
		want:    "Dev completed task T-42",
	}, {
		// The event that has no role of its own: the creator is the publisher,
		// and the summary names it exactly as the Python line does.
		name:    "from the publisher",
		payload: TaskCreated{Title: "Build API", TargetRole: "Engineer"},
		source:  "PM",
		want:    "PM created task 'Build API' for Engineer",
	}, {
		name:    "from the agent id, when nothing else names anyone",
		payload: TaskCompleted{Agent: "a1", TaskID: "T-42"},
		want:    "a1 completed task T-42",
	}, {
		name:    "from the override",
		payload: MessageSent{Sender: "PM", Channel: "#backend"},
		source:  "notification_service.slack",
		want:    "PM sent a message to #backend",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := summaryOf(tc.payload, tc.source); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

// The <think> wire format between the engine and the dashboard's response
// renderer. Both the live per-round event and the per-phase record build their
// response through this one function: when they built it two ways, the live one
// dropped reasoning entirely and a thinking model's thoughts did not exist on
// the dashboard until its phase ended.
func TestFormatReasoningAndContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		reasoning string
		content   string
		want      string
	}{{
		name:      "reasoning precedes the visible content",
		reasoning: "weighing it",
		content:   "the answer",
		want:      "<think>weighing it</think>\n\nthe answer",
	}, {
		name:    "a non-thinking model keeps its plain shape",
		content: "the answer",
		want:    "the answer",
	}, {
		name:      "whitespace-only reasoning is no reasoning",
		reasoning: "  ",
		content:   "the answer",
		want:      "the answer",
	}, {
		// A thinking model that hit its output cap mid-thought still renders:
		// the thinking is then the only signal there is.
		name:      "reasoning alone still renders",
		reasoning: "still deciding",
		want:      "<think>still deciding</think>",
	}, {
		name: "nothing renders as nothing",
		want: "",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FormatReasoningAndContent(tc.reasoning, tc.content); got != tc.want {
				t.Errorf("FormatReasoningAndContent(%q, %q) = %q, want %q",
					tc.reasoning, tc.content, got, tc.want)
			}
		})
	}
}
