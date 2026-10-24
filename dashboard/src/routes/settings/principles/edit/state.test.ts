/**
 * The editing state: a part keeps what was typed until it is saved or
 * discarded, a conflict is answered only by the person, a failure by their
 * next keystroke, and a saved part is shown saved until the page has it.
 */

import { describe, expect, test } from "vitest";
import {
  INITIAL,
  effective,
  hasSettled,
  isDirty,
  reducer,
  type Action,
  type EditingState,
} from "./state.ts";

const run = (...actions: Action[]): EditingState => actions.reduce(reducer, INITIAL);
const open: Action = {
  type: "open",
  part: "mission",
  base: "Ship it",
  origin: "edit",
  focus: true,
};

describe("reducer", () => {
  test("opens a part with its draft at the base", () => {
    const session = run(open).sessions.mission!;
    expect(session).toMatchObject({
      base: "Ship it",
      draft: "Ship it",
      phase: { kind: "editing" },
    });
    expect(isDirty(session)).toBe(false);
  });

  // A SECOND PRESS, or the address arriving again, never loses the typing.
  test("opening an open part keeps its draft", () => {
    const state = run(open, { type: "change", part: "mission", draft: "Typed" }, open);
    expect(state.sessions.mission!.draft).toBe("Typed");
  });

  test("parts are edited independently", () => {
    const state = run(
      open,
      { type: "open", part: "policies", base: ["a"], origin: "add", focus: false },
      { type: "close", part: "mission" },
    );
    expect(state.sessions.mission).toBeUndefined();
    expect(state.sessions.policies).toBeDefined();
  });

  test("a keystroke answers a failure and clears what the engine refused", () => {
    const state = run(
      open,
      { type: "failed", part: "mission", message: "No.", refused: { field: "Too long." } },
      { type: "change", part: "mission", draft: "Shorter" },
    );
    expect(state.sessions.mission).toMatchObject({ phase: { kind: "editing" }, refused: null });
  });

  // A CONFLICT IS A QUESTION ABOUT WHOSE WORDS STAND, and typing on does not
  // answer it.
  test("a keystroke does not answer a conflict", () => {
    const state = run(
      open,
      { type: "conflict", part: "mission", theirs: "Theirs" },
      { type: "change", part: "mission", draft: "Mine" },
    );
    expect(state.sessions.mission!.phase).toEqual({ kind: "conflict", theirs: "Theirs" });
  });

  test("keeping the draft makes their words the base, so the next save writes over them", () => {
    const state = run(
      open,
      { type: "change", part: "mission", draft: "Mine" },
      { type: "conflict", part: "mission", theirs: "Theirs" },
      { type: "keep_mine", part: "mission" },
    );
    expect(state.sessions.mission).toMatchObject({
      base: "Theirs",
      draft: "Mine",
      phase: { kind: "editing" },
    });
  });

  test("using theirs closes the part and shows their words until the page has them", () => {
    const state = run(
      open,
      { type: "conflict", part: "mission", theirs: "Theirs" },
      { type: "use_theirs", part: "mission", shownWas: "Ship it" },
    );
    expect(state.sessions.mission).toBeUndefined();
    expect(state.landed.mission).toEqual({ value: "Theirs", epoch: null, shownWas: "Ship it" });
  });

  test("nothing is typed while a save is in flight", () => {
    const state = run(
      open,
      { type: "saving", part: "mission" },
      {
        type: "change",
        part: "mission",
        draft: "Late",
      },
    );
    expect(state.sessions.mission!.draft).toBe("Ship it");
  });

  test("a save closes the part and leaves what it saved to show", () => {
    const state = run(
      open,
      { type: "change", part: "mission", draft: "New" },
      { type: "saving", part: "mission" },
      { type: "saved", part: "mission", value: "New", epoch: 4, shownWas: "Ship it" },
    );
    expect(state.sessions.mission).toBeUndefined();
    expect(state.landed.mission).toEqual({ value: "New", epoch: 4, shownWas: "Ship it" });
    expect(reducer(state, { type: "settle", part: "mission" }).landed.mission).toBeUndefined();
  });
});

describe("the new policy box", () => {
  const policies: Action = {
    type: "open",
    part: "policies",
    base: ["a"],
    origin: "edit",
    focus: true,
  };

  // A POLICY TYPED AND NOT YET ADDED is the person's writing: losing it to a
  // Save pressed before Enter would drop it without a word.
  test("is part of the draft and makes it a change", () => {
    const state = run(policies, { type: "pending", part: "policies", text: " b " });
    const session = state.sessions.policies!;
    expect(isDirty(session)).toBe(true);
    expect(effective(session)).toEqual(["a", " b "]);
  });

  test("adds nothing while it is blank", () => {
    const state = run(policies, { type: "pending", part: "policies", text: "   " });
    expect(isDirty(state.sessions.policies)).toBe(false);
  });
});

describe("seeding from the engine's charter", () => {
  test("an untouched draft starts from the engine's words", () => {
    const state = run(open, { type: "seed", part: "mission", value: "Newer" });
    expect(state.sessions.mission).toMatchObject({ base: "Newer", draft: "Newer", seeded: true });
  });

  // ONCE THE PERSON HAS TYPED, what they started from stays the base, so a
  // save that finds the engine elsewhere asks them rather than moving it.
  test("a touched draft keeps what the person started from", () => {
    const state = run(
      open,
      { type: "change", part: "mission", draft: "Typed" },
      { type: "seed", part: "mission", value: "Newer" },
    );
    expect(state.sessions.mission).toMatchObject({ base: "Ship it", draft: "Typed" });
  });

  test("only once", () => {
    const state = run(
      open,
      { type: "seed", part: "mission", value: "Newer" },
      { type: "seed", part: "mission", value: "Newest" },
    );
    expect(state.sessions.mission!.base).toBe("Newer");
  });
});

describe("hasSettled", () => {
  const landed = { value: "New", epoch: 4, shownWas: "Old" };

  test("not while the page still shows what the save replaced", () => {
    expect(hasSettled(landed, "Old", 3)).toBe(false);
  });

  test("once the page shows the saved words", () => {
    expect(hasSettled(landed, "New", 3)).toBe(true);
  });

  // A LATER CHANGE BY SOMEBODY ELSE is the page's to show, not this save's
  // words over it.
  test("once the page shows anything newer than it did", () => {
    expect(hasSettled(landed, "A colleague's", 3)).toBe(true);
  });

  test("once this node has applied the save's epoch", () => {
    expect(hasSettled(landed, "Old", 4)).toBe(true);
    expect(hasSettled({ ...landed, epoch: null }, "Old", 99)).toBe(false);
  });
});
