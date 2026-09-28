/**
 * The broker panel renders the ENGINE'S OWN ANSWER: `GET /fleet/broker` as the
 * Go test in `internal/api` writes it from an `engine.BrokerView`
 * (`internal/api/testdata/fleet_broker_answer.json`), for the reason the
 * placement card reads its own golden — a fixture typed here agrees with the
 * panel whatever the engine sends.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { BrokerMembership, brokerRows, groupSummary, voterState } from "./FleetBroker.tsx";
import { Router } from "~/app/router.tsx";
import { engineFile } from "~/test/engineFiles.ts";
import type { FleetBrokerAnswer } from "~/protocol/index.ts";

const golden = engineFile<FleetBrokerAnswer>("internal/api/testdata/fleet_broker_answer.json");
function answer(): FleetBrokerAnswer {
  return structuredClone(golden);
}

/** A fetch that answers every request with `reply`, recording what was sent. */
function engine(reply: { status: number; body: unknown }): URL[] {
  const sent: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      sent.push(new URL(String(input), "http://engine.test"));
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

function panel(a?: FleetBrokerAnswer, error?: string, onChanged?: () => void) {
  // IN A ROUTER, because a live node's name links to its own fleet page.
  return render(
    <Router>
      <BrokerMembership answer={a} error={error} onChanged={onChanged} />
    </Router>,
  );
}

describe("broker membership", () => {
  beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  test("every live node and every voter is a row, the voter no node is included", () => {
    // node-c is counted by the group and no live node is it: the member an
    // operator is looking for, so it must have a row of its own.
    const rows = brokerRows(answer());
    expect(rows.map((r) => r.node)).toEqual(["node-a", "node-b", "node-c", "old-1", "sat-eu-1"]);
    const gone = rows.find((r) => r.node === "node-c")!;
    expect(gone.kind).toBeUndefined();
    expect(voterState(gone.voter)).toBe("offline, last heard 1h 30m ago");
    expect(voterState(rows.find((r) => r.node === "sat-eu-1")!.voter)).toBe("not a voter");
    expect(voterState(rows.find((r) => r.node === "node-a")!.voter)).toBe("leader");
  });

  test("the group line says whose view it is and who leads — or that nobody does", () => {
    const a = answer();
    expect(groupSummary(a.group!, a.group_from)).toBe(
      "Metadata group of acme, as node-a reports it: 3 voters, led by node-a",
    );
    expect(groupSummary({ ...a.group!, leader: undefined }, a.group_from)).toMatch(/no leader/);
  });

  test("each disagreement is said in words, and a dead member offers its removal", () => {
    panel(answer());
    expect(screen.getByText("Dead member")).toBeTruthy();
    expect(screen.getByText("Unknown broker")).toBeTruthy();
    expect(screen.getByText("no live node")).toBeTruthy();
    expect(screen.getByText("unknown")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove from the group" })).toBeTruthy();
  });

  test("an unread group is said to be unread, never drawn as an empty one", () => {
    const a = answer();
    delete a.group;
    delete a.group_from;
    a.group_error = "node-a did not answer";
    panel(a);
    expect(screen.getByText(/could not be read: node-a did not answer/)).toBeTruthy();
  });

  test("an external cluster's fleet is told whose membership it is", () => {
    panel({ node: "node-a", kind: "client", external: true, nodes: [], findings: [] });
    expect(screen.getByText("An external NATS cluster")).toBeTruthy();
    expect(screen.queryByText("Advertises")).toBeNull();
  });

  test("removing a gone member types its id and sends no force", async () => {
    const sent = engine({
      status: 200,
      body: { node: "node-c", by: "node-a", group: { cluster: "acme", peers: [] } },
    });
    const onChanged = vi.fn();
    panel(answer(), undefined, onChanged);
    fireEvent.click(screen.getByRole("button", { name: "Remove from the group" }));
    const remove = screen.getByRole("button", { name: "Remove" });
    expect((remove as HTMLButtonElement).disabled).toBe(true);
    // A GONE MEMBER NEEDS NO FORCE, so the dialog asks for none.
    expect(screen.queryByText("Remove it although it is running")).toBeNull();
    fireEvent.change(screen.getByLabelText("Type node-c to confirm"), {
      target: { value: "node-c" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(screen.getByText(/is no longer a voter/)).toBeTruthy());
    expect(sent).toHaveLength(1);
    expect(sent[0]!.pathname).toBe("/fleet/broker/remove/node-c");
    expect(sent[0]!.searchParams.get("confirm")).toBe("node-c");
    expect(sent[0]!.searchParams.get("force")).toBeNull();
    expect(onChanged).toHaveBeenCalled();
  });

  test("a member still running is removed only as a second, forced decision", async () => {
    // A voter whose node came back as a leaf: the engine counts it dead, and
    // its node still holds a live presence lease.
    const a = answer();
    a.group!.peers.push({ name: "sat-eu-1", peer: "p4", current: false, offline: true, active: 0 });
    a.findings = [{ kind: "dead_member", node: "sat-eu-1", detail: "its node now runs as a leaf" }];
    const sent = engine({ status: 200, body: { node: "sat-eu-1", by: "node-a" } });
    panel(a);
    fireEvent.click(screen.getByRole("button", { name: "Remove from the group" }));
    fireEvent.change(screen.getByLabelText("Type sat-eu-1 to confirm"), {
      target: { value: "sat-eu-1" },
    });
    const remove = screen.getByRole("button", { name: "Remove" }) as HTMLButtonElement;
    expect(remove.disabled).toBe(true);
    fireEvent.click(screen.getByText("Remove it although it is running"));
    expect(remove.disabled).toBe(false);
    fireEvent.click(remove);
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]!.searchParams.get("force")).toBe("true");
  });

  test("a refusal reaches the operator with the engine's own detail and hint", async () => {
    engine({
      status: 503,
      body: {
        error: "no_leader",
        detail: "the metadata group has no leader",
        hint: "bring enough members back for a quorum",
      },
    });
    panel(answer());
    fireEvent.click(screen.getByRole("button", { name: "Remove from the group" }));
    fireEvent.change(screen.getByLabelText("Type node-c to confirm"), {
      target: { value: "node-c" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(screen.getByText(/bring enough members back for a quorum/)).toBeTruthy(),
    );
  });
});
