/**
 * A room's lifecycle and record, as values: who may archive it, and the three
 * destructions' requests, each confirmed exactly as the engine checks.
 *
 * Mutation: let any member archive and the second case goes red; confirm a
 * horizon the engine does not spell (a null as "null") and the retention case
 * does; count an erase's duplicates and the erase case does.
 */

import { expect, test } from "vitest";

import type { ChatChannel } from "~/protocol/index.ts";
import {
  ARCHIVE_HELD,
  archivable,
  archiveArgs,
  archiveHold,
  eraseRequest,
  horizonConfirmation,
  keepDays,
  pruneRequest,
  retentionRequest,
} from "./roomLifecycle.ts";

const room = (over: Partial<ChatChannel> = {}): ChatChannel => ({
  v: 1,
  id: "ch-1",
  kind: "public",
  name: "launch",
  created_at: "2026-11-01T09:00:00Z",
  created_by: "ada",
  ...over,
});

const reader = (handle: string, admin = false, leads: string[] = []) => ({
  handle,
  admin,
  leads: new Map(leads.map((unit) => [unit, unit])),
});

test("its creator, a lead of its team or above, and an admin may archive a room", () => {
  expect(archiveHold(room(), reader("ada"))).toBe("");
  expect(archiveHold(room({ created_by: "lee" }), reader("ada", true))).toBe("");
  // THE LEAD SCOPE HOLDS THE UNIT AND EVERY UNIT INSIDE IT, so a lead of a
  // team above is in it too.
  expect(
    archiveHold(room({ kind: "unit", unit: "web", created_by: "" }), reader("ada", false, ["web"])),
  ).toBe("");
});

test("anybody else is held, in the engine's own words", () => {
  expect(archiveHold(room({ created_by: "lee" }), reader("ada"))).toBe(ARCHIVE_HELD);
  // LEADING A TEAM IS NOT LEADING A ROOM THAT IS NOT THE TEAM'S.
  expect(archiveHold(room({ created_by: "lee" }), reader("ada", false, ["web"]))).toBe(
    ARCHIVE_HELD,
  );
  // A CREDENTIAL BOUND TO NO SEAT made no room.
  expect(archiveHold(room({ created_by: "" }), reader(""))).toBe(ARCHIVE_HELD);
  expect(ARCHIVE_HELD).toContain(
    "whoever created it, the lead of its team or of a team above it, or an admin",
  );
});

test("a named room offers archiving and a direct conversation does not", () => {
  expect(archivable(room())).toBe(true);
  expect(archivable(room({ kind: "unit" }))).toBe(true);
  expect(archivable(room({ kind: "dm", name: undefined }))).toBe(false);
  expect(archivable(room({ kind: "group", name: undefined }))).toBe(false);
  expect(archiveArgs("ch-1", true)).toEqual({ channel: "ch-1", archived: true });
  expect(archiveArgs("ch-1", false)).toEqual({ channel: "ch-1", archived: false });
});

test("a horizon is confirmed as its days, 0 for ever, or company", () => {
  expect(horizonConfirmation(null)).toBe("company");
  expect(horizonConfirmation(0)).toBe("0");
  expect(horizonConfirmation(30)).toBe("30");
  expect(retentionRequest("ch/1", 30)).toEqual({
    method: "PUT",
    path: "/chatlet/channels/ch%2F1/retention",
    query: { confirm: "30" },
    body: { retention_days: 30 },
  });
  // THE BODY NAMES A NULL, never leaves it out: absent would read as the
  // company's horizon by accident rather than by choice.
  expect(retentionRequest("ch-1", null).body).toEqual({ retention_days: null });
  expect(retentionRequest("ch-1", null).query).toEqual({ confirm: "company" });
});

test("a prune repeats its cutoff, and an erase counts what it names once", () => {
  expect(pruneRequest("ch-1", "2026-11-01T00:00:00.000Z")).toEqual({
    method: "POST",
    path: "/chatlet/channels/ch-1/prune",
    query: { cutoff: "2026-11-01T00:00:00.000Z", confirm: "2026-11-01T00:00:00.000Z" },
  });
  expect(eraseRequest("ch-1", ["m1", "m2", "m1"], "  a pasted key  ")).toEqual({
    method: "POST",
    path: "/chatlet/channels/ch-1/erase",
    query: { confirm: "2" },
    body: { message_ids: ["m1", "m2"], reason: "a pasted key" },
  });
});

test("a typed horizon is a whole number of days from 1 to a century", () => {
  expect(keepDays("30")).toEqual({ days: 30 });
  expect(keepDays(" 7 ")).toEqual({ days: 7 });
  expect(keepDays("0")).toHaveProperty("problem");
  expect(keepDays("2.5")).toHaveProperty("problem");
  expect(keepDays("ten")).toHaveProperty("problem");
  expect(keepDays("36501")).toHaveProperty("problem");
});
