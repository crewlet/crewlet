/**
 * THE ESTATE SCREEN RENDERS THE ENGINE'S OWN ANSWERS: every state of the map,
 * every gesture answer and every refusal below is read from the golden the Go
 * renderers write (`internal/api/testdata/estate_answer.json`), for the reason
 * the placement card's suite reads its own — a fixture typed here agrees with
 * the screen whatever the engine sends.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { EstateView, estateSummary } from "./Estate.tsx";
import { Router } from "~/app/router.tsx";
import { engineFile } from "~/test/engineFiles.ts";
import type { EstateGestureAnswer, FleetEstate, PlacedEstate } from "~/protocol/index.ts";

interface EstateGolden {
  estate: Record<string, FleetEstate>;
  answers: Record<string, EstateGestureAnswer>;
  refusals: Record<string, { status: number; body: Record<string, unknown> }>;
}
const golden = engineFile<EstateGolden>("internal/api/testdata/estate_answer.json");

function state(name: string): FleetEstate {
  const s = golden.estate[name];
  if (!s) throw new Error(`the golden has no estate named ${name}`);
  return structuredClone(s);
}
function placed(name: string): PlacedEstate {
  const s = state(name);
  if (s.state !== "placed") throw new Error(`${name} is not a placed map`);
  return s;
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

function view(estate: FleetEstate, onChanged?: () => void) {
  // IN A ROUTER, because a node links to its own fleet page.
  return render(
    <Router>
      <EstateView estate={estate} onChanged={onChanged} />
    </Router>,
  );
}

beforeEach(() => localStorage.setItem("crewlet_api_token", "t"));
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe("layout 0", () => {
  test("says every data node holds the whole estate, and lists them", () => {
    view(state("whole"));
    // THE ENGINE'S SENTENCE, the one every surface gives this layout.
    expect(
      screen.getByText(
        /Layout 0: every data node holds the whole estate, so there is no partition/,
      ),
    ).toBeTruthy();
    expect(screen.getByText("data-a")).toBeTruthy();
    expect(screen.getByText("serving")).toBeTruthy();
    // A BUILD FROM BEFORE THE ESTATE LEASE holds the whole estate all the
    // same, and is listed — named as such, never dropped.
    expect(screen.getByText("data-b")).toBeTruthy();
    expect(screen.getAllByText(/from before the estate lease/).length).toBeGreaterThan(0);
    // AND THERE IS NOTHING TO MOVE: no gesture is offered.
    expect(screen.queryByRole("button", { name: "Hold map" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Take out" })).toBeNull();
  });
});

describe("the states with nothing to show", () => {
  test.each(["no_map", "unavailable", "unreadable"])("%s says its own sentence", (name) => {
    const s = state(name);
    view(s);
    if (s.state === "placed" || s.state === "whole") throw new Error("not a bare state");
    expect(screen.getByText(s.detail)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Hold map" })).toBeNull();
  });
});

describe("a placed map", () => {
  test("the summary says the layout, the epoch, the copies and the balance", () => {
    expect(estateSummary(placed("placed"))).toBe(
      "layout 1 · epoch 9 · 2 copies of each partition · across 3 zone values · balanced within 3.1%",
    );
  });

  test("the counts are the engine's, and the first space's partitions are drawn", () => {
    view(state("placed"));
    const p = placed("placed");
    const stat = (label: string) =>
      screen
        .getByText(label)
        .closest(".crewlet-statcard")
        ?.querySelector(".crewlet-statcard__value")?.textContent;
    expect(stat("Unserved")).toBe(String(p.unserved));
    expect(stat("Short of copies")).toBe(String(p.short));
    expect(stat("Moves in force")).toBe(String(p.moves));
    // THE TRACKER SPACE FIRST, the layout's own order.
    expect(screen.getByText("tracker.000")).toBeTruthy();
    expect(screen.queryByText("pages.000")).toBeNull();
  });

  test("a holder's chip says what the map says, what its lease says, and when its node is gone", () => {
    view(state("placed"));
    // tracker.000: data-a joining and still adopting; data-d leaving and
    // draining — the map's state first, the node's own word beside it.
    expect(screen.getByText("data-a joining · adopting")).toBeTruthy();
    expect(screen.getByText("data-d leaving · draining")).toBeTruthy();
    // tracker.001's only serving holder is on a node the map counts gone:
    // a copy routers reach and nothing answers, drawn as danger.
    const gone = screen.getByText("data-b serving · absent");
    expect(gone.closest(".crewlet-tag")!.className).toContain("crewlet-tag--danger");
  });

  test("another space is a tab away", () => {
    view(state("placed"));
    fireEvent.click(screen.getByRole("tab", { name: /pages/ }));
    expect(screen.getByText("pages.000")).toBeTruthy();
    expect(screen.queryByText("tracker.000")).toBeNull();
  });

  test("a member says its share, what it holds, its presence and its store", () => {
    view(state("placed"));
    const row = screen
      .getByText("data-c", { selector: "a *, a" })
      .closest(".grid-row") as HTMLElement;
    expect(within(row).getByText(/moved off 1/)).toBeTruthy();
    const b = screen
      .getAllByText("data-b")
      .map((el) => el.closest(".grid-row"))
      .find(Boolean) as HTMLElement;
    expect(within(b).getByText("absent 12/40")).toBeTruthy();
    expect(within(b).getByText("It holds no estate lease")).toBeTruthy();
  });

  test("an evicted node is barred on its row, and one the map does not hold is listed", () => {
    view(state("barred"));
    const d = screen
      .getAllByText("data-d")
      .map((el) => el.closest(".grid-row"))
      .find(Boolean) as HTMLElement;
    expect(within(d).getByText("barred")).toBeTruthy();
    expect(within(d).queryByText("out")).toBeNull();
    // data-x, never held, and data-e, which the map removed: each named once,
    // as barred — data-e is not also offered a removed node's "Put back now".
    expect(screen.getAllByText(/is barred from the estate map by founder/)).toHaveLength(2);
    expect(screen.getByText("data-x")).toBeTruthy();
    expect(screen.getAllByText("data-e")).toHaveLength(1);
    expect(screen.queryByText(/was removed for being/)).toBeNull();
  });

  test("a hold in force is a banner, and the header offers its release", () => {
    view(state("held"));
    expect(screen.getByText("Held")).toBeTruthy();
    expect(screen.getByText(/kernel upgrade/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Release hold" })).toBeTruthy();
  });
});

describe("the gestures", () => {
  test("taking a member out is typed out, sends the reason, and re-reads the map", async () => {
    const sent = engine({ status: 200, body: golden.answers.out });
    const changed = vi.fn();
    view(state("placed"), changed);
    const row = screen
      .getByText("data-c", { selector: "a *, a" })
      .closest(".grid-row") as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: "Take out" }));
    const confirm = () =>
      within(screen.getByRole("dialog")).getByRole("button", { name: "Take out" });
    expect((confirm() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Type data-c to confirm"), {
      target: { value: "data-c" },
    });
    fireEvent.change(screen.getByLabelText("Why"), { target: { value: "disk swap" } });
    fireEvent.click(confirm());
    await waitFor(() =>
      expect(screen.getByText(/is out: no partition's target names it/)).toBeTruthy(),
    );
    expect(sent[0]!.pathname).toBe("/estate/out/data-c");
    expect(sent[0]!.searchParams.get("confirm")).toBe("data-c");
    expect(sent[0]!.searchParams.get("reason")).toBe("disk swap");
    expect(changed).toHaveBeenCalled();
  });

  test("an out with nowhere to rebuild says what to change, and takes nothing out", async () => {
    const refusal = golden.refusals.nowhere_to_rebuild!;
    engine({ status: refusal.status, body: refusal.body });
    const changed = vi.fn();
    view(state("placed"), changed);
    const row = screen
      .getByText("data-c", { selector: "a *, a" })
      .closest(".grid-row") as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: "Take out" }));
    fireEvent.change(screen.getByLabelText("Type data-c to confirm"), {
      target: { value: "data-c" },
    });
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Take out" }));
    await waitFor(() => expect(screen.getByText(String(refusal.body.detail))).toBeTruthy());
    // THE FIELD THAT MEANS FEWER COPIES is named, in the engine's own words.
    expect(screen.getByText(String(refusal.body.hint))).toBeTruthy();
    expect(String(refusal.body.hint)).toMatch(/estate\.replicas/);
    expect(screen.queryByText(/is out: no partition's target names it/)).toBeNull();
  });

  test("a hold is confirmed by the map's generation, and says until when", async () => {
    const sent = engine({ status: 200, body: golden.answers.hold });
    view(state("placed"));
    fireEvent.click(screen.getByRole("button", { name: "Hold map" }));
    const dialog = within(screen.getByRole("dialog"));
    // NO DEFAULT LENGTH.
    expect((dialog.getByRole("button", { name: "Hold" }) as HTMLButtonElement).disabled).toBe(true);
    // THE GENERATION IT IS CONFIRMED BY IS NAMED, so the reader sees which map.
    expect(dialog.getByText(placed("placed").generation)).toBeTruthy();
    fireEvent.click(dialog.getByRole("combobox", { name: "For how long" }));
    fireEvent.mouseDown(screen.getByRole("option", { name: "2h" }));
    fireEvent.click(dialog.getByRole("button", { name: "Hold" }));
    await waitFor(() => expect(screen.getByText(/The estate map is held until/)).toBeTruthy());
    expect(sent[0]!.pathname).toBe("/estate/hold");
    expect(sent[0]!.searchParams.get("for")).toBe("2h");
    expect(sent[0]!.searchParams.get("confirm")).toBe(placed("placed").generation);
  });

  test("a release is confirmed by the generation too", async () => {
    const sent = engine({ status: 200, body: golden.answers.release });
    view(state("held"));
    fireEvent.click(screen.getByRole("button", { name: "Release hold" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Release" }));
    await waitFor(() => expect(screen.getByText(/The hold is released/)).toBeTruthy());
    expect(sent[0]!.pathname).toBe("/estate/release");
    expect(sent[0]!.searchParams.get("confirm")).toBe(placed("held").generation);
  });

  test("a move names the node among the holders, typed out, and says the new target", async () => {
    const sent = engine({ status: 200, body: golden.answers.move });
    view(state("placed"));
    const row = screen.getByText("tracker.000").closest(".grid-row") as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: "Move a copy" }));
    const dialog = within(screen.getByRole("dialog"));
    fireEvent.click(dialog.getByRole("combobox", { name: "Off which node" }));
    fireEvent.mouseDown(screen.getByRole("option", { name: "data-c" }));
    const move = () => dialog.getByRole("button", { name: "Move" });
    expect((move() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText("Type data-c to confirm"), {
      target: { value: "data-c" },
    });
    fireEvent.change(dialog.getByLabelText("Why"), { target: { value: "disk swap" } });
    fireEvent.click(move());
    await waitFor(() => expect(screen.getByText(/moves off/)).toBeTruthy());
    expect(screen.getByText(/its target is now data-a, data-b/)).toBeTruthy();
    expect(sent[0]!.pathname).toBe("/estate/move/tracker.000");
    expect(sent[0]!.searchParams.get("from")).toBe("data-c");
    expect(sent[0]!.searchParams.get("confirm")).toBe("data-c");
    expect(sent[0]!.searchParams.get("reason")).toBe("disk swap");
  });

  test("a move in force is cancelled from its partition's row", async () => {
    const sent = engine({ status: 200, body: golden.answers.cancel });
    view(state("placed"));
    fireEvent.click(screen.getByRole("button", { name: "Off data-c · cancel" }));
    fireEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel the move" }),
    );
    await waitFor(() => expect(screen.getByText(/is lifted/)).toBeTruthy());
    expect(sent[0]!.pathname).toBe("/estate/move/tracker.002/cancel");
    expect(sent[0]!.searchParams.get("from")).toBe("data-c");
    expect(sent[0]!.searchParams.get("confirm")).toBe("data-c");
  });

  test("a move the members left no room for says it waits", () => {
    view(state("waiting"));
    // THE MOVE IS ON THE MAP AND NOT IN EFFECT: data-b is out, so the two
    // members left hold tracker.002's two copies, data-c among them.
    const move = screen.getByRole("button", { name: "Off data-c · waiting · cancel" });
    expect(move.getAttribute("title")).toMatch(/no other member can hold its copy/);
    cleanup();
    view(state("placed"));
    expect(screen.getByRole("button", { name: "Off data-c · cancel" })).toBeTruthy();
  });

  test("a refusal says what is wrong and what to do", async () => {
    const refusal = golden.refusals.nowhere_to_move!;
    engine({ status: refusal.status, body: refusal.body });
    view(state("placed"));
    const row = screen.getByText("tracker.002").closest(".grid-row") as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: "Move a copy" }));
    const dialog = within(screen.getByRole("dialog"));
    fireEvent.click(dialog.getByRole("combobox", { name: "Off which node" }));
    fireEvent.mouseDown(screen.getByRole("option", { name: "data-a" }));
    fireEvent.change(dialog.getByLabelText("Type data-a to confirm"), {
      target: { value: "data-a" },
    });
    fireEvent.click(dialog.getByRole("button", { name: "Move" }));
    await waitFor(() => expect(screen.getByText(String(refusal.body.detail))).toBeTruthy());
    expect(screen.getByText(String(refusal.body.hint))).toBeTruthy();
  });

  test("a hold nobody answered says a resend replaces it", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("Failed to fetch");
      }),
    );
    view(state("placed"));
    fireEvent.click(screen.getByRole("button", { name: "Hold map" }));
    const dialog = within(screen.getByRole("dialog"));
    fireEvent.click(dialog.getByRole("combobox", { name: "For how long" }));
    fireEvent.mouseDown(screen.getByRole("option", { name: "1h" }));
    fireEvent.click(dialog.getByRole("button", { name: "Hold" }));
    await waitFor(() => expect(screen.getByText("No answer.")).toBeTruthy());
    expect(screen.getByText(/replaces the one in force/)).toBeTruthy();
  });
});
