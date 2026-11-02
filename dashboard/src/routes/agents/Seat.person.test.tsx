/**
 * Whose queue this screen asks for, and on whose authority.
 *
 * Somebody's work record — the claims on their plate, their inbox, the order
 * they mean to work in — is the TRACKER's, and the engine answers it to a
 * Worklet holder who owns it or leads them, and to an admin, who holds every
 * app. The screen asks on exactly that rule, the app first: Worklet alone
 * asks nothing of somebody the reader does not lead, a lead asks once they
 * hold Worklet, and a lead without it asks nothing at all: every one of
 * those questions is Worklet's, so leading somebody opens none of them.
 *
 * Asserted on the QUESTION, not on rows: what this screen is responsible for is
 * whether it asks at all.
 */

import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { emptyDay } from "~/test/myWork.ts";
import { settled } from "~/test/settle.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/company/org/seats/ceo?tab=work";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

/** What the engine answers the profile's other questions, in the wire's shapes. */
const ANSWERS: Record<string, unknown> = {
  work_items: { items: [], total_hint: 0 },
  inbox: { handle: "ana", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  seat_activity: { since: "", until: "", days: 7, seats: [], quantile_resolution: 0.06 },
  turns: { turns: [], next: null },
  work_my_work: emptyDay("ceo"),
};

/** One seat as the engine's derived hierarchy states it. */
const derivedSeat = (handle: string, name: string, reports: string[], managers: string[]) => ({
  handle,
  name,
  kind: "human",
  placed_by_ref: false,
  manager: managers[0] ?? "",
  managers: managers.length ? managers : null,
  reports: reports.length ? reports : null,
  auto_reports: null,
  onboarding_chain: null,
});

/**
 * The company, with the engine's own hierarchy: Ana and the CEO, and Ana
 * leading the CEO only where `anaLeads` says so — every reporting line is
 * KNOWN either way, so "does not lead them" is the chart's answer rather
 * than an absence of one.
 */
const company = (anaLeads: boolean) => ({
  roles: [
    { name: "Ana", handle: "ana", kind: "human" },
    { name: "CEO", handle: "ceo", kind: "human" },
  ],
  units: [],
  derived: {
    units: [],
    seats: [
      derivedSeat("ana", "Ana", anaLeads ? ["ceo"] : [], []),
      derivedSeat("ceo", "CEO", [], anaLeads ? ["ana"] : []),
    ],
  },
});

/**
 * askedAs renders a colleague's seat for a reader bound to `ana` holding
 * exactly these grants, and reports every question the screen asked.
 */
async function askedAs(grants: string[], anaLeads = false): Promise<string[]> {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    if (what === "viewer") {
      return Promise.resolve({
        login: "ana.lee",
        grants,
        handle: "ana",
        name: "Ana",
        kind: "human",
        owner: "ana",
        acts: [],
      });
    }
    return Promise.resolve(ANSWERS[what] ?? {});
  };
  store.applyOrg(company(anaLeads));
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ViewerProvider>
          <InboxCountsProvider>
            <SeatScreen handle="ceo" />
          </InboxCountsProvider>
        </ViewerProvider>
      </Router>
    </ClientContext.Provider>,
  );
  // THE VIEWER HAS ANSWERED and (for a Worklet holder) the work tab has
  // asked for its board, so whatever the screen was going to ask on the
  // viewer's authority it has.
  await waitFor(() => expect(asked).toContain("viewer"));
  if (grants.includes("worklet") || grants.includes("admin")) {
    await waitFor(() => expect(asked).toContain("work_items"));
  }
  // WHAT IT DID NOT ASK is read once it asks nothing more: every answer is a
  // stub, so a pass that asked again would move the count across a boundary.
  await settled(() => asked.length);
  return asked;
}

test("Worklet alone does not ask for a colleague's queue", async () => {
  const asked = await askedAs(["worklet"]);
  expect(
    asked,
    "a colleague's record is theirs, their lead's and an admin's to read",
  ).not.toContain("work_my_work");
});

test("Admin alone does", async () => {
  const asked = await askedAs(["admin"]);
  await waitFor(() => expect(asked).toContain("work_my_work"));
});

// A LEAD IS ANSWERED A REPORT'S RECORD, and asked for it: the engine's rule is
// owner, lead or Admin, and a page that asked on Admin and the owner alone
// told a lead the record needs "Admin, or leading them" about somebody they
// lead.
test("a lead holding Worklet asks for a report's queue", async () => {
  const asked = await askedAs(["worklet"], true);
  await waitFor(() => expect(asked).toContain("work_my_work"));
});

// THE APP COMES FIRST: the record is Worklet's, so a lead without Worklet
// is asked nothing — leading somebody opens no app. The CONTROL is the case
// above, the same lead holding it. Mutation: decide the lead before the app
// and this reader asks for a queue the engine refuses them.
test("a lead without Worklet asks nothing of a report's record", async () => {
  location.hash = "#/company/org/seats/ceo";
  const asked = await askedAs(["doclet"], true);
  expect(asked).not.toContain("work_my_work");
  expect(asked).not.toContain("work_person");
  expect(asked).not.toContain("work_items");
});

// AND THE OVERVIEW'S DAY READS THE SAME RULE.
test("a lead's overview of a report asks for their day", async () => {
  location.hash = "#/company/org/seats/ceo";
  const asked = await askedAs(["worklet"], true);
  await waitFor(() => expect(asked).toContain("work_person"));
});
