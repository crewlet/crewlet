/**
 * An alarm in words: the kind read as words with its initialisms kept whole,
 * one name per kind, and the logs an alarm stands on as the lines a screen
 * draws.
 */

import { describe, expect, test } from "vitest";

import { alarmKindTitle, alarmLogLines, namedAlarms } from "./alarms.ts";

describe("an alarm kind is read as words", () => {
  // EVERY KIND THE ENGINE'S TABLE RAISES TODAY (`statelog.Kinds`), so the rule
  // is held to the whole vocabulary rather than to the two it was written
  // for. A kind the engine adds later is still named by the rule; this list
  // is what says the rule reads every one of today's well.
  test.each([
    ["apply_lag", "Apply lag"],
    ["read_refusals", "Read refusals"],
    ["barrier_slow", "Barrier slow"],
    ["log_headroom", "Log headroom"],
    ["backup_age", "Backup age"],
    ["trim_blocked", "Trim blocked"],
    ["deferred_old", "Deferred old"],
    ["floor_unknown", "Floor unknown"],
    ["prefetch_slow", "Prefetch slow"],
    ["search_slow", "Search slow"],
    ["search_degraded", "Search degraded"],
    ["search_scoped", "Search scoped"],
    ["history_partial", "History partial"],
    ["recall_below_floor", "Recall below floor"],
    ["ivf_recall_below_floor", "IVF recall below floor"],
    ["records_gated", "Records gated"],
    ["feed_unreadable", "Feed unreadable"],
    ["maintenance_open", "Maintenance open"],
    ["volume_low", "Volume low"],
    ["wal_large", "WAL large"],
    ["pool_starved", "Pool starved"],
    ["census_drift", "Census drift"],
    ["objects_missing", "Objects missing"],
  ])("%s is %s", (kind, title) => {
    expect(alarmKindTitle(kind)).toBe(title);
  });

  test("a kind this build has never seen is still named by the rule", () => {
    expect(alarmKindTitle("shard_skew")).toBe("Shard skew");
  });
});

describe("a list of alarms", () => {
  const report = [{ kind: "trim_blocked" }, { kind: "backup_age" }, { kind: "volume_low" }];

  test("leads with the one that has stood longest, and keeps the report's order after it", () => {
    expect(namedAlarms(report, "backup_age").map((a) => a.title)).toEqual([
      "Backup age",
      "Trim blocked",
      "Volume low",
    ]);
    // With nothing named as the longest-standing, the report's order stands.
    expect(namedAlarms(report, undefined).map((a) => a.title)).toEqual([
      "Trim blocked",
      "Backup age",
      "Volume low",
    ]);
  });

  // THE KIND IS THE IDENTITY: the engine raises each one once, with its logs
  // in it, so a list keyed on it draws every alarm it was given.
  test("keys each alarm by its kind", () => {
    expect(namedAlarms(report, undefined).map((a) => a.key)).toEqual([
      "trim_blocked",
      "backup_age",
      "volume_low",
    ]);
  });
});

describe("the logs an alarm stands on", () => {
  /** The engine's `trim_blocked` sentence for a log keeping `n` records past its window. */
  const kept = (n: number) =>
    `the trim has not advanced for 46h, and the log is keeping up to ${n} record(s) older than its 24h replay window`;

  // LOGS ONE TERM HOLDS ARE ONE LINE: four logs waiting on one backup are one
  // fact with four names in it, dated by the one held longest.
  test("logs held by one term are one line, from the earliest hold", () => {
    const lines = alarmLogLines({
      domains: [
        {
          domains: ["tracker"],
          detail: kept(1200),
          term: "backup_floor",
          since: "2031-04-02T01:00:00Z",
        },
        {
          domains: ["vectors"],
          detail: kept(40),
          term: "backup_floor",
          since: "2031-04-01T22:00:00Z",
        },
        {
          domains: ["pages"],
          detail: kept(7),
          term: "backup_floor",
          since: "2031-04-02T02:00:00Z",
        },
      ],
    });
    expect(lines).toEqual([
      {
        term: "backup_floor",
        domains: ["tracker", "vectors", "pages"],
        since: "2031-04-01T22:00:00Z",
        // EACH LOG KEEPS ITS OWN MEASUREMENT on the shared line, in the
        // report's order: what it keeps past its window is no date's to say.
        logs: [
          { domain: "tracker", detail: kept(1200) },
          { domain: "vectors", detail: kept(40) },
          { domain: "pages", detail: kept(7) },
        ],
      },
    ]);
  });

  // DIFFERENT TERMS ARE DIFFERENT FACTS, each with its own line.
  test("logs held by different terms keep a line each, in the report's order", () => {
    const lines = alarmLogLines({
      domains: [
        { domains: ["tracker"], detail: "", term: "applied", since: "2031-04-02T01:00:00Z" },
        { domains: ["pages"], detail: "", term: "feed_ack_floor", since: "2031-04-02T03:00:00Z" },
        { domains: ["usage"], detail: "", term: "applied", since: "2031-04-02T00:00:00Z" },
      ],
    });
    expect(lines.map((l) => [l.term, l.domains])).toEqual([
      ["applied", ["tracker", "usage"]],
      ["feed_ack_floor", ["pages"]],
    ]);
    expect(lines.map((l) => l.logs?.map((m) => m.domain))).toEqual([
      ["tracker", "usage"],
      ["pages"],
    ]);
  });

  // A LOG WITH A MEASUREMENT OF ITS OWN AND NO TERM (a log nearing its
  // ceiling) is its own line, carrying that measurement.
  test("a log with its own measurement is a line of its own", () => {
    expect(
      alarmLogLines({
        domains: [
          { domains: ["vectors"], detail: "3% of the log's byte ceiling is left" },
          { domains: ["usage"], detail: "8% of the log's byte ceiling is left" },
        ],
      }),
    ).toEqual([
      { domains: ["vectors"], detail: "3% of the log's byte ceiling is left" },
      { domains: ["usage"], detail: "8% of the log's byte ceiling is left" },
    ]);
    // A node-wide alarm stands on no log at all.
    expect(alarmLogLines({})).toEqual([]);
  });

  // A MEASUREMENT THE ENGINE TOOK ALIKE ON SEVERAL LOGS is one line naming
  // them, as it arrives: a young fleet's missing backup, on every log, was a
  // line per log under a headline that already said it.
  test("a measurement carried for several logs is one line naming them", () => {
    const missing = "no verified backup of this log has been recorded";
    expect(
      alarmLogLines({
        domains: [
          { domains: ["tracker", "pages", "usage"], detail: missing },
          { domains: ["iam"], detail: "the newest verified backup of this log is 30h old" },
        ],
      }),
    ).toEqual([
      { domains: ["tracker", "pages", "usage"], detail: missing },
      { domains: ["iam"], detail: "the newest verified backup of this log is 30h old" },
    ]);
  });
});
