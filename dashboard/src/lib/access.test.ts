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
  groupLock,
  lockReason,
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
  // stored row of `[admin]` is read as holding every app without listing
  // them.
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
      "artifacts",
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
    expect(accessNames(["tracker"])).toBe("Tracker");
    expect(accessNames(["tracker", "knowledge"])).toBe("Tracker or Knowledge");
    // THREE OR MORE AS THE ENGINE SAYS THEM, commas and "or" before the last.
    expect(accessNames(["tracker", "knowledge", "artifacts"])).toBe(
      "Tracker, Knowledge or Artifacts",
    );
    expect(needsWords("Work", ["tracker"])).toBe(
      "Work needs Tracker access. An admin can give it to you.",
    );
  });

  // A LOCK NAMING NO ACCESS SAYS ONLY THAT THERE IS NONE: it said "Sign in
  // to see this.", the 401's sentence, so a signed-in person a rule refused
  // naming nothing was told to sign in. Which refusal it was is
  // `authorityWords`', which is told what refused.
  test("a lock naming no access never asks a signed-in reader to sign in", () => {
    const said = needsWords("Reading one pass", []);
    expect(said).toBe("You don’t have access to this.");
    expect(said).not.toMatch(/sign in|admin|operator|token/i);
  });
});

// A WITHHELD QUESTION IS SAID, never drawn as a run that asked nothing: only
// a run with no question whose body the engine said it withheld is one.
test("a coding run's question is withheld only where the engine said so", () => {
  expect(questionWithheld({ body_withheld: true })).toBe(true);
  expect(questionWithheld({ question: "Which branch?", body_withheld: true })).toBe(false);
  expect(questionWithheld({})).toBe(false);
});

// ONE SENTENCE FOR A LOCK, and one rule for a group's: the reason every row
// shares, or nothing where a row is open or the rows want different access.
describe("a lock's words", () => {
  test("say what is missing, and nothing where nothing is", () => {
    expect(lockReason(["admin"])).toBe("Needs Admin access");
    expect(lockReason([])).toBeUndefined();
  });

  test("a group carries the lock only where every row shares one reason", () => {
    expect(groupLock(["Needs Admin access", "Needs Admin access"])).toBe("Needs Admin access");
    expect(groupLock(["Needs Admin access", undefined])).toBeUndefined();
    expect(groupLock(["Needs Admin access", "Needs Tracker access"])).toBeUndefined();
    expect(groupLock([])).toBeUndefined();
  });
});
