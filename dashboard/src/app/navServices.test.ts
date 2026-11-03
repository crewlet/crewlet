/**
 * My work is claims on a person's attention from more than one service: its
 * Worklet sections need the Worklet permission and a company running Worklet,
 * and Reviews is the code
 * host's, which no app gates. So the row is offered wherever EITHER service
 * runs, and it lands where the reader can read something.
 *
 * The invariants: My work is no row for a company running neither service
 * and a row for one running either; a reader without Worklet, or a company
 * with no Worklet, lands on Reviews rather than on a locked or hidden
 * Queue; a reader holding Worklet on a company running it lands on the
 * Queue as before; and the row is locked for Worklet exactly where its
 * Reviews is not offered, so it never opens onto a refusal.
 */

import { expect, test } from "vitest";
import {
  landingPath,
  workspaceMissing,
  workspaceOpen,
  workspaceRow,
  workspaceShown,
  type ServicesRun,
} from "./nav.ts";

const runs = (tracker: boolean, code: boolean): ServicesRun => ({
  tracker,
  knowledge: true,
  chat: true,
  code,
});

// A WORKSPACE WHOSE EVERY SECTION NAMES A SERVICE IS OFFERED WHERE ONE OF
// THEM RUNS. Mutation: decide My work by its own service alone (it has none
// now), or by every section's rather than any, and this fails.
test("My work is offered where Worklet or the code host runs, and nowhere else", () => {
  const me = workspaceRow("me")!;
  expect(workspaceShown(me, runs(true, false))).toBe(true);
  expect(workspaceShown(me, runs(false, true))).toBe(true);
  expect(workspaceShown(me, runs(false, false))).toBe(false);
  // A workspace with an unserviced section is offered whatever runs.
  expect(workspaceShown(workspaceRow("home")!, runs(false, false))).toBe(true);
});

// THE LANDING SKIPS A SECTION THE READER CANNOT OPEN OR THE COMPANY DOES NOT
// RUN. Mutation: land on the workspace's own path whatever service its
// section reads, or keep Worklet's grant on the row, and this fails.
test("My work lands on Reviews for a reader without Worklet or a company without Worklet", () => {
  const me = workspaceRow("me")!;
  expect(landingPath(me, ["worklet"], runs(true, true))).toEqual(["me"]);
  expect(landingPath(me, [], runs(true, true))).toEqual(["me", "reviews"]);
  expect(landingPath(me, ["worklet"], runs(false, true))).toEqual(["me", "reviews"]);
});

// A SECTION HIDDEN FOR ITS SERVICE OPENS NOTHING. Reviews needs no
// permission, so where the engine hosts code My work is open to a reader
// without Worklet (landing on Reviews); where it does not, Reviews is drawn
// nowhere, and the row is locked naming Worklet rather than opening onto the
// Queue's refusal. Mutation: count every section toward the row's lock,
// whatever the company runs, and the second pair fails.
test("My work is locked for Worklet where its Reviews is not offered, and open where it is", () => {
  const me = workspaceRow("me")!;
  expect(workspaceOpen(me, [], runs(true, true))).toBe(true);
  expect(workspaceMissing(me, [], runs(true, true))).toEqual([]);
  expect(workspaceOpen(me, [], runs(true, false))).toBe(false);
  expect(workspaceMissing(me, [], runs(true, false))).toEqual(["worklet"]);
  expect(workspaceOpen(me, ["worklet"], runs(true, false))).toBe(true);
});
