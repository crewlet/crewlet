/**
 * Writing the charter on Company › Principles, as an administrator meets it:
 * a part opens in its own card and saves on its own, a draft is held to the
 * charter's rules where it is typed, a colleague's change is shown and left
 * for the person to settle, unsaved writing is never lost to a stray key or
 * a click elsewhere, and a reader who may not write it is offered nothing.
 *
 * The cards here are a minimal stand-in for the page's own (`../Principles.tsx`
 * places the same pieces), so what is pinned is the editing, not the layout.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { ClientContext, useOrg } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { OrgProjection } from "~/protocol/types.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { InertWebSocket } from "~/test/client.tsx";
import { fakeCharterEngine, type CharterEngine } from "~/test/charterEngine.ts";
import type { CharterPart, PrincipleCardEdit } from "../PrincipleCard.tsx";
import { usePrinciplesEdit } from "./usePrinciplesEdit.tsx";
import { afterMicrotasks } from "~/test/settle.ts";

const ACME = {
  name: "Acme",
  mission: "Ship it",
  vision: "Every team ships daily",
  policies: ["Pay on time.", "Ask before deleting anything."],
  roles: [],
};

let engine: CharterEngine;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
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

/** One card, placing the pieces the page places. */
function Card({
  part,
  edit,
}: {
  part: CharterPart;
  edit: PrincipleCardEdit<string> | PrincipleCardEdit<readonly string[]>;
}) {
  const shown = edit.shown;
  const empty = typeof shown === "string" ? shown === "" : shown.length === 0;
  return (
    <section aria-label={part}>
      <h3>{part}</h3>
      {edit.actions}
      {edit.editor ? (
        edit.editor
      ) : empty ? (
        <>
          <p>Nothing written</p>
          {edit.add}
        </>
      ) : typeof shown === "string" ? (
        <p>{shown}</p>
      ) : (
        <ol>
          {shown.map((policy: string, i: number) => (
            <li key={i}>{policy}</li>
          ))}
        </ol>
      )}
      {edit.note}
    </section>
  );
}

function Cards({ org }: { org: OrgProjection }) {
  const edit = usePrinciplesEdit(org);
  return (
    <>
      {edit.notice}
      <Card part="mission" edit={edit.mission} />
      <Card part="vision" edit={edit.vision} />
      <Card part="policies" edit={edit.policies} />
    </>
  );
}

function Page() {
  const org = useOrg();
  return org ? <Cards org={org} /> : null;
}

/** The page for a reader holding `grants`, its chart applied. */
function mount(grants: string[] = ["admin"], org: Record<string, unknown> = ACME) {
  const store = new Store();
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
            <AppAnnouncer />
            <Router>
              <Page />
            </Router>
          </ToastProvider>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  act(() => store.applyOrg(withDerived(org)));
  return store;
}

const card = (part: CharterPart) => screen.getByRole("region", { name: part });
const editOf = (part: string) => screen.findByRole("button", { name: `Edit the ${part}` });
const missionBox = () => within(card("mission")).getByRole("textbox", { name: "Mission" });
const saveKey = (el: Element) => fireEvent.keyDown(el, { key: "Enter", ctrlKey: true });

// ADMIN, ON A DOCUMENT THIS DEPLOYMENT LETS THEM CHANGE: nobody else is
// offered a control that could only ever say no. Mutation: drop the
// `mayChangeConfig` gate and the Edit appears for the tracker reader.
test("a reader who may not write the charter is offered no control at all", async () => {
  location.hash = "#/company/principles?edit=mission";
  mount(["worklet"], { ...ACME, vision: "" });
  // The address naming a part is dropped once the reader is known.
  await waitFor(() => expect(location.hash).toBe("#/company/principles"));
  expect(screen.queryByRole("button", { name: /^Edit/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^Add a/ })).toBeNull();
  expect(screen.queryByRole("textbox")).toBeNull();
});

test("Edit opens the part in its card with the focus in its words, and says so in the address", async () => {
  mount();
  fireEvent.click(await editOf("mission"));
  const box = missionBox();
  expect(box).toHaveProperty("value", "Ship it");
  expect(document.activeElement).toBe(box);
  expect(location.hash).toBe("#/company/principles?edit=mission");
  // THE OTHER PARTS STAY AS THEY READ: the person writes against them.
  expect(within(card("vision")).getByText("Every team ships daily")).toBeDefined();
});

test("Cancel on an unchanged part closes it and puts the focus back on its Edit", async () => {
  mount();
  fireEvent.click(await editOf("mission"));
  fireEvent.click(within(card("mission")).getByRole("button", { name: "Cancel" }));
  const edit = await editOf("mission");
  await waitFor(() => expect(document.activeElement).toBe(edit));
  expect(location.hash).toBe("#/company/principles");
});

// ONE PART PER SAVE, onto the charter as it is now, conditional on its tag,
// recorded with what changed. Mutation: send the draft charter as the page
// drew it and a colleague's vision is overwritten.
test("Save writes this part onto the charter as it is now and says it landed", async () => {
  const store = mount();
  fireEvent.click(await editOf("mission"));
  // A COLLEAGUE CHANGES ANOTHER PART meanwhile: not this card's conflict.
  engine.charter = { ...engine.charter, vision: "A colleague's vision" };
  engine.tag = 2;
  fireEvent.change(missionBox(), { target: { value: "Ship it well  " } });
  saveKey(missionBox());
  await screen.findByText("Mission saved");
  expect(engine.puts).toEqual([
    {
      body: { mission: "Ship it well", vision: "A colleague's vision", policies: ACME.policies },
      ifMatch: '"charter-2"',
      summary: "Changed the mission",
    },
  ]);
  // SHOWN AS SAVED before this browser's chart has it, and then from the chart.
  expect(within(card("mission")).getByText("Ship it well")).toBeDefined();
  expect(within(card("mission")).getByText(/^Saved\./)).toBeDefined();
  act(() => store.applyOrg(withDerived({ ...ACME, mission: "Ship it well" })));
  expect(within(card("mission")).queryByText(/^Saved\./)).toBeNull();
  await waitFor(() =>
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Edit the mission" })),
  );
});

// A SAVE THIS NODE REFUSED IS NOT SHOWN AS ONE ON ITS WAY. The applied epoch
// a refusal leaves where it was said nothing, so the card drew the saved
// words, "shown as soon as this node applies" them, for as long as the page
// stayed open while every agent here read the old ones. Mutation: settle a
// landed save by the applied epoch alone and the card keeps "Ship it well".
test("a save this node refused shows the words it still runs, and says so", async () => {
  const store = mount();
  fireEvent.click(await editOf("mission"));
  fireEvent.change(missionBox(), { target: { value: "Ship it well" } });
  saveKey(missionBox());
  await screen.findByText("Mission saved");
  expect(within(card("mission")).getByText("Ship it well")).toBeDefined();

  // The fake engine stores every write as revision "r", activated at 5.
  act(() =>
    store.applyHealth(
      healthFrame({
        applied_epoch: 4,
        config_attempt: { epoch: 5, revision_id: "r", status: "error" },
      }),
    ),
  );
  expect(within(card("mission")).getByText("Ship it")).toBeDefined();
  expect(within(card("mission")).queryByText("Ship it well")).toBeNull();
  expect(within(card("mission")).getByText(/this node refused the new revision/)).toBeDefined();
  expect(within(card("mission")).queryByText(/as soon as this node applies/)).toBeNull();

  // A later revision this node applies is the answer, whatever it says.
  act(() =>
    store.applyHealth(
      healthFrame({
        applied_epoch: 6,
        config_attempt: { epoch: 6, revision_id: "r6", status: "ok" },
      }),
    ),
  );
  expect(within(card("mission")).queryByText(/this node refused/)).toBeNull();
  expect(within(card("mission")).getByText("Ship it")).toBeDefined();
});

// A DROPPED SOCKET CLEARS THE HEALTH SLICE, and says nothing about the save:
// read as this node's standing, the missing push was "applying", and the card
// drew the saved words again over the refusal it had said, until the socket
// came back. Mutation: hand the cards the live slice (`useEngineHealth`) and
// this reads "Ship it well", "as soon as this node applies".
test("a save this node refused stays refused through a dropped socket", async () => {
  const store = mount();
  fireEvent.click(await editOf("mission"));
  fireEvent.change(missionBox(), { target: { value: "Ship it well" } });
  saveKey(missionBox());
  await screen.findByText("Mission saved");
  act(() =>
    store.applyHealth(
      healthFrame({
        applied_epoch: 4,
        config_attempt: { epoch: 5, revision_id: "r", status: "error" },
      }),
    ),
  );
  expect(within(card("mission")).getByText(/this node refused the new revision/)).toBeDefined();

  act(() => store.setConnected(false));
  expect(within(card("mission")).getByText("Ship it")).toBeDefined();
  expect(within(card("mission")).queryByText("Ship it well")).toBeNull();
  expect(within(card("mission")).getByText(/this node refused the new revision/)).toBeDefined();
  expect(within(card("mission")).queryByText(/as soon as this node applies/)).toBeNull();
});

// SOMEBODY ELSE'S WORDS ARE NEVER OVERWRITTEN UNSEEN: the change is shown and
// the person chooses. Mutation: skip the base comparison and the PUT goes out.
test("a colleague's change to the same part is shown, and keeping the draft writes over it knowingly", async () => {
  mount();
  fireEvent.click(await editOf("mission"));
  fireEvent.change(missionBox(), { target: { value: "My mission" } });
  engine.charter = { ...engine.charter, mission: "Their mission" };
  engine.tag = 2;
  saveKey(missionBox());
  expect(
    await screen.findByText(/^The mission changed while you were editing\. Your draft is kept/),
  ).toBeDefined();
  expect(engine.puts).toEqual([]);
  // WHAT CHANGED, line by line: the words the person started from, and theirs.
  const diff = screen.getByRole("group", { name: "Mission: what changed since you started" });
  expect(within(diff).getByText("Ship it")).toBeDefined();
  expect(within(diff).getByText("Their mission")).toBeDefined();
  // THE PERSON'S PLACE IS KEPT: the focus stays in their draft, untouched.
  expect(document.activeElement).toBe(missionBox());
  expect(missionBox()).toHaveProperty("value", "My mission");
  fireEvent.click(screen.getByRole("button", { name: "Keep my draft" }));
  saveKey(missionBox());
  await screen.findByText("Mission saved");
  expect(engine.puts[0]).toMatchObject({ body: { mission: "My mission" }, ifMatch: '"charter-2"' });
});

test("using theirs drops the draft and shows their words", async () => {
  mount();
  fireEvent.click(await editOf("mission"));
  fireEvent.change(missionBox(), { target: { value: "My mission" } });
  engine.charter = { ...engine.charter, mission: "Their mission" };
  engine.tag = 2;
  saveKey(missionBox());
  fireEvent.click(await screen.findByRole("button", { name: "Use theirs" }));
  expect(within(card("mission")).getByText("Their mission")).toBeDefined();
  expect(within(card("mission")).queryByRole("textbox")).toBeNull();
});

// A REFUSAL KEEPS THE DRAFT and says why ONCE, beside the Save that caused
// it: the editor's alert, and no toast reading it again.
test("a refused save keeps the draft and says why", async () => {
  mount();
  fireEvent.click(await editOf("mission"));
  fireEvent.change(missionBox(), { target: { value: "My mission" } });
  engine.refuse = { status: 403, body: { error: "config_managed", managed_by: ["deploy-bot"] } };
  saveKey(missionBox());
  const said = await screen.findAllByText("Managed by deploy-bot. Change it there.");
  expect(said).toHaveLength(1);
  expect(said[0]!.closest('[role="alert"]')).not.toBeNull();
  expect(screen.queryByText("Mission not saved")).toBeNull();
  expect(missionBox()).toHaveProperty("value", "My mission");
});

// UNSAVED WRITING ASKS FIRST, whichever way out: Escape, Cancel, or a move to
// another page. Mutation: drop the dirty check and Escape discards silently.
test("Escape on a changed part asks before discarding it", async () => {
  mount();
  fireEvent.click(await editOf("mission"));
  fireEvent.change(missionBox(), { target: { value: "Half a thought" } });
  fireEvent.keyDown(missionBox(), { key: "Escape" });
  // AN ALERT DIALOG, because confirming loses writing.
  const dialog = await screen.findByRole("alertdialog", { name: "Discard your changes?" });
  expect(within(dialog).getByText("The mission goes back to how it was saved.")).toBeDefined();
  fireEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
  await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  expect(missionBox()).toHaveProperty("value", "Half a thought");
  fireEvent.keyDown(missionBox(), { key: "Escape" });
  fireEvent.click(
    within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Discard changes" }),
  );
  await waitFor(() => expect(within(card("mission")).queryByRole("textbox")).toBeNull());
  expect(within(card("mission")).getByText("Ship it")).toBeDefined();
});

test("moving to another page with unsaved writing asks first", async () => {
  mount();
  fireEvent.click(await editOf("mission"));
  fireEvent.change(missionBox(), { target: { value: "Half a thought" } });
  act(() => {
    location.hash = "#/worklet";
  });
  const dialog = await screen.findByRole("alertdialog", { name: "Leave without saving?" });
  expect(within(dialog).getByText("Your changes to the mission will be lost.")).toBeDefined();
});

// THE RULES ARE SAID WHERE THEY ARE BROKEN, and Save sends nothing while one
// stands. Mutation: drop the repeat rule and the PUT goes out with two copies.
test("a repeated policy is said under it and nothing is sent", async () => {
  mount();
  fireEvent.click(await editOf("standing policies"));
  const second = within(card("policies")).getByRole("textbox", { name: /Policy 2 of 2/ });
  fireEvent.change(second, { target: { value: "Pay on time." } });
  expect(await screen.findByText("This repeats policy 1.")).toBeDefined();
  expect(second.getAttribute("aria-invalid")).toBe("true");
  // SAVE SAYS WHY IT WAITS rather than sending what the engine would refuse.
  fireEvent.click(within(card("policies")).getByRole("button", { name: "Save" }));
  expect(document.body.textContent).toContain("Fix the highlighted field first.");
  expect(engine.puts).toEqual([]);
});

test("Add on an unwritten part opens it ready for its first words", async () => {
  mount(["admin"], { ...ACME, vision: "" });
  fireEvent.click(await screen.findByRole("button", { name: "Add a vision" }));
  const box = within(card("vision")).getByRole("textbox", { name: "Vision" });
  expect(document.activeElement).toBe(box);
});

// AN ADDRESS OPENS THE PARTS IT NAMES: the org chart's company node sends a
// person here to write the charter.
test("the address opens the parts it names, the first with the focus", async () => {
  location.hash = "#/company/principles?edit=vision,policies";
  mount();
  const box = await within(card("vision")).findByRole("textbox", { name: "Vision" });
  expect(within(card("policies")).getAllByRole("textbox").length).toBeGreaterThan(0);
  await waitFor(() => expect(document.activeElement).toBe(box));
});

// THE ENGINE'S WORDS, NOT THE PAGE'S COPY: the chart this browser holds can
// lag the engine, and a draft started from it would hide a change.
test("an opened part starts from the engine's charter", async () => {
  engine.charter = { ...engine.charter, mission: "Newer on the engine" };
  mount();
  fireEvent.click(await editOf("mission"));
  await waitFor(() => expect(missionBox()).toHaveProperty("value", "Newer on the engine"));
});

// A POLICY TYPED AND NOT ADDED is saved with the rest, never dropped.
test("a policy typed in the new box and not added is saved with the rest", async () => {
  mount();
  fireEvent.click(await editOf("standing policies"));
  const box = within(card("policies")).getByRole("textbox", { name: "New policy" });
  fireEvent.change(box, { target: { value: "Never share a password." } });
  fireEvent.click(within(card("policies")).getByRole("button", { name: "Save" }));
  await screen.findByText("Standing policies saved");
  expect(engine.puts[0]!.body).toMatchObject({
    policies: [...ACME.policies, "Never share a password."],
  });
});

test("a removed policy can be put back where it was", async () => {
  mount();
  fireEvent.click(await editOf("standing policies"));
  fireEvent.click(within(card("policies")).getByRole("button", { name: /Remove policy 1 of 2/ }));
  expect(within(card("policies")).queryByDisplayValue("Pay on time.")).toBeNull();
  fireEvent.click(within(card("policies")).getByRole("button", { name: "Undo" }));
  const restored = within(card("policies")).getByDisplayValue("Pay on time.");
  expect(restored.getAttribute("aria-label")).toMatch(/Policy 1 of 2/);
});

test("Save says why it waits while nothing has changed", async () => {
  mount();
  fireEvent.click(await editOf("mission"));
  const save = within(card("mission")).getByRole("button", { name: "Save" });
  expect(save.getAttribute("aria-disabled")).toBe("true");
  expect(document.body.textContent).toContain("Change something first.");
});

// THE KEYBOARD SAVES FROM INSIDE THE LIST TOO: the command key with Enter is
// the editor's, not the list's Enter (which adds the next policy).
test("the command key with Enter saves from a policy", async () => {
  mount();
  fireEvent.click(await editOf("standing policies"));
  const first = within(card("policies")).getByRole("textbox", { name: /Policy 1 of 2/ });
  fireEvent.change(first, { target: { value: "Pay on time, always." } });
  saveKey(first);
  await screen.findByText("Standing policies saved");
  expect(engine.puts[0]!.body.policies).toEqual([
    "Pay on time, always.",
    "Ask before deleting anything.",
  ]);
});

// THE BARE COMPANY LANDING IS THIS PAGE TOO: an address naming parts opens
// them there, and moving to `#/company/principles` mid-edit is no move away.
test("the bare Company landing opens the parts its address names", async () => {
  location.hash = "#/company?edit=vision";
  mount();
  await within(card("vision")).findByRole("textbox", { name: "Vision" });
});

test("moving between the Company landing and Principles keeps the writing without asking", async () => {
  location.hash = "#/company";
  mount();
  fireEvent.click(await editOf("mission"));
  fireEvent.change(missionBox(), { target: { value: "Half a thought" } });
  // BOTH WAYS: to the sidebar's Principles row, and back to the Company
  // landing, which a path compared with `#/company/principles` called away.
  for (const hash of ["#/company/principles?edit=mission", "#/company?edit=mission"]) {
    act(() => {
      location.hash = hash;
    });
    await afterMicrotasks();
    expect(screen.queryByRole("alertdialog"), hash).toBeNull();
    expect(missionBox()).toHaveProperty("value", "Half a thought");
  }
});

// ONLY WHAT THE PERSON CHANGED is held to the rules, as the engine holds it:
// a policy stored before them is said, and never holds Save.
test("a stored policy written before the rules is a warning, and Save still saves", async () => {
  const long = "x".repeat(1100);
  engine.charter = { ...engine.charter, policies: [long, "Ask first."] };
  mount(["admin"], { ...ACME, policies: [long, "Ask first."] });
  fireEvent.click(await editOf("standing policies"));
  const second = await within(card("policies")).findByDisplayValue("Ask first.");
  fireEvent.change(second, { target: { value: "Ask before deleting." } });
  expect(await screen.findByText("Saved before the current rules")).toBeDefined();
  fireEvent.click(within(card("policies")).getByRole("button", { name: "Save" }));
  await screen.findByText("Standing policies saved");
  expect(engine.puts[0]!.body.policies).toEqual([long, "Ask before deleting."]);
});

// ONE LINE THAT WRAPS: a long mission is read whole while it is written,
// never scrolled sideways to its tail, and Enter puts no line break in it.
test("the mission edits on one line that wraps", async () => {
  mount();
  fireEvent.click(await editOf("mission"));
  const box = missionBox();
  expect(box.tagName).toBe("TEXTAREA");
  expect(box.getAttribute("aria-multiline")).toBe("false");
  fireEvent.change(box, { target: { value: "Ship it\nnow" } });
  expect((box as HTMLTextAreaElement).value).not.toMatch(/\n/);
});

// THE CARD'S TITLE IS THE VISIBLE NAME: the field keeps its own, for a reader
// listing fields, in a label that is hidden and takes no room under the title.
test("each statement field is named by a hidden label that takes no room", async () => {
  mount();
  for (const [part, name] of [
    ["mission", "Mission"],
    ["vision", "Vision"],
  ] as const) {
    fireEvent.click(await editOf(part));
    const box = within(card(part)).getByRole("textbox", { name });
    const label = document.querySelector(`label[for="${box.id}"]`);
    expect(label?.classList.contains("crewlet-visually-hidden"), part).toBe(true);
  }
});
