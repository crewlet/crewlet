/**
 * Settings › Code reads three answers the engine already serves and draws the
 * code host's part of each. The invariants worth breaking a build over: the
 * alarms it lists are the code host's kinds and no other, the longest-standing
 * first, and a node that evaluated no table is named apart rather than read
 * as quiet; and an unset policy field is drawn as the default it reads as,
 * said to be the default, never as blank.
 */

import { expect, test } from "vitest";
import type { CompanyDocument, FleetAnswer, FleetNode } from "~/protocol/index.ts";
import { codeAlarmsOf, codePolicyOf } from "./Code.tsx";

const node = (id: string, alarms?: FleetNode["alarms"]): FleetNode =>
  ({ id, roles: ["data", "git"], broker: "embedded", ...(alarms ? { alarms } : {}) }) as FleetNode;

const fleet = (nodes: FleetNode[]): FleetAnswer =>
  ({
    nodes,
    seats: [],
    duties: [],
    unplaceable: [],
    unmanned_roles: [],
    this_node: "a",
    target_epoch: 1,
  }) as FleetAnswer;

// THE CODE HOST'S ALARMS AND NO OTHER, the longest-standing first, and a node
// that evaluated no table is listed apart — never as a node with nothing
// firing. Mutation: keep a kind the code host does not raise, drop the sort,
// or count a node with no `alarms` as quiet, and this fails.
test("the alarms are the code host's kinds, oldest first, and a node that said nothing is named apart", () => {
  const got = codeAlarmsOf(
    fleet([
      node("git-1", [
        { kind: "apply_lag", since: "2026-10-30T08:00:00Z" },
        { kind: "code_commit_busy", since: "2026-10-31T09:00:00Z" },
      ]),
      node("git-2", [{ kind: "code_compaction_behind", since: "2026-10-31T07:00:00Z" }]),
      node("booting"),
      node("quiet", []),
    ]),
  );
  expect(got.firing).toEqual([
    { node: "git-2", kind: "code_compaction_behind", since: "2026-10-31T07:00:00Z" },
    { node: "git-1", kind: "code_commit_busy", since: "2026-10-31T09:00:00Z" },
  ]);
  expect(got.silent).toEqual(["booting"]);
});

// AN UNSET FIELD READS AS THE ENGINE'S DEFAULT, and says it is one; a set one
// is the document's value. Mutation: draw an unset field as zero or empty, or
// mark a set one as the default, and this fails.
test("an unset scratch policy and identity domain read as the engine's defaults, marked as defaults", () => {
  const unset = codePolicyOf({ code: { native: true } } as unknown as CompanyDocument);
  expect(unset.idleDays).toEqual({ value: 7, set: false });
  expect(unset.maxPerSeat).toEqual({ value: 50, set: false });
  expect(unset.identityDomain).toEqual({ value: "seats.invalid", set: false });

  const set = codePolicyOf({
    code: {
      native: true,
      scratch: { idle_days: 3, max_per_seat: 0 },
      identity_domain: "acme.example",
    },
  } as unknown as CompanyDocument);
  expect(set.idleDays).toEqual({ value: 3, set: true });
  expect(set.maxPerSeat).toEqual({ value: 0, set: true });
  expect(set.identityDomain).toEqual({ value: "acme.example", set: true });

  expect(codePolicyOf(null).idleDays).toEqual({ value: 7, set: false });
});
