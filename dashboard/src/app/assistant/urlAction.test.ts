/**
 * A page action over the address writes only the keys its screen named, in
 * one navigation with the screen's own history rule, and undoes only what it
 * wrote.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { parseHash } from "../router.tsx";
import { parseUrlPatch, tabKey, urlAction, type UrlActionKey, type UrlPatch } from "./urlAction.ts";

const KEYS: Record<string, UrlActionKey> = {
  status: {
    label: "Status",
    history: "filter",
    check: (v) => (v === "open" || v === "done" ? null : "Not a status this list draws."),
    words: (v) => (v === "open" ? "Open" : "Done"),
  },
  sort: { label: "Sort", history: "filter" },
  window: { label: "Window", history: "section" },
  "f.": { label: "Field", history: "filter" },
};

/** A router stand-in that writes the address the way `app/router.tsx` does. */
function navigator() {
  const nav = {
    filter: vi.fn((patch: Record<string, string | null>) => {
      const route = parseHash(location.hash);
      for (const [k, v] of Object.entries(patch)) {
        if (v == null || v === "") route.query.delete(k);
        else route.query.set(k, v);
      }
      location.hash = `/${route.path.join("/")}?${route.query}`;
    }),
    to: vi.fn((path: readonly string[], query?: URLSearchParams | Record<string, string>) => {
      location.hash = `/${path.join("/")}?${new URLSearchParams(query as URLSearchParams)}`;
    }),
  };
  return nav;
}

function spec(nav: ReturnType<typeof navigator>, blocked?: (p: UrlPatch) => string | null) {
  return urlAction("work.filter.apply", {
    scope: "Worklet list filters (not saved)",
    keys: KEYS,
    nav,
    ...(blocked ? { blocked } : {}),
  })["work.filter.apply"]!;
}

beforeEach(() => {
  location.hash = "/worklet/tasks?status=done&sort=rank";
});

afterEach(() => {
  location.hash = "";
});

describe("parse", () => {
  const s = spec(navigator());

  test("takes the screen's keys, a family's members and a clear", () => {
    expect(s.parse({ status: "open", "f.severity": "high", sort: null })).toEqual({
      ok: true,
      value: { status: "open", "f.severity": "high", sort: null },
    });
  });

  test("refuses a key the screen did not name, whole, by name", () => {
    expect(s.parse({ status: "open", assignee: "me" })).toEqual({
      ok: false,
      reason: 'This page has no "assignee" setting.',
    });
  });

  test("refuses a family member that is not a slug, and a family named bare", () => {
    expect(s.parse({ "f.Not A Slug": "x" }).ok).toBe(false);
    expect(s.parse({ "f.": "x" }).ok).toBe(false);
  });

  test("refuses what is not plain text, and what the key's own check refuses", () => {
    expect(s.parse({ status: 3 }).ok).toBe(false);
    expect(s.parse({ status: "open\u0000" }).ok).toBe(false);
    expect(s.parse({ status: "x".repeat(201) }).ok).toBe(false);
    expect(s.parse({ status: "blocked" })).toEqual({
      ok: false,
      reason: "Not a status this list draws.",
    });
  });

  test("refuses what is not a patch at all, and one that changes nothing", () => {
    for (const raw of [null, "status=open", ["status"], 7]) expect(s.parse(raw).ok).toBe(false);
    expect(s.parse({}).ok).toBe(false);
  });
});

describe("preview", () => {
  test("is one row per key, the new value and the one on the address, in the screen's words", () => {
    const s = spec(navigator());
    expect(s.preview({ status: "open", "f.severity": "high" })).toEqual([
      { field: "Status", next: "Open", current: "Done" },
      { field: "Field", next: "high", current: null },
    ]);
  });
});

describe("apply", () => {
  test("a patch of filters is one replace", () => {
    const nav = navigator();
    spec(nav).apply({ status: "open", sort: null });
    expect(nav.filter).toHaveBeenCalledTimes(1);
    expect(nav.to).not.toHaveBeenCalled();
    expect(parseHash(location.hash).query.toString()).toBe("status=open");
  });

  test("a patch touching a section is one push, carrying every key", () => {
    const nav = navigator();
    spec(nav).apply({ status: "open", window: "7d" });
    expect(nav.to).toHaveBeenCalledTimes(1);
    expect(nav.filter).not.toHaveBeenCalled();
    const query = parseHash(location.hash).query;
    expect([query.get("status"), query.get("window"), query.get("sort")]).toEqual([
      "open",
      "7d",
      "rank",
    ]);
  });

  test("blocked is the screen's own gate", () => {
    const s = spec(navigator(), (p) => ("status" in p ? "Status is what this page is." : null));
    expect(s.blocked({ status: "open" })).toBe("Status is what this page is.");
    expect(s.blocked({ sort: "due" })).toBeNull();
  });
});

describe("undo", () => {
  test("puts back exactly what the address held, while it still holds what the action wrote", () => {
    const nav = navigator();
    const s = spec(nav);
    const undo = s.undo!({ status: "open", sort: null });
    s.apply({ status: "open", sort: null });
    expect(undo()).toBe(true);
    const query = parseHash(location.hash).query;
    expect([query.get("status"), query.get("sort")]).toEqual(["done", "rank"]);
  });

  test("changes nothing once the person changed a key it wrote", () => {
    const nav = navigator();
    const s = spec(nav);
    const undo = s.undo!({ status: "open" });
    s.apply({ status: "open" });
    nav.filter({ status: "done" });
    nav.filter.mockClear();
    expect(undo()).toBe(false);
    expect(nav.filter).not.toHaveBeenCalled();
  });
});

describe("a tab strip's key", () => {
  test("takes only the object's own tabs, worded as the strip words them, its landing a clear", () => {
    const keys = {
      lens: tabKey("Lens", ["tasks", "about"] as const, { tasks: "Tasks", about: "About" }),
    };
    expect(parseUrlPatch({ lens: "about" }, keys)).toEqual({ ok: true, value: { lens: "about" } });
    expect(parseUrlPatch({ lens: "tasks" }, keys)).toEqual({ ok: true, value: { lens: null } });
    expect(parseUrlPatch({ lens: "files" }, keys).ok).toBe(false);
    expect(keys.lens.words!("about")).toBe("About");
    expect(keys.lens.history).toBe("section");
  });
});
