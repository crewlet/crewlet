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
// Mutation: read an absent answer as none, and the first case hides Worklet;
// read chat or the code host off another service, and its case goes red.
test("the engine's word decides what runs, and every service runs until it has said", () => {
  const every = {
    tracker: "worklet",
    knowledge: "doclet",
    chat: "chatlet",
    code: "native",
  } as const;
  expect(servicesRun(null)).toEqual({ tracker: true, knowledge: true, chat: true, code: true });
  expect(servicesRun(every)).toEqual({ tracker: true, knowledge: true, chat: true, code: true });
  expect(servicesRun({ ...every, tracker: "jira" })).toEqual({
    tracker: false,
    knowledge: true,
    chat: true,
    code: true,
  });
  expect(servicesRun({ ...every, knowledge: "confluence" })).toEqual({
    tracker: true,
    knowledge: false,
    chat: true,
    code: true,
  });
  expect(servicesRun({ ...every, chat: "vendor" })).toEqual({
    tracker: true,
    knowledge: true,
    chat: false,
    code: true,
  });
  // THE CODE HOST IS ITS OWN AXIS, read off its own field and no other.
  expect(servicesRun({ ...every, code: "none" })).toEqual({
    tracker: true,
    knowledge: true,
    chat: true,
    code: false,
  });
  // ONE OBJECT PER ANSWER, so a memo keyed on it moves only when it changes.
  const bare = { tracker: "jira", knowledge: "none", chat: "chatlet", code: "none" } as const;
  expect(servicesRun(bare)).toBe(
    servicesRun({ ...bare, tracker: "none", knowledge: "confluence" }),
  );
  expect(servicesRun(bare)).not.toBe(servicesRun({ ...bare, code: "native" }));
});

// HIDDEN, NOT LOCKED, where the engine hosts no code: no Code row, no Code
// destination and no address under it, and nothing else of the company goes
// with it. Mutation: drop `service` from Code's row, and it is offered.
test("a company hosting no code in the engine is offered no Code", () => {
  const none = { tracker: true, knowledge: true, chat: true, code: false } as const;
  const rows = WORKSPACES.filter((w) => w.place !== "menu" && serviceShown(w, none)).map(
    (w) => w.key,
  );
  expect(rows).not.toContain("code");
  expect(rows).toEqual(expect.arrayContaining(["worklet", "chatlet", "doclet"]));
  const places = DESTINATIONS.filter((d) => !serviceShown(d, none)).map((d) => d.key);
  expect(places.sort()).toEqual(["code", "code-pushes", "code-repos"]);
  expect(serviceOfPath(["code", "pushes"])).toBe("code");
  expect(pathShown(["code", "repos"], none)).toBe(false);
  expect(pathShown(["worklet"], none)).toBe(true);
});

// HIDDEN, NOT LOCKED: a company on another tracker is offered no Worklet, no My
// work and no way to its tasks, and keeps the Inbox, whose seat and budget
// conditions arrive whatever runs the tracker.
test("a company on another tracker is offered nothing of Worklet", () => {
  const jira = { tracker: false, knowledge: true, chat: true, code: true } as const;
  const rows = WORKSPACES.filter((w) => w.place !== "menu" && serviceShown(w, jira)).map(
    (w) => w.key,
  );
  expect(rows).not.toContain("worklet");
  expect(rows).not.toContain("me");
  expect(rows).toContain("inbox");
  expect(rows).toContain("doclet");
  const places = DESTINATIONS.filter((d) => !serviceShown(d, jira)).map((d) => d.key);
  expect(places).toEqual(
    expect.arrayContaining([
      "me",
      "worklet",
      "worklet-tasks",
      "settings-spend-tasks",
      "artifacts-new-design",
    ]),
  );
  // AN ADDRESS UNDER ONE IS NOT OFFERED EITHER, an item's included.
  expect(pathShown(["worklet", "ENG-42"], jira)).toBe(false);
  expect(serviceOfPath(["me", "asked-of-me"])).toBe("tracker");
  expect(pathShown(["inbox"], jira)).toBe(true);
  // NEW DESIGN'S ONE WAY TO FINISH IS AN AGENT, which files a task.
  expect(offeredKinds(jira)).toEqual(MAKE_ORDER.filter((k) => k !== "design"));
  expect(offeredKinds({ tracker: true, knowledge: true, chat: true, code: true })).toEqual([
    ...MAKE_ORDER,
  ]);
});

test("a company on another knowledge base is offered no Doclet", () => {
  const confluence = { tracker: true, knowledge: false, chat: true, code: true } as const;
  const places = DESTINATIONS.filter((d) => !serviceShown(d, confluence)).map((d) => d.key);
  expect(places.sort()).toEqual(["doclet", "doclet-diaries", "doclet-skills"]);
  expect(pathShown(["doclet", "pages", "0f0f0f0f-1111-4222-8333-444455556666"], confluence)).toBe(
    false,
  );
  expect(pathShown(["worklet"], confluence)).toBe(true);
});

// A COMPANY TALKING ON A VENDOR'S CHAT, or on none, is offered no Chatlet at all:
// no row, no section, no address and no Home widget's page, while its rooms
// would be refused anyway. Mutation: drop Chatlet's `service` from its row and
// every expectation here fails.
test("a company on another chat is offered nothing of Chatlet", () => {
  const elsewhere = { tracker: true, knowledge: true, chat: false, code: true } as const;
  const rows = WORKSPACES.filter((w) => w.place !== "menu" && serviceShown(w, elsewhere)).map(
    (w) => w.key,
  );
  expect(rows).not.toContain("chatlet");
  expect(rows).toContain("worklet");
  const places = DESTINATIONS.filter((d) => !serviceShown(d, elsewhere)).map((d) => d.key);
  // THE ROW AND EVERY VIEW IT DECLARES, each a place of its own, Activity
  // being the row's own: Chatlet opens on it.
  expect(places.sort()).toEqual([
    "chatlet",
    ...["browse", "drafts", "new", "saved", "search", "threads"].map((v) => `chatlet-${v}`),
  ]);
  expect(serviceOfPath(["chatlet", "0f0f0f0f-1111-4222-8333-444455556666"])).toBe("chat");
  expect(pathShown(["chatlet", "activity"], elsewhere)).toBe(false);
  expect(pathShown(["doclet"], elsewhere)).toBe(true);
});
