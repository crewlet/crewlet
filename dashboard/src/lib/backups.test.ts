import { describe, expect, test } from "vitest";
import type { BackupPointRow } from "~/contract/backups.ts";
import { RestError } from "~/protocol/rest.ts";
import { backupRefusal, coverWords, isAbsoluteDir, suggestDir, takenBytes } from "./backups.ts";

const point = (over: Partial<BackupPointRow>): BackupPointRow => ({
  owner: "node-a",
  kind: "node",
  taken_at: "2026-09-29T02:00:00Z",
  verified: true,
  covers: [],
  counted: true,
  newest: false,
  ...over,
});

describe("the directory a backup goes to", () => {
  // RESOLVED ON THE ENGINE'S HOST, so a relative path would land wherever the
  // engine's working directory is — somewhere nobody at this browser chose.
  test("only an absolute path is asked for", () => {
    expect(isAbsoluteDir("/var/backups/one")).toBe(true);
    expect(isAbsoluteDir("  /var/backups/one")).toBe(true);
    expect(isAbsoluteDir("backups/one")).toBe(false);
    expect(isAbsoluteDir("~/backups")).toBe(false);
    expect(isAbsoluteDir("")).toBe(false);
  });

  // BESIDE THIS NODE'S OWN LAST COPY, with a fresh leaf — and nothing at all
  // for a node that never backed up, or whose copy sat directly under `/`,
  // because choosing where a company's credentials go is the operator's
  // decision.
  test("the offer is a sibling of this node's newest copy, or nothing", () => {
    const now = new Date("2026-09-30T12:04:05Z");
    const points = [
      point({ owner: "node-b", dir: "/elsewhere/b" }),
      point({ owner: "node-a", dir: "/var/backups/crewlet-20260929-020000/" }),
    ];
    expect(suggestDir(points, "node-a", now)).toBe("/var/backups/crewlet-20260930-120405");
    expect(suggestDir(points, "node-c", now)).toBe("");
    expect(suggestDir([point({ owner: "node-a", dir: "/top" })], "node-a", now)).toBe("");
    expect(suggestDir([point({ owner: "node-a", dir: "/top/" })], "node-a", now)).toBe("");
    expect(suggestDir([point({ owner: "operator", kind: "operator" })], "operator", now)).toBe("");
  });

  // THE ENGINE REFUSES A DIRECTORY THAT IS NOT EMPTY, so reopening the dialog
  // in the second of the last copy — or after a request that failed and left
  // debris — must not offer a directory a backup already went to.
  test("the offer is never a directory this node was already asked for", () => {
    const now = new Date("2026-09-30T01:00:07Z");
    const last = "/var/backups/crewlet-20260930-010007";
    const points = [point({ owner: "node-a", dir: last })];
    expect(suggestDir(points, "node-a", now)).toBe(`${last}-2`);
    expect(suggestDir(points, "node-a", now, [`${last}-2/`, `${last}-3`])).toBe(`${last}-4`);
    expect(suggestDir(points, "node-a", new Date("2026-09-30T01:00:08Z"))).toBe(
      "/var/backups/crewlet-20260930-010008",
    );
  });
});

test("a taken backup's size is every copy and every snapshot", () => {
  expect(
    takenBytes({
      taken_at: "",
      finished_at: "",
      node_id: "node-a",
      stores: [{ bytes: 100 }, { bytes: 20 }],
      streams: [{ bytes: 3 }],
    }),
  ).toBe(123);
  expect(takenBytes({ taken_at: "", finished_at: "", node_id: "node-a" })).toBe(0);
});

describe("why a backup was not taken", () => {
  // THE ENGINE'S DETAIL ONLY FOR THE CALLER'S OWN MISTAKE, and it belongs on
  // the field: the thing to fix is the directory that was typed.
  test("a bad directory is the field's, in the engine's words", () => {
    const refusal = backupRefusal(
      new RestError(400, {
        error: "backup_failed",
        detail: "backup: unusable destination: not empty",
      }),
    );
    expect(refusal).toEqual({ field: true, message: "backup: unusable destination: not empty" });
  });

  test("an engine failure sends the reader to its log, and says the directory is not a backup", () => {
    const refusal = backupRefusal(new RestError(500, { error: "backup_failed" }));
    expect(refusal.field).toBe(false);
    expect(refusal.message).toMatch(/api_backup_failed/);
    expect(refusal.message).toMatch(/not a backup/);
  });

  test("a wait that ran out is not called a failure", () => {
    const refusal = backupRefusal(new RestError(0, { error: "unreachable" }));
    expect(refusal.message).toMatch(/may still finish/);
  });

  // A BACKUP ALREADY RUNNING IS NOT A FAILURE: nothing was written and the
  // engine's log has nothing wrong in it, so it is not sent there.
  test("a backup already running is said, and not as a failed copy", () => {
    const refusal = backupRefusal(new RestError(409, { error: "backup_running" }));
    expect(refusal.field).toBe(false);
    expect(refusal.message).toMatch(/already running/);
    expect(refusal.message).not.toMatch(/api_backup_failed/);
  });

  // AN OBJECT STORE THAT DID NOT ANSWER is the fleet's state, and the engine's
  // hint says what to check — a clause, said as a sentence of its own before
  // the one that follows it rather than run into it.
  test("an unreachable object store is said in the engine's hint", () => {
    const refusal = backupRefusal(
      new RestError(503, { error: "objects_unreachable", hint: "check the S3 bucket" }),
    );
    expect(refusal.field).toBe(false);
    expect(refusal.message).toMatch(/^Check the S3 bucket\. Whatever was written there /);
    expect(refusal.message).not.toMatch(/api_backup_failed/);
    // A hint already written as a sentence keeps its one full stop, and the
    // page's own sentence for none is followed by the next one.
    const stopped = backupRefusal(
      new RestError(503, { error: "objects_unreachable", hint: "Check the S3 bucket." }),
    );
    expect(stopped.message).toMatch(/^Check the S3 bucket\. Whatever was written there /);
    const bare = backupRefusal(new RestError(503, { error: "objects_unreachable" }));
    expect(bare.message).toMatch(/[^.]\. Whatever was written there /);
  });

  test("a refused credential says what to set", () => {
    expect(backupRefusal(new RestError(401, { error: "unauthorized" })).message).toMatch(
      /operator token/,
    );
  });
});

// A REACH IS A TRIPLE: a bare sequence from before a reanchor names a dead
// number space, so the generation is said with it.
test("a cover names its stream, its sequence and its generation", () => {
  expect(coverWords({ stream: "CREWLET_TRACKER_LOG", generation: 2, seq: 900 })).toBe(
    "CREWLET_TRACKER_LOG @900 (generation 2)",
  );
});
