// @vitest-environment node
/**
 * The look a new agent seat is suggested.
 *
 * What these protect: a new agent is offered a character no agent in the
 * draft already wears, counting a seat that chose nothing as the Crewlet it
 * is drawn as; its colour goes round the six as the company grows; a person
 * is never counted as wearing one; and once every character is worn the
 * suggestion still names one rather than nothing.
 */

import { describe, expect, test } from "vitest";
import { AVATAR_CHARACTERS, AVATAR_COLORS } from "~/contract/avatar.ts";
import type { CompanyDocument, ConfigRole } from "~/protocol/index.ts";
import { fromDocument } from "./document.ts";
import { suggestAvatar } from "./draft.ts";
import { fixtureDerived } from "./testkit.ts";

const draftOf = (roles: ConfigRole[]) => {
  const doc: CompanyDocument = { name: "Acme", roles };
  return fromDocument(doc, fixtureDerived(doc));
};

describe("suggestAvatar", () => {
  test("an empty company starts with the original Crewlet, in purple", () => {
    expect(suggestAvatar(draftOf([]))).toEqual({ character: "crewlet", color: "purple" });
  });

  test("offers the first character no agent wears, a seat that chose nothing wearing the Crewlet", () => {
    const draft = draftOf([
      { name: "CEO" },
      { name: "Dev", avatar: { character: "hexlet", color: "cyan" } },
    ]);
    expect(suggestAvatar(draft)).toEqual({ character: "peaklet", color: "green" });
  });

  test("a person wears no character and moves no colour along", () => {
    const draft = draftOf([
      { name: "Founder", kind: "human", contact: { slack_user_id: "U0FOUNDER" } },
      { name: "CEO", avatar: { character: "foxlet" } },
    ]);
    expect(suggestAvatar(draft)).toEqual({ character: "crewlet", color: "cyan" });
  });

  test("once every character is worn, still names one, going round in order", () => {
    const roles = AVATAR_CHARACTERS.map((character, i) => ({
      name: `Agent ${i}`,
      avatar: { character },
    }));
    roles.push({ name: "Agent 30", avatar: { character: "crewlet" } });
    const suggested = suggestAvatar(draftOf(roles));
    expect(suggested.character).toBe(AVATAR_CHARACTERS[31 % AVATAR_CHARACTERS.length]);
    expect(suggested.color).toBe(AVATAR_COLORS[31 % AVATAR_COLORS.length]);
  });
});
