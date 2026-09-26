/**
 * The facts a node wears, and the one that had no reader at all.
 *
 * `internal/api/queries/fleet.go` has written `projections_ready` /
 * `projections_total` on every node row since they were added, with a comment
 * saying where the fact belongs: "the fleet view", because it is the answer to
 * "why is the new node holding nothing". The client type never declared them
 * and no screen read them, so the answer carried the fact and nobody could see
 * it — the same shape `config_revision_id` was in until it was fixed in this
 * same file.
 *
 * Asserted over the facts function rather than through a render because that
 * is where page and rail agree: `ObjectHeader` takes this list on both.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";

import { Fleet, nodeFacts } from "./Fleet.tsx";
import { Shell } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { PAGE_ACTIONS_SLOT } from "~/app/frame/PageActions.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryError, Store } from "~/protocol/index.ts";
import type { FleetNode } from "~/protocol/types.ts";

function node(over: Partial<FleetNode> = {}): FleetNode {
  return { id: "n1", roles: [], seats: 0, config_epoch: 3, ...over };
}

function fact(n: FleetNode, label: string) {
  return nodeFacts({ node: n, target: 3 }).find((f) => f.label === label);
}

describe("nodeFacts", () => {
  test("a node that is still replaying says how far it has come", () => {
    // Two integers, not a bool: "3 of 5" and "0 of 2" are the two readings an
    // operator has to tell apart, which is why the engine sends a pair.
    expect(fact(node({ projections_ready: 3, projections_total: 5 }), "Copies")?.value).toBe(
      "3 of 5 ready",
    );
    expect(fact(node({ projections_ready: 0, projections_total: 2 }), "Copies")?.value).toBe(
      "0 of 2 ready",
    );
  });

  test("a node that published nothing states no reading at all", () => {
    // ABSENT IS NOT ZERO. The node publishes the pair only once the total is
    // non-zero, so a dash here would claim a reading the engine does not keep
    // — and `FactLine` drops an empty value, which is the honest rendering.
    expect(fact(node(), "Copies")?.value).toBe("");
    expect(fact(node({ projections_total: 0 }), "Copies")?.value).toBe("");
  });

  test("the reading sits beside the epoch, because it is the other question", () => {
    // A node can hold the current revision and still be replaying the log its
    // state is derived from; only one of those makes its seats servable.
    const labels = nodeFacts({
      node: node({ projections_ready: 1, projections_total: 4 }),
      target: 3,
    }).map((f) => f.label);
    expect(labels.indexOf("Copies")).toBe(labels.indexOf("Epoch") + 1);
  });
});

// A REFUSED READ IS UNKNOWN, NOT ZERO. With no reading ever taken the screen
// drew "0 nodes" in its header, four tiles of 0, "No nodes are reporting" and
// a banner calling the refusal "the last reading that succeeded" — while the
// Settings column beside it said 1 node. The frame now answers a reader it
// knows holds no credential (see App.test.tsx); this is the screen's own half,
// for the reader nobody has answered for yet, whose read is refused anyway.
describe("the fleet, refused before any reading", () => {
  class InertWebSocket {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 3;
    readyState = InertWebSocket.CONNECTING;
    send(): void {}
    close(): void {}
  }

  afterEach(() => {
    cleanup();
    location.hash = "#/";
  });

  test("draws the refusal alone: no count, no tiles, no empty fleet", async () => {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    location.hash = "#/settings/nodes";
    const store = new Store();
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
      what === "viewer" ? new Promise(() => {}) : Promise.reject(new QueryError("unauthorized"));
    const view = render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <Shell>
            <Fleet />
          </Shell>
        </Router>
      </ClientContext.Provider>,
    );
    expect(await screen.findByText(/This answer is auth-gated/)).toBeDefined();
    expect(document.getElementById(PAGE_ACTIONS_SLOT)?.textContent).toBe("");
    expect(view.container.querySelector(".crewlet-stat-card")).toBeNull();
    expect(screen.queryByText("No nodes are reporting")).toBeNull();
    expect(screen.queryByText(/last reading that succeeded/)).toBeNull();
  });
});
