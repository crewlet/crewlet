// @vitest-environment node
/**
 * Access as the dashboard reads it: admin, which includes every app, and one
 * permission per app.
 *
 * Every lock, picker and People row reads a set through these functions, so
 * a mistake here is a lock that opens for the wrong reader or a row that
 * draws an administrator as holding no app.
 */

import { describe, expect, test } from "vitest";

import { APPS, GRANTS } from "~/contract/identity.ts";
import {
  ACCESS_WORDS,
  accessLabel,
  accessLine,
  accessNames,
  expand,
  holds,
  holdsAny,
  needsWords,
  questionWithheld,
} from "./access.ts";

describe("expand", () => {
  // ADMIN INCLUDES EVERY APP, as the engine's `iam.Expand` reads it, so a
  // stored row of `[admin]` is read as holding the tracker, the knowledge
  // base and the chat without listing them.
  test("adds every app to a set holding admin, and nothing to one without", () => {
    expect(expand(["admin"])).toEqual(["admin", ...APPS]);
    expect(expand(["tracker"])).toEqual(["tracker"]);
    expect(expand([])).toEqual([]);
  });

  test("keeps a name it does not know, and adds no app twice", () => {
    expect(expand(["later", "admin", "knowledge"])).toEqual([
      "later",
      "admin",
      "knowledge",
      "tracker",
      "chat",
    ]);
  });

  test("is a fresh array that settles at once", () => {
    const set = ["admin"];
    const once = expand(set);
    expect(once).not.toBe(set);
    expect(set).toEqual(["admin"]);
    expect(expand(once)).toEqual(once);
  });
});

describe("holds", () => {
  test("counts every app an admin carries", () => {
    expect(holds(["admin"], "tracker")).toBe(true);
    expect(holds(["admin"], "knowledge")).toBe(true);
    expect(holds(["admin"], "chat")).toBe(true);
    expect(holds(["tracker"], "knowledge")).toBe(false);
    expect(holds(["tracker"], "chat")).toBe(false);
    expect(holds(["tracker"], "admin")).toBe(false);
  });

  test("any one of a lock's permissions opens it, and a lock naming none is open", () => {
    expect(holdsAny(["knowledge"], ["tracker", "knowledge"])).toBe(true);
    expect(holdsAny([], ["tracker"])).toBe(false);
    expect(holdsAny([], [])).toBe(true);
    expect(holdsAny([], undefined)).toBe(true);
  });
});

describe("the words", () => {
  // KEYED ON GRANTS, so a permission the engine grows is a compile error
  // until it has words; and the admin's sentence says it is host access,
  // because the picker is the last place to say so before it is given.
  test("every permission has a label and a sentence, and admin says host access", () => {
    expect(Object.keys(ACCESS_WORDS).sort()).toEqual([...GRANTS].sort());
    for (const grant of GRANTS) {
      expect(ACCESS_WORDS[grant].label).not.toBe("");
      expect(ACCESS_WORDS[grant].description).not.toBe("");
    }
    expect(ACCESS_WORDS.admin.description).toMatch(/host access/);
  });

  test("a set is said as Admin, its apps, or the overview", () => {
    expect(accessLine(["admin", "tracker", "knowledge"])).toBe("Admin");
    expect(accessLine(["knowledge", "tracker"])).toBe("Tracker · Knowledge");
    expect(accessLine(["chat", "tracker", "knowledge"])).toBe("Tracker · Knowledge · Chat");
    expect(accessLine(["tracker"])).toBe("Tracker");
    expect(accessLine([])).toBe("Overview only");
  });

  test("a name this build has no word for is said as itself, never dropped", () => {
    expect(accessLine(["tracker", "later"])).toBe("Tracker · later");
    expect(accessLine(["later"])).toBe("later");
    expect(accessLabel("later")).toBe("later");
  });

  test("a lock names its access and who can give it", () => {
    expect(accessNames(["tracker", "knowledge"])).toBe("Tracker or Knowledge");
    expect(needsWords("Work", ["tracker"])).toBe(
      "Work needs Tracker access. An admin can give it to you.",
    );
  });

  // NOTHING THE ENGINE ACCEPTED WAS PRESENTED — a 401 — so the repair is to
  // sign in, not to ask an admin for access.
  test("a refusal naming no access asks for a credential, not an admin", () => {
    const said = needsWords("Reading one pass", []);
    expect(said).toBe("Sign in to see this.");
    expect(said).not.toMatch(/admin|operator|token/);
  });
});

// A WITHHELD QUESTION IS SAID, never drawn as a run that asked nothing: only
// a run with no question whose body the engine said it withheld is one.
test("a coding run's question is withheld only where the engine said so", () => {
  expect(questionWithheld({ body_withheld: true })).toBe(true);
  expect(questionWithheld({ question: "Which branch?", body_withheld: true })).toBe(false);
  expect(questionWithheld({})).toBe(false);
});
