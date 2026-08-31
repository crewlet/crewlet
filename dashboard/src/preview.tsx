import { createRoot } from "react-dom/client";
import "./styles/tokens.css";
import "./styles/fonts.css";
import "./styles/base.css";
import "./styles/components.css";
import "./styles/shell.css";
import "./styles/screens.css";
import { TurnCard } from "./components/TurnCard.tsx";
import { groupTurns, narrations, toolCalls, type PhaseRecord } from "./lib/phases.ts";

const now = Date.now();
const rec: PhaseRecord = {
  key: "t|onboarding|1",
  turnId: "t",
  phase: "onboarding",
  iteration: 1,
  role: "agent-devrel",
  model: "moonshotai/kimi-k2.6",
  providerKey: "",
  live: true,
  failed: false,
  error: "",
  errorKind: "",
  systemPrompt: "",
  userPrompt: "",
  response: "",
  inputTokens: 12000,
  outputTokens: 4200,
  totalTokens: 26700,
  roundNum: 2,
  roundsUsed: 3,
  exhaustedRounds: false,
  rescueFired: false,
  decision: "",
  notes: "",
  conversationKey: "",
  toolsAvailable: [],
  toolCatalogue: [],
  worker: "",
  hostPhase: "",
  backend: "",
  codingAgent: "",
  trigger: {
    type: "chat",
    summary: "Message from founder: Mattermost message",
    integration: "mattermost",
  },
  at: new Date(now - 1000).toISOString(),
  startedAt: new Date(now - 92_000).toISOString(),
  eventId: "",
  tools: toolCalls([
    { name: "activate_tool", round: 1, arguments: "{}", result: "ok", success: true },
    {
      name: "list_mcp_server_tools",
      round: 2,
      arguments: '{"server":"confluence"}',
      error: "unknown server",
      success: false,
    },
  ]),
  narration: narrations([
    {
      round: 1,
      reasoning: "First I should see what tools exist on this role before doing anything else.",
      content: "Checking the tool surface.",
    },
    {
      round: 2,
      reasoning: "There is no wiki server. Let me re-read the instructions and try another name.",
      content: "Looking for a knowledge base server.",
    },
  ]),
  partial: {
    round: 3,
    reasoning:
      "Now I need to search for the onboarding pages. Let me look for a search tool for the knowledge base, since the instructions say it is an MCP tool",
    content: "",
  },
};

createRoot(document.getElementById("root")!).render(
  <div className="screen">
    <div className="screen-inner">
      <section className="col gap-1 live-region">
        <div className="t-label">
          Running now<span className="faint"> · 1 phase mid-flight</span>
        </div>
        <div className="col gap-2">
          {groupTurns([rec]).map((g) => (
            <TurnCard key={g.turnId} group={g} defaultOpen showRole />
          ))}
        </div>
      </section>
    </div>
  </div>,
);
