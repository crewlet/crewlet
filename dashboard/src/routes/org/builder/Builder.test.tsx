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
const liveRegion = () => document.querySelector(".org-builder-live")!;

/** A menu's entries by their labels, without the key hints some of them carry. */
const labels = (menu: HTMLElement) =>
  within(menu)
    .getAllByRole("menuitem")
    .map((item) => item.querySelector(".menu-item-label")!.textContent);

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
      await screen.findByText("Editing the organization needs an operator token."),
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
      await screen.findByText("Editing the organization needs an operator token."),
    ).toBeDefined();
    act(() => {
      storeToken("good");
    });
    expect(await screen.findByText("No problems")).toBeDefined();
    expect(screen.getByText("editable")).toBeDefined();
    expect(engine.checks().at(-1)!.headers.Authorization).toBe("Bearer good");
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
    expect(
      await screen.findByText(
        "This process does not serve the configuration. Open the dashboard on a node running the engine.",
      ),
    ).toBeDefined();
  });

  test("a body that is not JSON is a process that does not serve the configuration", async () => {
    const engine = new Engine(company());
    engine.script = () => new Response("<html></html>", { status: 200 });
    mountBuilder({ engine });
    expect(
      await screen.findByText(
        "This process does not serve the configuration. Open the dashboard on a node running the engine.",
      ),
    ).toBeDefined();
  });

  test("an engine that never answers is unreachable", async () => {
    const engine = new Engine(company());
    engine.script = () => Promise.reject(new TypeError("Failed to fetch"));
    mountBuilder({ engine });
    expect(await screen.findByText("The engine could not be reached")).toBeDefined();
  });

  test("a first dry run refused for want of a coordination store makes the lens read-only", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("dry_run") === "true" ? json({ error: "no_control_plane" }, 503) : null;
    mountBuilder({ engine });
    expect(await screen.findByText("Read-only here")).toBeDefined();
    expect(
      screen.getByText(
        "This process cannot write the configuration because it has no coordination store.",
      ),
    ).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    // An edit is refused before it reaches the log, and says why.
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(liveRegion().textContent).toContain("Editing is paused"));
    expect(engine.checks()).toHaveLength(1);

    // And the toolbar's own actions say so where they are read: an entry that
    // is offered, pressed, and answers only into a live region nobody sees
    // is worse than one marked unavailable.
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

  test("the canvas is handed the chart the toolbar chooses", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    expect(await screen.findByText("Drawing the structure chart")).toBeDefined();
    fireEvent.click(screen.getByRole("tab", { name: "Reporting" }));
    expect(await screen.findByText("Drawing the reporting chart")).toBeDefined();
    // A section, so the chart on screen is in the URL and a link opens it.
    expect(location.hash).toContain("chart=reporting");
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
    mountBuilder({ engine, hash: "#/org?lens=builder&view=canvas&seat=ceo" });
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
    // The path only: this harness keeps the Builder mounted under any route,
    // where the app replaces the whole screen.
    await waitFor(() => expect(location.hash.split("?")[0]).toBe("#/seats/ceo"));
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
        "Editing the organization needs an operator token. Your draft is kept on this page.",
      ),
    ).toBeDefined();
    expect(screen.getByText("read only")).toBeDefined();
    // The status names what is missing: no token was refused, none is set.
    expect(await screen.findByText("Needs an operator token")).toBeDefined();
    expect(screen.queryByText("The engine refused the token")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() =>
      expect(liveRegion().textContent).toBe("Editing is paused because no operator token is set."),
    );
    // The seats are still drawn from the draft, not replaced by a refusal.
    expect(within(screen.getByRole("list", { name: "Seats" })).getByText("CEO")).toBeDefined();
  });
});
