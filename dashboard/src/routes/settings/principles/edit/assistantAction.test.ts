// @vitest-environment node

/**
 * The Principles editor's draft as the assistant sees it: every part's value,
 * and a new value only for a part the person has open, in that part's shape.
 */

import { describe, expect, test, vi } from "vitest";
import { draftSnapshot, parseDraftChange } from "~/app/assistant/draft.ts";
import { charterDraft } from "./assistantAction.ts";
import { INITIAL, reducer } from "./state.ts";

const CHARTER = { mission: "Ship calm software.", vision: "Old vision", policies: ["Be kind"] };

const open = reducer(INITIAL, {
  type: "open",
  part: "vision",
  base: "Old vision",
  origin: "edit",
  focus: false,
});

const spec = (change = vi.fn()) =>
  charterDraft({ state: open, charter: CHARTER, change, blocked: () => null });

describe("the principles draft", () => {
  test("shows every part, and only an open one as settable", () => {
    const snapshot = draftSnapshot(spec());
    expect(snapshot).toMatchObject({ kind: "client", step: "vision", more: 0 });
    expect(snapshot.fields).toEqual([
      { path: "mission", label: expect.any(String), kind: "text", value: "Ship calm software." },
      {
        path: "vision",
        label: expect.any(String),
        kind: "text",
        value: "Old vision",
        editable: true,
      },
      {
        path: "policies",
        label: expect.any(String),
        value: "Be kind",
        hint: expect.any(String),
        kind: "list",
      },
    ]);
  });

  test("sets an open part through the editor's own change", () => {
    const change = vi.fn();
    const draft = spec(change);
    const parsed = parseDraftChange(draft, { values: { vision: "A new vision." } });
    expect(parsed).toMatchObject({
      ok: true,
      value: { rows: [{ next: "A new vision.", current: "Old vision" }] },
    });
    if (parsed.ok) draft.apply!(parsed.value.next);
    expect(change).toHaveBeenCalledWith("vision", "A new vision.");
  });

  test("refuses a part the person has not opened, rather than opening it", () => {
    expect(parseDraftChange(spec(), { values: { mission: "x" } })).toEqual({
      ok: false,
      reason: expect.stringMatching(/not something Ask Crewlet can set/),
    });
  });

  test("refuses a value of the wrong shape, an unknown part and extra keys", () => {
    expect(parseDraftChange(spec(), { values: { vision: ["a"] } }).ok).toBe(false);
    expect(parseDraftChange(spec(), { values: { values: "x" } }).ok).toBe(false);
    expect(parseDraftChange(spec(), { values: { vision: "x" }, save: true }).ok).toBe(false);
    expect(parseDraftChange(spec(), "A new vision.").ok).toBe(false);
  });
});
