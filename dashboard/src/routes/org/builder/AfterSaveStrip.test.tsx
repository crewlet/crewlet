/**
 * After a save, the builder follows what it wrote until every node has
 * applied it — the settings revision by each node's epoch, the chart's writes
 * by each node's applied position on the chart's log — or says which node
 * refused it, and the read lenses admit that they still draw the company
 * before it.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import {
  LiveSocket,
  Store,
  type FleetAnswer,
  type RetentionNode,
  type RetentionReport,
} from "~/protocol/index.ts";
import { CompanyScreen } from "~/routes/company/Company.tsx";
import { applyState, chartApplyState } from "./AfterSaveStrip.tsx";
import { clearSavedChanges, recordSavedChanges } from "./savedChanges.ts";
import { company, Engine, InertWebSocket, mountBuilder } from "./testkit.tsx";
import { toastText } from "~/testing.tsx";

beforeEach(() => {
  sessionStorage.clear();
  clearSavedChanges();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  clearSavedChanges();
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

describe("what the strip says of the chart", () => {
  const saved = { position: "CREWLET_CHART_LOG@2:40", appliedHere: false };
  const at = (node_id: string, generation: number, applied_through: number): RetentionNode => ({
    node_id,
    counted: true,
    live: true,
    domains: {
      chart: { generation, seq: applied_through, applied_through, generation_state: "current" },
    },
  });
  const report = (nodes: RetentionNode[]) => ({ nodes }) as unknown as RetentionReport;

  test("every node through the writes' position is applied, and part way says how far", () => {
    expect(chartApplyState(saved, report([at("a", 2, 40), at("b", 2, 41)]))).toMatchObject({
      message: "Applied.",
      resolved: true,
    });
    expect(chartApplyState(saved, report([at("a", 2, 40), at("b", 2, 39)]))).toMatchObject({
      message: "Applied on 1 of 2 nodes.",
      resolved: false,
    });
    expect(chartApplyState(saved, report([at("a", 2, 3)]))).toMatchObject({
      message: "The nodes are applying it.",
      resolved: false,
    });
  });

  // A LOG RE-CREATED AFTER THE WRITES carries everything before it: a node on
  // a later generation has applied them, however low its sequence.
  test("a node on a later generation of the log has passed the writes", () => {
    expect(chartApplyState(saved, report([at("a", 3, 1)]))).toMatchObject({
      message: "Applied.",
      resolved: true,
    });
    // The control: an earlier generation, however far along, has not.
    expect(chartApplyState(saved, report([at("a", 1, 900)]))).toMatchObject({ resolved: false });
  });

  // WHERE THIS READER IS NOT SHOWN THE FLEET, the strip says what this node's
  // own answer said, and says it as that.
  test("without the fleet's positions it says what this node's answer said", () => {
    expect(chartApplyState(saved, null)).toMatchObject({
      message: "This node is applying it.",
      resolved: false,
    });
    expect(chartApplyState({ ...saved, appliedHere: true }, null)).toMatchObject({
      message: "Applied on this node.",
      resolved: true,
    });
  });
});

describe("in the builder", () => {
  /** Saves an edit of the CEO, and a rename of the company with it when `settings`. */
  async function save(
    engine: Engine,
    query: (what: string) => unknown,
    { settings = false }: { settings?: boolean } = {},
  ) {
    mountBuilder({ engine, query });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    if (settings) {
      fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
      await waitFor(() => expect(engine.checks()).toHaveLength(1));
    }
    await waitFor(() =>
      expect(
        (screen.getByRole("button", { name: "Review and save" }) as HTMLButtonElement).disabled,
      ).toBe(false),
    );
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
    const dialog = await screen.findByRole("dialog", { name: "Review and save" });
    if (settings) fireEvent.click(within(dialog).getByRole("checkbox"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
  }

  test("the strip follows the saved revision and offers the diff", async () => {
    const engine = new Engine(company());
    let applied = 1;
    await save(
      engine,
      (what) => (what === "stream" ? { status: "ok", applied_epoch: applied } : null),
      { settings: true },
    );
    const strip = () => screen.getByText(/Saved settings revision/);
    await screen.findByText(/Saved settings revision/);
    expect(strip().textContent).toContain("The engine is applying it.");
    expect(within(strip()).getByText("r-saved")).toBeDefined();
    // What the save changed is the saved revision against the one it was
    // built on. Against the active revision, which the save now is, the diff
    // would be empty.
    expect(screen.getByRole("link", { name: "View changes" }).getAttribute("href")).toBe(
      "#/admin/config?lens=diff&revision=r-saved&against=r1",
    );
    applied = 2;
    await waitFor(() => expect(strip().textContent).toContain("Applied."), { timeout: 8000 });
  }, 12_000);

  // THE CHART IS FOLLOWED ON ITS OWN LOG: the save's furthest position, and
  // each node's applied position read off the retention report.
  test("the strip follows the chart's writes to every node", async () => {
    const engine = new Engine(company());
    let through = 10;
    const retention = () => ({
      nodes: [
        {
          node_id: "a",
          counted: true,
          live: true,
          domains: { chart: { generation: 1, seq: through, applied_through: through } },
        },
      ],
    });
    await save(engine, (what) => (what === "retention" ? retention() : null));
    const strip = await screen.findByText(/Saved the org chart at/);
    expect(strip.textContent).toContain("CREWLET_CHART_LOG@1:11");
    expect(strip.textContent).toContain("The nodes are applying it.");
    // A save that wrote no settings offers nothing about a revision.
    expect(screen.queryByRole("link", { name: "View changes" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy settings as YAML" })).toBeNull();
    through = 11;
    await waitFor(
      () => expect(screen.getByText(/Saved the org chart at/).textContent).toContain("Applied."),
      { timeout: 8000 },
    );
  }, 12_000);

  test("Copy the chart reads the company export, credentials named and never valued", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.path === "/company/export"
        ? new Response(
            JSON.stringify({
              seats: [{ handle: "ceo", runtime: { mcp_env: { t: { TOKEN: "${CHART_X}" } } } }],
            }),
            {
              status: 200,
              headers: { "Content-Type": "application/json" },
            },
          )
        : null;
    await save(engine, () => null);
    fireEvent.click(await screen.findByRole("button", { name: "Copy the chart" }));
    const dialog = await screen.findByRole("dialog", { name: "The org chart" });
    expect(await within(dialog).findByText(/CHART_X/)).toBeDefined();
    expect(within(dialog).getByRole("button", { name: "Copy" })).toBeDefined();
  });

  test("Copy settings as YAML reads the settings as YAML rather than as a refusal", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("format") === "yaml"
        ? new Response("name: Acme\n", {
            status: 200,
            headers: { "Content-Type": "application/yaml", ETag: '"r-saved"' },
          })
        : null;
    await save(engine, () => null, { settings: true });
    fireEvent.click(await screen.findByRole("button", { name: "Copy settings as YAML" }));
    const dialog = await screen.findByRole("dialog", { name: "The settings as YAML" });
    expect(await within(dialog).findByText(/name: Acme/)).toBeDefined();
    expect(within(dialog).getByRole("button", { name: "Copy" })).toBeDefined();
    // The caption reads as a sentence: JSX drops the line break before an
    // element, and "keeps its${NAME} form" is what it rendered.
    expect(dialog.textContent).toContain("a reference keeps its ${NAME} form.");
  });

  test("Copy settings as YAML says when what it read is a later revision", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.query.get("format") === "yaml"
        ? new Response("name: Acme\n", {
            status: 200,
            headers: { "Content-Type": "application/yaml", ETag: '"r-later"' },
          })
        : null;
    await save(engine, () => null, { settings: true });
    fireEvent.click(await screen.findByRole("button", { name: "Copy settings as YAML" }));
    const dialog = await screen.findByRole("dialog", { name: "The settings as YAML" });
    expect(await within(dialog).findByText(/which is active now/)).toBeDefined();
  });
});

describe("the read lenses", () => {
  function mountCompany(appliedEpoch: number) {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    location.hash = "#/company";
    const store = new Store();
    store.applyHealth({ status: "ok" });
    store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }], units: [] });
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
      Promise.resolve(what === "stream" ? { status: "ok", applied_epoch: appliedEpoch } : null);
    return render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <CompanyScreen />
        </Router>
      </ClientContext.Provider>,
    );
  }

  const settings = { revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 };

  test("say they still draw the previous revision until this node applies the saved one", async () => {
    recordSavedChanges({ settings, chart: null });
    mountCompany(3);
    expect(await screen.findByText(/still applying settings revision/)).toBeDefined();
  });

  test("say nothing once the node has applied it", async () => {
    recordSavedChanges({ settings, chart: null });
    mountCompany(4);
    await waitFor(() => expect(screen.queryByText(/still applying/)).toBeNull());
  });

  test("name the chart's changes beside a revision this node has not applied", async () => {
    recordSavedChanges({
      settings,
      chart: { position: "CREWLET_CHART_LOG@1:12", appliedHere: false },
    });
    mountCompany(3);
    const note = await screen.findByText(/still applying settings revision/);
    expect(note.textContent).toContain("and the org chart's changes");
  });
});
