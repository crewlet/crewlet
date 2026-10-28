/**
 * ONE VOCABULARY FOR A SEAT'S STATE, across every surface that names one.
 *
 * A seat a person paused read "Paused" on its own page and "Stopped" on the
 * roster, the chart and in the Inbox, and a seat whose last turn failed was
 * titled "stopped" in the Inbox while the roster filed it with the idle ones.
 * Each surface had its own words. They now all come from `lib/seats.ts`, and
 * this suite renders the real surfaces for one paused seat, one seat a fault
 * stopped and one whose last turn failed, and holds every word to the module's.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";

import { StateBadge } from "~/components/common.tsx";
import { OrgLegend } from "~/routes/agents/OrgChart.tsx";
import { attentionQueue, type AttentionInput } from "./attention.ts";
import { failureWords, seatWord, stateGroupWord, stateWord } from "./seats.ts";
import type { ProviderErrorKind } from "~/contract/failures.ts";
import { healthFrame } from "~/test/health.ts";
import type { AgentRow } from "~/protocol/index.ts";

afterEach(cleanup);

const PAUSED: AgentRow = {
  id: "qa",
  agent_id: "qa",
  role: "QA Engineer",
  handle: "qa",
  activity: "stopped",
  stopped_reason: "paused",
  paused: { by_kind: "human", by: "jane", at: "2026-01-01T11:00:00Z", stop_running: false },
};

const PROVIDER_STOP: AgentRow = {
  id: "sre",
  agent_id: "sre",
  role: "SRE",
  handle: "sre",
  activity: "stopped",
  stopped_reason: "provider",
};

const FAILED_TURN: AgentRow = {
  id: "dev",
  agent_id: "dev",
  role: "Developer",
  handle: "dev",
  activity: "idle",
  last_error: {
    kind: "server",
    detail: "llm chain exhausted after 1 providers (default)",
    phase: "execute",
    turn_id: "t1",
    at: "2026-01-01T11:30:00Z",
    event_id: "e1",
  },
};

function queue(agents: AgentRow[]) {
  const input: AttentionInput = {
    agents,
    runs: [],
    budget: null,
    engine: healthFrame({ configured: true }),
    connected: true,
    authRejected: false,
    now: Date.parse("2026-01-01T12:00:00Z"),
    nameOf: (key) => key,
  };
  return attentionQueue(input);
}

describe("a paused seat", () => {
  test("reads Paused on its badge and in the Inbox, from one word", () => {
    expect(seatWord(PAUSED)).toBe("Paused");
    render(<StateBadge agent={PAUSED} />);
    expect(screen.getByText(seatWord(PAUSED))).toBeTruthy();
    const [item] = queue([PAUSED]);
    expect(item?.title).toBe(`QA Engineer is ${seatWord(PAUSED).toLowerCase()}`);
  });

  test("is counted under a legend entry that names it", () => {
    render(<OrgLegend counts={{ working: 0, needs: 0, stopped: 1, idle: 0 }} />);
    const legend = screen.getByRole("group", { name: "Seats by what they are doing" });
    expect(legend.textContent).toContain(`${stateGroupWord("stopped")} 1`);
    expect(stateGroupWord("stopped").toLowerCase()).toContain(seatWord(PAUSED).toLowerCase());
  });
});

describe("a seat a fault stopped", () => {
  test("reads Stopped with its plain reason", () => {
    expect(seatWord(PROVIDER_STOP)).toBe(stateWord("stopped"));
    render(<StateBadge agent={PROVIDER_STOP} />);
    expect(screen.getByText("Stopped")).toBeTruthy();
    const [item] = queue([PROVIDER_STOP]);
    expect(item?.title).toBe("SRE is stopped");
    expect(item?.detail).toBe("The seat cannot take work: its models are at their usage limit.");
  });
});

describe("a seat whose last turn failed", () => {
  test("is never called stopped", () => {
    const [item] = queue([FAILED_TURN]);
    expect(item?.title).toBe("Developer's last turn failed");
    expect(item?.title.toLowerCase()).not.toContain("stopped");
    render(<StateBadge agent={FAILED_TURN} />);
    expect(screen.getByText("Idle")).toBeTruthy();
  });
});

describe("a failure's words", () => {
  // EVERY KIND THE CLASSIFIER WRITES has a tag and a sentence of its own; the
  // union is held to Go's `llm.ErrorKind` by
  // `internal/providers/llm.TestTheDashboardKnowsEveryErrorKind`.
  const KINDS: Record<ProviderErrorKind, true> = {
    fatal: true,
    rate_limit: true,
    auth: true,
    timeout: true,
    server: true,
    canceled: true,
  };
  test("every kind reads as plain words, never its machine code", () => {
    const generic = failureWords("something-else");
    for (const kind of Object.keys(KINDS)) {
      const words = failureWords(kind);
      expect(words).not.toEqual(generic);
      expect(words.tag).not.toContain(kind);
      expect(words.sentence).not.toContain(kind);
      expect(words.sentence).toMatch(/^[A-Z].*\.$/);
    }
    expect(failureWords(undefined)).toEqual(generic);
    expect(failureWords("turn.guard_breach").tag).toBe("Failed");
  });

  test("the Inbox row leads with the sentence and keeps the raw message aside", () => {
    const [item] = queue([FAILED_TURN]);
    expect(item?.detail).toBe(failureWords("server").sentence);
    expect(item?.detail).not.toContain("llm chain exhausted");
    expect(item?.technical).toBe("llm chain exhausted after 1 providers (default)");
  });
});
