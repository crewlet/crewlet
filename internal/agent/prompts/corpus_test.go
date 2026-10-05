package prompts

import "github.com/crewlet/crewlet/internal/org"

// The REAL-SHAPED inputs the outline tests build every prompt from.
//
// Real-shaped is the point: what an outline exists to get right is the content
// a builder EMBEDS, and every one of these carries headings of its own the way
// production content does — a chat trigger's "## Triage — decide BEFORE
// replying" and "## Thread context", a pull request's "# Title", a model's
// "## Summary" quoted back as evidence, a ledger's "###" entries — beside
// multi-byte text (em dashes, accents, emoji), so a boundary measured in runes
// or one that drifts by a byte lands inside a character and is caught.

// chatTrigger is a chat notification body as internal/notify renders one: the
// message, then the engine's own sections INSIDE the body.
const chatTrigger = "Ana (@ana) in #eng — \"can you post the deploy summary? 🚀\"\n" +
	"\n## Triage — decide BEFORE replying\n" +
	"1. Is this addressed to you? If not, `no_action`.\n" +
	"2. Answer in the thread — never a new top-level post.\n" +
	"\n## Thread context\n" +
	"- Ana: we shipped v2.3 this morning\n" +
	"- Bo: café rollout finished — naïve cache warmed ✅\n"

// prTrigger is a code-host trigger whose description is a markdown document
// with a top-level title of its own.
const prTrigger = "Review requested on crewlet/api#42 by @bo\n\n" +
	"# Add rate limiting to the ingest path\n\n" +
	"## Why\nBursts of 10k events/s — the p99 jumped to 4 s.\n\n" +
	"## How\n- token bucket per tenant\n- 429 with Retry-After 🧯\n"

// producedReport is an executor's final prose that is itself a document.
const producedReport = "Posted the summary.\n\n## Summary\n- deploy 2.3 shipped ✅\n" +
	"- one flaky test — `TestIngest` — quarantined\n\n## Next\nwatch p99 → 200 ms"

// iterationLedger is the prior-work ledger's shape: one `###` entry per round.
const iterationLedger = "### Round 1 — sent back\n" +
	"- `slack_history` (read) → success\n" +
	"- `slack_post` → success: \"## Deploy 2.3 — done 🎉\"\n" +
	"### Round 2\n- `jira_create` → error: field \"résumé\" is required\n"

// richSeat is the lead of a mixed company with a sandbox: every identity
// section the executor renders has something to say.
func richSeat() Seat {
	o := &org.Organization{
		Name:     "Acme — Zürich",
		Mission:  "Build great things 🚀.",
		Vision:   "Be the best.",
		Policies: []string{"Respect teammates.", "No secrets in code — ever."},
		Units: []*org.Unit{{
			Name:    "Eng Team",
			Type:    org.UnitTypeTeam,
			Purpose: "Build the thing.",
			Lead:    "Engineering Lead",
			Goals:   []string{"Ship v1.0.", "Keep p99 < 200 ms."},
			Channel: "C_ENG",
			Roles: []*org.Role{
				{
					Name:                 "Engineering Lead",
					DeclaredHandle:       "lead",
					Goal:                 "Lead the engineering team.",
					Backstory:            "Ran platform at a fintech — twelve years.",
					Responsibilities:     []string{"Guide the team.", "Own incidents."},
					BehavioralGuidelines: []string{"Be concise.", "Cite the ticket."},
					Manages:              []string{"Engineer", "Sarah Chen"},
					Sandbox:              &org.RoleSandbox{Enabled: true},
					MCPEnv: org.MCPEnv{
						"atlassian": {"token": "x"},
						"github":    {"Authorization": "Bearer x"},
					},
				},
				{
					Name:             "Engineer",
					DeclaredHandle:   "eng",
					Goal:             "Ship quality code.",
					Backstory:        "Self-taught; loves Go.",
					Responsibilities: []string{"Write tests."},
				},
				{
					Name: "Sarah Chen",
					Kind: org.KindHuman,
					Contact: &org.HumanContact{
						SlackUserID: "U0HUMAN",
					},
					Availability: "CET business hours",
				},
			},
		}},
	}
	o.Normalize()
	return seatIn(o, "Engineering Lead")
}

// richSkills is a catalogue with a required and an advisory skill on every
// phase, so each phase's skill section renders.
func richSkills() *fakeCatalogue {
	all := []Phase{PhaseExecute, PhaseReview, PhaseSubagent}
	return &fakeCatalogue{skills: []fakeSkill{
		{key: "chat-conventions", summary: "How we post — threads, never top-level 🧵",
			required: true, mcpServer: "atlassian", phases: all},
		{key: "pr-etiquette", summary: "Link the ticket.\nSquash on merge.",
			tool: "read_file", mcpServer: "github", phases: all},
	}}
}

// richExecutorInput fills every block the executor renders, each carrying a
// heading of its own the way real retrieved content does.
func richExecutorInput() ExecutorInput {
	return ExecutorInput{
		ToolCatalogue: "- `read_file` — read a file\n- MCP servers: atlassian, github",
		AvailableTools: []string{
			"read_file", "mark_onboarded", "delegate", "comment_on_work_item",
		},
		Workers:             "- `researcher` — digs; returns `findings`",
		CounterpartyProfile: "Ana prefers bullet points — and emoji 👍",
		SynthesizedSkills:   "## Deploy summaries\nLead with the version.",
		EpisodeRecall:       "### 2026-09-30 — deploy 2.2\nposted in #eng",
		OnboardingHint:      "Read `## Engineering handbook` first.",
		PersonalMemory:      "# Diary\n- Ana likes threads",
		RelevantKnowledge:   "## Runbook: deploys\n1. tag\n2. ship 🚢",
		ThreadContext:       "- Ana: ## not a heading of ours\n- Bo: ok",
		Skills:              richSkills(),
	}
}

func richReviewInput() ReviewInput {
	return ReviewInput{
		Intent:            "Post the deploy summary — in the thread.",
		Outcome:           "delivered",
		Evidence:          "## Blocked\nno write tool",
		OpenQuestions:     "Which channel next week? 🤔",
		Produced:          producedReport,
		ToolLog:           "- `slack_post` → success",
		EarlierIterations: iterationLedger,
		Skills:            richSkills(),
	}
}

func richSubagentInput() SubagentInput {
	return SubagentInput{
		ParentSystemPrompt: "You research incidents.\n\n## Steps\n1. read the timeline — all of it\n",
		AvailableTools:     []string{"read_file"},
		ToolCatalogue:      "- `read_file` — read a file",
		Submits:            true,
		Skills:             richSkills(),
	}
}

func richUserMessage() UserMessage {
	return UserMessage{
		TaskDescription:     chatTrigger,
		PriorWork:           iterationLedger,
		ConversationHistory: "### Turn 1 — 2026-10-01\nYou said: \"## Done ✅\"",
	}
}
