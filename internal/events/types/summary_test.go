package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// The summary and actor assertions are stated through the envelope rather than
// against the payload methods, because the envelope is what every consumer
// calls and where the fallbacks live.

func summaryOf(payload events.Payload, source string) string {
	event := events.NewFrom(payload, events.TraceContext{})
	event.Source = source
	return event.Summary()
}

func actorOf(payload events.Payload, source string) string {
	event := events.NewFrom(payload, events.TraceContext{})
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
		name:    "a stop mirrors the start",
		payload: OrgStopped{OrgName: "Acme"},
		want:    "Organization 'Acme' stopped",
	}, {
		name:    "an assignment names the seat and what it is for",
		payload: TaskAssigned{RoleName: "Dev", TaskID: "T-42"},
		want:    "Dev was assigned task T-42",
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
		name: "a prompt measurement names the tool array it was offered",
		payload: PromptSize{
			RoleName: "CTO", Phase: PhaseExecute, ApproximateTokens: 7400,
			SystemBytes: 21000, UserBytes: 800, ToolCount: 12, ToolBytes: 8200,
		},
		want: "CTO execute prompt ~7400 tokens (12 tool definitions, 8200 chars)",
	}, {
		name: "a phase that was offered no tools claims none",
		payload: PromptSize{
			RoleName: "CTO", Phase: PhaseExecute, ApproximateTokens: 5400,
			SystemBytes: 21000, UserBytes: 800,
		},
		want: "CTO execute prompt ~5400 tokens",
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
		payload: AgentPhaseCompleted{RoleName: "Dev", Phase: PhaseReview, Decision: "self_iterate", Model: "gpt-4o", TotalTokens: 12},
		want:    "Dev review → self_iterate (gpt-4o, 12 tokens)",
	}, {
		name:    "a guard breach names the invariant",
		payload: TurnGuardBreach{RoleName: "Dev", Kind: GuardStall, Detail: "unchanged artifact"},
		want:    "Dev guard stall: unchanged artifact",
	}, {
		name:    "a prefetch summary counts its blocks",
		payload: PrefetchSummary{RoleName: "Dev", CounterpartyHit: true, PersonalMemoryHit: true},
		want:    "Dev prefetch: 2/7 hits",
	}, {
		// THE DENOMINATOR IS THE NUMBER OF BLOCKS, and it was a literal
		// beside a hand-written list with nothing holding the two
		// together. A count that goes on saying six after a seventh block
		// lands reads to an operator as a turn that hit everything — or,
		// on a turn that hit all seven, as "7/6".
		name: "every block hit counts, and the denominator follows",
		payload: PrefetchSummary{RoleName: "Dev",
			CounterpartyHit: true, SynthesizedSkillsHit: true, EpisodeRecallHit: true,
			OnboardingHintHit: true, PersonalMemoryHit: true, RelevantKnowledgeHit: true,
			ThreadContextHit: true},
		want: "Dev prefetch: 7/7 hits",
	}, {
		name:    "a gated prefetch says it was gated",
		payload: PrefetchSummary{RoleName: "Dev", TriggerRequiresRecon: true},
		want:    "Dev prefetch: 0/7 hits (thin trigger — filters gated)",
	}, {
		// THE THREAD BLOCK IS NOT GATED: it is what makes a thin trigger
		// thick, so a turn whose filters were all skipped still counts it.
		name: "a gated turn still counts the thread it was handed",
		payload: PrefetchSummary{RoleName: "Dev",
			TriggerRequiresRecon: true, ThreadContextHit: true},
		want: "Dev prefetch: 1/7 hits (thin trigger — filters gated)",
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
		// RoundNum is 0-based, and -1 on the opening update every phase
		// publishes before its first provider call. Both were rendered wrong:
		// the sentinel reached the line as "round -1", and the first real round
		// printed no round at all.
		name:    "the opening update reports no round yet",
		payload: AgentTurnProgress{RoleName: "Dev", Phase: PhaseExecute, RoundNum: -1},
		want:    "Dev working (execute)",
	}, {
		name:    "the first completed round is round 1",
		payload: AgentTurnProgress{RoleName: "Dev", Phase: PhaseExecute, RoundNum: 0},
		want:    "Dev working (execute, round 1)",
	}, {
		name:    "later rounds count on from there",
		payload: AgentTurnProgress{RoleName: "Dev", Phase: PhaseReview, RoundNum: 2},
		want:    "Dev working (review, round 3)",
	}, {
		name: "a sign-in names its method and its second factor",
		payload: IAMSessionStarted{Login: "jane.doe", Method: SignInPassword,
			SecondFactor: FactorTOTP},
		want: "jane.doe signed in (password + totp)",
	}, {
		name:    "a session ended by a deadline names nobody as its author",
		payload: IAMSessionEnded{Person: "p-1", Lineage: "0192aabbccddeeff", Reason: EndIdle},
		source:  "node-a",
		want:    "Session 0192aabb of p-1 ended (idle)",
	}, {
		name:    "a sign-out everywhere says every session",
		payload: IAMSessionEnded{Person: "p-1", Reason: EndLogoutAll, By: "jane.doe"},
		want:    "Every session of p-1 ended (logout_all)",
	}, {
		// THE COUNT IS BOTH ARMS: an attacker the throttle turned away
		// made those requests too, and a line counting only the verified
		// ones reads as a quieter minute than it was.
		name: "a minute of failures counts every arm and names the methods",
		payload: IAMLoginFailures{Client: "203.0.113.9", Attempts: 4, Throttled: 2,
			Methods: []FailureMethod{FailBearer, FailPassword}},
		want: "6 failed attempt(s) from 203.0.113.9 in one minute (bearer, password; 2 throttled)",
	}, {
		name:    "the overflow row says how many clients it folded",
		payload: IAMLoginFailures{Client: "*", Clients: 300, Attempts: 900},
		want:    "900 failed attempt(s) from 300 clients past the per-minute cap in one minute",
	}, {
		name:    "a token's first use names where it was used from",
		payload: IAMTokenFirstUse{Token: "ops", Route: "/secrets", Remote: "198.51.100.4"},
		want:    "Token ops used (first use this hour) on /secrets from 198.51.100.4",
	}, {
		name: "an unverifiable record names the key and what the node holds",
		payload: RecordUnverifiable{Domain: "iam", KeyID: "k3",
			Held: []string{"k2", "k1"}},
		want: `iam record signed under key "k3", which this node does not hold ` +
			`(it holds k1, k2); nothing under it is applied until it does`,
	}, {
		name:    "a frame that is not a frame names no key",
		payload: RecordTampered{Domain: "tracker", Position: "CREWLET_TRACKER_LOG@1:918"},
		want:    "tracker record at CREWLET_TRACKER_LOG@1:918 fails its signature under no key (not a signed frame); the applier stopped",
	}, {
		name:    "a spent recovery code says how many are left",
		payload: IAMRecoveryCodeUsed{Login: "jane.doe", Remaining: 0},
		want:    "jane.doe used a recovery code (0 left)",
	}, {
		name:    "a grant change lists both directions",
		payload: IAMGrantsChanged{Person: "p-1", Added: []string{"audit:read"}, Removed: []string{"config:write", "fleet:operate"}},
		want:    "Grants of p-1 changed: +audit:read; -config:write, -fleet:operate",
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
// AN OLDER PEER'S PROMPT MEASUREMENT NEVER CLAIMS A TOOL-LESS PHASE.
//
// tool_chars and tool_count are newer than the prompt.size event, so a row
// published by a node that predates them — an ordinary state for as long as a
// rolling upgrade takes — carries neither key and decodes to zero. Rendered
// unconditionally, that row tells the feed a phase was offered no tools at
// all, which is a statement about that build's prompt rather than about a
// measurement nobody took.
func TestAPromptSizeFromBeforeTheToolTermAssertsNoZero(t *testing.T) {
	t.Parallel()
	const older = `{"agent_id":"a-1","role":"CTO","turn_id":"t-1","iteration":1,
		"phase":"execute","approximate_tokens":5400,"system_chars":21000,
		"user_chars":800}`
	var row PromptSize
	if err := json.Unmarshal([]byte(older), &row); err != nil {
		t.Fatalf("a row from before the tool term does not decode: %v", err)
	}
	// The case's own premise: absent keys, not zeroed ones written out.
	if strings.Contains(older, "tool_") {
		t.Fatal("the fixture carries a tool key, so it is not an older peer's row")
	}
	if got := summaryOf(row, ""); got != "CTO execute prompt ~5400 tokens" {
		t.Fatalf("an older peer's row reads %q", got)
	}

	// AND THE MEASURED SHAPE STILL REPORTS IT — the clause is conditional
	// on the row, never dropped.
	row.ToolCount, row.ToolBytes = 12, 8200
	want := "CTO execute prompt ~5400 tokens (12 tool definitions, 8200 chars)"
	if got := summaryOf(row, ""); got != want {
		t.Fatalf("a measured row reads %q, want %q", got, want)
	}
}

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
		payload: TaskAssigned{RoleName: "Engineer", Agent: "a1"},
		source:  "task_engine",
		want:    "Engineer",
	}, {
		name:    "the publisher beats the agent id",
		payload: TaskAssigned{Agent: "a1"},
		source:  "task_engine",
		want:    "task_engine",
	}, {
		// The tail. A seat whose role and publisher are both unset is still
		// attributable, which is the whole reason the chain has a fourth link.
		name:    "the agent id answers when role and publisher are both empty",
		payload: TaskAssigned{Agent: "a1"},
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
		payload: ScheduledTaskFired{},
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
		payload: TaskAssigned{RoleName: "Dev", TaskID: "T-42"},
		source:  "task_engine",
		want:    "Dev was assigned task T-42",
	}, {
		// The event that has no role of its own: the publisher is what
		// the chain resolves to, and the summary names it.
		name:    "from the publisher",
		payload: OrgStarted{},
		source:  "Acme",
		want:    "Organization 'Acme' started",
	}, {
		name:    "from the agent id, when nothing else names anyone",
		payload: TaskAssigned{Agent: "a1", TaskID: "T-42"},
		want:    "a1 was assigned task T-42",
	}, {
		// The payload's OWN sender beats the publisher, because a
		// message the bus published on somebody's behalf is still that
		// person's message.
		name: "from the payload's own sender",
		payload: A2AMessageSent{
			ChannelID: "chan-1", Sender: "PM", Recipient: "Dev",
		},
		source: "a2a_service",
		want:   "PM sent A2A message on chan-1 → Dev",
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
