import { expect, test } from "vitest";
import { CONNECT_HINT, homeFacts, type HomeFacts, type InUse } from "./home.ts";

// A HINT IS A WHOLE SENTENCE in the engine's voice.
test("the connect hint is a whole sentence", () => {
  expect(CONNECT_HINT).toMatch(/^[A-Z].*\.$/);
});

const facts = (inUse: number, hint: string | null = null): HomeFacts => ({
  inUse,
  hint,
  empty: inUse === 0,
});

test.each<[string, InUse[], boolean, HomeFacts]>([
  // A FRESH COMPANY: only Crewlet's own services, counted, and the way to more.
  ["fresh, both services, Admin", ["service", "service"], true, facts(2, CONNECT_HINT)],
  ["fresh, one service, Admin", ["service"], true, facts(1, CONNECT_HINT)],
  // EVERYTHING IN USE COUNTS, built-ins included, never "nothing" beside them.
  ["services and two apps", ["service", "service", "app", "app"], true, facts(4)],
  ["apps only", ["app", "app"], true, facts(2)],
  // A PRODUCT IS ITS OWN CARD, so Jira and Confluence count apart.
  ["a Jira company", ["product", "service"], true, facts(2)],
  ["Jira and Confluence", ["product", "product"], true, facts(2)],
  ["everything", ["app", "app", "product", "product", "app", "app", "app"], true, facts(7)],
  // NOTHING IN USE is said by the empty state, not by a count of zero or a hint.
  ["nothing, Admin", [], true, facts(0)],
  // A READER WITHOUT ADMIN is offered nothing to connect: their note says who does.
  ["reader, both services", ["service", "service"], false, facts(2)],
  ["reader, nothing", [], false, facts(0)],
])("%s", (_, items, admin, want) => {
  expect(homeFacts(items, admin)).toEqual(want);
});
