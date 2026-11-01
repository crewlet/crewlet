// @vitest-environment node

/**
 * Which of Crewlet's own services a company runs, read off the engine's word,
 * and what the navigation offers for each answer.
 */

import { expect, test } from "vitest";
import { servicesRun } from "./services.ts";
import {
  DESTINATIONS,
  WORKSPACES,
  offeredKinds,
  pathShown,
  serviceOfPath,
  serviceShown,
} from "~/app/nav.ts";
import { MAKE_ORDER } from "./artifacts/words.ts";

// NOTHING IS HIDDEN UNTIL THE ENGINE HAS SAID: the projection names no services
// only on a node running no company, which is "not known yet", never "none".
// Mutation: read an absent answer as none, and the first case hides Work.
test("the engine's word decides what runs, and every service runs until it has said", () => {
  expect(servicesRun(null)).toEqual({ tracker: true, knowledge: true, code: true });
  expect(servicesRun({ tracker: "native", knowledge: "native", code: "native" })).toEqual({
    tracker: true,
    knowledge: true,
    code: true,
  });
  expect(servicesRun({ tracker: "jira", knowledge: "native", code: "native" })).toEqual({
    tracker: false,
    knowledge: true,
    code: true,
  });
  expect(servicesRun({ tracker: "native", knowledge: "confluence", code: "native" })).toEqual({
    tracker: true,
    knowledge: false,
    code: true,
  });
  // THE CODE HOST IS ITS OWN AXIS, read off its own field and no other.
  expect(servicesRun({ tracker: "native", knowledge: "native", code: "none" })).toEqual({
    tracker: true,
    knowledge: true,
    code: false,
  });
  // ONE OBJECT PER ANSWER, so a memo keyed on it moves only when it changes.
  expect(servicesRun({ tracker: "jira", knowledge: "none", code: "none" })).toBe(
    servicesRun({ tracker: "none", knowledge: "confluence", code: "none" }),
  );
  expect(servicesRun({ tracker: "jira", knowledge: "none", code: "none" })).not.toBe(
    servicesRun({ tracker: "jira", knowledge: "none", code: "native" }),
  );
});

// HIDDEN, NOT LOCKED, where the engine hosts no code: no Code row, no Code
// destination and no address under it — My work's Reviews and Settings › Code
// are the code host's too — and nothing else of the company goes with it. Mutation: drop `service` from Code's row, and it is offered.
test("a company hosting no code in the engine is offered no Code", () => {
  const none = { tracker: true, knowledge: true, code: false } as const;
  const rows = WORKSPACES.filter((w) => w.place !== "menu" && serviceShown(w, none)).map(
    (w) => w.key,
  );
  expect(rows).not.toContain("code");
  expect(rows).toEqual(expect.arrayContaining(["work", "knowledge"]));
  const places = DESTINATIONS.filter((d) => !serviceShown(d, none)).map((d) => d.key);
  expect(places.sort()).toEqual([
    "code",
    "code-pushes",
    "code-repos",
    "me-reviews",
    "settings-code",
  ]);
  expect(serviceOfPath(["code", "pushes"])).toBe("code");
  expect(pathShown(["code", "repos"], none)).toBe(false);
  expect(pathShown(["work"], none)).toBe(true);
});

// HIDDEN, NOT LOCKED: a company on another tracker is offered no Work and no
// way to its tasks, and keeps the Inbox, whose seat and budget conditions
// arrive whatever runs the tracker — and My work, for its Reviews, where the
// engine hosts its code (navServices.test.ts holds My work to either app).
test("a company on another tracker is offered nothing of Crewlet Works", () => {
  const jira = { tracker: false, knowledge: true, code: true } as const;
  const rows = WORKSPACES.filter((w) => w.place !== "menu" && serviceShown(w, jira)).map(
    (w) => w.key,
  );
  expect(rows).not.toContain("work");
  expect(rows).toContain("me");
  expect(rows).toContain("inbox");
  expect(rows).toContain("knowledge");
  const places = DESTINATIONS.filter((d) => !serviceShown(d, jira)).map((d) => d.key);
  expect(places).toEqual(
    expect.arrayContaining([
      "me",
      "work",
      "work-tasks",
      "settings-spend-tasks",
      "artifacts-new-design",
    ]),
  );
  // AN ADDRESS UNDER ONE IS NOT OFFERED EITHER, an item's included.
  expect(pathShown(["work", "ENG-42"], jira)).toBe(false);
  expect(serviceOfPath(["me", "asked-of-me"])).toBe("tracker");
  expect(pathShown(["inbox"], jira)).toBe(true);
  // NEW DESIGN'S ONE WAY TO FINISH IS AN AGENT, which files a task.
  expect(offeredKinds(jira)).toEqual(MAKE_ORDER.filter((k) => k !== "design"));
  expect(offeredKinds({ tracker: true, knowledge: true, code: true })).toEqual([...MAKE_ORDER]);
});

test("a company on another knowledge base is offered no Knowledge", () => {
  const confluence = { tracker: true, knowledge: false, code: true } as const;
  const places = DESTINATIONS.filter((d) => !serviceShown(d, confluence)).map((d) => d.key);
  expect(places.sort()).toEqual(["knowledge", "knowledge-diaries", "knowledge-skills"]);
  expect(
    pathShown(["knowledge", "pages", "0f0f0f0f-1111-4222-8333-444455556666"], confluence),
  ).toBe(false);
  expect(pathShown(["work"], confluence)).toBe(true);
});
