/**
 * A model at its usage limit, said once per entry in a person's words.
 *
 * The failure this guards: five seats on one spent subscription drawn as five
 * rows, each flipping between "stopped" and "last turn failed" as the engine
 * retried it, which was the Inbox's flicker; and a condition naming the entry
 * by its config key ("default"), which says nothing to the person it stops and
 * is an admin's to read.
 */

import { afterEach, beforeEach, describe, expect, test } from "vitest";
import type { AgentRow, ProviderLimit } from "~/protocol/index.ts";
import { pinZoneForTest } from "./prefs.ts";
import { entryLimits, limitDetail, limitTitle } from "./providerLimit.ts";

// 16:16 in Vancouver, the minute the limit was reported.
const now = Date.parse("2026-10-27T23:16:00Z");

function parked(over: Partial<ProviderLimit> = {}): ProviderLimit {
  return {
    parked: true,
    resets_at: "2026-10-27T23:30:00Z",
    since: "2026-10-27T23:16:00Z",
    cli: "claude-code",
    subscription: true,
    ...over,
  };
}

function seat(id: string, limit: ProviderLimit | null, role = id.toUpperCase()): AgentRow {
  return { id, agent_id: id, role, handle: id, provider_limit: limit };
}

beforeEach(() => pinZoneForTest("America/Vancouver"));
afterEach(() => pinZoneForTest(""));

describe("an entry at its limit", () => {
  test("is one condition however many seats it holds, keyed by what it is of", () => {
    const limits = entryLimits([
      seat("pm", parked()),
      seat("qa", parked({ since: "2026-10-27T23:15:00Z" })),
      seat("designer", null),
      seat("dev", parked({ cli: "codex", subscription: false })),
    ]);
    expect(limits.map((l) => l.key)).toEqual(["claude-code-plan", "codex-usage"]);
    expect(limits[0]!.parked.map((r) => r.id)).toEqual(["pm", "qa"]);
    expect(limits[0]!.since).toBe("2026-10-27T23:15:00Z");
  });

  test("is named by its CLI and how it signs in, never by its key", () => {
    const title = (over: Partial<ProviderLimit>) =>
      limitTitle(entryLimits([seat("pm", parked(over))])[0]!);
    expect(title({})).toBe("Claude Code subscription limit reached");
    expect(title({ cli: "codex", subscription: false })).toBe("Codex usage limit reached");
    // A CLI of the company's own is called a model, never by its entry.
    expect(title({ cli: "custom", subscription: false })).toBe("Model usage limit reached");
  });

  test("says when the agents resume, in the reader's zone", () => {
    const [limit] = entryLimits([seat("pm", parked()), seat("qa", parked())]);
    expect(limitDetail(limit!, now)).toBe("Agents resume at 4:30 PM.");
  });

  test("names a lone seat by its role", () => {
    const [limit] = entryLimits([seat("cto", parked(), "CTO")]);
    expect(limitDetail(limit!, now)).toBe("CTO resumes at 4:30 PM.");
  });

  test("says the resume time is unknown where the vendor did not say, or seats disagree", () => {
    const [unknown] = entryLimits([seat("pm", parked({ resets_at: null }))]);
    expect(limitDetail(unknown!, now)).toBe("PM resumes when it lifts. Resume time unknown.");
    const [split] = entryLimits([
      seat("pm", parked()),
      seat("qa", parked({ resets_at: "2026-10-28T02:00:00Z" })),
    ]);
    expect(split!.resetsAt).toBeNull();
  });

  test("says whether a fallback carries the seats, and who still waits", () => {
    const carried = parked({ parked: false });
    const [limit] = entryLimits([seat("pm", parked()), seat("qa", carried), seat("dev", carried)]);
    expect(limitDetail(limit!, now)).toBe(
      "1 agent resumes at 4:30 PM. 2 agents are running on their fallback model until it lifts at 4:30 PM.",
    );
    const [all] = entryLimits([seat("qa", carried), seat("dev", carried)]);
    expect(limitDetail(all!, now)).toBe(
      "Agents are running on their fallback model until it lifts at 4:30 PM.",
    );
  });

  test("names the day when the reset is not the reader's today", () => {
    const [limit] = entryLimits([
      seat("pm", parked({ resets_at: "2026-10-28T09:00:00Z" })),
      seat("qa", parked({ resets_at: "2026-10-28T09:00:00Z" })),
    ]);
    expect(limitDetail(limit!, now)).toBe("Agents resume at Wed 2:00 AM.");
  });
});
