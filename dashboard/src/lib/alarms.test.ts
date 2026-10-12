/**
 * An alarm in words: the kind read as words, its initialisms kept whole, and
 * the domain an alarm was raised on named after a colon.
 */

import { describe, expect, test } from "vitest";

import { alarmDomain, alarmKindTitle, alarmTitle, namedAlarms } from "./alarms.ts";

const DOMAINS = ["tracker", "pages", "vectors", "usage"];

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

describe("an alarm raised per domain names its domain", () => {
  test("the report's per-domain detail names the domain after the kind", () => {
    const alarm = {
      kind: "trim_blocked",
      detail: "tracker: the trim has not advanced for 3 days: backup_max_age",
    };
    expect(alarmTitle(alarm, DOMAINS)).toBe("Trim blocked: tracker");
    expect(
      alarmTitle({ kind: "log_headroom", detail: "pages: 4% of the ceiling left" }, DOMAINS),
    ).toBe("Log headroom: pages");
  });

  // RECOGNISED AGAINST THE REPORT'S OWN DOMAINS, never by the shape of the
  // text: a node-wide alarm's detail can start with a word and a colon too.
  test("a word before a colon is not a domain unless the report lists it", () => {
    expect(alarmDomain("/var/lib/crewlet: 3 GiB free of 40 GiB", DOMAINS)).toBeUndefined();
    expect(
      alarmTitle({ kind: "volume_low", detail: "/var/lib/crewlet: 3 GiB free" }, DOMAINS),
    ).toBe("Volume low");
    expect(alarmTitle({ kind: "trim_blocked", detail: "tracker: held" }, [])).toBe("Trim blocked");
    // A domain only matches whole, up to its colon.
    expect(alarmDomain("trackers: held", DOMAINS)).toBeUndefined();
    expect(alarmTitle({ kind: "backup_age" })).toBe("Backup age");
  });
});

describe("a list of alarms", () => {
  const report = [
    { kind: "trim_blocked", detail: "tracker: the trim has not advanced" },
    { kind: "trim_blocked", detail: "pages: the trim has not advanced" },
    { kind: "backup_age", detail: "no verified backup has been recorded" },
    { kind: "volume_low", detail: "/data: 1 GiB free" },
  ];

  test("leads with the one that has stood longest, and keeps the report's order after it", () => {
    expect(namedAlarms(report, DOMAINS, "backup_age").map((a) => a.title)).toEqual([
      "Backup age",
      "Trim blocked: tracker",
      "Trim blocked: pages",
      "Volume low",
    ]);
    // With nothing named as the longest-standing, the report's order stands.
    expect(namedAlarms(report, DOMAINS, undefined).map((a) => a.title)).toEqual([
      "Trim blocked: tracker",
      "Trim blocked: pages",
      "Backup age",
      "Volume low",
    ]);
  });

  // ONE KIND, TWO ALARMS: the kind alone is not an identity, so a list keyed
  // on it would draw one of the two.
  test("tells two alarms of one kind apart", () => {
    const keys = namedAlarms(report, DOMAINS, "trim_blocked").map((a) => a.key);
    expect(new Set(keys).size).toBe(report.length);
  });
});
