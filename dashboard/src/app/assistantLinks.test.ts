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
 */

// @vitest-environment node

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import { resolve } from "./routes.ts";

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
    const route = resolve(link.href.replace(/^#\//, "").split("/"));
    if (!route.resolved || route.screen !== link.screen) {
      wrong.push(
        `${link.href} names ${link.screen} and opens ${route.resolved ? route.screen : "Not Found"}`,
      );
    }
  }
  expect(wrong).toEqual([]);
});
