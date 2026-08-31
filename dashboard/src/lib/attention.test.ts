// @vitest-environment node

/**
 * The attention queue: what needs a person, in one list.
 *
 * Every condition here was already known to the previous dashboard and each
 * lived in a different screen — a paused coding run was a badge on the board
 * (and a run whose box had been reclaimed appeared NOWHERE), a stopped seat was
 * a card among the healthy ones, a refusing budget was a bar on one seat's
 * page, and an engine with no active configuration was a line in a popover.
 */

import { describe, expect, test } from "vitest";
import { attentionQueue, type AttentionInput } from "./attention.ts";

const now = Date.parse("2026-01-01T12:00:00Z");

function input(over: Partial<AttentionInput> = {}): AttentionInput {
  return {
    agents: [],
    sandboxes: [],
    budget: {},
    engine: { status: "ok", configured: true },
    seats: [],
    connected: true,
    authRejected: false,
    now,
    ...over,
  };
}

describe("what it surfaces", () => {
  test("a healthy company has nothing waiting", () => {
    expect(attentionQueue(input())).toEqual([]);
  });

  test("an engine with NO ACTIVE CONFIG is critical, not quiet", () => {
    // It looks exactly like a healthy idle one and drops every inbound
    // webhook. This is the whole reason the engine carries the flag.
    const items = attentionQueue(input({ engine: { status: "ok", configured: false } }));
    expect(items[0]?.id).toBe("unconfigured");
    expect(items[0]?.severity).toBe("critical");
    expect(items[0]?.detail).toContain("webhook");
  });

  test("a refused token and an unreachable engine are different items", () => {
    // The repair differs: one comes back on its own, the other never does.
    expect(attentionQueue(input({ connected: false }))[0]?.id).toBe("offline");
    expect(attentionQueue(input({ authRejected: true }))[0]?.id).toBe("auth");
  });

  test("a run paused on a question carries the question", () => {
    const items = attentionQueue(
      input({
        sandboxes: [
          {
            turn_id: "t1",
            role: "Dev A",
            agent_handle: "dev-a",
            agent_id: "",
            coding_agent: "claude-code",
            sandbox_id: "s1",
            task: "",
            status: "awaiting_input",
            started_at: "2026-01-01T11:00:00Z",
            question: "Which branch should I target?",
          },
        ],
      }),
    );
    expect(items[0]?.detail).toBe("Which branch should I target?");
    expect(items[0]?.path).toEqual(["runs"]);
  });

  test("a live round that stopped moving is surfaced, and escalates", () => {
    // A spinning row hides exactly this: the animation is identical whether
    // the round started two seconds or eleven minutes ago.
    const call = (updated: string) => ({
      id: "a",
      role: "Dev A",
      handle: "dev-a",
      live_call: {
        turn_id: "t1",
        phase: "execute",
        iteration: 1,
        model: "",
        trigger: null,
        prompt: "",
        prompt_messages: null,
        response: "",
        input_tokens: 0,
        output_tokens: 0,
        total_tokens: 0,
        tool_executions: null,
        round_num: 3,
        rounds: 3,
        in_progress: true,
        updated_at: updated,
      },
    });
    const fresh = attentionQueue(input({ agents: [call(new Date(now - 1000).toISOString())] }));
    expect(fresh).toEqual([]);

    const stale = attentionQueue(input({ agents: [call(new Date(now - 200_000).toISOString())] }));
    expect(stale[0]?.severity).toBe("caution");

    const stalled = attentionQueue(
      input({ agents: [call(new Date(now - 900_000).toISOString())] }),
    );
    expect(stalled[0]?.severity).toBe("critical");
  });

  test("a budget refusing charges outranks one merely near its cap", () => {
    const refusing = attentionQueue(
      input({ budget: { org: { used: 10, max: 100, refused_at: "2026-01-01T11:00:00Z" } } }),
    );
    expect(refusing[0]?.severity).toBe("critical");

    const near = attentionQueue(input({ budget: { org: { used: 95, max: 100, refused_at: "" } } }));
    expect(near[0]?.severity).toBe("caution");
  });
});

describe("ordering", () => {
  test("severity first, then newest — what it costs to ignore", () => {
    const items = attentionQueue(
      input({
        connected: false,
        budget: { org: { used: 95, max: 100, refused_at: "" } },
        agents: [
          {
            id: "a",
            role: "Dev A",
            last_error: {
              kind: "llm_unavailable",
              message: "provider unreachable",
              phase: "execute",
              turn_id: "t1",
              at: "2026-01-01T11:59:00Z",
              event_id: "e1",
            },
          },
        ],
      }),
    );
    const severities = items.map((i) => i.severity);
    expect(severities).toEqual(
      [...severities].sort((a, b) => (a === b ? 0 : a === "critical" ? -1 : 1)),
    );
    expect(items.some((i) => i.detail.includes("provider unreachable"))).toBe(true);
  });

  test("every id is unique, so a list can key on it", () => {
    const items = attentionQueue(
      input({
        connected: false,
        engine: { status: "ok", configured: false },
        agents: [
          { id: "a", role: "A", state: "afk", afk_reason: "stall" },
          { id: "b", role: "B", state: "afk", afk_reason: "stall" },
        ],
      }),
    );
    expect(new Set(items.map((i) => i.id)).size).toBe(items.length);
  });

  test("every item says what it costs to leave it", () => {
    const items = attentionQueue(
      input({ connected: false, engine: { status: "ok", configured: false } }),
    );
    for (const item of items) {
      expect(item.title.length, item.id).toBeGreaterThan(8);
      expect(item.detail.length, item.id).toBeGreaterThan(20);
    }
  });
});
