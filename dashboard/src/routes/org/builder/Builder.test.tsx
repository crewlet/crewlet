/**
 * The builder decides its posture from what the engine answers, checks
 * every draft with a dry run of exactly the write a save would send, and
 * keeps the operator's work through a change of reader.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { setStepUpConfirmer, type OrgProjection } from "~/protocol/index.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { PROVIDER_STEPS } from "~/contract/providersteps.ts";
import { DRAFT_STORAGE_KEY } from "./model/persistence.ts";
import { useBuilder, type BuilderViewHandle } from "./BuilderContext.tsx";
import { menuEntryLabel } from "~/testing.tsx";
import { href } from "~/app/router.tsx";
import { seatPath } from "~/lib/seats.ts";
import { FillRequest, type FillShape } from "~/app/fill.tsx";
import {
  company,
  Engine,
  EDITOR,
  fakeSurfaces,
  FakeView,
  json,
  mountBuilder,
  openTab,
  refusal,
} from "./testkit.tsx";
import { withDerived } from "~/test/org.ts";
import { CHECK_BACKOFF_BASE_MS, CHECK_DEBOUNCE_MS } from "./model/scheduler.ts";
import { settled } from "~/test/settle.ts";
import {
  applyPageAction,
  pageSnapshot,
  resetPageContextForTest,
  setFramePage,
} from "~/app/assistant/context.ts";

/** Nobody signed in, as the `viewer` question answers it. */
const NOBODY = { login: "", owner: "", grants: [] };

/** Somebody signed in who holds no Admin. */
const READER = { login: "reader", owner: "reader", grants: [] };

/**
 * Somebody bound to a seat whom the `viewer` question answers as holding
 * Admin — on a node that serves it.
 */
const BOUND_ADMIN = { ...EDITOR, handle: "ops" };

/**
 * What a node whose `api.auth.admin` is `refuse` answers [BOUND_ADMIN]'s check
 * of the whole company. The posture is each node's, applied per request, so a
 * fleet mid-rollout legally disagrees about it, and a reader the `viewer`
 * answer called an admin can have a check land where admin is clamped away:
 * the route admits anybody bound to a seat until it has read what the write
 * changes, and the admission then refuses every part of it, starting with the
 * unchanged document, naming Admin. It is the one refusal for want of Admin a
 * whole-company draft meets, since a reader holding no Admin and leading no
 * unit is refused the configuration's read before any check is sent.
 */
const refusedByANodeRefusingAdmin = () =>
  json(
    {
      error: "unauthorized",
      reason: "no_grant",
      grants: ["admin"],
      refused: [{ kind: "document", id: "", place: "", why: "unchanged", reason: "no_grant" }],
    },
    403,
  );

/** The socket comes back, which is when the frame asks who is reading again. */
function reconnect(store: { setConnected: (up: boolean) => void }) {
  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
}

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  resetPageContextForTest();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  location.hash = "#/";
});

const named: OrgProjection = { name: "Acme", roles: [], units: [] };

/** The Builder's one polite live region. */
const liveRegion = () => document.querySelector("[data-live-region]")!;

/** A menu's entries by their labels, without the key hints some of them carry. */
const labels = (menu: HTMLElement) => within(menu).getAllByRole("menuitem").map(menuEntryLabel);

/** The reason a held control says after its name, off the kit's described-by. */
function reasonOf(control: HTMLElement): string {
  return (control.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ")
    .trim();
}

/**
 * The toolbar's Add, held: a plain outline button saying why, which opens no
 * menu. Mutation: draw the Add menu whatever the reader may do and the held
 * button is a menu trigger again, with entries a press would refuse.
 */
function expectAddHeld(reason: string) {
  const add = screen.getByRole("button", { name: "Add to the organization" });
  expect(add.getAttribute("aria-disabled")).toBe("true");
  expect(add.getAttribute("aria-haspopup")).toBeNull();
  expect(reasonOf(add)).toBe(reason);
  fireEvent.click(add);
  expect(screen.queryByRole("menu", { name: "Add to the organization" })).toBeNull();
}

describe("the posture table", () => {
  test("a served configuration opens edit mode", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    expect(await screen.findByText("CEO")).toBeDefined();
    // The first check runs at once, on the draft with no operations, as the
    // write a save would send: a PATCH conditional on the revision it read.
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const check = engine.checks()[0]!;
    expect(check.method).toBe("PATCH");
    expect(check.headers["If-Match"]).toBe('"r1"');
    expect(check.headers["Content-Type"]).toBe("application/merge-patch+json");
    expect(check.body).toEqual({});
    expect(await screen.findByText("Configuration OK")).toBeDefined();
  });

  test("no active revision and no company in the org opens create mode", async () => {
    const engine = new Engine(null);
    mountBuilder({ engine });
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const check = engine.checks()[0]!;
    // Create-only, fleet-wide: a company that appeared meanwhile is a 412.
    expect(check.method).toBe("PUT");
    expect(check.headers["If-None-Match"]).toBe("*");
  });

  test("no active revision while the org names a company is a node that has not caught up", async () => {
    const engine = new Engine(null);
    const { clock } = mountBuilder({ engine, org: named });
    expect(await screen.findByText("This node is still catching up")).toBeDefined();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
    // Never create mode: nothing is checked, let alone written — nothing the
    // builder asked is still out, and no check is armed on its clock.
    await settled(() => engine.requests.length);
    expect(clock.pending).toBe(0);
    expect(engine.checks()).toHaveLength(0);
  });

  // THE SOCKET OPENING IS NOT THE SNAPSHOT: an answer that beats it would
  // offer create mode on a node the fleet runs a company without.
  test("create mode waits for the org snapshot, not the socket opening, before deciding", async () => {
    const engine = new Engine(null);
    const { store, clock } = mountBuilder({ engine, org: null });
    await settled(() => engine.requests.length);
    expect(clock.pending).toBe(0);
    expect(engine.checks()).toHaveLength(0);
    act(() => store.applyOrg(named));
    expect(await screen.findByText("This node is still catching up")).toBeDefined();
  });

  test("a refusal for nobody signed in asks them to sign in", async () => {
    const engine = new Engine(company());
    engine.script = () => json({ error: "unauthorized" }, 401);
    mountBuilder({ engine, viewer: () => NOBODY });
    expect(await screen.findByText("Editing the organization needs you to sign in.")).toBeDefined();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeDefined();
  });

  // SIGNED IN AND REFUSED IS A GRANT, NOT A SESSION: telling this reader to
  // sign in would send them looking for a session they already hold.
  test("a refusal of somebody signed in names the grant they lack", async () => {
    const engine = new Engine(company());
    engine.script = () => json({ error: "unauthorized" }, 403);
    mountBuilder({ engine, viewer: () => READER });
    expect(await screen.findByText("Reading the configuration needs Admin access.")).toBeDefined();
    expect(screen.getByRole("button", { name: "Sign in as somebody else" })).toBeDefined();
  });

  /*
   * A SIGNED-IN READER THE CONFIGURATION IS GUARDED FROM, on a company the org
   * snapshot names, reads that snapshot: the builder draws it read-only, every
   * edit held with the org write gate's own reason, checks nothing and so
   * draws no check pill, and follows the snapshot as the company changes.
   * Until the snapshot has arrived it decides nothing, because a refusal on a
   * node running no company is the guarded screen. Mutation: answer every
   * signed-in refusal with `guarded` and the reader meets a refusal where the
   * org used to be; read the snapshot once and a change to the company is
   * never drawn; read the configuration for this reader, and a refused GET
   * goes out.
   */
  test("a signed-in reader refused the configuration reads the org snapshot, held, and follows it", async () => {
    const engine = new Engine(company());
    engine.script = (r) => (r.path === "/config" ? json({ error: "unauthorized" }, 403) : null);
    const { store } = mountBuilder({ engine, org: null, viewer: () => READER });
    await settled(() => engine.requests.length);
    expect(screen.queryByText("Reading the configuration needs Admin access.")).toBeNull();
    expect(screen.queryByText("CEO")).toBeNull();

    const roles = [{ name: "CEO", handle: "ceo" }];
    act(() => store.applySnapshot({ org: withDerived({ name: "Acme", roles, units: [] }) }));
    expect(await screen.findByText("CEO")).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    await waitFor(() =>
      expect(screen.getByTestId("edits held").textContent).toBe(CONFIG_WRITE_REASONS.no_grant),
    );
    expectAddHeld(CONFIG_WRITE_REASONS.no_grant);
    expect(screen.queryByText("Configuration OK")).toBeNull();
    expect(screen.queryByText("Reading the configuration needs Admin access.")).toBeNull();

    const grown = [...roles, { name: "Analyst", handle: "analyst" }];
    act(() => store.applyOrg(withDerived({ name: "Acme", roles: grown, units: [] })));
    expect(await screen.findByText("Analyst")).toBeDefined();
    expect(engine.checks()).toHaveLength(0);
    // NOT EVEN ASKED: the engine refuses the configuration to a reader with no
    // Admin who leads no unit, so the read only logged a 403 in the console.
    expect(engine.sent("GET")).toHaveLength(0);
  });

  // A READER WHO MAY CHANGE THE ORG has nothing held.
  test("an editor finds no edit held", async () => {
    mountBuilder({ engine: new Engine(company()) });
    await screen.findByText("Configuration OK");
    expect(screen.getByTestId("edits held").textContent).toBe("");
  });

  // SIGNING IN IS ALL IT TAKES. The posture is read again once the frame
  // learns who is reading, so a person who was asked to sign in edits
  // without reloading the page.
  test("a sign-in that brings the grant opens edit mode without a reload", async () => {
    let who: unknown = NOBODY;
    const engine = new Engine(company());
    engine.script = () => (who === NOBODY ? json({ error: "unauthorized" }, 401) : null);
    const { store } = mountBuilder({ engine, viewer: () => who });
    expect(await screen.findByText("Editing the organization needs you to sign in.")).toBeDefined();
    who = EDITOR;
    reconnect(store);
    expect(await screen.findByText("Configuration OK")).toBeDefined();
    expect(screen.getByText("editable")).toBeDefined();
  });

  test("a plain 404 is a process that does not serve the configuration", async () => {
    const engine = new Engine(company());
    engine.script = () => new Response("404 page not found", { status: 404 });
    mountBuilder({ engine });
    expect(await screen.findByText("This process does not serve the configuration")).toBeDefined();
  });

  test("a body that is not JSON is a process that does not serve the configuration", async () => {
    const engine = new Engine(company());
    engine.script = () => new Response("<html></html>", { status: 200 });
    mountBuilder({ engine });
    expect(await screen.findByText("This process does not serve the configuration")).toBeDefined();
  });

  // GET /config ANSWERS THE FLEET'S POINTER, so a coordination store between
  // leaders refuses it with 503 `unavailable`, saying why in `reason`.
  test("a fleet whose pointer cannot be read says why, with Retry", async () => {
    const engine = new Engine(company());
    engine.script = () =>
      json(
        {
          error: "unavailable",
          message: "The engine cannot serve this right now.",
          reason: "the fleet's activation could not be read from the coordination store",
        },
        503,
      );
    mountBuilder({ engine });
    expect(await screen.findByText("The engine could not serve the configuration")).toBeDefined();
    expect(
      screen.getByText("the fleet's activation could not be read from the coordination store"),
    ).toBeDefined();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
  });

  test("an engine that never answers is unreachable", async () => {
    const engine = new Engine(company());
    engine.script = () => Promise.reject(new TypeError("Failed to fetch"));
    mountBuilder({ engine });
    expect(await screen.findByText("The engine could not be reached")).toBeDefined();
  });

  // THE TOOLBAR SAYS SO WHERE IT IS READ. An entry that is offered, pressed,
  // and answers only into a live region nobody sees is worse than one marked
  // unavailable.
  //
  // Driven through `guarded`, which is now the halting check state this is
  // about, as a whole-company draft meets it: a check landing on a node that
  // refuses admin. `readonly` was the other one, and it went with the 503
  // `no_control_plane` that was its only producer: no process serves the API
  // without the coordination store that refusal described.
  test("a halted builder holds the toolbar's Add, saying why", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("dry_run") === "true" ? refusedByANodeRefusingAdmin() : null;
    mountBuilder({ engine, viewer: () => BOUND_ADMIN });
    expect(await screen.findByText("Needs Admin access")).toBeDefined();

    // An edit is refused before it reaches the log, and says why.
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() =>
      expect(liveRegion().textContent).toBe(`Editing is paused. ${CONFIG_WRITE_REASONS.no_grant}`),
    );
    // The same reason, on the Add a press would refuse.
    expectAddHeld(CONFIG_WRITE_REASONS.no_grant);
  });

  // ADD IS ITS OWN MENU, an outline beside Review and save, and it is the
  // same on both tabs: a selection never takes its place.
  test("a reader who may change the org adds from an outline menu on both tabs", async () => {
    const { view } = mountBuilder({ engine: new Engine(company()) });
    await screen.findByText("Configuration OK");
    const add = () => screen.getByRole("button", { name: "Add to the organization" });
    expect(add().getAttribute("aria-haspopup")).toBe("menu");
    expect(add().getAttribute("aria-disabled")).toBeNull();
    expect(add().textContent).toBe("Add");
    // The brand's outline, the toolbar's one fill kept for the save.
    expect(add().className).toMatch(/outline/);
    expect(screen.getByRole("button", { name: "Review and save" }).className).not.toMatch(
      /outline/,
    );
    expect(view.container.querySelector(".org-builder-act")!.contains(add())).toBe(true);

    // A selection adds the selection's own menu beside it, and leaves Add as it is.
    fireEvent.click(screen.getByRole("button", { name: "Select CEO" }));
    await screen.findByRole("button", { name: "Actions for CEO, selected" });
    fireEvent.click(add());
    const menu = await screen.findByRole("menu", { name: "Add to the organization" });
    expect(labels(menu)).toEqual(["Add unit", "Add agent seat", "Add human seat"]);
    for (const item of within(menu).getAllByRole("menuitem")) {
      expect(item.getAttribute("aria-disabled")).not.toBe("true");
    }
    fireEvent.keyDown(menu, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());

    await openTab("table");
    await waitFor(() => expect(screen.queryByText("Drawing the structure chart")).toBeNull());
    expect(add().getAttribute("aria-haspopup")).toBe("menu");
  });
});

describe("the views the builder hosts", () => {
  /** A view that registers `handle` exactly as `useBuilderView` does, counting each registration. */
  function registering(handle: BuilderViewHandle, count: { n: number }) {
    return function RegisteringView() {
      const { registerView } = useBuilder();
      useEffect(() => {
        count.n++;
        return registerView(handle);
      }, [registerView]);
      return <FakeView />;
    };
  }

  // THE TOOLBAR AND THE BUILDER ACT THROUGH THE VIEW ON SCREEN: Expand all,
  // Collapse all and the focus that follows an operation all reach the
  // handle the mounted view registered, and it is registered once.
  test("the toolbar and every operation reach the mounted view's handle", async () => {
    const handle = { focusNode: vi.fn(), expandAll: vi.fn(), collapseAll: vi.fn() };
    const count = { n: 0 };
    const engine = new Engine(company());
    const { checkChange } = mountBuilder({
      engine,
      surfaces: { ...fakeSurfaces, canvas: registering(handle, count) },
    });
    await screen.findByText("Configuration OK");
    const registered = count.n;

    fireEvent.click(screen.getByRole("button", { name: "Expand all" }));
    expect(handle.expandAll).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Collapse all" }));
    expect(handle.collapseAll).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(handle.focusNode).toHaveBeenCalledWith("seat:ceo"));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    await screen.findByText("Configuration OK");
    // An edit and its answer changed the state twice, and the view was not
    // registered again for either.
    expect(count.n).toBe(registered);
  });

  // EXPAND ALL AND COLLAPSE ALL FIT THE CHART AGAIN, even where no branch
  // moves (everything already open): the canvas is handed a request that
  // changes on each press. Mutation: drop the request and it stays at 0.
  test("Expand all and Collapse all each ask the chart to fit again", async () => {
    mountBuilder({ engine: new Engine(company()) });
    await screen.findByText("Configuration OK");
    const asked = () => screen.getByTestId("fit requests").textContent;
    expect(asked()).toBe("0");
    fireEvent.click(screen.getByRole("button", { name: "Expand all" }));
    expect(asked()).toBe("1");
    fireEvent.click(screen.getByRole("button", { name: "Collapse all" }));
    expect(asked()).toBe("2");
  });

  /*
   * THE TOOLBAR MIRRORS THE SELECTION ON THE CHART ALONE. A chart's card holds
   * no control a keyboard reaches, so the toolbar carries its actions, named
   * on a quiet trigger by the node; a table row draws its own, and a second
   * copy in the toolbar would be one action with two owners. The selection
   * itself carries between the tabs.
   */
  test("the toolbar offers the selection's actions on the chart tab, and not on the table", async () => {
    mountBuilder({ engine: new Engine(company()) });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Select CEO" }));
    const actions = await screen.findByRole("button", { name: "Actions for CEO, selected" });
    expect(actions.textContent).toBe("CEO");

    await openTab("table");
    expect(location.hash).toContain("seat=ceo");
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Actions for CEO, selected" })).toBeNull(),
    );
    expect(screen.getByRole("button", { name: "Add to the organization" })).toBeDefined();

    await openTab("chart");
    expect(await screen.findByRole("button", { name: "Actions for CEO, selected" })).toBeDefined();
  });

  /*
   * THE ARRANGEMENT IS ONE SWITCH DRAWN BY THE CHART, "Reporting view" at the
   * start of the canvas's own controls, and the page still owns it: it writes
   * the builder's own section param. It sat in the page toolbar, naming two
   * things that exist only inside the canvas, 800px from them.
   */
  test("the Reporting view switch is handed to the canvas's start, and writes the chart it draws", async () => {
    const engine = new Engine(company());
    const { view } = mountBuilder({ engine });
    expect(await screen.findByText("Drawing the structure chart")).toBeDefined();
    const toolbar = view.container.querySelector(".org-builder-toolbar")!;
    const reporting = () =>
      screen.getByRole<HTMLInputElement>("switch", { name: "Reporting view" });
    // Inside what the builder hands the canvas's start, not in the toolbar.
    expect(toolbar.contains(reporting())).toBe(false);
    expect(screen.getByTestId("canvas start").contains(reporting())).toBe(true);
    expect(reporting().checked).toBe(false);
    expect(location.hash).not.toContain("chart=");

    fireEvent.click(reporting());
    expect(await screen.findByText("Drawing the reporting chart")).toBeDefined();
    // A section, so the chart on screen is in the URL and a link opens it.
    expect(location.hash).toContain("chart=reporting");
    expect(reporting().checked).toBe(true);

    fireEvent.click(reporting());
    expect(await screen.findByText("Drawing the structure chart")).toBeDefined();
    expect(location.hash).not.toContain("chart=reporting");
    expect(reporting().checked).toBe(false);
  });

  // AND A LINK TO THE REPORTING CHART opens on it, the switch on.
  test("an address naming the reporting chart draws it, its switch on", async () => {
    mountBuilder({ engine: new Engine(company()), hash: "#/company/org?chart=reporting" });
    expect(await screen.findByText("Drawing the reporting chart")).toBeDefined();
    expect(screen.getByRole<HTMLInputElement>("switch", { name: "Reporting view" }).checked).toBe(
      true,
    );
  });

  /*
   * AN ADD IS THE ONE REQUEST THIS BUILDER DOES NOT ALWAYS ANSWER WITH A DIALOG.
   * The structure chart draws the form in the ghost of the node about to
   * exist, on the branch it will hang from; nothing is mounted over the
   * picture, and the picture is not pushed back either, so `about` stays
   * empty for it.
   */
  test("an add is handed to the structure chart rather than opened over it", async () => {
    mountBuilder({ engine: new Engine(company()) });
    await screen.findByText("Drawing the structure chart");
    fireEvent.click(screen.getByRole("button", { name: "Add to the organization" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
    expect(await screen.findByText("Adding agent to the company")).toBeDefined();
    expect(screen.queryByRole("dialog")).toBeNull();
    // Not a surface ABOUT a node: the chart is neither dimmed nor moved off
    // what the reader was looking at, because the ghost is what it eases onto.
    expect(screen.queryByText(/^About /)).toBeNull();
  });

  /*
   * AND THE TWO VIEWS WITH NO PLACE TO DRAW IT IN STILL ASK IN ONE. The table
   * is a grid of the rows that exist; the reporting chart draws who reports to
   * whom, which is derived, and has no slot a seat can take before it has a
   * manager. Both fall back to the same form in a dialog rather than to a
   * second, quieter add.
   */
  test("an add asked from the table or the reporting chart is a dialog", async () => {
    mountBuilder({ engine: new Engine(company()), hash: "#/company/org/table" });
    await screen.findByText("Configuration OK");
    expect(screen.queryByText(/^Drawing the /)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Add to the organization" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
    expect(await screen.findByRole("dialog", { name: "Add to Acme" })).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    await openTab("chart");
    await screen.findByText("Drawing the structure chart");
    fireEvent.click(screen.getByRole("switch", { name: "Reporting view" }));
    await screen.findByText("Drawing the reporting chart");
    fireEvent.click(screen.getByRole("button", { name: "Add to the organization" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
    expect(await screen.findByRole("dialog", { name: "Add to Acme" })).toBeDefined();
  });

  /*
   * AND AN ADD FOLLOWS THE VIEW. The request is the builder's, not the chart's,
   * so moving to the table while a ghost is open asks the same question in the
   * dialog rather than leaving the reader with a form they can no longer see.
   */
  test("an add open in the chart becomes a dialog when the reader leaves the chart", async () => {
    mountBuilder({ engine: new Engine(company()) });
    await screen.findByText("Drawing the structure chart");
    fireEvent.click(screen.getByRole("button", { name: "Add to the organization" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Add unit" }));
    await screen.findByText("Adding unit to the company");
    await openTab("table");
    expect(await screen.findByRole("dialog", { name: "Add to Acme" })).toBeDefined();
    await openTab("chart");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText("Adding unit to the company")).toBeDefined();
  });

  /*
   * AND SO IS THE FULLSCREEN TOGGLE, at the end of the chart's own zoom bar.
   * The table has no canvas to hang it in, so it stays in the toolbar there:
   * one control, in the one place each view puts it, never both.
   */
  test("the fullscreen toggle follows the view: the chart's corner, or the toolbar", async () => {
    Object.defineProperty(Element.prototype, "requestFullscreen", {
      configurable: true,
      value: () => Promise.resolve(),
    });
    Object.defineProperty(document, "fullscreenEnabled", { configurable: true, value: true });
    const engine = new Engine(company());
    const { view } = mountBuilder({ engine });
    await screen.findByText("Drawing the structure chart");
    const toolbar = () => view.container.querySelector(".org-builder-toolbar")!;
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Fullscreen" })).not.toBeNull(),
    );
    const toggle = () => screen.getByRole("button", { name: "Fullscreen" });
    expect(toolbar().contains(toggle())).toBe(false);

    await openTab("table");
    await waitFor(() => expect(screen.queryByText("Drawing the structure chart")).toBeNull());
    expect(toolbar().contains(toggle())).toBe(true);
  });

  /*
   * ONE QUESTION, ONE SHAPE. The node editor asks exactly this and asks it as
   * a prompt; the builder's own asked it as a framed dialog with a head band, a
   * mark and a close control that did what the Keep editing two inches below
   * it did. Announced as an `alertdialog`, so the consequence is read with the
   * name rather than after it.
   */
  test("discarding the draft is asked as a prompt, not a framed dialog", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() =>
      expect(
        (screen.getByRole("button", { name: "Discard changes" }) as HTMLButtonElement).disabled,
      ).toBe(false),
    );
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    const prompt = await screen.findByRole("alertdialog", { name: "Discard changes?" });
    // The two answers and NOTHING ELSE: a framed dialog draws a close control
    // in its head band that does exactly what Keep editing does two inches
    // below it, which is the third control this prompt does not have.
    expect(
      [...prompt.querySelectorAll("button")].map(
        (b) => b.getAttribute("aria-label") ?? b.textContent,
      ),
    ).toEqual(["Keep editing", "Discard changes"]);
  });

  /*
   * WHICH NODE A SURFACE IS ABOUT reaches the chart, so it can push itself
   * back behind the decision: an add is about the PARENT the child will hang
   * from, and closing draws the chart plainly again.
   */
  test("the chart is told which node an open surface is about", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    expect(screen.queryByText(/^About /)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Select CEO" }));
    fireEvent.click(await screen.findByRole("button", { name: "Actions for CEO, selected" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Edit reports" }));
    expect(await screen.findByText("About seat:ceo")).toBeDefined();
  });
});

describe("checking the draft", () => {
  // A PHONE'S TOOLBAR KEEPS ONE ROW WITH NOTHING TO SAVE: Review and save,
  // held, is in its More, and the button, wide windows only, comes back to
  // every width with a change. Drawn beside Add it wrapped the toolbar onto a
  // third row. Mutation: leave the button at every width, or keep the entry
  // in the menu once there is a change.
  test("on a phone Review and save is in the More menu until there is a change", async () => {
    const engine = new Engine(company());
    const { checkChange } = mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    const button = () => screen.getByRole("button", { name: "Review and save" });
    const more = async () => {
      fireEvent.click(screen.getByRole("button", { name: "More builder actions" }));
      const menu = await screen.findByRole("menu", { name: "More builder actions" });
      const found = within(menu)
        .getAllByRole("menuitem")
        .find((item) => menuEntryLabel(item).startsWith("Review and save"));
      fireEvent.keyDown(menu, { key: "Escape" });
      await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
      return found;
    };
    expect(button().closest(".org-builder-wide")).not.toBeNull();
    const held = await more();
    expect(held?.getAttribute("aria-disabled")).toBe("true");
    expect(held?.textContent).toContain("There are no changes to save.");

    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    expect(button().closest(".org-builder-wide")).toBeNull();
    expect(await more()).toBeUndefined();
  });

  test("an edit is checked as the merge patch a save would send", async () => {
    const engine = new Engine(company());
    const { checkChange } = mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    const patch = engine.checks()[1]!.body as { roles: { name: string; goal: string }[] };
    expect(Object.keys(patch)).toEqual(["roles"]);
    expect(patch.roles[0]).toMatchObject({ name: "CEO", goal: "Lead and more" });
  });

  test("problems the engine reports are placed on the seat they name", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("dry_run") === "true" && r.method === "PATCH" && "roles" in (r.body as object)
        ? refusal([
            {
              path: "roles[1].goal",
              segments: ["roles", 1, "goal"],
              kind: "invalid",
              message: "roles[1].goal: too long",
            },
          ])
        : null;
    const { checkChange } = mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    expect(await screen.findByText("1 configuration problem")).toBeDefined();
    expect(screen.getByTestId("problems Designer").textContent).toBe("1");
    expect(screen.getByTestId("problems CEO").textContent).toBe("0");
  });

  test("the live region says what an edit did, and undo from the keyboard reverts it", async () => {
    const engine = new Engine(company());
    const { checkChange } = mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    const region = liveRegion;
    await waitFor(() => expect(region().textContent).toBe("Edited CEO: goal."));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    expect(JSON.stringify(engine.checks()[1]!.body)).toContain("Lead and more");
    // Ctrl or Command with Z, from anywhere in the page that is not a text
    // field. The region says the edit was undone: the bare sentence would
    // tell a screen reader it was just made. Each step is checked on its own,
    // so the undo's check is told from the load's, which also sent `{}`.
    fireEvent.keyDown(document.body, { key: "z", code: "KeyZ", ctrlKey: true });
    await waitFor(() => expect(region().textContent).toBe("Undone: Edited CEO: goal."));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(3));
    expect(engine.checks()[2]!.body).toEqual({});
    // And Shift with it redoes, saying so.
    fireEvent.keyDown(document.body, { key: "Z", code: "KeyZ", ctrlKey: true, shiftKey: true });
    await waitFor(() => expect(region().textContent).toBe("Redone: Edited CEO: goal."));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(4));
    expect(JSON.stringify(engine.checks()[3]!.body)).toContain("Lead and more");
  });

  test("undo is left to a text field that has focus", async () => {
    const engine = new Engine(company());
    const { advance, checkChange, clock } = mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    // The edit's check is SENT, and its answer drawn a render later; the case
    // reads that answer last, so it waits for it here, where nothing else is
    // outstanding, rather than racing it to the end.
    await screen.findByText("Configuration OK");
    const field = document.createElement("input");
    document.querySelector(".org-builder")!.appendChild(field);
    fireEvent.keyDown(field, { key: "z", code: "KeyZ", metaKey: true });
    // An undo the builder took would have changed the draft while the key
    // was handled, arming the check's debounce: none is armed, and running
    // the debounce out sends nothing.
    expect(clock.pending).toBe(0);
    advance(CHECK_DEBOUNCE_MS);
    expect(engine.checks()).toHaveLength(2);
    expect(screen.getByText("Configuration OK")).toBeDefined();
    field.remove();
  });
});

// ONE CHECK PER DRAFT AND BASE. On a cold open the draft is read from
// `/config` while the socket's snapshot delivers the company, and the snapshot
// is the configuration the read already returned rather than an apply: checked
// again, the builder sent the same dry run twice on every such open and aborted
// the first. A push that REPLACES an org the tab already held is an apply.
describe("what an org push re-checks", () => {
  test("the snapshot's first org is not an apply, and the draft is checked once", async () => {
    const engine = new Engine(company());
    const { store, clock } = mountBuilder({ engine, org: null });
    await screen.findByText("Configuration OK");
    expect(engine.checks()).toHaveLength(1);
    act(() => store.applySnapshot({ org: named }));
    await settled(() => engine.requests.length);
    expect(clock.pending).toBe(0);
    expect(engine.checks()).toHaveLength(1);

    act(() => store.applyOrg({ ...named }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    expect(engine.checks()[1]!.body).toEqual(engine.checks()[0]!.body);
  });

  test("an org the tab held when the builder opened is re-checked when it is replaced", async () => {
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine, org: named });
    await screen.findByText("Configuration OK");
    expect(engine.checks()).toHaveLength(1);
    act(() => store.applyOrg({ ...named }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
  });
});

// NO PROVIDER, NO TURN. The builder writes none, so it says where one comes
// from rather than leaving agents whose work waits with nothing to say why.
test("a company with no model provider is told so, and one with a provider is not", async () => {
  const without = company();
  delete without.providers;
  const engine = new Engine(without);
  mountBuilder({ engine });
  // The engine applies the company and holds its agents' work, so the
  // caution says the agents cannot work rather than claiming nothing is applied.
  const caution = (
    await screen.findByText(/No model provider is set, so agents can’t work\./)
  ).closest(".crewlet-callout") as HTMLElement;
  // It sends the reader to Settings › Models to add one, signed in and tested.
  expect(within(caution).getByRole("link", { name: "Settings, Models" }).getAttribute("href")).toBe(
    "#/settings/models?flow=add",
  );
  // And it gives the steps that add one from a terminal, the card's own: the
  // declaration the engine walks from a company only the store holds.
  fireEvent.click(within(caution).getByRole("button", { name: "The steps" }));
  expect(within(caution).getByText(/crewlet config export > company\.yaml/).textContent).toBe(
    PROVIDER_STEPS,
  );
  cleanup();

  const engineWith = new Engine(company());
  mountBuilder({ engine: engineWith });
  await screen.findByText("Configuration OK");
  expect(screen.queryByText(/No model provider is set/)).toBeNull();
});

describe("the selection in the URL", () => {
  // A LINK NAMES THE UNIT BY ITS KEY, which is its identity: a rename moves
  // nothing a link was made from, so a link shared before it opens the same
  // unit after.
  test("a selected unit is named in the URL by its key, which a rename leaves alone", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Select Engineering" }));
    await waitFor(() => expect(location.hash).toContain("unit=engineering"));

    fireEvent.click(screen.getByRole("button", { name: "Rename Engineering" }));
    await screen.findByRole("button", { name: "Select Engineering Two" });
    expect(location.hash).toContain("unit=engineering");
    expect(location.hash).not.toContain("Engineering");
  });

  // A CLICK ON NOTHING LETS THE SELECTION GO, and only a click on nothing: a
  // control acts and keeps it, and nothing under an open dialog is the space
  // beside the views. A selection used to stay put wherever the reader
  // clicked next.
  test("a click on nothing clears the selection, and a click on a control keeps it", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Select Engineering" }));
    await waitFor(() => expect(location.hash).toContain("unit=engineering"));

    // The toolbar's own actions for the selection are a control.
    fireEvent.click(
      await screen.findByRole("button", { name: "Actions for Engineering, selected" }),
    );
    const menu = await screen.findByRole("menu", { name: "Actions for Engineering, selected" });
    fireEvent.keyDown(menu, { key: "Escape" });
    expect(location.hash).toContain("unit=engineering");

    // Words on the page are nothing to select.
    fireEvent.click(screen.getByText("editable"));
    await waitFor(() => expect(location.hash).not.toContain("unit="));
    expect(screen.queryByRole("button", { name: "Actions for Engineering, selected" })).toBeNull();
  });

  test("a click under an open dialog keeps the selection the dialog is about", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Select CEO" }));
    await waitFor(() => expect(location.hash).toContain("seat=ceo"));
    fireEvent.click(await screen.findByRole("button", { name: "Actions for CEO, selected" }));
    const menu = await screen.findByRole("menu", { name: "Actions for CEO, selected" });
    fireEvent.click(within(menu).getByRole("menuitem", { name: /^Edit\b/ }));
    await screen.findByRole("dialog", { name: "Edit CEO" });
    fireEvent.click(screen.getByText("editable"));
    expect(location.hash).toContain("seat=ceo");
  });

  // THE COMPANY IS A NODE TOO, and the only one the draft's tree cannot
  // locate: it is the root rather than an element of a list. Read as absent,
  // the card an operator selected was deselected again on the next answer,
  // and the company's actions were unreachable from the toolbar, the one place
  // its principles are offered.
  test("the company stays selected, and the toolbar offers the company's actions", async () => {
    const engine = new Engine(company());
    const { checkChange } = mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Select the company" }));
    const actions = await screen.findByRole("button", { name: "Actions for Acme, selected" });
    fireEvent.click(actions);
    const menu = await screen.findByRole("menu", { name: "Actions for Acme, selected" });
    // Nothing moves or deletes the document the company IS.
    expect(labels(menu)).toEqual([
      "Add unit",
      "Add agent seat",
      "Add human seat",
      "Edit principles",
      "Rename company",
    ]);
    fireEvent.keyDown(menu, { key: "Escape" });

    // And it survives the next answer about the draft, which is what a
    // locate-based reading of the selection did not.
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    await screen.findByText("Configuration OK");
    expect(screen.getByRole("button", { name: "Actions for Acme, selected" })).toBeDefined();
  });

  // THE PRINCIPLES ARE COMPANY › PRINCIPLES, in edit for a reader who may
  // write them and plainly for one who may not, and never a form of the
  // builder's own, whose draft is kept for the return.
  test("the principles open Company › Principles, in edit only for a reader who may write them", async () => {
    mountBuilder({ engine: new Engine(company()) });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Open the principles" }));
    await waitFor(() => expect(location.hash).toBe("#/company/principles?edit=mission"));
    expect(screen.queryByRole("dialog")).toBeNull();
    cleanup();

    // A READER WITH NO ADMIN reads the org snapshot (the configuration is
    // Admin's), and the principles open for reading.
    mountBuilder({
      engine: new Engine(company()),
      org: named,
      viewer: () => ({ ...EDITOR, grants: ["worklet", "doclet"], leads: [] }),
    });
    fireEvent.click(await screen.findByRole("button", { name: "Open the principles" }));
    await waitFor(() => expect(location.hash).toBe("#/company/principles"));
  });

  test("a link naming a seat selects it, opens nothing, and the toolbar offers that seat's actions", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine, hash: "#/company/org?seat=ceo" });
    await screen.findByText("Configuration OK");
    // A SELECTION ALONE IS A PLACE, not a request: a reload, a Back or a seat
    // page's crumb lands on the address the builder wrote, so the seat is
    // selected and no editor opens (`editor=open` asks for one, which
    // `editWiring.test.tsx` holds).
    const actions = await screen.findByRole("button", { name: "Actions for CEO, selected" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(location.hash).toContain("seat=ceo");
    expect(actions.getAttribute("aria-haspopup")).toBe("menu");
    fireEvent.click(actions);
    const menu = await screen.findByRole("menu", { name: "Actions for CEO, selected" });
    // The same actions, in the same order, as the seat's own card offers.
    expect(labels(menu)).toEqual(["Edit", "Open seat", "Edit reports", "Move to", "Delete"]);
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Open seat" }));
    // THE SEAT'S OWN PAGE, asked for the way every other link in this tree
    // asks: `seatPath` is the ONE place that knows where a seat lives, so
    // this claim survives the page moving and would fail a builder that built
    // the address itself. The path only — this harness keeps the Builder
    // mounted under any route, where the app replaces the whole screen.
    await waitFor(() =>
      expect(location.hash.split("?")[0]).toBe(href(seatPath({ handle: "ceo", name: "CEO" }))),
    );
  });

  // "COULD NOT REACH" ONLY WHERE THE ENGINE'S ANSWER NEVER ARRIVED. Every one
  // of these leaves the draft unchecked and saving allowed; the toolbar says
  // which happened, and a drain or an engine that failed is not a network.
  test.each([
    [
      "a gateway's page",
      () => new Response("<html>502</html>", { status: 502 }),
      "Could not reach the engine to check",
    ],
    ...([403, 413] as const).map((status): [string, () => Response, string] => [
      `a proxy's ${status} in the engine's shape, without its mark`,
      () =>
        new Response(JSON.stringify({ error: "forbidden" }), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
      "Could not reach the engine to check",
    ]),
    [
      "the engine's own 500",
      () => json({ error: "internal", detail: "store: disk full" }, 500),
      "The engine failed to check",
    ],
    [
      "a draining node",
      () => json({ error: "draining", detail: "shutting down" }, 503),
      "Not checked: this node is draining",
    ],
    [
      "a starting node",
      () => json({ error: "starting", detail: "recovering" }, 503),
      "Not checked: this node is starting",
    ],
  ])("a check met by %s is labelled as what happened", async (_, fail, label) => {
    const engine = new Engine(company());
    const { checkChange } = mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    engine.script = (r) => (r.query.get("dry_run") === "true" ? fail() : null);
    fireEvent.click(screen.getByRole("button", { name: "Add an analyst" }));
    await checkChange();
    await screen.findByText(label);
  });

  // A SEAT ADDED IN THIS DRAFT HAS NO SCREEN YET. A check derives its handle
  // at once, and a link built from that handle opened a seat the engine does
  // not have; its reports, like any seat's, can still be edited.
  test("a seat added in the draft is offered no screen, and its reports can be edited", async () => {
    const engine = new Engine(company());
    const { advance, checkChange } = mountBuilder({ engine });
    await screen.findByText("Configuration OK");
    // First with no answer about the new seat, so no handle is known for it.
    engine.script = (r) => (r.query.get("dry_run") === "true" ? json({ error: "bad" }, 502) : null);
    fireEvent.click(screen.getByRole("button", { name: "Add an analyst" }));
    await checkChange();
    await screen.findByText("The engine failed to check");
    fireEvent.click(screen.getByRole("button", { name: "Select Analyst" }));
    const actionsFor = async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Actions for Analyst, selected" }));
      return screen.findByRole("menu", { name: "Actions for Analyst, selected" });
    };
    let menu = await actionsFor();
    expect(within(menu).queryByRole("menuitem", { name: "Open seat" })).toBeNull();
    const reports = within(menu).getByRole("menuitem", { name: "Edit reports" });
    expect(reports.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.keyDown(menu, { key: "Escape" });

    // Then once a check has derived its handle: the URL names it, and there
    // is still no screen to open. The refused check is asked again after the
    // backoff's first wait.
    engine.script = () => null;
    advance(CHECK_BACKOFF_BASE_MS);
    expect(await screen.findByText("Configuration OK")).toBeDefined();
    await waitFor(() => expect(location.hash).toContain("seat=analyst"));
    menu = await actionsFor();
    expect(within(menu).queryByRole("menuitem", { name: "Open seat" })).toBeNull();
    expect(within(menu).getByRole("menuitem", { name: "Edit reports" })).toBeDefined();
  });
});

// A seat declaring no handle is keyed by its path until the first check
// names its handle; a selection made in between follows it rather than
// being dropped when that key goes.
test("a selection made before the engine described the company follows the seat", async () => {
  const engine = new Engine(company());
  let answer: () => void = () => {};
  engine.script = (r, e) =>
    r.query.get("dry_run") === "true"
      ? new Promise<Response>((resolve) => {
          answer = () => resolve(e.answer(r));
        })
      : null;
  mountBuilder({ engine });
  await waitFor(() => expect(engine.checks()).toHaveLength(1));
  fireEvent.click(await screen.findByRole("button", { name: "Select CEO" }));
  expect(await screen.findByRole("button", { name: "Actions for CEO, selected" })).toBeDefined();
  engine.script = () => null;
  act(() => answer());
  await screen.findByText("Configuration OK");
  await waitFor(() => expect(location.hash).toContain("seat=ceo"));
  expect(screen.getByRole("button", { name: "Actions for CEO, selected" })).toBeDefined();
});

describe("a revision saved by somebody else", () => {
  // NOTHING TO PROTECT, SO NOTHING TO ASK. A builder with no changes on it is
  // stood on the newer revision; the conflict banner, and the pause that
  // comes with it, are for a draft that holds work.
  test("an untouched draft is stood on the newer revision the org push reports", async () => {
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine, org: named });
    await screen.findByText("Configuration OK");
    const next = company();
    next.roles![1]!.goal = "Design things";
    engine.document = next;
    engine.revision = "r2";
    act(() => store.applyOrg(withDerived({ name: "Acme", roles: [], units: [] })));

    await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r2"'));
    expect(await screen.findByText("Configuration OK")).toBeDefined();
    expect(screen.queryByText("The configuration changed since you started editing.")).toBeNull();
    expect(screen.getByText("editable")).toBeDefined();
  });

  test("a draft with work is not moved: the change is offered as an update", async () => {
    const engine = new Engine(company());
    const { store, checkChange } = mountBuilder({ engine, org: named });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    await screen.findByText("Configuration OK");
    engine.document = company();
    engine.revision = "r2";
    const reads = engine.sent("GET").length;
    act(() => store.applyOrg(withDerived({ name: "Acme", roles: [], units: [] })));

    expect(await screen.findByText("The configuration changed")).toBeDefined();
    expect(engine.sent("GET")).toHaveLength(reads);
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more");
  });
});

describe("somebody else signing in mid-edit", () => {
  test("keeps the draft, reads the configuration again and checks for the new reader", async () => {
    let who: unknown = EDITOR;
    const engine = new Engine(company());
    const { store, checkChange } = mountBuilder({ engine, viewer: () => who });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    const reads = engine.sent("GET").length;

    who = { ...EDITOR, login: "second", owner: "second" };
    reconnect(store);
    await waitFor(() => expect(engine.sent("GET").length).toBe(reads + 1));
    await waitFor(() => expect(engine.checks()).toHaveLength(3));
    // The edit is still in the draft that was checked.
    expect(JSON.stringify(engine.checks()[2]!.body)).toContain("Lead and more");
  });

  test("a dry run refused for the new reader pauses editing while the configuration still reads", async () => {
    let who: unknown = EDITOR;
    const engine = new Engine(company());
    const { store, checkChange } = mountBuilder({ engine, viewer: () => who });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));

    // The new reader holds Admin as the node answering `viewer` and the read
    // serves it, while their checks land on a node that refuses admin — a
    // fleet mid-rollout, which legally disagrees about the posture — so only
    // the check finds out, and the configuration still reads.
    const second = { ...BOUND_ADMIN, login: "second", owner: "second", handle: "second" };
    engine.script = (r) =>
      r.query.get("dry_run") === "true" && who === second ? refusedByANodeRefusingAdmin() : null;
    who = second;
    reconnect(store);
    expect(await screen.findByText("Needs Admin access")).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more");
  });

  test("nobody signed in pauses editing without discarding the draft", async () => {
    let who: unknown = EDITOR;
    const engine = new Engine(company());
    const { store, checkChange } = mountBuilder({ engine, viewer: () => who });
    await screen.findByText("Configuration OK");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();
    await waitFor(() => expect(engine.checks()).toHaveLength(2));

    engine.script = () => (who === EDITOR ? null : json({ error: "unauthorized" }, 401));
    who = NOBODY;
    reconnect(store);
    expect(
      await screen.findByText(
        "Sign in to edit. Your draft is kept, and leaving to sign in asks first.",
      ),
    ).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    // The status names what is missing: nobody is signed in, not a grant.
    expect(await screen.findByText("Needs sign-in")).toBeDefined();
    expect(screen.queryByText("Needs Admin access")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() =>
      expect(liveRegion().textContent).toBe("Editing is paused. Nobody is signed in."),
    );
    // The seats are still drawn from the draft, not replaced by a refusal.
    expect(within(screen.getByRole("list", { name: "Seats" })).getByText("CEO")).toBeDefined();
  });
});

// A DECLINED STEP-UP IS NOT A MISSING GRANT. A configuration write asks for a
// recent proof, so a dry run sent an hour into a session is refused
// `403 step_up_required` once the person declines to confirm. They hold the
// grant and are signed in: the builder says what is asked of them, offers to
// check again — which asks them again — and keeps their draft, rather than
// sending them to an administrator and dropping the draft as a tab that
// changed hands.
describe("a step-up the person declined", () => {
  let uninstall: (() => void) | undefined;
  afterEach(() => {
    uninstall?.();
    uninstall = undefined;
  });

  test("is worded as a confirmation, keeps the draft, and checks again on asking", async () => {
    const engine = new Engine(company());
    const { checkChange } = mountBuilder({ engine, viewer: () => EDITOR });
    await screen.findByText("Configuration OK");

    let confirmed = false;
    engine.script = (r) =>
      r.query.get("dry_run") === "true" && !confirmed
        ? json({ error: "step_up_required", reason: "step_up" }, 403)
        : null;
    // No confirmer is installed, so the transport's question is declined.
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await checkChange();

    expect(await screen.findByText("Needs confirmation")).toBeDefined();
    expect(screen.queryByText("Needs Admin access")).toBeNull();
    expect(screen.getByText(`${ACT_ERRORS.step_up_required} Your draft is kept.`)).toBeDefined();
    expect(screen.queryByRole("button", { name: "Sign in as somebody else" })).toBeNull();
    // The same person declining is not the tab changing hands.
    expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain("Lead and more");

    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() =>
      expect(liveRegion().textContent).toBe(`Editing is paused. ${ACT_ERRORS.step_up_required}`),
    );

    // Asked again, the person confirms, and the replayed check answers.
    uninstall = setStepUpConfirmer(async () => {
      confirmed = true;
      return true;
    });
    const sent = engine.checks().length;
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));
    expect(await screen.findByText("Configuration OK")).toBeDefined();
    expect(engine.checks()).toHaveLength(sent + 2);
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more");
    expect(screen.getByText("editable")).toBeDefined();
  });

  // THE CONTROL: a refusal of the grant itself still names it — a check
  // landing on a node that refuses admin, which the engine words part by part
  // for a reader bound to a seat.
  test("a refusal of the grant still names Admin", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("dry_run") === "true" ? refusedByANodeRefusingAdmin() : null;
    mountBuilder({ engine, viewer: () => BOUND_ADMIN });
    expect(await screen.findByText("Needs Admin access")).toBeDefined();
    expect(screen.queryByText("1 configuration problem")).toBeNull();
    expect(screen.getByText("read only")).toBeDefined();
    expect(screen.getByRole("button", { name: "Sign in as somebody else" })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Check again" })).toBeNull();
  });
});

/**
 * THE CHART VIEW TAKES THE WINDOW, and gives it back.
 *
 * A canvas fills the box its screen gives it and clips, so the application
 * frame's scroller has to stop being one while the chart is on. The shell
 * used to work that out for itself, from a rule in this package's stylesheet
 * that reached up at a wrapper the shell drew; the design system's shell
 * draws no such wrapper, which left the canvas with no definite height
 * anywhere above it. The builder says so now, and this is what says it still
 * does, and still stops saying it on the outline.
 */
describe("the builder that fills the window", () => {
  function asked(hash: string): FillShape[] {
    const calls: FillShape[] = [];
    mountBuilder({
      engine: new Engine(company()),
      hash,
      wrap: (tree) => (
        <FillRequest.Provider value={(on) => calls.push(on)}>{tree}</FillRequest.Provider>
      ),
    });
    return calls;
  }

  test("the chart view asks the frame for the window's height", async () => {
    const calls = asked("#/company/org");
    // Not before the engine has answered: until then the builder draws a posture
    // screen, which is an ordinary column and scrolls like one.
    expect(calls).not.toContain(true);
    await waitFor(() => expect(calls).toContain(true));
  });

  test("the table view asks for nothing and leaves the scroller alone", async () => {
    const calls = asked("#/company/org/table");
    // The toolbar is what both views draw once the engine has answered, so
    // waiting for it is waiting for the same moment the case above measures.
    await screen.findByRole("toolbar", { name: "Organization builder" });
    expect(calls).not.toContain(true);
  });
});

// A MANAGED DOCUMENT IS WRITTEN SOMEWHERE ELSE (ADR-0030). The builder stays a
// reader of it: the chart is drawn, every edit is paused with who manages it,
// and the sentence says where the change is made instead — for whoever reads,
// since where the company is written is no question of who may write it.
describe("a document another system manages", () => {
  const MANAGED = { ...EDITOR, config_writer: false, config_managed_by: ["gitops"] };

  test("the viewer's answer pauses every edit, naming who manages it", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine, viewer: () => MANAGED });
    expect(await screen.findByText("CEO")).toBeDefined();
    expect(await screen.findByText(/Managed by gitops\. Change it there\./)).toBeDefined();

    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() =>
      expect(liveRegion().textContent).toBe(
        "Editing is paused. This company's configuration is managed by gitops.",
      ),
    );
    expectAddHeld("This company's configuration is managed by gitops.");
  });

  // AND THE ENGINE'S OWN REFUSAL DECIDES THE SAME WAY, when a check is
  // answered `config_managed` before the viewer has said anything: it halts,
  // and it is not mistaken for a grant the reader lacks — whose banner would
  // send somebody holding Admin to an administrator for nothing.
  test("a check answered config_managed halts as managed, not as a missing grant", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("dry_run") === "true"
        ? json(
            { error: "config_managed", managed_by: ["gitops"], detail: "managed", hint: "there" },
            403,
          )
        : null;
    mountBuilder({ engine });
    expect(await screen.findByText("Managed by another system")).toBeDefined();
    expect(screen.queryByText("Needs Admin access")).toBeNull();
    expect(screen.queryByText("Refused")).toBeNull();
    expect(screen.getByText(/Managed by gitops\. Change it there\./)).toBeDefined();
    const checks = engine.checks().length;
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(liveRegion().textContent).toContain("Editing is paused"));
    expect(engine.checks()).toHaveLength(checks);
  });

  test("a writer the viewer names edits as before", async () => {
    const engine = new Engine(company());
    mountBuilder({
      engine,
      viewer: () => ({ ...EDITOR, config_writer: true, config_managed_by: ["gitops"] }),
    });
    expect(await screen.findByText("Configuration OK")).toBeDefined();
    expect(screen.queryByText(/managed by gitops/i)).toBeNull();
  });

  // ITS MODEL IS ADDED WHERE IT IS WRITTEN. The terminal steps end in an
  // offline `crewlet config import`, which a managed document refuses to every
  // caller, and the managing system would replace any other write at its next
  // reconcile — so a managed company with no model is told where the model
  // comes from and is given no steps, a writer it names included: managed is
  // where the document is written, not who may edit it here.
  test.each([
    ["a reader it blocks", MANAGED],
    ["a writer it names", { ...EDITOR, config_writer: true, config_managed_by: ["gitops"] }],
  ])(
    "a managed company with no model is told it is added there, not given steps: %s",
    async (_, viewer) => {
      const without = company();
      delete without.providers;
      mountBuilder({ engine: new Engine(without), viewer: () => viewer });
      const caution = (
        await screen.findByText(/No model provider is set, so agents can’t work\./)
      ).closest(".crewlet-callout") as HTMLElement;
      expect(caution.textContent).toContain(
        "The company’s configuration is managed by gitops, so the model is added there.",
      );
      expect(within(caution).queryByRole("button", { name: "The steps" })).toBeNull();
      expect(document.body.textContent).not.toContain("crewlet config");
      expect(document.body.textContent).not.toContain("crewlet llm");
    },
  );
});

// WORK'S NEW PROJECT ARRIVES HERE, naming the unit by its key: the node's
// editor opens on its Project field holding the key, as a change still to
// apply, and the address lets go of the key so a reload asks nothing twice.
test("a link naming a unit and a project opens its editor on the Project field holding the key", async () => {
  const engine = new Engine(company());
  mountBuilder({
    engine,
    hash: "#/company/org?unit=engineering&project=ENG",
  });
  await screen.findByText("Configuration OK");
  const editor = await screen.findByRole("dialog", { name: "Edit Engineering" });
  const field = within(editor).getByRole("combobox", { name: /Tracker project/ });
  expect((field as HTMLInputElement).value).toBe("ENG");
  await waitFor(() => expect(document.activeElement).toBe(field));
  // A CHANGE STILL TO APPLY: the editor marks the part holding it as changed.
  expect(within(editor).getByRole("tab", { name: /^Work/ }).textContent).toContain("Changed");
  await waitFor(() => expect(location.hash).not.toContain("project="));
  expect(location.hash).toContain("unit=engineering");
});

// A NODE THAT ALREADY OWNS A PROJECT KEEPS IT: the key is offered only where
// there is none.
test("a project link to a node that owns one leaves its project as it is", async () => {
  const doc = company();
  doc.units![0]!.project = "CORE";
  const engine = new Engine(doc);
  mountBuilder({
    engine,
    hash: "#/company/org?unit=engineering&project=ENG",
  });
  await screen.findByText("Configuration OK");
  const editor = await screen.findByRole("dialog", { name: "Edit Engineering" });
  expect(
    (within(editor).getByRole("combobox", { name: /Tracker project/ }) as HTMLInputElement).value,
  ).toBe("CORE");
});

describe("the draft, for the assistant", () => {
  test("names the revision it was opened on, what it holds and each change, read only", async () => {
    setFramePage({
      screen: "structure",
      title: "Structure › Org chart",
      path: ["company", "org"],
      params: {},
      query: {},
    });
    const engine = new Engine(company());
    mountBuilder({ engine });
    expect(await screen.findByText("CEO")).toBeDefined();
    const open = () => pageSnapshot()!.context;
    const field = (path: string) => open().draft?.fields.find((f) => f.path === path);
    await waitFor(() =>
      expect(open().draft).toMatchObject({
        kind: "client",
        id: "company",
        revision: "r1",
        step: "visualization",
      }),
    );
    expect(field("scope")?.value).toBe("The whole company");
    expect(field("changes")).toMatchObject({ value: "", kind: "list" });
    expect(Number(field("seats")?.value)).toBeGreaterThan(0);
    // STRUCTURE IS CHANGED THROUGH THE BUILDER'S OWN DOOR, never draft.set.
    expect(open().draft!.fields.some((f) => f.editable)).toBe(false);
    expect(open().accepts).toContain("org.draft.apply_ops");
    act(() => {
      applyPageAction({
        correlation_id: "c1",
        action: "org.draft.apply_ops",
        payload: { ops: [{ op: "rename", seat: "designer", name: "Product designer" }] },
        source_revision: open().source_revision,
      });
    });
    await waitFor(() => expect(field("changes")?.value).toMatch(/Product designer/));
  });
});
