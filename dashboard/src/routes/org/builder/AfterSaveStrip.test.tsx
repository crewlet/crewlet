/**
 * After a save, the builder follows the revision until every node has applied
 * it, or says which node refused it, and the read lenses admit that they
 * still draw the revision before it.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type FleetAnswer } from "~/protocol/index.ts";
import { CompanyScreen } from "~/routes/company/Company.tsx";
import { applyState } from "./AfterSaveStrip.tsx";
import { clearSavedRevision, recordSavedRevision } from "./savedRevision.ts";
import { company, Engine, InertWebSocket, mountBuilder } from "./testkit.tsx";
import { toastText } from "~/testing.tsx";

beforeEach(() => {
  sessionStorage.clear();
  clearSavedRevision();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  clearSavedRevision();
  location.hash = "#/";
});

const node = (over: Partial<FleetAnswer["nodes"][number]> & { id: string }) => ({
  roles: ["seats"],
  seats: 1,
  ...over,
});

const fleet = (nodes: FleetAnswer["nodes"], target: number): FleetAnswer => ({
  nodes,
  seats: [],
  duties: [],
  unplaceable: [],
  unmanned_roles: [],
  this_node: nodes[0]?.id ?? "",
  target_epoch: target,
});

describe("what the strip says", () => {
  const saved = { revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 };

  test("a node that has applied the epoch, with no fleet answer, is applied", () => {
    expect(applyState(saved, { status: "ok", applied_epoch: 4 }, null)).toMatchObject({
      message: "Applied.",
      resolved: true,
    });
    expect(applyState(saved, { status: "ok", applied_epoch: 3 }, null)).toMatchObject({
      message: "The engine is applying it.",
      resolved: false,
    });
  });

  test("a fleet part way through says how far", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok" }),
        node({ id: "b", config_epoch: 3, config_status: "ok" }),
      ],
      4,
    );
    expect(applyState(saved, null, answer)).toMatchObject({
      message: "Applied on 1 of 2 nodes.",
      resolved: false,
    });
  });

  // A NODE THAT REFUSED IS THE WHOLE POINT: the revision is active and the
  // node goes on serving the previous one, which nothing else on this screen
  // would show.
  test("a node that refused the revision is named, with its reason", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok" }),
        node({
          id: "b",
          config_epoch: 4,
          config_status: "error",
          config_error: "provider openai: model is required",
        }),
      ],
      4,
    );
    expect(applyState(saved, null, answer)).toMatchObject({
      tone: "critical",
      message: "Node b refused this revision: provider openai: model is required.",
      resolved: true,
      showFleet: true,
    });
  });

  // A node records the epoch it ATTEMPTED beside the outcome, so a failure
  // names the revision it failed on. Read without that match, a node that
  // refused an earlier revision would be reported as refusing this one.
  test("a node that refused another epoch is not read as refusing this one", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok" }),
        node({
          id: "b",
          config_epoch: 3,
          config_status: "error",
          config_error: "provider openai: model is required",
        }),
      ],
      4,
    );
    expect(applyState(saved, null, answer)).toMatchObject({
      tone: "info",
      message: "Applied on 1 of 2 nodes.",
      resolved: false,
    });
  });

  // The strip follows ONE revision. A node reporting a later epoch has passed
  // this one, so waiting on it would leave the strip applying for ever.
  test("a node that has moved on to a later epoch has passed this one", () => {
    const answer = fleet(
      [
        node({ id: "a", config_epoch: 4, config_status: "ok" }),
        node({ id: "b", config_epoch: 5, config_status: "degraded", config_error: "mcp child" }),
      ],
      5,
    );
    expect(applyState(saved, null, answer)).toMatchObject({
      message: "Applied.",
      resolved: true,
    });
  });

  test("a save whose answer was lost takes the fleet's target as its epoch", () => {
    const answer = fleet([node({ id: "a", config_epoch: 7, config_status: "ok" })], 7);
    expect(
      applyState({ revisionId: "r-saved", parentRevisionId: "r1", epoch: null }, null, answer),
    ).toMatchObject({
      message: "Applied.",
      resolved: true,
    });
  });
});

describe("in the builder", () => {
  async function save(engine: Engine, query: (what: string) => unknown) {
    const { store } = mountBuilder({ engine, query });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
    const dialog = await screen.findByRole("dialog", { name: "Review and save" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    return store;
  }

  // THIS NODE'S EPOCH ARRIVES ON THE HEALTH PUSH — the strip asks for nothing
  // to learn it, so the tick moving it is what resolves the strip.
  test("the strip follows the saved revision and offers the diff", async () => {
    const engine = new Engine(company());
    const store = await save(engine, () => null);
    act(() => store.applyHealth({ status: "ok", applied_epoch: 1 }));
    expect(await screen.findByText("The engine is applying it.")).toBeDefined();
    expect(screen.getByText("r-saved")).toBeDefined();
    // What the save changed is the saved revision against the one it was
    // built on. Against the active revision, which the save now is, the diff
    // would be empty.
    expect(screen.getByRole("link", { name: "View changes" }).getAttribute("href")).toBe(
      "#/admin/config?lens=diff&revision=r-saved&against=r1",
    );
    act(() => store.applyHealth({ status: "ok", applied_epoch: 2 }));
    expect(await screen.findByText("Applied.")).toBeDefined();
  });

  test("Copy as YAML reads the company as YAML rather than as a refusal", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("format") === "yaml"
        ? new Response("name: Acme\n", {
            status: 200,
            headers: { "Content-Type": "application/yaml", ETag: '"r-saved"' },
          })
        : null;
    await save(engine, () => null);
    fireEvent.click(await screen.findByRole("button", { name: "Copy as YAML" }));
    const dialog = await screen.findByRole("dialog", { name: "The company as YAML" });
    expect(await within(dialog).findByText(/name: Acme/)).toBeDefined();
    expect(within(dialog).getByRole("button", { name: "Copy" })).toBeDefined();
    // The caption reads as a sentence: JSX drops the line break before an
    // element, and "keeps its${NAME} form" is what it rendered.
    expect(dialog.textContent).toContain("a reference keeps its ${NAME} form.");
  });

  test("Copy as YAML says when what it read is a later revision", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("format") === "yaml"
        ? new Response("name: Acme\n", {
            status: 200,
            headers: { "Content-Type": "application/yaml", ETag: '"r-later"' },
          })
        : null;
    await save(engine, () => null);
    fireEvent.click(await screen.findByRole("button", { name: "Copy as YAML" }));
    const dialog = await screen.findByRole("dialog", { name: "The company as YAML" });
    expect(await within(dialog).findByText(/which is active now/)).toBeDefined();
  });
});

describe("the read lenses", () => {
  function mountCompany(appliedEpoch: number) {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    location.hash = "#/company";
    const store = new Store();
    // The health push, which is where this node's applied epoch comes from.
    store.applyHealth({ status: "ok", applied_epoch: appliedEpoch });
    store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }], units: [] });
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = () =>
      Promise.resolve(null);
    return render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <CompanyScreen />
        </Router>
      </ClientContext.Provider>,
    );
  }

  test("say they still draw the previous revision until this node applies the saved one", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    mountCompany(3);
    expect(await screen.findByText(/still applying revision/)).toBeDefined();
  });

  test("say nothing once the node has applied it", async () => {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    mountCompany(4);
    await waitFor(() => expect(screen.queryByText(/still applying revision/)).toBeNull());
  });
});
