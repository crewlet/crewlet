/**
 * My work is claims on a person's attention from more than one service: its
 * Worklet sections need the Worklet permission and a company running Worklet,
 * and Reviews needs the Gitlet permission (ADR-0110) and a company hosting its
 * code in Gitlet. So the row is offered wherever EITHER service runs, and it
 * lands where the reader can read something.
 *
 * The invariants: My work is no row for a company running neither service
 * and a row for one running either; a reader holding Gitlet without Worklet,
 * or holding Gitlet in a company with no Worklet, lands on Reviews rather
 * than on a locked or hidden Queue; a reader holding Worklet in a company
 * running it lands on the Queue as before; and the row is locked naming only
 * what would open a section the company offers — Worklet where Reviews is not
 * offered, Gitlet where Reviews is all there is, both where both are — so it
 * never opens onto a refusal.
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
test("My work is offered where Worklet or Gitlet runs, and nowhere else", () => {
  const me = workspaceRow("me")!;
  expect(workspaceShown(me, runs(true, false))).toBe(true);
  expect(workspaceShown(me, runs(false, true))).toBe(true);
  expect(workspaceShown(me, runs(false, false))).toBe(false);
  // A workspace with an unserviced section is offered whatever runs.
  expect(workspaceShown(workspaceRow("home")!, runs(false, false))).toBe(true);
});

// THE LANDING SKIPS A SECTION THE READER CANNOT OPEN OR THE COMPANY DOES NOT
// RUN. Mutation: land on the workspace's own path whatever service its
// section reads, or keep Worklet's grant on the row, and this fails; drop
// Reviews' Gitlet grant and the Worklet reader in a company with no Worklet
// is sent to Reviews, which the engine refuses them.
test("My work lands on Reviews for a Gitlet holder without Worklet, or in a company without Worklet", () => {
  const me = workspaceRow("me")!;
  expect(landingPath(me, ["worklet"], runs(true, true))).toEqual(["me"]);
  expect(landingPath(me, ["admin"], runs(true, true))).toEqual(["me"]);
  expect(landingPath(me, ["gitlet"], runs(true, true))).toEqual(["me", "reviews"]);
  expect(landingPath(me, ["worklet", "gitlet"], runs(false, true))).toEqual(["me", "reviews"]);
  expect(landingPath(me, ["worklet"], runs(false, true))).not.toEqual(["me", "reviews"]);
});

// A SECTION HIDDEN FOR ITS SERVICE OPENS NOTHING, AND ONE CLOSED TO THE
// READER OPENS NOTHING EITHER. Reviews needs Gitlet, so where the engine hosts
// code My work is open to a Gitlet holder without Worklet (landing on
// Reviews) and locked naming both apps to a reader holding neither; where it
// does not, Reviews is drawn nowhere, and the row is locked naming Worklet
// alone rather than an app that would open nothing; and where only Gitlet
// runs, it is locked naming Gitlet. Mutations: count every section toward the
// row's lock, whatever the company runs, and the Worklet-only lock names
// Gitlet too; drop Reviews' grant and the reader holding nothing is let in.
test("My work is locked naming what would open a section the company offers", () => {
  const me = workspaceRow("me")!;
  expect(workspaceOpen(me, ["gitlet"], runs(true, true))).toBe(true);
  expect(workspaceMissing(me, ["gitlet"], runs(true, true))).toEqual([]);
  expect(workspaceOpen(me, [], runs(true, true))).toBe(false);
  expect(workspaceMissing(me, [], runs(true, true))).toEqual(["worklet", "gitlet"]);
  expect(workspaceOpen(me, [], runs(true, false))).toBe(false);
  expect(workspaceMissing(me, [], runs(true, false))).toEqual(["worklet"]);
  expect(workspaceOpen(me, ["gitlet"], runs(true, false))).toBe(false);
  expect(workspaceMissing(me, ["gitlet"], runs(true, false))).toEqual(["worklet"]);
  expect(workspaceOpen(me, ["worklet"], runs(false, true))).toBe(false);
  expect(workspaceMissing(me, ["worklet"], runs(false, true))).toEqual(["gitlet"]);
  expect(workspaceOpen(me, ["worklet"], runs(true, false))).toBe(true);
});
