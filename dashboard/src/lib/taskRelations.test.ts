/**
 * A link drawn as the kind chosen, and taken off on the end that holds it.
 *
 * The failure modes: a dependency sent as a plain link (the blocker never
 * blocks); a link stated as the whole set (every other edge of that kind
 * dropped); a link taken off on the end that does not hold it (the engine
 * finds nothing to remove and the rail keeps drawing it); and a duplicate no
 * gesture could take off at all.
 */

import { describe, expect, test } from "vitest";
import { linkArgs, linkedSentence, unlinkChange, unlinkedSentence } from "./taskRelations.ts";

const self = { id: "id-1", address: "ENG-1" };

describe("linking", () => {
  test("each kind is a delta on its own argument, never the whole set", () => {
    expect(linkArgs("linked", "ENG-2")).toEqual({ linked: { add: ["ENG-2"] } });
    expect(linkArgs("blocking", "ENG-2")).toEqual({ blocking: { add: ["ENG-2"] } });
    expect(linkArgs("waiting_on", "ENG-2")).toEqual({ waiting_on: { add: ["ENG-2"] } });
  });

  test("a dependency carries why, and a plain link carries none", () => {
    expect(linkArgs("waiting_on", "ENG-2", " the keys rotate first ")).toEqual({
      waiting_on: { add: ["ENG-2"] },
      dependency_note: "the keys rotate first",
    });
    expect(linkArgs("linked", "ENG-2", "ignored")).toEqual({ linked: { add: ["ENG-2"] } });
    expect(linkArgs("blocking", "ENG-2", "  ")).toEqual({ blocking: { add: ["ENG-2"] } });
  });

  test("the sentence says which way the dependency runs", () => {
    expect(linkedSentence("blocking", "ENG-1", "ENG-2")).toBe("ENG-1 blocks ENG-2 now");
    expect(linkedSentence("waiting_on", "ENG-1", "ENG-2")).toBe("ENG-1 waits on ENG-2 now");
    expect(linkedSentence("linked", "ENG-1", "ENG-2")).toBe("Linked ENG-2 to ENG-1");
  });
});

describe("unlinking", () => {
  test("a dependency comes off this end, through the argument that names it", () => {
    expect(unlinkChange({ kind: "waiting_on", other: "id-2" }, self, "ENG-2")).toEqual({
      item: "ENG-1",
      args: { waiting_on: { remove: ["id-2"] } },
    });
    // THE MIRROR: this task blocks the other, which is `blocking` here.
    expect(
      unlinkChange({ kind: "waiting_on", other: "id-2", derived: true }, self, "ENG-2"),
    ).toEqual({ item: "ENG-1", args: { blocking: { remove: ["id-2"] } } });
  });

  test("a related link comes off the task that drew it", () => {
    expect(unlinkChange({ kind: "linked", other: "id-2" }, self, "ENG-2")).toEqual({
      item: "ENG-1",
      args: { linked: { remove: ["id-2"] } },
    });
    expect(unlinkChange({ kind: "linked", other: "id-2", derived: true }, self, "ENG-2")).toEqual({
      item: "ENG-2",
      args: { linked: { remove: ["id-1"] } },
    });
  });

  test("a duplicate link is cleared on the duplicate", () => {
    expect(unlinkChange({ kind: "duplicates", other: "id-2" }, self, "ENG-2")).toEqual({
      item: "ENG-1",
      args: { duplicate_of: null },
    });
    expect(
      unlinkChange({ kind: "duplicates", other: "id-2", derived: true }, self, "ENG-2"),
    ).toEqual({ item: "ENG-2", args: { duplicate_of: null } });
  });

  test("a page and an atlas entry are not taken off here", () => {
    expect(unlinkChange({ kind: "page", other: "p-1" }, self, "p-1")).toBeNull();
    expect(unlinkChange({ kind: "atlas_entry", other: "e-1" }, self, "e-1")).toBeNull();
  });

  test("the sentence is worded as the rail heads the link", () => {
    expect(unlinkedSentence({ kind: "waiting_on" }, "ENG-1", "ENG-2")).toBe(
      "ENG-1 no longer waits on ENG-2",
    );
    expect(unlinkedSentence({ kind: "waiting_on", derived: true }, "ENG-1", "ENG-2")).toBe(
      "ENG-1 no longer blocks ENG-2",
    );
    expect(unlinkedSentence({ kind: "duplicates", derived: true }, "ENG-1", "ENG-2")).toBe(
      "ENG-2 is no longer marked a duplicate of ENG-1",
    );
    expect(unlinkedSentence({ kind: "linked" }, "ENG-1", "ENG-2")).toBe("Unlinked ENG-2 from ENG-1");
  });
});
