/**
 * THE CONTRIBUTION GATE: every widget a section offers for Home is well formed.
 *
 * A section offers widgets in a `home.widgets.tsx` beside its screens, found
 * at build time, and `defineWidgets` leaves out a contribution that fails its
 * check rather than taking Home down with it. That is right for a reader and
 * wrong for a pull request: a widget that names a page the navigation does not
 * have would ship missing, and nobody would see it go. So this suite loads
 * every contribution and fails on any refusal, and holds what the tray and the
 * board rely on: unique keys, a real page behind each, the sidebar's order and
 * its locks.
 */

import { describe, expect, test, vi } from "vitest";
import { HashGlyph } from "@crewlethq/icons/glyphs";
import { DESTINATIONS, WORKSPACES } from "~/app/nav.ts";
import { AREAS, WIDGETS, canUse } from "./registry.tsx";
import {
  WIDGET_PROBLEMS,
  defineWidgets,
  destinationOf,
  pageOpens,
  type WidgetDef,
} from "./kit.tsx";

const ADMIN = ["admin"];
const MEMBER = ["worklet", "doclet"];

describe("every contribution", () => {
  test("passes its check: nothing a section offers is left out", () => {
    expect(WIDGETS.size).toBeGreaterThan(0);
    expect(WIDGET_PROBLEMS).toEqual([]);
  });

  test("has a key of its own, starting with its page's workspace", () => {
    const keys = [...WIDGETS.values()].map((d) => d.key);
    expect(new Set(keys).size).toBe(keys.length);
    for (const def of WIDGETS.values()) {
      const dest = destinationOf(def.from)!;
      expect(def.key.startsWith(`${dest.workspace}.`), def.key).toBe(true);
    }
  });

  test("names a page the navigation has", () => {
    const pages = new Set(DESTINATIONS.map((d) => d.key));
    for (const def of WIDGETS.values()) expect(pages.has(def.from), def.key).toBe(true);
  });
});

describe("the tray's sections", () => {
  test("follow the sidebar's order, one per workspace that offers widgets", () => {
    const order = WORKSPACES.map((w) => w.key);
    const areas = AREAS.map((a) => a.key);
    expect(areas).toEqual([...areas].sort((a, b) => order.indexOf(a) - order.indexOf(b)));
    for (const key of [
      "home",
      "inbox",
      "me",
      "org",
      "schedules",
      "integrations",
      "budgets",
      "worklet",
      "chatlet",
      "live",
      "doclet",
      "settings",
    ] as const) {
      expect(areas, key).toContain(key);
    }
  });

  test("carry the sidebar's own name and sentence for each workspace", () => {
    for (const area of AREAS) {
      const row = WORKSPACES.find((w) => w.key === area.key)!;
      expect(area.label).toBe(row.label);
      expect(area.hint).toBe(row.hint);
      for (const def of area.widgets) expect(destinationOf(def.from)?.workspace).toBe(area.key);
    }
  });
});

describe("who may add what", () => {
  const settings = AREAS.find((a) => a.key === "settings")!.widgets;

  // SETTINGS IS ADMIN'S BUT FOR SPEND'S READINGS, which every reader reads:
  // its widgets follow its sections.
  test("a reader without admin is offered of Settings' widgets only Spend's readings and the models", () => {
    expect(settings.length).toBeGreaterThan(0);
    for (const def of settings) {
      // Expensive tasks names its tasks by the tracker's keys, which MEMBER holds;
      // each model's condition is every member's (`llm_entries`' member reading).
      const open =
        def.from === "settings-spend" ||
        def.from === "settings-spend-tasks" ||
        def.key === "settings.models";
      expect(canUse(def, MEMBER), def.key).toBe(open);
    }
  });

  // A PAGE EVERY READER OPENS, A WIDGET AN ADMIN'S: Integrations shows every
  // reader Crewlet's own services, while its widget counts the connected
  // apps, which only an Admin reads. Mutation: drop the widget's `grants` and
  // a reader without Admin is offered a card the engine refuses.
  test("the Integrations page opens for every reader and its widget stays an Admin's", () => {
    const def = WIDGETS.get("integrations.connected")!;
    expect(pageOpens(destinationOf(def.from)!, MEMBER)).toBe(true);
    expect(canUse(def, MEMBER)).toBe(false);
    expect(canUse(def, ADMIN)).toBe(true);
  });

  test("an admin is offered every widget", () => {
    for (const def of WIDGETS.values()) expect(canUse(def, ADMIN), def.key).toBe(true);
  });

  test("a reader without Chatlet is offered none of Chatlet's widgets", () => {
    const chat = AREAS.find((a) => a.key === "chatlet")!.widgets;
    expect(chat.length).toBeGreaterThan(0);
    for (const def of chat) {
      expect(canUse(def, MEMBER), def.key).toBe(false);
      expect(canUse(def, ["chatlet"]), def.key).toBe(true);
    }
  });

  test("a reader without Worklet is offered none of Worklet's widgets", () => {
    for (const key of ["inbox", "me", "worklet"] as const) {
      for (const def of AREAS.find((a) => a.key === key)!.widgets) {
        expect(canUse(def, ["doclet"]), def.key).toBe(false);
        expect(canUse(def, ["worklet"]), def.key).toBe(true);
      }
    }
  });
});

describe("a contribution that fails its check", () => {
  const def = (over: Partial<WidgetDef>): WidgetDef => ({
    key: "home.example",
    from: "home",
    label: "Example",
    description: "An example widget",
    icon: HashGlyph,
    hue: "blue",
    size: { w: 4, minW: 2, minH: 4, vertical: true },
    Component: () => null,
    ...over,
  });

  test("is reported and left out, and the rest of its file is kept", () => {
    const before = WIDGET_PROBLEMS.length;
    // Said to the console too, for whoever is watching a build that left one out.
    const said = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const kept = defineWidgets([
        def({}),
        def({ key: "home.lost", from: "nowhere" }),
        def({ key: "worklet.misfiled", from: "home" }),
        def({ key: "home.tiny", size: { w: 1, minW: 1, minH: 1, vertical: true } }),
        def({
          key: "home.twice",
          params: [
            { key: "a", label: "A", useChoices: () => ({ options: [], loading: false }) },
            { key: "a", label: "A", useChoices: () => ({ options: [], loading: false }) },
          ],
        }),
      ]);
      expect(kept.map((d) => d.key)).toEqual(["home.example"]);
      const reported = WIDGET_PROBLEMS.slice(before).join("\n");
      expect(reported).toContain('home.lost: "nowhere" is not a page in the navigation.');
      expect(reported).toContain("worklet.misfiled: its key starts with its page's workspace");
      expect(reported).toContain("home.tiny: 2 <= minW <= w <= 12.");
      expect(reported).toContain('home.twice: param "a" is a unique lowercase key.');
      expect(said).toHaveBeenCalledTimes(4);
    } finally {
      WIDGET_PROBLEMS.length = before;
      said.mockRestore();
    }
  });
});
