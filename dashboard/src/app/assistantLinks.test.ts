/**
 * EVERY LINK THE ASSISTANT OFFERS OPENS THE SCREEN IT NAMES.
 *
 * An open in app card's address is built by the engine from its own link
 * table (`internal/assistant/cards/links.go`), never from text a model wrote,
 * and the engine renders every screen of that table into a golden file
 * (`internal/assistant/cards/testdata/links.json`, written by `make
 * assistant-fixtures`). This suite replays that file through the router the
 * dashboard navigates by: an address that resolves to another screen, or to
 * Not Found, is a link that would send the person somewhere the card did not
 * say. `links.test.ts` already holds that each address resolves at all; this
 * holds that it resolves to the screen the card names.
 *
 * The second half holds a link's QUERY to the keys the screen it opens reads
 * on arrival: a map link carries the frame's peek key with an atlas entry,
 * and one lit along a trace the map's trace key with a way it can light. A
 * screen that renamed its key would open the map and show nothing of what
 * the card promised, and nothing else would notice.
 */

// @vitest-environment node

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import { resolve } from "./routes.ts";
import { parseRef } from "./frame/objects.ts";
import { isTraceChoice, TRACE_KEY } from "~/routes/atlas/filters.ts";
import { INTEGRATION_TABS } from "./routes.ts";
import { INTEGRATION_FORMS } from "~/routes/settings/integrations/address.ts";

interface EngineLink {
  label: string;
  screen: string;
  href: string;
}

const FIXTURE = join(
  process.cwd(),
  "..",
  "internal",
  "assistant",
  "cards",
  "testdata",
  "links.json",
);

test("every link the engine renders opens the screen it names", () => {
  const links = JSON.parse(readFileSync(FIXTURE, "utf8")) as EngineLink[];
  expect(links.length, "the fixture holds too few links to be the engine's table").toBeGreaterThan(
    20,
  );
  const wrong: string[] = [];
  for (const link of links) {
    const [path = ""] = link.href.replace(/^#\//, "").split("?");
    const route = resolve(path.split("/"));
    if (!route.resolved || route.screen !== routeScreenOf(link.screen)) {
      wrong.push(
        `${link.href} names ${link.screen} and opens ${route.resolved ? route.screen : "Not Found"}`,
      );
    }
  }
  expect(wrong).toEqual([]);
});

/**
 * The router's screen a link-table screen opens: the table names the map
 * opened on an entry, and lit along a trace, as screens of their own, and
 * both are the atlas to the router, told apart by their query; one
 * integration's page is the integrations screen with its kind, and the
 * Google page is an Integrations page.
 */
function routeScreenOf(screen: string): string {
  if (screen.startsWith("integration-") || screen === "integration" || screen === "google") {
    return "integrations";
  }
  return screen === "atlas-entry-on-map" || screen === "atlas-trace-on-map" ? "atlas" : screen;
}

// AN INTEGRATION'S LINKS OPEN THAT INTEGRATION, never the list, each at a key
// its page reads: the form at an `edit=` it opens, a tab at a `tab=` it has.
// Mutation: rename a tab or the form's key on the page and this fails.
test("an integration's links open that integration, at a tab or form its page has", () => {
  const links = JSON.parse(readFileSync(FIXTURE, "utf8")) as EngineLink[];
  const ours = links.filter((link) => routeScreenOf(link.screen) === "integrations");
  expect(ours.map((link) => link.screen).sort()).toEqual([
    "google",
    "integration",
    "integration-activity",
    "integration-agents",
    "integration-settings",
    "integration-setup",
    "integrations",
  ]);
  const google = resolve(
    ours
      .find((l) => l.screen === "google")!
      .href.replace(/^#\//, "")
      .split("/"),
  );
  expect(google.resolved && google.screen === "integrations" && google.kind).toBe("google");
  for (const link of ours.filter((l) => l.screen !== "integrations" && l.screen !== "google")) {
    const [path = "", query = ""] = link.href.replace(/^#\//, "").split("?");
    const route = resolve(path.split("/"));
    expect(route.resolved && route.screen === "integrations" && route.kind).toBe("google_cloud");
    expect(link.label.startsWith("Open Google Cloud")).toBe(true);
    const asked = new URLSearchParams(query);
    const tab = asked.get("tab");
    const edit = asked.get("edit");
    if (tab !== null) expect(INTEGRATION_TABS).toContain(tab);
    if (edit !== null) expect(INTEGRATION_FORMS).toContain(edit);
  }
  expect(ours.find((l) => l.screen === "integration-setup")!.label).toBe("Open Google Cloud setup");
});

test("a map link's query is what the atlas screen reads on arrival", () => {
  const links = JSON.parse(readFileSync(FIXTURE, "utf8")) as EngineLink[];
  const onMap = links.filter((link) => link.screen.endsWith("-on-map"));
  expect(onMap.map((link) => link.screen).sort()).toEqual([
    "atlas-entry-on-map",
    "atlas-trace-on-map",
  ]);
  for (const link of onMap) {
    const query = new URLSearchParams(link.href.split("?")[1] ?? "");
    const peek = parseRef(query.get("peek"));
    expect(peek?.kind, `${link.href} peeks no atlas entry`).toBe("atlas");
    const trace = query.get(TRACE_KEY);
    if (link.screen === "atlas-trace-on-map") {
      expect(trace !== null && isTraceChoice(trace), `${link.href} lights no trace`).toBe(true);
    } else {
      expect(trace, `${link.href} carries a trace the entry link never lights`).toBeNull();
    }
    const stray = [...query.keys()].filter((key) => key !== "peek" && key !== TRACE_KEY);
    expect(stray, `${link.href} carries keys the atlas never reads`).toEqual([]);
  }
});
