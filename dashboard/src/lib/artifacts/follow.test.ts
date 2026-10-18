// @vitest-environment node
/** One entry a seat on the People rail, and what following somebody moves. */

import { describe, expect, test } from "vitest";
import type { ArtifactPresenceMember } from "~/protocol/index.ts";
import { bySeat, followMoves, followTarget, moves, type FollowView } from "./follow.ts";

function member(over: Partial<ArtifactPresenceMember>): ArtifactPresenceMember {
  return {
    session: "s-1",
    seat: "maya",
    kind: "human",
    activity: "viewing",
    focus: {},
    at: "2026-10-18T10:00:00Z",
    ...over,
  };
}

const page: FollowView = { kind: "html", head: 12, showing: 12, tab: "view", sheet: "" };

describe("the room, one entry a seat", () => {
  test("keeps a person's most active tab", () => {
    const got = bySeat([
      member({ session: "a", activity: "idle", at: "2026-10-18T10:05:00Z" }),
      member({ session: "b", activity: "editing", at: "2026-10-18T10:00:00Z" }),
      member({ session: "c", seat: "pm", kind: "agent" }),
    ]);
    expect(got.map((m) => m.session)).toEqual(["b", "c"]);
  });

  test("breaks a tie by the most recent", () => {
    const got = bySeat([
      member({ session: "a", at: "2026-10-18T10:00:00Z" }),
      member({ session: "b", at: "2026-10-18T10:01:00Z" }),
    ]);
    expect(got.map((m) => m.session)).toEqual(["b"]);
  });

  test("follows a seat while it is here, and nobody once it left", () => {
    const room = [member({ session: "a" }), member({ session: "z", seat: "pm" })];
    expect(followTarget(room, "maya")?.session).toBe("a");
    expect(followTarget(room, "jane")).toBeNull();
  });
});

describe("following somebody", () => {
  test("opens the version they have open", () => {
    expect(followMoves(member({ focus: { version: 7 } }), page)).toEqual({ version: "7" });
    expect(followMoves(member({ focus: { version: 12 } }), { ...page, showing: 7 })).toEqual({
      version: "",
    });
  });

  test("brings the reader back to the View tab to see it", () => {
    expect(followMoves(member({ focus: { version: 7 } }), { ...page, tab: "history" })).toEqual({
      version: "7",
      tab: "view",
    });
  });

  test("moves a sheet to the collection they are on", () => {
    const sheet: FollowView = { kind: "sheet", head: 1, showing: 1, tab: "data", sheet: "review" };
    expect(
      followMoves(member({ focus: { collection: "budget", doc: "r1", field: "amount" } }), sheet),
    ).toEqual({ tab: "view", sheet: "budget" });
  });

  test("moves nothing where the reader already is", () => {
    const got = followMoves(member({ focus: { version: 12, region: "Totals" } }), page);
    expect(moves(got)).toBe(false);
  });

  test("never opens a version past the head this reader knows", () => {
    expect(moves(followMoves(member({ focus: { version: 13 } }), page))).toBe(false);
  });
});
