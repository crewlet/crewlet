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

import { describe, expect, test } from "vitest";

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach } from "vitest";

import { nodeFacts, ObjectPlacement, placementSummary } from "./Fleet.tsx";
import { Router } from "~/app/router.tsx";
import type { FleetNode, FleetObjects } from "~/protocol/types.ts";

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

describe("object placement", () => {
  afterEach(cleanup);
  const now = Date.parse("2026-09-01T12:05:00Z");

  test("a fleet short of data nodes says so, not just a number", () => {
    // Two members asked for three copies hold two: every file has one copy
    // fewer than configured, which is the fact this line exists to carry.
    expect(
      placementSummary({ available: true, placed: true, epoch: 4, replicas: 3, copies: 2 }),
    ).toBe("epoch 4 · 2 of 3 copies of every chunk — fewer data nodes than replicas");
    expect(
      placementSummary({ available: true, placed: true, epoch: 4, replicas: 3, copies: 3 }),
    ).toBe("epoch 4 · 3 copies of every chunk");
  });

  test("an absent member is marked, with when it went", () => {
    const objects: FleetObjects = {
      available: true,
      placed: true,
      epoch: 2,
      replicas: 2,
      copies: 2,
      members: [
        { node: "data-a", weight: 1 },
        { node: "data-b", weight: 4, absent_since: "2026-09-01T12:00:00Z" },
      ],
    };
    // IN A ROUTER, because a member's node links to its own fleet page.
    render(
      <Router>
        <ObjectPlacement objects={objects} now={now} />
      </Router>,
    );
    expect(screen.getByText("data-b")).toBeTruthy();
    expect(screen.getAllByText("absent")).toHaveLength(1);
    expect(screen.getAllByText("present")).toHaveLength(1);
  });

  test.each([
    [{ available: false }, "could not be read"],
    [{ available: true, placed: false }, "No placement map yet"],
    [{ available: true, placed: true, unreadable: true }, "newer build"],
  ] as [FleetObjects, string][])(
    "each state the engine names renders apart: %j",
    (objects, says) => {
      // An unreadable store, a fleet with no map and a newer build's map are
      // three different trips for an operator, and none of them is an empty grid.
      render(<ObjectPlacement objects={objects} now={now} />);
      expect(screen.getByText(new RegExp(says))).toBeTruthy();
    },
  );

  test("a node that reads no map draws no card", () => {
    const { container } = render(<ObjectPlacement now={now} />);
    expect(container.textContent).toBe("");
  });
});
