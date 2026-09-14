/**
 * After a save, the builder follows the revision until every node has applied
 * it, or says which node refused it, and the read lenses admit that they
 * still draw the revision before it.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type FleetAnswer } from "~/protocol/index.ts";
import { OrgScreen } from "~/routes/Org.tsx";
import { applyState } from "./AfterSaveStrip.tsx";
import { clearSavedRevision, recordSavedRevision } from "./savedRevision.ts";
import { company, Engine, InertWebSocket, mountBuilder } from "./testkit.tsx";

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
  const saved = { revisionId: "r-saved", epoch: 4 };

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

  test("a save whose answer was lost takes the fleet's target as its epoch", () => {
    const answer = fleet([node({ id: "a", config_epoch: 7, config_status: "ok" })], 7);
    expect(applyState({ revisionId: "r-saved", epoch: null }, null, answer)).toMatchObject({
      message: "Applied.",
      resolved: true,
    });
  });
});

describe("in the builder", () => {
  async function save(engine: Engine, query: (what: string) => unknown) {
    mountBuilder({ engine, query });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
    const dialog = await screen.findByRole("dialog", { name: "Review and save" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await screen.findByText("Saved. The engine is applying it.");
  }

  test("the strip follows the saved revision and offers the diff", async () => {
    const engine = new Engine(company());
    let applied = 1;
    await save(engine, (what) =>
      what === "stream" ? { status: "ok", applied_epoch: applied } : null,
    );
    expect(await screen.findByText("The engine is applying it.")).toBeDefined();
    expect(screen.getByText("r-saved")).toBeDefined();
    expect(screen.getByRole("link", { name: "View changes" }).getAttribute("href")).toBe(
      "#/config?lens=diff&revision=r-saved",
    );
    applied = 2;
    expect(await screen.findByText("Applied.", {}, { timeout: 8000 })).toBeDefined();
  }, 12_000);

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
  function mountOrg(appliedEpoch: number) {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    location.hash = "#/org";
    const store = new Store();
    store.applyHealth({ status: "ok" });
    store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }], units: [] });
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
      Promise.resolve(what === "stream" ? { status: "ok", applied_epoch: appliedEpoch } : null);
    return render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <OrgScreen />
        </Router>
      </ClientContext.Provider>,
    );
  }

  test("say they still draw the previous revision until this node applies the saved one", async () => {
    recordSavedRevision({ revisionId: "r-saved", epoch: 4 });
    mountOrg(3);
    expect(await screen.findByText(/still applying revision/)).toBeDefined();
  });

  test("say nothing once the node has applied it", async () => {
    recordSavedRevision({ revisionId: "r-saved", epoch: 4 });
    mountOrg(4);
    await waitFor(() => expect(screen.queryByText(/still applying revision/)).toBeNull());
  });
});
