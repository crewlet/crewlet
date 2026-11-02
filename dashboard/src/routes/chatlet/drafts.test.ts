import { describe, expect, it, vi } from "vitest";

import { ChatDrafts, draftKey } from "./drafts.ts";

describe("ChatDrafts", () => {
  it("keeps a room's draft and a thread's apart, under their own keys", () => {
    const drafts = new ChatDrafts();
    drafts.set(draftKey("r1"), "to the room", "r1");
    drafts.set(draftKey("r1", "m9"), "to the thread", "r1", "m9");
    expect(drafts.get("r1")).toBe("to the room");
    expect(drafts.get("r1/m9")).toBe("to the thread");
    expect(drafts.count).toBe(2);
  });

  it("marks a room that holds a draft in its own box or in one of its threads", () => {
    const drafts = new ChatDrafts();
    drafts.set(draftKey("r1", "m9"), "half a reply", "r1", "m9");
    expect(drafts.has("r1")).toBe(true);
    expect(drafts.has("r2")).toBe(false);
  });

  it("treats a box holding only whitespace as no draft", () => {
    const drafts = new ChatDrafts();
    drafts.set("r1", "words", "r1");
    drafts.set("r1", "   \n", "r1");
    expect(drafts.has("r1")).toBe(false);
    expect(drafts.count).toBe(0);
  });

  it("tells its readers about a change and stays quiet about a write that changes nothing", () => {
    const drafts = new ChatDrafts();
    const heard = vi.fn();
    const stop = drafts.subscribe(heard);
    drafts.set("r1", "a", "r1");
    drafts.set("r1", "a", "r1");
    drafts.set("r2", "", "r2");
    expect(heard).toHaveBeenCalledTimes(1);
    stop();
    drafts.set("r1", "b", "r1");
    expect(heard).toHaveBeenCalledTimes(1);
  });

  it("lists the newest edit first", () => {
    vi.useFakeTimers();
    try {
      const drafts = new ChatDrafts();
      vi.setSystemTime(1_000);
      drafts.set("r1", "older", "r1");
      vi.setSystemTime(2_000);
      drafts.set("r2/m1", "newer", "r2", "m1");
      expect(drafts.list().map((d) => d.text)).toEqual(["newer", "older"]);
      expect(drafts.list()[0]).toMatchObject({ room: "r2", root: "m1" });
    } finally {
      vi.useRealTimers();
    }
  });
});
