// @vitest-environment node

/**
 * What a brief is of, in the drawer's words: the open record's own word, its
 * kind's where the screen named none, and the page where nothing is open.
 */

import { describe, expect, test } from "vitest";
import type { AssistantPageContext } from "~/contract/assistant.ts";
import { briefQuestion, briefSubject, recordWord } from "./brief.ts";

const context = (selection: AssistantPageContext["selection"]): AssistantPageContext => ({
  screen: "inbox",
  title: "Inbox",
  params: {},
  query: {},
  resources: [],
  selection,
  source_revision: "inbox@1",
  accepts: [],
});

describe("a brief", () => {
  test("is of the page where nothing is open", () => {
    expect(briefSubject(context(null))).toBe("page");
    expect(briefQuestion(context(null))).toBe("Brief me on this page");
  });

  test("is of the open record, by the word the screen gave it", () => {
    expect(briefQuestion(context({ kind: "task", id: "ENG-4", noun: "decision" }))).toBe(
      "Brief me on this decision",
    );
    expect(briefQuestion(context({ kind: "run", id: "r1", noun: "run" }))).toBe(
      "Brief me on this coding run",
    );
    expect(briefQuestion(context({ kind: "room", id: "r2", noun: "room" }))).toBe(
      "Brief me on this room",
    );
  });

  test("names a record by its kind where the screen named no noun", () => {
    expect(recordWord({ kind: "seat", id: "qa" })).toBe("agent");
    expect(recordWord({ kind: "unit", id: "core" })).toBe("team");
    expect(recordWord({ kind: "container", id: "FAIR" })).toBe("space");
    expect(recordWord({ kind: "room", id: "r2" })).toBe("room");
  });
});
