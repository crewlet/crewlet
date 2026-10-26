/**
 * Writing the charter on Company › Principles WHOLE: the page's own cards, the
 * editor in them, and the charter route behind them, as an administrator and
 * a reader meet them together.
 *
 * `edit/usePrinciplesEdit.test.tsx` walks the editing on stand-in cards, and
 * `Principles.test.tsx` walks the page as it reads. What only the two together
 * can show is here: the standing policies moved, removed and saved through the
 * real list; two parts open at once, each saved onto the charter as it is
 * then; a draft held to the engine's own bounds before anything is sent; the
 * engine's refusal said under the policy it names; a save that never arrived
 * kept and sent again; the browser asked before a tab with writing in it
 * closes; and a reader who may not write it never asking the guarded route.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { MISSION_MAX_CHARS } from "~/contract/config.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { fakeCharterEngine, type CharterEngine } from "~/test/charterEngine.ts";
import { InertWebSocket } from "~/test/client.tsx";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { Principles } from "./Principles.tsx";

const ACME = {
  name: "Acme",
  mission: "Ship it",
  vision: "Every team ships daily",
  policies: ["Pay on time.", "Ask before deleting anything.", "Every amount is exact to the cent."],
  roles: [],
};

let engine: CharterEngine;

beforeEach(() => {
  engine = fakeCharterEngine({
    mission: ACME.mission,
    vision: ACME.vision,
    policies: ACME.policies,
  });
  location.hash = "#/company/principles";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** The page for a reader holding `grants`, its chart applied. */
function mount(grants: string[] = ["admin"], org: Record<string, unknown> = ACME) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  // AN OPEN LINK, so what the page offers is decided by the reader's grants.
  store.applyHealth(healthFrame());
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer"
      ? Promise.resolve({ login: "ana", owner: "ana", handle: "ana", grants, acts: [] })
      : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <ToastProvider>
            <Router>
              <Principles />
            </Router>
          </ToastProvider>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  act(() => store.applyOrg(withDerived(org)));
  return store;
}

const card = (name: "Mission" | "Vision" | "Standing policies") =>
  screen.getByRole("region", { name });
const open = async (part: "mission" | "vision" | "standing policies") =>
  fireEvent.click(await screen.findByRole("button", { name: `Edit the ${part}` }));
const policyBox = (n: number, of: number) =>
  within(card("Standing policies")).getByRole("textbox", { name: `Policy ${n} of ${of}` });
const saveKey = (el: Element) => fireEvent.keyDown(el, { key: "Enter", ctrlKey: true });

// THE ORDER IS THE RULE BOOK'S: every seat reads the policies in it, so a
// move made from the keyboard is saved as made, and the revision says so.
// Mutation: compare two policy lists as sets in `samePart` and a reorder is
// held as no change.
test("a policy moved with Alt and an arrow is saved in its new place, recorded as reordered", async () => {
  mount();
  await open("standing policies");
  fireEvent.keyDown(policyBox(1, 3), { key: "ArrowDown", altKey: true });
  // THE FOCUS FOLLOWS THE POLICY, so a second press moves it again.
  await waitFor(() => expect(document.activeElement).toBe(policyBox(2, 3)));
  expect(policyBox(2, 3)).toHaveProperty("value", "Pay on time.");
  saveKey(policyBox(2, 3));
  await screen.findByText("Standing policies saved");
  expect(engine.puts).toEqual([
    {
      body: {
        mission: ACME.mission,
        vision: ACME.vision,
        policies: [ACME.policies[1], ACME.policies[0], ACME.policies[2]],
      },
      ifMatch: '"charter-1"',
      summary: "Standing policies: reordered",
    },
  ]);
  const read = within(card("Standing policies")).getByRole("list", { name: "Standing policies" });
  expect(
    within(read)
      .getAllByRole("listitem")
      .map((item) => item.textContent),
  ).toEqual([ACME.policies[1], ACME.policies[0], ACME.policies[2]]);
});

// THE MOVE BUTTONS ARE THE SAME MOVE, for a reader who does not hold Alt.
test("a policy moved with its button is saved in its new place", async () => {
  mount();
  await open("standing policies");
  fireEvent.click(
    within(card("Standing policies")).getByRole("button", { name: "Move policy 3 of 3 up" }),
  );
  saveKey(policyBox(1, 3));
  await screen.findByText("Standing policies saved");
  expect(engine.puts[0]!.body.policies).toEqual([
    ACME.policies[0],
    ACME.policies[2],
    ACME.policies[1],
  ]);
});

// A REMOVED RULE IS NOT READ BY ANYBODY FROM THE NEXT TURN, and the company's
// history says one went. Mutation: keep removed rows in the draft and the PUT
// still carries three.
test("a removed policy is left out of the save and recorded as removed", async () => {
  mount();
  await open("standing policies");
  fireEvent.click(
    within(card("Standing policies")).getByRole("button", { name: /^Remove policy 2 of 3/ }),
  );
  saveKey(policyBox(1, 2));
  await screen.findByText("Standing policies saved");
  expect(engine.puts).toEqual([
    {
      body: {
        mission: ACME.mission,
        vision: ACME.vision,
        policies: [ACME.policies[0], ACME.policies[2]],
      },
      ifMatch: '"charter-1"',
      summary: "Standing policies: removed 1",
    },
  ]);
});

// EACH OPEN PART SAVES ON ITS OWN, onto the charter AS IT IS THEN: saving the
// mission sends the company's policies rather than the draft open beside it,
// and leaves that draft where it was; the policies then save onto the mission
// just saved, under the tag that save handed back. Mutation: close every open
// part when one saves and the policies draft is lost.
test("two parts open at once save one at a time, each onto the charter as it is then", async () => {
  mount();
  await open("mission");
  await open("standing policies");
  const mission = within(card("Mission")).getByRole("textbox", { name: "Mission" });
  fireEvent.change(mission, { target: { value: "Split costs fairly" } });
  fireEvent.change(policyBox(3, 3), { target: { value: "Round nothing." } });
  saveKey(mission);
  await screen.findByText("Mission saved");
  expect(engine.puts[0]!.body).toEqual({
    mission: "Split costs fairly",
    vision: ACME.vision,
    policies: ACME.policies,
  });
  // THE OTHER DRAFT IS UNTOUCHED, still open with what was typed.
  expect(policyBox(3, 3)).toHaveProperty("value", "Round nothing.");
  saveKey(policyBox(3, 3));
  await screen.findByText("Standing policies saved");
  expect(engine.puts[1]).toEqual({
    body: {
      mission: "Split costs fairly",
      vision: ACME.vision,
      policies: [ACME.policies[0], ACME.policies[1], "Round nothing."],
    },
    ifMatch: '"charter-2"',
    summary: "Standing policies: reworded 1",
  });
});

// THE VISION IS PARAGRAPHS: Enter is a new line in it, and only the command
// key with Enter saves. Mutation: make the vision a one-line field and its
// paragraphs never reach the engine.
test("the vision keeps its paragraphs, and only the command key with Enter saves it", async () => {
  mount();
  await open("vision");
  const vision = within(card("Vision")).getByRole("textbox", { name: "Vision" });
  fireEvent.change(vision, { target: { value: "First paragraph.\n\nSecond paragraph." } });
  fireEvent.keyDown(vision, { key: "Enter" });
  expect(engine.puts).toEqual([]);
  saveKey(vision);
  await screen.findByText("Vision saved");
  expect(engine.puts[0]!.body.vision).toBe("First paragraph.\n\nSecond paragraph.");
});

// THE ENGINE'S BOUND, COUNTED AS THE ENGINE COUNTS IT, before anything is sent:
// a mission of emoji at the bound is twice as long to `.length` and is taken,
// one more character is held with its count. Mutation: count with `.length`
// in `lengthOf` and the emoji at the bound is held.
test("a mission is held to the engine's bound in characters before anything is sent", async () => {
  mount();
  await open("mission");
  const mission = within(card("Mission")).getByRole("textbox", { name: "Mission" });
  fireEvent.change(mission, { target: { value: "😀".repeat(MISSION_MAX_CHARS + 1) } });
  expect(mission.getAttribute("aria-invalid")).toBe("true");
  saveKey(mission);
  await act(async () => {});
  expect(engine.puts).toEqual([]);
  expect(document.activeElement).toBe(mission);
  fireEvent.change(mission, { target: { value: "😀".repeat(MISSION_MAX_CHARS) } });
  saveKey(mission);
  await screen.findByText("Mission saved");
  expect(engine.puts).toHaveLength(1);
});

// WHAT THE ENGINE REFUSES IS SAID WHERE IT IS: a problem at `policies[1]`
// under the second policy, the draft kept. Mutation: drop `refusedProblems`
// and the reason is a sentence over the list, under no row.
test("the engine's refusal of one policy is said under that policy, and the draft is kept", async () => {
  mount();
  await open("standing policies");
  fireEvent.change(policyBox(2, 3), { target: { value: "Ask first." } });
  engine.refuse = {
    status: 400,
    body: {
      error: "validation_error",
      message: "The charter is not valid.",
      problems: [
        {
          path: "policies[1]",
          segments: ["policies", 1],
          kind: "shape",
          message: "Policy 2 says something the engine refused.",
        },
      ],
    },
  };
  saveKey(policyBox(2, 3));
  // SAID ONCE, beside the Save that caused it, and under the row it names.
  const alert = await within(card("Standing policies")).findByRole("alert");
  expect(alert.textContent).toContain("Policy 2 says something the engine refused.");
  const row = policyBox(2, 3);
  expect(row).toHaveProperty("value", "Ask first.");
  expect(row.getAttribute("aria-invalid")).toBe("true");
  const said = (row.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
  expect(said).toContain("Policy 2 says something the engine refused.");
});

// A SAVE THAT NEVER ARRIVED IS SETTLED BY A READ, not guessed: the charter
// still reads as it did, so nothing landed, the draft stays, and Save again
// writes it. Mutation: report an unanswered write as saved and the second
// save is never offered.
test("a save that never reached the engine keeps the draft, and Save again writes it", async () => {
  mount();
  await open("mission");
  const mission = within(card("Mission")).getByRole("textbox", { name: "Mission" });
  fireEvent.change(mission, { target: { value: "Split costs fairly" } });
  engine.fail = new TypeError("Failed to fetch");
  saveKey(mission);
  const alert = await within(card("Mission")).findByRole("alert");
  expect(alert.textContent).toMatch(/nothing was saved/);
  expect(engine.charter.mission).toBe(ACME.mission);
  expect(mission).toHaveProperty("value", "Split costs fairly");
  saveKey(mission);
  await screen.findByText("Mission saved");
  expect(engine.charter.mission).toBe("Split costs fairly");
});

// A TAB CLOSED WITH WRITING IN IT IS ASKED ABOUT by the browser, and one with
// nothing unsaved is not, whether nothing was typed or it was all saved.
// Mutation: hold the unload guard on an open part rather than a dirty one and
// the unchanged editor asks.
test("the browser asks before closing a tab with unsaved writing, and not otherwise", async () => {
  const unload = () => {
    const event = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  };
  mount();
  await open("mission");
  expect(unload()).toBe(false);
  const mission = within(card("Mission")).getByRole("textbox", { name: "Mission" });
  fireEvent.change(mission, { target: { value: "Half a thought" } });
  expect(unload()).toBe(true);
  saveKey(mission);
  await screen.findByText("Mission saved");
  // WAITED FOR, not read at once: the editor lets go of the browser's prompt in
  // an effect that runs after the render that shows the toast, so a read the
  // moment the toast is in the document can land between the two. Under a busy
  // runner it did, and the guard read as still held for writing already saved.
  await waitFor(() => expect(unload()).toBe(false));
});

// A READER WHO MAY NOT WRITE THE CHARTER never asks the route that would
// refuse them, whatever address sent them here (the org chart's company node
// sends every reader to the same one), and reads the page with nothing open.
// Mutation: admit every reader to the editor (`mayWriteCharter` without
// `mayChangeConfig`) and the parts open and the route is read.
test("a reader who may not write it, sent to the edit address, reads the page and asks nothing", async () => {
  location.hash = "#/company/principles?edit=mission,vision,policies";
  mount(["worklet"]);
  await waitFor(() => expect(location.hash).toBe("#/company/principles"));
  expect(within(card("Mission")).getByText(ACME.mission)).toBeDefined();
  expect(screen.queryByRole("textbox")).toBeNull();
  expect(engine.gets).toBe(0);
  expect(engine.puts).toEqual([]);
});
