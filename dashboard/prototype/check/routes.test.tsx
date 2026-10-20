/**
 * Every route a reader can reach renders on the demo company, in the variant
 * under check, with nothing said on the console.
 *
 * The prototype is booted exactly as its page boots (`bootPrototype`, the
 * demo client, the fetch shim, `<App/>` under the hash router) and then
 * walked: every workspace and every section the navigation offers, and one
 * of each object page the demo company holds. A route passes when its screen
 * settles with no error boundary drawn, no "no such screen", and no console
 * error or warning, which is where the demo engine says it was asked
 * something it does not model.
 */

import { captureOwnerStack } from "react";
import { afterAll, beforeAll, describe, expect, test, vi } from "vitest";
import { WORKSPACES } from "~/app/nav.ts";
import { bootPrototype } from "../harness/boot.tsx";
import type { Topic } from "../harness/defineTopic.ts";
import { fixtureId } from "../harness/engine/ids.ts";

const { default: topic } = (await import(/* @vite-ignore */ __PROTOTYPE_TOPIC__)) as {
  default: Topic;
};
const variant = __PROTOTYPE_VARIANT__;

/** Every section the navigation offers, then one of each object page. */
const ROUTES: string[] = [
  ...new Set([
    ...WORKSPACES.flatMap((ws) => [ws.path, ...ws.sections.map((s) => s.path)]).map(
      (p) => `#/${p.join("/")}`,
    ),
    "#/agents/seats/pm",
    "#/agents/seats/engineer",
    "#/agents/seats/pm?tab=work",
    "#/agents/seats/pm?tab=turns",
    "#/agents/seats/pm?tab=memory",
    "#/agents/seats/pm?tab=schedules",
    "#/agents/seats/pm?tab=settings",
    "#/agents/teams/Product%20and%20Engineering",
    "#/agents/schedules/role/pm/daily-standup",
    "#/inbox?row=" + encodeURIComponent("notice:" + fixtureId("notice-1")),
    "#/work/FAIR",
    "#/work/FAIR-1",
    "#/work/FAIR-2",
    `#/live/turns/${fixtureId("pm-turn-0")}`,
    `#/live/events/${fixtureId("pm-turn-0-started")}`,
    `#/live/traces/${fixtureId("pm-trace-0")}`,
    `#/knowledge/pages/${fixtureId("page-brief")}`,
    "#/knowledge/FAIR",
    "#/knowledge/diaries/pm",
    "#/settings/integrations/slack",
    "#/settings/models/default",
    "#/settings/nodes/node-0",
  ]),
];

/**
 * What the SHIPPED dashboard says on a route by itself: a bug in production
 * code this check found, named with the module it is in. Each is reported to
 * be fixed in the dashboard rather than worked around here.
 *
 * AN ENTRY EXCUSES ONLY ITS OWN MODULE'S WORDS. A line is attributed to the
 * component that drew what it is about, the first frame of React's owner
 * stack when it was said, so the entry covers a replacement that renders the
 * shipped screen through `@original` and never a replacement's own code,
 * which is the topic's and is judged like the rest of it.
 *
 * THE LIST IS TWO-SIDED where the variant ships the module as it is: an entry
 * whose route was walked and that no longer occurs fails the check, so it is
 * deleted the day the dashboard is fixed. A variant that swaps the module out
 * may not draw the shipped screen at all, so there the entry is never stale.
 */
const KNOWN: { route: RegExp; said: RegExp; module: string; why: string }[] = [
  {
    route: /^#\/settings\/integrations\/[a-z]+$/,
    said: /Encountered two children with the same key/,
    module: "src/routes/settings/Integrations.tsx",
    why: "renders <EntryRow key={focus.key}> and <SetupPasses key={focus.key}> as siblings of one list",
  },
];

/** The shipped modules this variant swaps out, as paths under `dashboard/`. */
const SWAPPED = new Set(__PROTOTYPE_SWAPPED__);

/**
 * The module whose component drew what a console line is about, as a path
 * under `dashboard/`: the first frame of React's owner stack at the moment
 * the line was said. Empty when React was not rendering, or the frame is not
 * a dashboard module.
 */
function ownerModule(): string {
  const frame = (captureOwnerStack() ?? "")
    .split("\n")
    .map((line) => line.trim())
    .find((line) => line.startsWith("at "));
  const file = /\((.+):\d+:\d+\)$/.exec(frame ?? "")?.[1] ?? "";
  const root = `${__PROTOTYPE_DASHBOARD__}/`;
  return file.startsWith(root) ? file.slice(root.length) : "";
}

/** What the console said while one route was on screen, each line with the module that drew it. */
let said: { line: string; owner: string }[] = [];
/** The known entries a walked route actually produced, and the ones expected to occur on a walked route. */
const heard = new Set<number>();
const walked = new Set<number>();

/** Resolve once the page has stopped changing: a screen's chunk, its reads and their renders are in. */
async function settle(): Promise<void> {
  let last = "";
  let steady = 0;
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 120));
    const now = document.body.innerHTML;
    steady = now === last ? steady + 1 : 0;
    last = now;
    if (steady >= 4) return;
  }
}

function text(el: Element | null): string {
  return (el?.textContent ?? "").replace(/\s+/g, " ").trim();
}

describe(`${topic.id}, variant ${variant}`, () => {
  beforeAll(async () => {
    const record =
      (level: string) =>
      (...args: unknown[]) =>
        said.push({
          line: `${level}: ${args.map((a) => (a instanceof Error ? a.stack : typeof a === "string" ? a : JSON.stringify(a))).join(" ")}`,
          owner: ownerModule(),
        });
    vi.spyOn(console, "error").mockImplementation(record("error"));
    vi.spyOn(console, "warn").mockImplementation(record("warn"));
    vi.spyOn(console, "info").mockImplementation(() => {});
    document.body.innerHTML = '<div id="root"></div>';
    history.replaceState(null, "", topic.start);
    bootPrototype(topic, variant);
    await settle();
  });

  afterAll(() => {
    vi.restoreAllMocks();
    const stale = KNOWN.filter((_, i) => walked.has(i) && !heard.has(i)).map(
      (k) => `${k.module} ${k.why}`,
    );
    expect(stale, "a known dashboard issue no longer occurs: delete its entry").toEqual([]);
  });

  test("boots on the topic's start address with the variant bar", () => {
    expect(location.hash).toBe(topic.start);
    expect(text(document.getElementById("crewlet-prototype-bar"))).toContain(topic.title);
    expect(said.map((s) => s.line)).toEqual([]);
  });

  test.each(ROUTES)("%s renders", async (route) => {
    said = [];
    location.hash = route;
    await settle();
    const root = document.getElementById("root");
    const failed = [...(root?.querySelectorAll('[role="alert"]') ?? [])]
      .map(text)
      .filter((t) => /could not be drawn/i.test(t));
    expect(failed, "an error boundary was drawn").toEqual([]);
    expect(text(root), "the route resolved to no screen").not.toMatch(
      /This URL names .* and there is no such screen/,
    );
    expect(text(root).length, "the screen drew nothing").toBeGreaterThan(40);
    const unexplained = said.filter(({ line, owner }) => {
      const known = KNOWN.findIndex(
        (k) => k.module === owner && k.route.test(route) && k.said.test(line),
      );
      if (known >= 0) heard.add(known);
      return known < 0;
    });
    KNOWN.forEach((k, i) => !SWAPPED.has(k.module) && k.route.test(route) && walked.add(i));
    expect(
      unexplained.map(({ line, owner }) => (owner ? `${line} (drawn by ${owner})` : line)),
      "the console was told something",
    ).toEqual([]);
  });
});
