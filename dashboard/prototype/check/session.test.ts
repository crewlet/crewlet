/**
 * A tab remembers the company a reader's writes left it in, so switching
 * variants compares two proposals of the same state; and a remembered state
 * this build cannot use, or storage that refuses, starts the scenario over
 * rather than stopping the page.
 */

import { afterEach, describe, expect, test, vi } from "vitest";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { fairsplit } from "../fixtures/fairsplit.ts";
import { resumeAll, scenario } from "../fixtures/scenarios.ts";
import { defineTopic } from "../harness/defineTopic.ts";
import type { DemoCompany } from "../harness/engine/company.ts";
import { DemoEngine } from "../harness/engine/engine.ts";
import {
  barFolded,
  loadSession,
  saveSession,
  scenarioActions,
  setBarFolded,
  startOver,
} from "../harness/session.ts";

const topic = defineTopic({
  id: "session-check",
  title: "Session round trip",
  start: "#/home",
  scenarios: [scenario("today", "As today", (c) => c), scenario("working", "Working", resumeAll)],
  variants: [{ id: "today", label: "Today", summary: "The dashboard as it ships." }],
});
const KEY = "crewlet-prototype:session-check:state";

afterEach(() => {
  vi.restoreAllMocks();
  sessionStorage.clear();
  localStorage.clear();
});

describe("the company a reader left", () => {
  test("survives a page load whole, writes included", () => {
    const engine = new DemoEngine(fairsplit());
    let kept: DemoCompany | null = null;
    engine.onChange((c) => (kept = c));
    const reply = engine.rest({
      method: "POST",
      path: "/operator/act/resume_seat",
      query: new URLSearchParams(),
      headers: new Headers(),
      body: JSON.stringify({
        request_id: "0192f0c4-0000-7000-8000-000000000000",
        args: { handle: "pm" },
      }),
    });
    expect(reply.status).toBe(200);
    expect(kept).not.toBeNull();

    saveSession(topic, "working", kept);
    const loaded = loadSession(topic);
    expect(loaded?.scenario).toBe("working");
    // Strict: a value JSON cannot carry (a Date, a Map, an undefined in a
    // list) would come back as something else, and this would say so.
    expect(loaded?.company).toStrictEqual(kept);
    const again = new DemoEngine(loaded!.company!);
    expect(again.agentRows().find((a) => a.handle === "pm")).toMatchObject({
      activity: "idle",
      paused: null,
    });
  });

  test("a scenario the topic no longer offers or a torn write starts over, and another build's company is rebuilt in the scenario chosen", () => {
    const format = __PROTOTYPE_COMPANY_FORMAT__;
    expect(format).toMatch(/^[0-9a-f]{16}$/);
    for (const raw of [JSON.stringify({ format, scenario: "gone", company: null }), "{"]) {
      sessionStorage.setItem(KEY, raw);
      expect(loadSession(topic)).toBeNull();
    }
    for (const other of [1, "another-build"]) {
      sessionStorage.setItem(
        KEY,
        JSON.stringify({ format: other, scenario: "working", company: fairsplit() }),
      );
      expect(loadSession(topic)).toEqual({ scenario: "working", company: null });
    }
    sessionStorage.setItem(KEY, JSON.stringify({ format, scenario: "today", company: null }));
    expect(loadSession(topic)).toEqual({ scenario: "today", company: null });
  });

  test("storage that refuses is a demo that cannot remember, never one that cannot boot", () => {
    const denied = () => {
      throw new DOMException("The operation is insecure.", "SecurityError");
    };
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(denied);
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(denied);
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(denied);
    expect(() => saveSession(topic, "today", fairsplit())).not.toThrow();
    expect(loadSession(topic)).toBeNull();
    expect(() => startOver(topic, "today")).not.toThrow();
    expect(() => setBarFolded(true)).not.toThrow();
    expect(barFolded()).toBe(false);
  });
});

describe("starting over", () => {
  test("keeps the scenario chosen and drops the company and the org builder's draft", () => {
    saveSession(topic, "working", fairsplit());
    sessionStorage.setItem(STORAGE_KEYS.orgDraft, JSON.stringify({ units: [] }));
    startOver(topic, "working");
    expect(loadSession(topic)).toEqual({ scenario: "working", company: null });
    expect(sessionStorage.getItem(STORAGE_KEYS.orgDraft)).toBeNull();
  });
});

describe("the bar's scenario menu and Reset", () => {
  test("Reset reloads into the scenario on screen, a choice into the one chosen", () => {
    const reload = vi.fn<() => void>();
    const actions = scenarioActions(topic, "working", reload);
    saveSession(topic, "working", fairsplit());
    actions.onReset();
    expect(loadSession(topic)).toEqual({ scenario: "working", company: null });
    actions.onScenario("today");
    expect(loadSession(topic)).toEqual({ scenario: "today", company: null });
    expect(reload).toHaveBeenCalledTimes(2);
  });
});

describe("the variant bar's fold", () => {
  test("is kept as the reader left it", () => {
    setBarFolded(true);
    expect(barFolded()).toBe(true);
    setBarFolded(false);
    expect(barFolded()).toBe(false);
  });
});
