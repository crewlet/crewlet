/**
 * Settings' Home widgets count a node behind on config by the Nodes screen's
 * own rule (`configStanding`), never by its epoch alone.
 *
 * Both used to read `config_epoch < target_epoch`, so a node that refused the
 * fleet's revision — it reports the target's epoch beside `error` and runs the
 * one before — was missing from "Behind on config" and "Nodes behind" while
 * the Nodes screen listed it, and a node on another revision at the target's
 * epoch was missing everywhere.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import type { ComponentType } from "react";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { FleetActivation, FleetNode } from "~/protocol/types.ts";
import { widgets } from "./home.widgets.tsx";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const ACTIVE = "01JACTIVE00000000000000000A";
const PREVIOUS = "01JPREVIOUS000000000000000P";
const activation: FleetActivation = {
  epoch: 3,
  revision_id: ACTIVE,
  at: "2026-10-26T08:00:00Z",
  summary: "",
};

const node = (id: string, over: Partial<FleetNode> = {}): FleetNode => ({
  id,
  roles: [],
  broker: "member",
  seats: 0,
  config_epoch: 3,
  config_status: "ok",
  config_revision_id: ACTIVE,
  config_serving_revision_id: ACTIVE,
  ...over,
});

/** Runs the activated revision at the activated epoch. */
const current = node("current-0");
/** Refused the activated revision at its epoch, and runs the one before. */
const refused = node("refused-0", {
  config_status: "error",
  config_error: "env var CREWLET_X is not set",
  config_serving_revision_id: PREVIOUS,
});
/** Reports the activated epoch on another revision: a rebuilt coordination store. */
const other = node("other-0", {
  config_revision_id: PREVIOUS,
  config_serving_revision_id: PREVIOUS,
});

const fleet = (nodes: FleetNode[], active: FleetActivation | null = activation) => ({
  nodes,
  seats: [],
  duties: [],
  unplaceable: [],
  unmanned_roles: [],
  this_node: nodes[0]?.id ?? "",
  target_epoch: active?.epoch ?? 0,
  activation: active,
});

const revision = {
  revision_id: ACTIVE,
  created_at: "2026-10-26T07:59:00Z",
  activated_at: "2026-10-26T08:00:00Z",
  created_by: "jane",
  summary: "",
  is_active: true,
};

function widget(key: string): ComponentType {
  const def = widgets.find((d) => d.key === key);
  expect(def, key).toBeDefined();
  return def!.Component as ComponentType;
}

function mount(key: string, answer: ReturnType<typeof fleet>) {
  return mountAnswering(key, { fleet: answer, config_audit: [revision] });
}

/** A widget with the questions it asks answered from `answers`, and every other one left waiting. */
function mountAnswering(key: string, answers: Record<string, unknown>) {
  const Widget = widget(key);
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what in answers ? Promise.resolve(answers[what]) : new Promise(() => {});
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <Widget />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

/** The figure on the stat card labelled `label`, once it is drawn. */
async function figure(container: HTMLElement, label: string) {
  await screen.findByText(label);
  const card = [...container.querySelectorAll<HTMLElement>(".crewlet-statcard")].find((c) =>
    c.textContent?.includes(label),
  );
  expect(card, `the ${label} card is drawn`).toBeDefined();
  return card!.querySelector(".crewlet-statcard__number")?.textContent;
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

describe("the Nodes widget", () => {
  test("counts a refusal and another revision, and tags each by why", async () => {
    const view = mount("settings.nodes", fleet([current, refused, other]));
    expect(await figure(view.container, "Behind on config")).toBe("2");
    const item = (id: string) =>
      screen.getByText(id).closest<HTMLElement>(".crewlet-list-item, li, a")!;
    // A REFUSAL IS THE ONE TO ACT ON, so it is the danger tone.
    const refusal = within(item("refused-0")).getByText("Refused");
    expect(refusal.closest(".crewlet-tag")!.className).toContain("crewlet-tag--danger");
    const elsewhere = within(item("other-0")).getByText("Elsewhere");
    expect(elsewhere.closest(".crewlet-tag")!.className).toContain("crewlet-tag--warning");
    expect(screen.queryByText("current-0")).toBeNull();
  });

  test("an answer with no activation counts no node behind", async () => {
    const view = mount("settings.nodes", fleet([refused, other], null));
    expect(await figure(view.container, "Behind on config")).toBe("0");
    expect(screen.getByText("no target yet")).toBeDefined();
  });
});

describe("the Configuration widget", () => {
  test("counts a refusal and another revision among the nodes behind", async () => {
    const view = mount("settings.config", fleet([current, refused, other]));
    await screen.findByText("of 3 nodes");
    expect(await figure(view.container, "Nodes behind")).toBe("2");
  });

  test("a fleet where every node runs the activated revision has none behind", async () => {
    const view = mount("settings.config", fleet([current, node("current-1")]));
    await screen.findByText("of 2 nodes");
    expect(await figure(view.container, "Nodes behind")).toBe("0");
  });
});

describe("the Models widget", () => {
  const model = (key: string, health: string, used: boolean) => ({
    key,
    agent: "claude-code",
    model: "sonnet",
    health,
    sign_in: { route: "api-key", resolves: true },
    sitting_out: null,
    last_success: null,
    waiting: null,
    used_by: used ? [{ seat: "pm", job: "executor", position: 0 }] : [],
  });

  // WHAT NEEDS THE READER LEADS: a model that cannot answer, then one nothing
  // runs on, then the rest. Mutation: drop the sort and "fine" leads.
  test("leads with a model that cannot answer, then one nothing runs on", async () => {
    mountAnswering("settings.models", {
      llm_entries: {
        reading: "member",
        node: "n0",
        nodes: [],
        alarms: null,
        entries: [
          model("fine", "ready", true),
          model("idle", "ready", false),
          model("spent", "sitting_out", true),
        ],
      },
    });
    await screen.findByText(/^spent:/);
    const rows = [...document.querySelectorAll(".crewlet-list-item, li")]
      .map((li) => li.textContent ?? "")
      .filter((t) => /^(fine|idle|spent):/.test(t));
    expect(rows.map((t) => t.split(":")[0])).toEqual(["spent", "idle", "fine"]);
    expect(rows[0]).toMatch(/Sitting out/);
    expect(rows[1]).toMatch(/Unused/);
    expect(rows[2]).toMatch(/Ready/);
    expect(screen.queryByText(/each ready and in use/)).toBeNull();
  });

  test("says so when every model is ready and in use", async () => {
    mountAnswering("settings.models", {
      llm_entries: {
        reading: "member",
        node: "n0",
        nodes: [],
        alarms: null,
        entries: [model("fine", "ready", true)],
      },
    });
    expect(await screen.findByText("1 model, each ready and in use.")).toBeTruthy();
  });
});
