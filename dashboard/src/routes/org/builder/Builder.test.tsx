/**
 * The Builder lens decides its posture from what the engine answers, checks
 * every draft against the chart it was made on and the settings it would
 * write, and keeps the operator's work through a change of reader.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { type OrgProjection } from "~/protocol/index.ts";
import { useBuilder, type BuilderViewHandle } from "./BuilderContext.tsx";
import { menuEntryLabel } from "~/testing.tsx";
import { href } from "~/app/router.tsx";
import { seatPath } from "~/lib/seats.ts";
import { FillRequest } from "~/app/fill.tsx";
import {
  asReader,
  company,
  Engine,
  fakeSurfaces,
  FakeView,
  json,
  mountBuilder,
  refusal,
  rereadViewer,
} from "./testkit.tsx";

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
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

describe("the posture table", () => {
  test("a served configuration and chart open edit mode", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    expect(await screen.findByText("CEO")).toBeDefined();
    expect(await screen.findByText("No problems")).toBeDefined();
    // The chart is read with its runtime half asked for, and the first check
    // runs at once: it reads the chart again, and — the draft changing no
    // setting — the settings, never a dry run of a write nobody asked for.
    expect(engine.chartReads().length).toBeGreaterThanOrEqual(2);
    expect(engine.chartReads().every((r) => r.query.get("runtime") === "true")).toBe(true);
    expect(engine.checks()).toHaveLength(0);
  });

  test("a settings edit is checked as the merge patch a save would send", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const check = engine.checks()[0]!;
    // A PATCH conditional on the revision it read, carrying only what changed.
    expect(check.method).toBe("PATCH");
    expect(check.headers["If-Match"]).toBe('"r1"');
    expect(check.headers["Content-Type"]).toBe("application/merge-patch+json");
    expect(check.body).toEqual({ name: "Acme Labs" });
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
    mountBuilder({ engine, org: named });
    expect(
      await screen.findByText("This node has not caught up with the fleet's configuration yet."),
    ).toBeDefined();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
    // Never create mode: nothing is checked, let alone written.
    await new Promise((r) => setTimeout(r, 50));
    expect(engine.checks()).toHaveLength(0);
  });

  test("create mode waits for the org snapshot before deciding", async () => {
    const engine = new Engine(null);
    const { store } = mountBuilder({ engine, connected: false });
    await new Promise((r) => setTimeout(r, 50));
    expect(engine.checks()).toHaveLength(0);
    act(() => store.applyOrg(named));
    expect(
      await screen.findByText("This node has not caught up with the fleet's configuration yet."),
    ).toBeDefined();
  });

  test("a refusal with nobody signed in asks for a sign-in", async () => {
    const engine = new Engine(company());
    engine.script = () => json({ error: "unauthorized" }, 401);
    mountBuilder({ engine, query: asReader(() => "") });
    expect(
      await screen.findByText(/^Editing the organization needs a credential the engine accepts\./),
    ).toBeDefined();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeDefined();
  });

  // A SIGN-IN IN ANOTHER TAB IS ALL IT TAKES. The browser's cookie is shared
  // by its tabs, the viewer is read again, and the posture with it, so a
  // reader who signed in elsewhere edits here without reloading the page.
  test("a sign-in in another tab opens edit mode without a reload", async () => {
    let who = "";
    const engine = new Engine(company());
    engine.script = () => (who === "" ? json({ error: "unauthorized" }, 401) : null);
    const { store } = mountBuilder({ engine, query: asReader(() => who) });
    expect(
      await screen.findByText(/^Editing the organization needs a credential the engine accepts\./),
    ).toBeDefined();
    who = "jane.doe";
    rereadViewer(store);
    expect(await screen.findByText("No problems")).toBeDefined();
    expect(screen.getByText("editable")).toBeDefined();
  });

  // A REFUSAL ON AUTHORITY NAMES THE GRANT IT NAMED. A reader whose credential
  // the engine ACCEPTED and whose grants do not reach the configuration lacks
  // a grant, not a token: "needs an operator token" sent a person signed in
  // without `config:read` to look for a token they have no use for, and the
  // toolbar and the paused-editing reason said the same thing.
  test("a refusal that names a grant says which, wherever the lens says it", async () => {
    const engine = new Engine(company());
    engine.script = () =>
      json({ error: "unauthorized", reason: "no_grant", grants: ["config:read"] }, 403);
    mountBuilder({ engine });
    expect(
      await screen.findByText(
        "Editing the organization needs config:read, which the credential you presented does not carry.",
      ),
    ).toBeDefined();
    expect(screen.queryByText(/operator token/)).toBeNull();
  });

  test("a refusal of a signed-in reader says the session was refused", async () => {
    const engine = new Engine(company());
    engine.script = () => json({ error: "unauthorized" }, 401);
    mountBuilder({ engine, query: asReader(() => "jane.doe") });
    expect(await screen.findByText("The engine refused this browser's session.")).toBeDefined();
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
  // about. `readonly` was the other one, and it went with the 503
  // `no_control_plane` that was its only producer: no process serves the API
  // without the coordination store that refusal described.
  test("a halted lens marks the toolbar's add entries unavailable", async () => {
    const engine = new Engine(company());
    // The lens reads the company, and the check that follows is refused.
    let reads = 0;
    engine.script = (r) =>
      r.method === "GET" && r.path === "/chart" && ++reads > 1
        ? json({ error: "forbidden" }, 403)
        : null;
    mountBuilder({ engine, query: asReader(() => "reader.only") });
    expect(await screen.findByText("The engine refused the session")).toBeDefined();

    // An edit is refused before it reaches the log, and says why.
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(liveRegion().textContent).toContain("Editing is paused"));

    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    const menu = await screen.findByRole("menu", { name: "Add to the organization" });
    for (const name of ["Add unit", "Add agent seat", "Add human seat"]) {
      expect(within(menu).getByRole("menuitem", { name }).getAttribute("aria-disabled")).toBe(
        "true",
      );
    }
  });
});

describe("the views the lens hosts", () => {
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
    mountBuilder({ engine, surfaces: { ...fakeSurfaces, canvas: registering(handle, count) } });
    await screen.findByText("No problems");
    const registered = count.n;

    fireEvent.click(screen.getByRole("button", { name: "Expand all" }));
    expect(handle.expandAll).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Collapse all" }));
    expect(handle.collapseAll).toHaveBeenCalledTimes(1);

    const reads = engine.chartReads().length;
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(handle.focusNode).toHaveBeenCalledWith("seat:ceo"));
    await waitFor(() => expect(engine.chartReads().length).toBe(reads + 1));
    await screen.findByText("No problems");
    // An edit and its answer changed the state twice, and the view was not
    // registered again for either.
    expect(count.n).toBe(registered);
  });

  /*
   * THE SWITCH BETWEEN THE TWO CHARTS IS DRAWN BY THE CHART, in the canvas's
   * own corner where the console keeps it, and the page still owns it: it
   * writes the lens's own section param. It sat in the page toolbar, naming
   * two things that exist only inside the canvas, 800px from them.
   */
  test("the canvas is handed the chart the toolbar chooses, and the switch that chooses it", async () => {
    const engine = new Engine(company());
    const { view } = mountBuilder({ engine });
    expect(await screen.findByText("Drawing the structure chart")).toBeDefined();
    // Inside what the lens hands the canvas, not in the toolbar beside it.
    const toolbar = view.container.querySelector(".org-builder-toolbar")!;
    const reporting = screen.getByRole("tab", { name: "Reporting" });
    expect(toolbar.contains(reporting)).toBe(false);
    fireEvent.click(reporting);
    expect(await screen.findByText("Drawing the reporting chart")).toBeDefined();
    // A section, so the chart on screen is in the URL and a link opens it.
    expect(location.hash).toContain("chart=reporting");
  });

  /*
   * AN ADD IS THE ONE REQUEST THIS LENS DOES NOT ALWAYS ANSWER WITH A DIALOG.
   * The structure chart draws the form in the ghost of the node about to
   * exist, on the branch it will hang from; nothing is mounted over the
   * picture, and the picture is not pushed back either, so `about` stays
   * empty for it.
   */
  test("an add is handed to the structure chart rather than opened over it", async () => {
    mountBuilder({ engine: new Engine(company()) });
    await screen.findByText("Drawing the structure chart");
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
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
    mountBuilder({ engine: new Engine(company()), hash: "#/company?lens=builder&view=table" });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
    expect(await screen.findByRole("dialog", { name: "Add to the company" })).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    fireEvent.click(screen.getByRole("tab", { name: "Visualization" }));
    await screen.findByText("Drawing the structure chart");
    fireEvent.click(screen.getByRole("tab", { name: "Reporting" }));
    await screen.findByText("Drawing the reporting chart");
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
    expect(await screen.findByRole("dialog", { name: "Add to the company" })).toBeDefined();
  });

  /*
   * AND AN ADD FOLLOWS THE VIEW. The request is the lens's, not the chart's,
   * so moving to the table while a ghost is open asks the same question in the
   * dialog rather than leaving the reader with a form they can no longer see.
   */
  test("an add open in the chart becomes a dialog when the reader leaves the chart", async () => {
    mountBuilder({ engine: new Engine(company()) });
    await screen.findByText("Drawing the structure chart");
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Add unit" }));
    await screen.findByText("Adding unit to the company");
    fireEvent.click(screen.getByRole("tab", { name: "Table" }));
    expect(await screen.findByRole("dialog", { name: "Add to the company" })).toBeDefined();
    fireEvent.click(screen.getByRole("tab", { name: "Visualization" }));
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

    fireEvent.click(screen.getByRole("tab", { name: "Table" }));
    await waitFor(() => expect(screen.queryByText("Drawing the structure chart")).toBeNull());
    expect(toolbar().contains(toggle())).toBe(true);
  });

  /*
   * ONE QUESTION, ONE SHAPE. The node editor asks exactly this and asks it as
   * a prompt; the lens's own asked it as a framed dialog with a head band, a
   * mark and a close control that did what the Keep editing two inches below
   * it did. Announced as an `alertdialog`, so the consequence is read with the
   * name rather than after it.
   */
  test("discarding the draft is asked as a prompt, not a framed dialog", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
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
   * WHICH NODE A SURFACE IS ABOUT reaches the chart, so it can ease onto it
   * and push the rest of itself back: an add is about the PARENT the child
   * will hang from, and closing gives the reader their view back.
   */
  test("the chart is told which node an open surface is about", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    expect(screen.queryByText(/^About /)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Select CEO" }));
    fireEvent.click(await screen.findByRole("button", { name: "CEO" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Edit reports" }));
    expect(await screen.findByText("About seat:ceo")).toBeDefined();
  });
});

describe("checking the draft", () => {
  // THE CHART HAS NO DRY RUN, so an edit of a seat is checked by reading the
  // chart it would be written over, and a draft that changes no setting sends
  // no settings write to validate.
  test("an edit of a seat is checked against the chart, with no dry run", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    const reads = engine.chartReads().length;
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.chartReads().length).toBe(reads + 1));
    await screen.findByText("No problems");
    expect(engine.checks()).toHaveLength(0);
  });

  test("what the chart would refuse is placed on the seat, and the settings' problems on the company", async () => {
    const engine = new Engine(company());
    // A goal past the chart's cap, which a batch would refuse and no dry run exists to say.
    engine.seats.find((s) => s.handle === "designer")!.goal = "x".repeat(16 * 1024 + 1);
    engine.script = (r) =>
      r.query.get("dry_run") === "true"
        ? refusal([
            { path: "name", segments: ["name"], kind: "invalid", message: "name: too long" },
          ])
        : null;
    mountBuilder({ engine });
    expect(await screen.findByText("1 problem")).toBeDefined();
    expect(screen.getByTestId("problems Designer").textContent).toBe("1");
    // The control: a seat whose fields fit carries none.
    expect(screen.getByTestId("problems CEO").textContent).toBe("0");

    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    expect(await screen.findByText("2 problems")).toBeDefined();
    // The settings' problem is the company's, never a seat's.
    expect(screen.getByTestId("problems Designer").textContent).toBe("1");
    expect(screen.getByTestId("problems CEO").textContent).toBe("0");
  });

  test("the live region says what an edit did, and undo from the keyboard reverts it", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    const region = liveRegion;
    await waitFor(() => expect(region().textContent).toBe("Edited CEO: goal."));
    // Ctrl or Command with Z, from anywhere in the page that is not a text
    // field. The region says the edit was undone: the bare sentence would
    // tell a screen reader it was just made.
    const reads = engine.chartReads().length;
    fireEvent.keyDown(document.body, { key: "z", code: "KeyZ", ctrlKey: true });
    await waitFor(() => expect(region().textContent).toBe("Undone: Edited CEO: goal."));
    // Every generation is checked: the undone draft too.
    await waitFor(() => expect(engine.chartReads().length).toBe(reads + 1));
    // And Shift with it redoes, saying so.
    fireEvent.keyDown(document.body, { key: "Z", code: "KeyZ", ctrlKey: true, shiftKey: true });
    await waitFor(() => expect(region().textContent).toBe("Redone: Edited CEO: goal."));
    await waitFor(() => expect(engine.chartReads().length).toBe(reads + 2));
  });

  test("undo is left to a text field that has focus", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(liveRegion().textContent).toBe("Edited CEO: goal."));
    await screen.findByText("No problems");
    const reads = engine.chartReads().length;
    const field = document.createElement("input");
    document.querySelector(".org-builder")!.appendChild(field);
    fireEvent.keyDown(field, { key: "z", code: "KeyZ", metaKey: true });
    await new Promise((r) => setTimeout(r, 400));
    expect(engine.chartReads()).toHaveLength(reads);
    expect(liveRegion().textContent).toBe("Edited CEO: goal.");
    field.remove();
  });
});

// NO PROVIDER, NO TURN. The dashboard writes none, so the lens says where one
// comes from rather than leaving agents whose work waits with nothing to say
// why.
test("a company with no model provider is told so, and one with a provider is not", async () => {
  const without = company();
  delete without.settings.providers;
  const engine = new Engine(without);
  mountBuilder({ engine });
  // The engine applies the company and holds its agents' work, so the
  // caution says what waits rather than claiming nothing is applied.
  expect(
    await screen.findByText(/no agent seat takes a turn: work sent to a seat waits on its inbox/),
  ).toBeDefined();
  cleanup();

  const engineWith = new Engine(company());
  mountBuilder({ engine: engineWith });
  await screen.findByText("No problems");
  expect(screen.queryByText(/No model provider is configured/)).toBeNull();
});

describe("the selection in the URL", () => {
  // BY ITS KEY, the address the chart resolves: a unit's name is prose two
  // teams may share, and a link naming one by it opened whichever came first.
  test("a selected unit is named in the URL by its key, which a new address rewrites", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Select Engineering" }));
    await waitFor(() => expect(location.hash).toContain("unit=engineering"));

    // The control: a new NAME is prose, and the address in the URL stays.
    fireEvent.click(screen.getByRole("button", { name: "Rename Engineering" }));
    await waitFor(() => expect(liveRegion().textContent).toContain("Engineering Two"));
    expect(location.hash).toMatch(/unit=engineering$/);
    fireEvent.click(screen.getByRole("button", { name: "Readdress Engineering Two" }));
    await waitFor(() => expect(location.hash).toContain("unit=engineering-two"));
  });

  test("a link naming a unit by its key selects it, and one naming it by its name does not", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine, hash: "#/company?lens=builder&view=visualization&unit=engineering" });
    await screen.findByText("No problems");
    expect(await screen.findByRole("button", { name: "Engineering" })).toBeDefined();
    cleanup();

    mountBuilder({
      engine: new Engine(company()),
      hash: "#/company?lens=builder&view=visualization&unit=Engineering",
    });
    await screen.findByText("No problems");
    expect(screen.queryByRole("button", { name: "Engineering" })).toBeNull();
  });

  // THE COMPANY IS A NODE TOO, and the only one the draft's tree cannot
  // locate: it is the root rather than an element of a list. Read as absent,
  // the card an operator selected was deselected again on the next answer,
  // and the charter's Edit was unreachable from the toolbar.
  test("the company stays selected, and the toolbar offers the charter's actions", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Select the company" }));
    const actions = await screen.findByRole("button", { name: "Acme" });
    fireEvent.click(actions);
    const menu = await screen.findByRole("menu", { name: "Actions for Acme" });
    // Nothing moves or deletes the document the company IS.
    expect(labels(menu)).toEqual(["Add unit", "Add agent seat", "Add human seat", "Edit"]);
    fireEvent.keyDown(menu, { key: "Escape" });

    // And it survives the next answer about the draft, which is what a
    // locate-based reading of the selection did not.
    const reads = engine.chartReads().length;
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.chartReads().length).toBe(reads + 1));
    await screen.findByText("No problems");
    expect(screen.getByRole("button", { name: "Acme" })).toBeDefined();
  });

  test("a link naming a seat selects it, and the toolbar offers that seat's actions", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine, hash: "#/company?lens=builder&view=visualization&seat=ceo" });
    await screen.findByText("No problems");
    const actions = await screen.findByRole("button", { name: "CEO" });
    expect(actions.getAttribute("aria-haspopup")).toBe("menu");
    fireEvent.click(actions);
    const menu = await screen.findByRole("menu", { name: "Actions for CEO" });
    // The same actions, in the same order, as the seat's own card offers.
    expect(labels(menu)).toEqual([
      "Edit",
      "Open seat",
      "Edit reports",
      "Change to human seat",
      "Move to",
      "Delete",
    ]);
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

  // A SEAT ADDED IN THIS DRAFT HAS NO SCREEN YET. Its handle is typed with
  // it, so the URL names it at once, and a link built from that handle would
  // open a seat the engine does not have; its kind, like any seat's, can
  // still be changed.
  test("a seat added in the draft is offered no screen, and can change kind", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Add an analyst" }));
    fireEvent.click(await screen.findByRole("button", { name: "Select Analyst" }));
    await waitFor(() => expect(location.hash).toContain("seat=analyst"));
    fireEvent.click(await screen.findByRole("button", { name: "Analyst" }));
    const menu = await screen.findByRole("menu", { name: "Actions for Analyst" });
    expect(within(menu).queryByRole("menuitem", { name: "Open seat" })).toBeNull();
    const kind = within(menu).getByRole("menuitem", { name: "Change to human seat" });
    expect(kind.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.keyDown(menu, { key: "Escape" });

    // The control: a seat the chart holds is offered its screen.
    fireEvent.click(screen.getByRole("button", { name: "Select CEO" }));
    fireEvent.click(await screen.findByRole("button", { name: "CEO" }));
    const saved = await screen.findByRole("menu", { name: "Actions for CEO" });
    expect(within(saved).getByRole("menuitem", { name: "Open seat" })).toBeDefined();
  });
});

describe("a company saved by somebody else", () => {
  /** The chart a colleague's save leaves: the Designer's goal rewritten. */
  const colleagueSaves = (engine: Engine) => {
    engine.seats.find((s) => s.handle === "designer")!.goal = "Design things";
    engine.position += 1;
  };

  // NOTHING TO PROTECT, SO NOTHING TO ASK. A lens with no changes on it is
  // stood on the newer company; the conflict banner, and the pause that
  // comes with it, are for a draft that holds work.
  test("an untouched draft is stood on the chart the org push reports", async () => {
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine });
    await screen.findByText("No problems");
    colleagueSaves(engine);
    act(() => store.applyOrg(engine.orgPush()));

    await waitFor(() => expect(screen.getByText("editable")).toBeDefined());
    expect(await screen.findByText("No problems")).toBeDefined();
    expect(
      screen.queryByText("Somebody changed the org chart since you started editing."),
    ).toBeNull();
  });

  // AND ON A CHARTER A COLLEAGUE SAVED, though the draft changes no setting
  // and so has no dry run to be refused: the check reads the settings too.
  test("an untouched draft is stood on the settings revision a colleague saved", async () => {
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine });
    await screen.findByText("No problems");
    engine.settings = { ...engine.settings, mission: "Make better things" };
    engine.revision = "r2";
    const reads = engine.sent("GET").length;
    act(() => store.applyOrg(engine.orgPush()));
    // The check's read finds the revision, and the update reads the company.
    await waitFor(() => expect(engine.sent("GET").length).toBeGreaterThanOrEqual(reads + 2));
    await screen.findByText("No problems");

    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    // The dry run the edit sends is conditional on the revision it now stands on.
    await waitFor(() => expect(engine.checks().at(-1)?.headers["If-Match"]).toBe('"r2"'));
    expect(screen.queryByText("The settings changed since you started editing.")).toBeNull();
  });

  test("a draft with work is not moved: the change is offered as an update", async () => {
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(liveRegion().textContent).toBe("Edited CEO: goal."));
    await screen.findByText("No problems");
    colleagueSaves(engine);
    const settingsReads = engine.sent("GET").length;
    act(() => store.applyOrg(engine.orgPush()));

    expect(
      await screen.findByText("Somebody changed the org chart since you started editing."),
    ).toBeDefined();
    // Nothing was loaded over the draft: the settings were not read to replace it.
    expect(engine.sent("GET")).toHaveLength(settingsReads + 1);
    expect(screen.getByText("read only")).toBeDefined();
  });
});

describe("a new reader mid-edit", () => {
  test("keeps the draft, reads the company again and checks as the new reader", async () => {
    let who = "jane.doe";
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine, query: asReader(() => who) });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const reads = engine.chartReads().length;

    who = "sam.lee";
    rereadViewer(store);
    // Read again (the chart once for the load and once for the check), and checked.
    await waitFor(() => expect(engine.chartReads().length).toBe(reads + 2));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    // The edit is still in the draft that was checked.
    expect(engine.checks()[1]!.body).toEqual({ name: "Acme Labs" });
  });

  // THE CONTROL: a viewer read again as the SAME person is not a new reader,
  // and a lens that re-read on every reconnect would re-check a draft for
  // every blip of the socket.
  test("the same reader read again changes nothing", async () => {
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine, query: asReader(() => "jane.doe") });
    await screen.findByText("No problems");
    const reads = engine.chartReads().length;
    rereadViewer(store);
    await new Promise((r) => setTimeout(r, 50));
    expect(engine.chartReads()).toHaveLength(reads);
  });

  test("a dry run refused for the new reader pauses editing while the company still reads", async () => {
    let who = "jane.doe";
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine, query: asReader(() => who) });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(1));

    // Reads are served, writes and dry runs are refused: the new reader lacks
    // the right to write, which only the check can find out.
    engine.script = (r) =>
      r.query.get("dry_run") === "true" && who === "reader.only"
        ? json({ error: "forbidden" }, 403)
        : null;
    who = "reader.only";
    rereadViewer(store);
    expect(await screen.findByText("The engine refused the session")).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    expect(engine.checks().at(-1)!.body).toEqual({ name: "Acme Labs" });
  });

  test("a reader signed out elsewhere pauses editing without discarding the draft", async () => {
    let who = "jane.doe";
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine, query: asReader(() => who) });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(liveRegion().textContent).toBe("Edited CEO: goal."));

    engine.script = () => (who === "" ? json({}, 401) : null);
    who = "";
    rereadViewer(store);
    expect(
      await screen.findByText(
        /^Editing the organization needs a credential the engine accepts\..* Your draft is kept on this page\.$/,
      ),
    ).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    // The status names what is missing: no session was refused, none is held.
    expect(await screen.findByText("Needs a credential")).toBeDefined();
    expect(screen.queryByText("The engine refused the session")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() =>
      expect(liveRegion().textContent).toBe(
        "Editing is paused because no credential was presented.",
      ),
    );
    // The seats are still drawn from the draft, not replaced by a refusal.
    expect(within(screen.getByRole("list", { name: "Seats" })).getByText("CEO")).toBeDefined();
  });
});

/**
 * THE CHART LENS TAKES THE WINDOW, and gives it back.
 *
 * A canvas fills the box its screen gives it and clips, so the application
 * frame's scroller has to stop being one while the chart is on. The shell
 * used to work that out for itself, from a rule in this package's stylesheet
 * that reached up at a wrapper the shell drew; the design system's shell
 * draws no such wrapper, which left the canvas with no definite height
 * anywhere above it. The lens says so now, and this is what says it still
 * does, and still stops saying it on the outline.
 */
describe("the lens that fills the window", () => {
  function asked(hash: string): boolean[] {
    const calls: boolean[] = [];
    mountBuilder({
      engine: new Engine(company()),
      hash,
      wrap: (tree) => (
        <FillRequest.Provider value={(on) => calls.push(on)}>{tree}</FillRequest.Provider>
      ),
    });
    return calls;
  }

  test("the chart lens asks the frame for the window's height", async () => {
    const calls = asked("#/company?lens=builder&view=visualization");
    // Not before the engine has answered: until then the lens draws a posture
    // screen, which is an ordinary column and scrolls like one.
    expect(calls).not.toContain(true);
    await waitFor(() => expect(calls).toContain(true));
  });

  test("the outline lens asks for nothing and leaves the scroller alone", async () => {
    const calls = asked("#/company?lens=builder&view=table");
    // The toolbar is what both lenses draw once the engine has answered, so
    // waiting for it is waiting for the same moment the case above measures.
    await screen.findByRole("toolbar", { name: "Organization builder" });
    expect(calls).not.toContain(true);
  });
});
