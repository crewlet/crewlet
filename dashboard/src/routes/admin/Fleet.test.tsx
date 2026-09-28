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

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import {
  balanceSummary,
  BrokerCell,
  nodeFacts,
  ObjectPlacement,
  placementSummary,
} from "./Fleet.tsx";
import { HOLD_LENGTHS } from "./ObjectsDialog.tsx";
import { Router } from "~/app/router.tsx";
import { engineFile } from "~/test/engineFiles.ts";
import type {
  FleetNode,
  FleetObjects,
  ObjectsGestureAnswer,
  PlacedObjects,
} from "~/protocol/types.ts";

function node(over: Partial<FleetNode> = {}): FleetNode {
  return { id: "n1", roles: [], seats: 0, config_epoch: 3, ...over };
}

function fact(n: FleetNode, label: string) {
  return nodeFacts({ node: n, target: 3 }).find((f) => f.label === label);
}

describe("the broker column", () => {
  afterEach(cleanup);

  // WHAT EACH NODE'S BROKER IS, off the engine's own row: `unknown` is a value
  // the engine sends for a presence that does not say, and it is the one kind
  // marked, because a capacity seal counts it as a member. An engine older
  // than the field sends nothing, which is no value rather than an empty tag.
  test("each kind is a tag, and unknown is the one marked", () => {
    for (const kind of ["member", "leaf", "client"]) {
      const { container } = render(<BrokerCell broker={kind} />);
      const tag = within(container).getByText(kind);
      expect(tag.closest(".crewlet-tag")!.className).toContain("crewlet-tag--neutral");
      cleanup();
    }
    const { container } = render(<BrokerCell broker="unknown" />);
    expect(within(container).getByText("unknown").closest(".crewlet-tag")!.className).toContain(
      "crewlet-tag--warning",
    );
    cleanup();
    render(<BrokerCell />);
    expect(screen.queryByText("unknown")).toBeNull();
    expect(screen.getByText("This engine predates the broker kind")).toBeTruthy();
  });
});

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

/**
 * THE PLACEMENT CARD RENDERS THE ENGINE'S OWN ANSWERS: every block, gesture
 * answer and refusal below is read from the golden the Go renderers write
 * (`internal/api/testdata/objects_answer.json`), for the reason the gate
 * dialog's suite reads its own — a fixture typed here agrees with the card
 * whatever the engine sends.
 */
interface ObjectsGolden {
  fleet: Record<string, FleetObjects>;
  answers: Record<string, ObjectsGestureAnswer>;
  refusals: Record<string, { status: number; body: Record<string, unknown> }>;
}
const golden = engineFile<ObjectsGolden>("internal/api/testdata/objects_answer.json");
function block(name: string): FleetObjects {
  const b = golden.fleet[name];
  if (!b) throw new Error(`the golden has no block named ${name}`);
  return structuredClone(b);
}
function placed(name: string): PlacedObjects {
  const b = block(name);
  if (b.state !== "placed") throw new Error(`${name} is not a placed map`);
  return b;
}

/** A fetch that answers each request with the next of `replies`. */
function engine(...replies: { status: number; body: unknown }[]): URL[] {
  const sent: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      sent.push(new URL(String(input), "http://engine.test"));
      const reply = replies[Math.min(sent.length - 1, replies.length - 1)]!;
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

describe("object placement", () => {
  beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    localStorage.clear();
  });
  const now = Date.parse("2026-09-01T12:05:00Z");

  function card(objects?: FleetObjects, onChanged?: () => void) {
    // IN A ROUTER, because a member's node links to its own fleet page.
    return (
      <Router>
        <ObjectPlacement objects={objects} now={now} onChanged={onChanged} />
      </Router>
    );
  }
  function renderCard(objects?: FleetObjects, onChanged?: () => void) {
    return render(card(objects, onChanged));
  }

  /**
   * The card as the screen drives it: every gesture's `onChanged` re-reads the
   * map, and the card is drawn again from what the re-read answered.
   */
  function renderRereading(first: FleetObjects, reread: FleetObjects) {
    const view = render(card(first, () => view.rerender(card(reread, () => {}))));
    return view;
  }

  test("the summary says the copies, the groups and the domains — and a shortfall in words", () => {
    const map = placed("placed");
    expect(placementSummary(map)).toBe(
      "epoch 7 · 2 copies of every chunk · 256 groups · across 3 zone values · balanced within 1.5%",
    );
    // Two placeable members asked for three copies hold two: every file has
    // one copy fewer than configured, which is the fact this line carries.
    expect(placementSummary({ ...map, replicas: 3 })).toBe(
      "epoch 7 · 2 of 3 copies of every chunk — fewer data nodes than replicas · 256 groups · across 3 zone values · balanced within 1.5%",
    );
  });

  test("the summary says how evenly the copies follow the weights, and a balance that could not", () => {
    // A balance that ran out of rounds writes the best map it measured, so
    // this is the one place the screen says a weight is not being kept —
    // rendered by the engine, not typed here.
    expect(balanceSummary(placed("unbalanced"))).toBe("not converged: 6.1% after 60 rounds");
    const map = placed("placed");
    expect(balanceSummary(map)).toBe("balanced within 1.5%");
    // No balance ran: a split's placement, measured and left as it was.
    expect(
      balanceSummary({
        ...map,
        balance: { ...map.balance!, rounds: 0, deviation_percent: 3.1, converged: false },
      }),
    ).toBe("within 3.1% as the split placed it");
    // A measurement of an older placement says nothing about this one.
    expect(balanceSummary({ ...map, balance: { ...map.balance!, epoch: 6 } })).toBe(
      "balance not yet measured at this epoch",
    );
    // A map nothing has measured is not an even one: the summary is silent.
    expect(balanceSummary({ ...map, balance: undefined })).toBeUndefined();
    expect(placementSummary({ ...map, balance: undefined })).toBe(
      "epoch 7 · 2 copies of every chunk · 256 groups · across 3 zone values",
    );
  });

  test("each member shows its share, domain, store, pending work and absence", () => {
    renderCard(block("placed"));
    // data-a at twice the weight holds the most copies.
    expect(screen.getByText("41.4%")).toBeTruthy();
    expect(screen.getByText("eu-1")).toBeTruthy();
    // data-b is counting ticks gone — as a fraction of the ticks that remove
    // it, since "gone for a while" means nothing without "of how long".
    expect(screen.getAllByText("absent 12/40").length).toBeGreaterThan(0);
    // data-c's store is nearly full and its last pass did not finish, which
    // makes its pending count a reading about an older map.
    expect(screen.getByText("nearfull · 88%")).toBeTruthy();
    expect(screen.getByText("unfinished")).toBeTruthy();
    // data-d is out, and what it holds is strays rather than pending work.
    expect(screen.getByText("out")).toBeTruthy();
    expect(screen.getByText("14 strays")).toBeTruthy();
    // data-b holds no lease, so it reports no store — absent, never "ok".
    expect(screen.getByText("It holds no lease")).toBeTruthy();
  });

  test("the map-wide facts are said above the members", () => {
    renderCard(block("placed"));
    expect(screen.getByText(/154 of 256 groups/)).toBeTruthy();
    expect(screen.getByText(/was removed for being unhealthy/)).toBeTruthy();
    expect(screen.queryByText(/^Held$/)).toBeNull();
    expect(screen.getByRole("button", { name: "Hold map" })).toBeTruthy();
  });

  test("a hold in force is a banner, and the header offers its release", () => {
    renderCard(block("held"));
    expect(screen.getByText("Held")).toBeTruthy();
    expect(screen.getByText(/kernel upgrade/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Release hold" })).toBeTruthy();
  });

  test.each(["unavailable", "no_map", "unreadable"])(
    "each state the engine names renders apart, under the one header: %s",
    (state) => {
      // An unreadable store, a fleet with no map and a newer build's map are
      // three different trips for an operator, and none of them is an empty
      // grid. AND ONE HEADER: it was written out twice, once per branch.
      renderCard(block(state));
      const says = {
        unavailable: /could not be read/,
        no_map: /No placement map yet/,
        unreadable: /newer build/,
      }[state]!;
      expect(screen.getByText(says)).toBeTruthy();
      expect(screen.getAllByText("Object placement")).toHaveLength(1);
      expect(screen.queryByRole("button", { name: "Hold map" })).toBeNull();
    },
  );

  test("a placed map has one header too", () => {
    renderCard(block("placed"));
    expect(screen.getAllByText("Object placement")).toHaveLength(1);
  });

  test("a node that reads no map draws no card", () => {
    const { container } = renderCard(undefined);
    expect(container.textContent).toBe("");
  });

  test("taking a member out is typed out, sends the reason, and re-reads the map", async () => {
    const sent = engine({ status: 200, body: golden.answers.out });
    const changed = vi.fn();
    renderCard(block("placed"), changed);
    fireEvent.click(screen.getAllByRole("button", { name: "Take out" })[0]!);
    // NOT BEFORE THE NAME IS TYPED: the gesture moves the member's share.
    const confirm = () =>
      within(screen.getByRole("dialog")).getByRole("button", { name: "Take out" });
    expect((confirm() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Type data-a to confirm"), {
      target: { value: "data-a" },
    });
    fireEvent.change(screen.getByLabelText("Why"), { target: { value: "disk swap" } });
    fireEvent.click(confirm());
    await waitFor(() => expect(screen.getByText(/is out: nothing new is placed/)).toBeTruthy());
    expect(sent[0]!.pathname).toBe("/objects/out/data-a");
    expect(sent[0]!.searchParams.get("confirm")).toBe("data-a");
    expect(sent[0]!.searchParams.get("reason")).toBe("disk swap");
    expect(changed).toHaveBeenCalled();
  });

  test("a refusal says what is wrong and what to do", async () => {
    const refusal = golden.refusals.objects_refused!;
    engine({ status: refusal.status, body: refusal.body });
    renderCard(block("placed"));
    fireEvent.click(screen.getAllByRole("button", { name: "Take out" })[0]!);
    fireEvent.change(screen.getByLabelText("Type data-a to confirm"), {
      target: { value: "data-a" },
    });
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Take out" }));
    await waitFor(() => expect(screen.getByText(String(refusal.body.detail))).toBeTruthy());
    expect(screen.getByText(String(refusal.body.hint))).toBeTruthy();
  });

  test("a gesture that lost every race says it is not in the map, and offers it again", async () => {
    engine({ status: 200, body: golden.answers.not_landed });
    renderCard(block("placed"));
    fireEvent.click(screen.getAllByRole("button", { name: "Take out" })[0]!);
    fireEvent.change(screen.getByLabelText("Type data-a to confirm"), {
      target: { value: "data-a" },
    });
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Take out" }));
    await waitFor(() => expect(screen.getByText("Not in the map.")).toBeTruthy());
    expect(
      screen.getByText(new RegExp(golden.answers.not_landed!.hint!.slice(0, 40))),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Send again" })).toBeTruthy();
  });

  test("a member on probation is read from, placed on nothing, and counting its way back", () => {
    renderCard(block("placed"));
    // data-f came back after the map removed it: a member again at once —
    // what it held is read through the ranking — but placed on nothing
    // until the ticks trust it, so its strays are KEPT rather than shed.
    expect(screen.getAllByText("probation 12/40").length).toBeGreaterThan(0);
    expect(screen.getByText("37 kept")).toBeTruthy();
    // NOT "present": it is back, and still not a place any copy goes.
    const row = screen.getByText("data-f").closest(".grid-row") as HTMLElement;
    expect(within(row).queryByText("present")).toBeNull();
    // It can be vouched for as well as taken out.
    expect(within(row).getByRole("button", { name: "Put back now" })).toBeTruthy();
    expect(within(row).getByRole("button", { name: "Take out" })).toBeTruthy();
  });

  test("a node removed and not seen since says it rejoins on probation", () => {
    renderCard(block("placed"));
    // NOT "placed on again once present": seen back it is a member at once,
    // on probation — the count it waits on starts then, not now.
    expect(screen.getByText(/has not been seen for 6 ticks/)).toBeTruthy();
    expect(screen.getByText(/rejoins on probation — read from at once/)).toBeTruthy();
    expect(screen.getByText(/gone\s+34\s+more, it is forgotten/)).toBeTruthy();
    // And a node on probation is not listed a second time beside its row.
    expect(screen.queryByText(/data-f.*has not been seen/)).toBeNull();
  });

  test("a scrub that met chunks its disk would not read says so above the members", () => {
    renderCard(block("placed"));
    expect(screen.getByText(/scrub found 2 chunks its disk would not read/)).toBeTruthy();
  });

  test("putting back a member on probation places on it at once", async () => {
    const sent = engine({ status: 200, body: golden.answers.in_probation });
    renderCard(block("placed"));
    const row = screen.getByText("data-f").closest(".grid-row") as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: "Put back now" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Put back" }));
    await waitFor(() => expect(screen.getByText(/is back in the map/)).toBeTruthy());
    expect(sent[0]!.pathname).toBe("/objects/in/data-f");
  });

  test("putting back a removed node says when it is placed on", async () => {
    const sent = engine({ status: 200, body: golden.answers.in_removed });
    renderCard(block("placed"));
    const removed = screen.getByText(/has not been seen for/).closest('[role="status"]')!;
    fireEvent.click(within(removed as HTMLElement).getByRole("button", { name: "Put back now" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Put back" }));
    await waitFor(() => expect(screen.getByText(/is no longer held back/)).toBeTruthy());
    expect(sent[0]!.pathname).toBe("/objects/in/data-e");
    expect(sent[0]!.searchParams.get("confirm")).toBe("data-e");
  });

  test("a hold names its length before it can be sent, and a held map is released", async () => {
    renderCard(block("placed"));
    fireEvent.click(screen.getByRole("button", { name: "Hold map" }));
    const hold = within(screen.getByRole("dialog")).getByRole("button", { name: "Hold" });
    // NO DEFAULT LENGTH: how long a gone member may keep its groups a copy
    // short is the operator's to say.
    expect((hold as HTMLButtonElement).disabled).toBe(true);
    cleanup();

    const sent = engine({ status: 200, body: golden.answers.release });
    renderCard(block("held"));
    fireEvent.click(screen.getByRole("button", { name: "Release hold" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Release" }));
    await waitFor(() => expect(screen.getByText(/The hold is released/)).toBeTruthy());
    expect(sent[0]!.pathname).toBe("/objects/release");
  });

  test("a hold sends the length chosen and says until when", async () => {
    const sent = engine({ status: 200, body: golden.answers.hold });
    renderCard(block("placed"));
    fireEvent.click(screen.getByRole("button", { name: "Hold map" }));
    const dialog = within(screen.getByRole("dialog"));
    fireEvent.click(dialog.getByRole("combobox", { name: "For how long" }));
    // THE LISTBOX COMMITS ON MOUSEDOWN, as a native select does, so the
    // list closes before the click that would land on what is under it.
    fireEvent.mouseDown(screen.getByRole("option", { name: "2h" }));
    fireEvent.change(dialog.getByLabelText("Why"), { target: { value: "kernel upgrade" } });
    fireEvent.click(dialog.getByRole("button", { name: "Hold" }));
    await waitFor(() => expect(screen.getByText(/The map is held/)).toBeTruthy());
    expect(sent[0]!.pathname).toBe("/objects/hold");
    expect(sent[0]!.searchParams.get("for")).toBe("2h");
    expect(sent[0]!.searchParams.get("reason")).toBe("kernel upgrade");
  });

  test("a hold that landed still says it held after the re-read shows the hold", async () => {
    // THE RE-READ THE GESTURE SETS OFF puts the hold on the card behind the
    // open dialog. The dialog is for the gesture it opened for: it must not
    // turn into "Release the hold" and report the opposite of what happened.
    engine({ status: 200, body: golden.answers.hold });
    renderRereading(block("placed"), block("held"));
    fireEvent.click(screen.getByRole("button", { name: "Hold map" }));
    const dialog = within(screen.getByRole("dialog"));
    fireEvent.click(dialog.getByRole("combobox", { name: "For how long" }));
    fireEvent.mouseDown(screen.getByRole("option", { name: "2h" }));
    fireEvent.click(dialog.getByRole("button", { name: "Hold" }));
    await waitFor(() => expect(screen.getByText(/The map is held until/)).toBeTruthy());
    // The card behind did re-read: its header now offers the release.
    expect(screen.getByRole("button", { name: "Release hold" })).toBeTruthy();
    expect(screen.queryByText(/The hold is released/)).toBeNull();
    expect(screen.queryByText("Release the hold")).toBeNull();
  });

  test("a release that landed still says it released after the re-read shows none", async () => {
    engine({ status: 200, body: golden.answers.release });
    renderRereading(block("held"), block("placed"));
    fireEvent.click(screen.getByRole("button", { name: "Release hold" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Release" }));
    await waitFor(() => expect(screen.getByText(/The hold is released/)).toBeTruthy());
    expect(screen.getByRole("button", { name: "Hold map" })).toBeTruthy();
    expect(screen.queryByText(/The map is held/)).toBeNull();
    // Still the dialog it opened as, not "Hold the placement map".
    expect(within(screen.getByRole("dialog")).getByText("Release the hold")).toBeTruthy();
  });

  test("the outcome says what the engine answered, never merely what was asked", async () => {
    // A landed answer that still names a hold in force is a map that is
    // HELD, whatever this dialog sent: the line under it is the engine's
    // account, so it cannot claim a release the map does not show.
    engine({ status: 200, body: golden.answers.hold });
    renderCard(block("held"));
    fireEvent.click(screen.getByRole("button", { name: "Release hold" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Release" }));
    await waitFor(() => expect(screen.getByText(/The map is held until/)).toBeTruthy());
    expect(screen.queryByText(/The hold is released/)).toBeNull();
  });

  test("a hold nobody answered says a resend restarts it; a release says it writes nothing", async () => {
    // A HOLD IS THE ONE GESTURE A REPEAT IS NOT A NO-OP FOR: sent again it
    // replaces the hold in force, its length counted from the resend.
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("Failed to fetch");
      }),
    );
    renderCard(block("placed"));
    fireEvent.click(screen.getByRole("button", { name: "Hold map" }));
    const dialog = within(screen.getByRole("dialog"));
    fireEvent.click(dialog.getByRole("combobox", { name: "For how long" }));
    fireEvent.mouseDown(screen.getByRole("option", { name: "1h" }));
    fireEvent.click(dialog.getByRole("button", { name: "Hold" }));
    await waitFor(() => expect(screen.getByText("No answer.")).toBeTruthy());
    expect(screen.getByText(/replaces the one in force/)).toBeTruthy();
    cleanup();

    renderCard(block("held"));
    fireEvent.click(screen.getByRole("button", { name: "Release hold" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Release" }));
    await waitFor(() => expect(screen.getByText("No answer.")).toBeTruthy());
    expect(screen.getByText(/changes nothing and writes nothing/)).toBeTruthy();
  });

  test("the hold lengths end at the engine's own ceiling", () => {
    // A LONGER CHOICE WOULD BE REFUSED `invalid_hold`: the engine's ceiling
    // is a day, and a hold nobody releases must still end.
    expect(HOLD_LENGTHS.at(-1)).toBe("24h");
  });
});
