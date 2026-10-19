// @vitest-environment node
/** Whether a change to an artifact may be made now, and the sentence it is held with. */

import { describe, expect, test } from "vitest";
import type { ViewerState } from "../viewer.ts";
import { artifactAccess, roleReason } from "./access.ts";

const PERSON: ViewerState = {
  login: "maya",
  grants: ["artifacts"],
  admin: false,
  worklet: false,
  doclet: false,
  chatlet: false,
  handle: "maya",
  owner: "maya",
  name: "Maya Ortiz",
  acts: [],
  project: "",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
  configWriter: false,
  configManagedBy: [],
  assistant: "disabled",
  assistantModel: null,
};

describe("a change to an artifact", () => {
  test("is held below the role it needs, with the role named", () => {
    const access = artifactAccess({ role: "editor", held: "contributor" }, PERSON, "open");
    expect(access).toMatchObject({ can: false, reason: roleReason("editor") });
    expect(artifactAccess({ role: "editor", held: "owner" }, PERSON, "open").can).toBe(true);
  });

  test("that only a person makes is held for an agent's session", () => {
    expect(
      artifactAccess(
        { person: true, role: "owner", held: "owner" },
        { ...PERSON, kind: "agent" },
        "open",
      ),
    ).toMatchObject({ can: false, reason: "Only a person can make this change." });
  });

  test("that needs the artifacts app is held without it, whatever the role", () => {
    expect(
      artifactAccess(
        { app: true, role: "editor", held: "owner" },
        { ...PERSON, grants: [] },
        "open",
      ).can,
    ).toBe(false);
  });

  test("that only an administrator makes is held for everybody else", () => {
    expect(artifactAccess({ admin: true }, PERSON, "open").can).toBe(false);
    expect(artifactAccess({ admin: true }, { ...PERSON, admin: true }, "open").can).toBe(true);
  });

  test("is held offline, while the reader is still being checked, and for nobody signed in", () => {
    expect(artifactAccess({}, PERSON, "offline")).toMatchObject({ can: false, block: "offline" });
    expect(artifactAccess({}, { ...PERSON, loading: true }, "open")).toMatchObject({
      can: false,
      block: "loading",
    });
    expect(artifactAccess({}, { ...PERSON, anonymous: true }, "open")).toMatchObject({
      can: false,
      block: "anonymous",
    });
  });
});
