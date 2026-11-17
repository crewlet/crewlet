// @vitest-environment node
import { describe, expect, test } from "vitest";
import type { CodeRepoCard, CodeRepoCounts } from "~/protocol/index.ts";
import { kindCount, lastPushWords, listingParams, listingStatus, sortHeld } from "./repoListing.ts";

// The live ones are the active, the archived and the importing, so `active` alone tells them apart.
const counted = (live: number, extra: Partial<CodeRepoCounts> = {}): CodeRepoCounts => ({
  live,
  importing: 0,
  active: live - (extra.archived ?? 0) - (extra.importing ?? 0),
  archived: 0,
  deleted: 0,
  purging: 0,
  ...extra,
});
const COUNTS = { project: counted(4, { archived: 2, deleted: 1 }), scratch: counted(1) };

describe("the kind counts", () => {
  // Mutation: read the default state as `active` rather than `live`, and the archived drop out.
  test("are the live ones by default, each state its own, both kinds summed for All", () => {
    expect(kindCount(COUNTS, "", "")).toBe(5);
    expect(kindCount(COUNTS, "project", "")).toBe(4);
    expect(kindCount(COUNTS, "project", "archived")).toBe(2);
    expect(kindCount(COUNTS, "project", "active")).toBe(2);
    expect(kindCount(COUNTS, "", "deleted")).toBe(1);
    expect(kindCount(COUNTS, "scratch", "purging")).toBe(0);
  });

  // Mutation: answer 0 for a state or a count not read, and a figure is drawn that nobody counted.
  test("are no figure where not counted or the state is unknown", () => {
    expect(kindCount(undefined, "", "")).toBeUndefined();
    expect(kindCount(COUNTS, "project", "frozen")).toBeUndefined();
  });
});

describe("the listing's question", () => {
  const BOUNDS = ["2026-11-03T00:00:00Z", "2026-11-04T00:00:00Z"];

  // Mutation: send `sort=pushed` beside a deleted state, and the engine refuses the listing.
  test("is by last push, held to name for the deleted and the purging", () => {
    expect(listingParams("", "", "", BOUNDS)).toEqual({
      sort: "pushed",
      repo_facts: true,
      day_bounds: BOUNDS.join(","),
    });
    expect(listingParams("archived", "project", "name", BOUNDS)).toMatchObject({
      status: "archived",
      kind: "project",
      sort: "name",
    });
    for (const status of ["deleted", "purging"]) {
      expect(sortHeld(status)).toBe(true);
      expect(listingParams(status, "", "", BOUNDS).sort).toBe("name");
    }
    expect(sortHeld("archived")).toBe(false);
  });

  // Mutation: send a limit, or a kind the engine does not know, and a page is refused.
  test("sends no limit and no kind this build does not know", () => {
    const params = listingParams("", "bogus", "bogus", BOUNDS);
    expect(params).not.toHaveProperty("limit");
    expect(params).not.toHaveProperty("kind");
    expect(params.sort).toBe("pushed");
  });

  // Mutation: send a state this build does not know, and the engine refuses the whole listing.
  test("sends no state this build does not know, asking for the live ones instead", () => {
    expect(listingStatus("bogus")).toBe("");
    expect(listingStatus("archived")).toBe("archived");
    expect(listingParams("bogus", "", "name", BOUNDS)).not.toHaveProperty("status");
  });
});

describe("the last push in words", () => {
  const NOW = Date.parse("2026-11-17T12:00:00Z");
  const push = (outcome: Record<string, number>): CodeRepoCard => ({
    last_push: {
      push_id: "p1",
      at: "2026-11-17T10:00:00Z",
      actor: "backend",
      actor_kind: "agent",
      actor_seat: "backend",
      via: "git",
      outcome,
    },
  });

  test.each([
    ["applied", push({ moved: 1 }), "2026-11-17T10:00:00Z"],
    ["applied", push({ moved: 1, refused: 1 }), "2026-11-17T10:00:00Z"],
    ["refused", push({ refused: 2 }), "2026-11-17T10:00:00Z"],
    ["refused", push({ unchanged: 1, refused: 1 }), "2026-11-17T10:00:00Z"],
  ])("say %s for a push whose commands came to %j", (kind, card) => {
    expect(lastPushWords(card, "2026-11-17T10:00:00Z", NOW).kind).toBe(kind);
  });

  // Mutation: guess a pusher where the card holds none, and a name is said nobody answered.
  test("say why who pushed is not said, or that nobody did", () => {
    const old = lastPushWords({}, "2026-05-01T00:00:00Z", NOW);
    expect(old).toMatchObject({ kind: "unknown", title: expect.stringContaining("181 days") });
    const recent = lastPushWords({}, "2026-11-14T00:00:00Z", NOW);
    expect(recent).toMatchObject({ kind: "unknown", title: expect.stringContaining("expiries") });
    expect(lastPushWords(undefined, "2026-11-14T00:00:00Z", NOW)).toEqual({
      kind: "bare",
      at: "2026-11-14T00:00:00Z",
    });
    expect(lastPushWords({}, undefined, NOW)).toEqual({ kind: "never" });
  });
});
