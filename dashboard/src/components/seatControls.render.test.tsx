/**
 * A seat's own controls in a row: one of each, however many turns it runs.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import type { SeatLive } from "~/lib/seatLive.ts";
import { SeatControlButtons, type SeatControls } from "./seatControls.tsx";

vi.mock("~/lib/useWriteAccess.ts", async (actual) => ({
  ...(await actual<typeof import("~/lib/useWriteAccess.ts")>()),
  useWriteAccess: () => ({ can: true }),
}));

afterEach(cleanup);

function seatOn(turns: { turn_id: string; work_item?: { key: string } }[]): SeatLive {
  return {
    seat: { handle: "swe", name: "SWE", kind: "agent" },
    row: { turns },
    state: "working",
    paused: false,
    controllable: true,
    ask: null,
  } as unknown as SeatLive;
}

function mount(s: SeatLive, labelled = false) {
  const steered: string[] = [];
  const controls: SeatControls = {
    open: () => {},
    steer: (_s, turnId) => steered.push(turnId),
    present: () => {},
    dialog: null,
  };
  render(
    <LayerHost>
      <SeatControlButtons s={s} controls={controls} labelled={labelled} />
    </LayerHost>,
  );
  return steered;
}

// ONE STEER PER SEAT: a seat on two turns drew two identical compass buttons,
// told apart only by their tooltips, and the row wrapped onto a second line.
test("a seat on several turns draws one Steer, whose menu names each turn", () => {
  const steered = mount(
    seatOn([{ turn_id: "t1", work_item: { key: "ENG-4" } }, { turn_id: "t2" }]),
  );
  const steer = screen.getAllByRole("button", { name: /^Steer/ });
  expect(steer).toHaveLength(1);
  expect(steer[0]!.getAttribute("aria-haspopup")).toBe("menu");
  fireEvent.click(steer[0]!);
  const items = screen.getAllByRole("menuitem").map((item) => item.textContent);
  expect(items).toEqual(["Steer on ENG-4", "Steer 2 of 2"]);
  fireEvent.click(screen.getByRole("menuitem", { name: "Steer 2 of 2" }));
  expect(steered).toEqual(["t2"]);
});

test("a seat on one turn steers it with one press, in a row and on a panel", () => {
  for (const labelled of [false, true]) {
    const steered = mount(seatOn([{ turn_id: "t1" }]), labelled);
    const steer = screen.getByRole("button", { name: "Steer SWE's turn" });
    expect(steer.getAttribute("aria-haspopup")).toBeNull();
    fireEvent.click(steer);
    expect(steered).toEqual(["t1"]);
    cleanup();
  }
});

// A ROW'S ICONS NEVER WRAP: the cell is sized for the widest set it draws.
test("a row's controls are the icon row, which holds one line", () => {
  mount(seatOn([{ turn_id: "t1" }, { turn_id: "t2" }]));
  const row = document.querySelector(".seat-controls");
  expect(row?.classList.contains("seat-controls--icons")).toBe(true);
  expect(row?.querySelectorAll("button")).toHaveLength(3);
});
