/**
 * The Rooms panel's views and the figure each carries.
 */

import { describe, expect, test } from "vitest";

import { workspaceRow } from "~/app/nav.ts";

import { PANEL_VIEWS, unsentCount, viewCount } from "./panelViews.ts";

describe("the panel's views", () => {
  test("are the chat row's sections drawn as tabs, under the row's names and glyphs", () => {
    // THE ROW IS THE ONE TABLE: a view added there is a row here.
    const tabs = (workspaceRow("chatlet")?.sections ?? []).filter(
      (s) => s.tab !== false && s.path.length > 1,
    );
    expect(PANEL_VIEWS.map((v) => [v.kind, v.label, v.icon])).toEqual(
      tabs.map((s) => [s.key, s.label, s.icon]),
    );
    expect(PANEL_VIEWS.map((v) => v.kind)).toEqual(["activity", "threads", "drafts", "saved"]);
  });
});

describe("what each view's row carries", () => {
  const none = { count: 0 };
  const quiet = { rows: [] };

  test("Activity is the accent badge of what waits on the reader; the rest are quiet counts", () => {
    const counts = { activity: 3, threads: 2, saved: 5 };
    expect(viewCount("activity", { counts, drafts: none, outbox: quiet })).toMatchObject({
      value: 3,
      tone: "attention",
    });
    expect(viewCount("threads", { counts, drafts: none, outbox: quiet })).toMatchObject({
      value: 2,
      tone: "quiet",
    });
    expect(viewCount("saved", { counts, drafts: none, outbox: quiet })).toMatchObject({
      value: 5,
      tone: "quiet",
    });
  });

  test("a zero, and counts not read yet, carry no figure", () => {
    expect(viewCount("activity", { counts: null, drafts: none, outbox: quiet })).toBeNull();
    const counts = { activity: 0, threads: 0, saved: 0 };
    expect(viewCount("saved", { counts, drafts: none, outbox: quiet })).toBeNull();
  });

  test("Drafts and sent counts every draft and every message the engine refused or never answered", () => {
    // A MESSAGE ON ITS WAY WAITS ON NOBODY. Mutation: count `sending` rows
    // and this goes red.
    const outbox = {
      rows: [
        { state: "sending" },
        { state: "accepted" },
        { state: "unknown" },
        { state: "refused" },
      ] as never,
    };
    expect(unsentCount({ count: 2 }, outbox)).toBe(4);
    expect(viewCount("drafts", { counts: null, drafts: { count: 2 }, outbox })).toMatchObject({
      value: 4,
      tone: "quiet",
    });
  });
});
