/**
 * The Builder lens decides its posture from what the engine answers, checks
 * every draft with a dry run of exactly the write a save would send, and
 * keeps the operator's work through a change of token.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { clearToken, storeToken, type OrgProjection } from "~/protocol/index.ts";
import { useBuilder, type BuilderViewHandle } from "./BuilderContext.tsx";
import { menuEntryLabel } from "~/testing.tsx";
import { href } from "~/app/router.tsx";
import { seatPath } from "~/lib/seats.ts";
import { FillRequest } from "~/app/fill.tsx";
import {
  company,
  Engine,
  fakeSurfaces,
  FakeView,
  json,
  mountBuilder,
  refusal,
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
    expect(await screen.findByText("No problems")).toBeDefined();
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

  test("a refusal with no stored token asks for one", async () => {
    const engine = new Engine(company());
    engine.script = () => json({ error: "unauthorized" }, 401);
    mountBuilder({ engine });
    expect(
      await screen.findByText(
        "Editing the organization needs a credential the engine accepts. Sign in, or set a token.",
      ),
    ).toBeDefined();
    expect(screen.getByRole("button", { name: "Set token" })).toBeDefined();
  });

  // SETTING A TOKEN IS ALL IT TAKES. The posture is read again on the change,
  // so an operator who was asked for a token edits without reloading the page.
  test("setting a token the engine accepts opens edit mode without a reload", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.headers.Authorization === "Bearer good" ? null : json({ error: "unauthorized" }, 401);
    mountBuilder({ engine });
    expect(
      await screen.findByText(
        "Editing the organization needs a credential the engine accepts. Sign in, or set a token.",
      ),
    ).toBeDefined();
    act(() => {
      storeToken("good");
    });
    expect(await screen.findByText("No problems")).toBeDefined();
    expect(screen.getByText("editable")).toBeDefined();
    expect(engine.checks().at(-1)!.headers.Authorization).toBe("Bearer good");
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

  test("a refusal of a stored token says the token was refused", async () => {
    localStorage.setItem("crewlet_api_token", "stale");
    const engine = new Engine(company());
    engine.script = () => json({ error: "unauthorized" }, 401);
    mountBuilder({ engine });
    expect(await screen.findByText("The engine refused this browser's token.")).toBeDefined();
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
    storeToken("reader");
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("dry_run") === "true" ? json({ error: "forbidden" }, 403) : null;
    mountBuilder({ engine });
    expect(await screen.findByText("The engine refused the token")).toBeDefined();

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

    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(handle.focusNode).toHaveBeenCalledWith("seat:ceo"));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
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
  test("an edit is checked as the merge patch a save would send", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
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
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    expect(await screen.findByText("1 problem")).toBeDefined();
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
    fireEvent.keyDown(document.body, { key: "z", code: "KeyZ", ctrlKey: true });
    await waitFor(() => expect(region().textContent).toBe("Undone: Edited CEO: goal."));
    await waitFor(() => expect(engine.checks().at(-1)!.body).toEqual({}));
    // And Shift with it redoes, saying so.
    fireEvent.keyDown(document.body, { key: "Z", code: "KeyZ", ctrlKey: true, shiftKey: true });
    await waitFor(() => expect(region().textContent).toBe("Redone: Edited CEO: goal."));
    await waitFor(() =>
      expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more"),
    );
  });

  test("undo is left to a text field that has focus", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    const field = document.createElement("input");
    document.querySelector(".org-builder")!.appendChild(field);
    fireEvent.keyDown(field, { key: "z", code: "KeyZ", metaKey: true });
    await new Promise((r) => setTimeout(r, 400));
    expect(engine.checks()).toHaveLength(2);
    field.remove();
  });
});

// NO PROVIDER, NO TURN. The dashboard writes none, so the lens says where one
// comes from rather than leaving agents whose work waits with nothing to say
// why.
test("a company with no model provider is told so, and one with a provider is not", async () => {
  const without = company();
  delete without.providers;
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
  test("a selected unit is named in the URL, and a rename rewrites it", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Select Engineering" }));
    await waitFor(() => expect(location.hash).toContain("unit=Engineering"));

    fireEvent.click(screen.getByRole("button", { name: "Rename Engineering" }));
    // The key is the unit's identity, so the name in the URL follows the draft.
    await waitFor(() => expect(location.hash).toContain("unit=Engineering+Two"));
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
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
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

  // A SEAT ADDED IN THIS DRAFT HAS NO SCREEN YET. A check derives its handle
  // at once, and a link built from that handle opened a seat the engine does
  // not have; its kind, like any seat's, can still be changed.
  test("a seat added in the draft is offered no screen, and can change kind", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    // First with no answer about the new seat, so no handle is known for it.
    engine.script = (r) => (r.query.get("dry_run") === "true" ? json({ error: "bad" }, 502) : null);
    fireEvent.click(screen.getByRole("button", { name: "Add an analyst" }));
    await screen.findByText("Could not reach the engine to check");
    fireEvent.click(screen.getByRole("button", { name: "Select Analyst" }));
    const actionsFor = async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Analyst" }));
      return screen.findByRole("menu", { name: "Actions for Analyst" });
    };
    let menu = await actionsFor();
    expect(within(menu).queryByRole("menuitem", { name: "Open seat" })).toBeNull();
    const kind = within(menu).getByRole("menuitem", { name: "Change to human seat" });
    expect(kind.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.keyDown(menu, { key: "Escape" });

    // Then once a check has derived its handle: the URL names it, and there
    // is still no screen to open.
    engine.script = () => null;
    expect(await screen.findByText("No problems", {}, { timeout: 4000 })).toBeDefined();
    await waitFor(() => expect(location.hash).toContain("seat=analyst"));
    menu = await actionsFor();
    expect(within(menu).queryByRole("menuitem", { name: "Open seat" })).toBeNull();
    expect(within(menu).getByRole("menuitem", { name: "Change to human seat" })).toBeDefined();
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
  expect(await screen.findByRole("button", { name: "CEO" })).toBeDefined();
  engine.script = () => null;
  act(() => answer());
  await screen.findByText("No problems");
  await waitFor(() => expect(location.hash).toContain("seat=ceo"));
  expect(screen.getByRole("button", { name: "CEO" })).toBeDefined();
});

describe("a revision saved by somebody else", () => {
  // NOTHING TO PROTECT, SO NOTHING TO ASK. A lens with no changes on it is
  // stood on the newer revision; the conflict banner, and the pause that
  // comes with it, are for a draft that holds work.
  test("an untouched draft is stood on the newer revision the org push reports", async () => {
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine });
    await screen.findByText("No problems");
    const next = company();
    next.roles![1]!.goal = "Design things";
    engine.document = next;
    engine.revision = "r2";
    act(() => store.applyOrg({ name: "Acme", roles: [], units: [] }));

    await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r2"'));
    expect(await screen.findByText("No problems")).toBeDefined();
    expect(screen.queryByText("The configuration changed since you started editing.")).toBeNull();
    expect(screen.getByText("editable")).toBeDefined();
  });

  test("a draft with work is not moved: the change is offered as an update", async () => {
    const engine = new Engine(company());
    const { store } = mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    await screen.findByText("No problems");
    engine.document = company();
    engine.revision = "r2";
    const reads = engine.sent("GET").length;
    act(() => store.applyOrg({ name: "Acme", roles: [], units: [] }));

    expect(await screen.findByText("The configuration changed")).toBeDefined();
    expect(engine.sent("GET")).toHaveLength(reads);
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more");
  });
});

describe("a token change mid-edit", () => {
  test("keeps the draft, reads the configuration again and checks under the new token", async () => {
    storeToken("first");
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    const reads = engine.sent("GET").length;

    act(() => {
      storeToken("second");
    });
    await waitFor(() => expect(engine.sent("GET").length).toBe(reads + 1));
    await waitFor(() => expect(engine.checks()).toHaveLength(3));
    const recheck = engine.checks()[2]!;
    expect(recheck.headers.Authorization).toBe("Bearer second");
    // The edit is still in the draft that was checked.
    expect(JSON.stringify(recheck.body)).toContain("Lead and more");
  });

  test("a dry run refused for the new token pauses editing while the configuration still reads", async () => {
    storeToken("first");
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));

    // Reads are served, writes and dry runs are refused: the token lacks the
    // right to write, which only the check can find out.
    engine.script = (r) =>
      r.query.get("dry_run") === "true" && r.headers.Authorization === "Bearer reader"
        ? json({ error: "forbidden" }, 403)
        : null;
    act(() => {
      storeToken("reader");
    });
    expect(await screen.findByText("The engine refused the token")).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more");
  });

  test("a token the engine refuses pauses editing without discarding the draft", async () => {
    storeToken("first");
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));

    engine.script = (r) => (r.headers.Authorization === "Bearer first" ? null : json({}, 401));
    act(() => {
      clearToken();
    });
    expect(
      await screen.findByText(
        "Editing the organization needs a credential the engine accepts. Sign in, or set a token. Your draft is kept on this page.",
      ),
    ).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    // The status names what is missing: no token was refused, none is set.
    expect(await screen.findByText("Needs a credential")).toBeDefined();
    expect(screen.queryByText("The engine refused the token")).toBeNull();
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
