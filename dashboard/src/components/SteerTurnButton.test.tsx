/**
 * Steer is offered only where a note can reach the turn.
 *
 * A note enters a turn at the top of its next round in the ENGINE'S OWN loop
 * (`internal/agent/steer`). A turn parked on a coding run is in nobody's loop
 * but the run's — its own `run_sandbox`, or an executor that runs as a coding
 * agent — and the engine would answer `steer_unsupported` one press too late.
 * So the control is drawn and HELD there, with the reason and what to do
 * instead, for a reader who could otherwise press it.
 */

import { cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { SteerTurnButton } from "./writes.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { STEER_NOTE_MAX_RUNES } from "~/contract/steer.ts";

vi.mock("~/lib/store-hooks.ts", () => ({
  useConnection: vi.fn(),
  useOrg: vi.fn(),
  useClient: vi.fn(),
}));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

const JANE: ViewerState = {
  // A PERSON BOUND TO A SEAT, signed in: the seat is who the write is
  // recorded as, and their record is kept under it.
  login: "jane",
  grants: ["work:write"],
  operatesFleet: false,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["steer_turn"],
  project: "",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
};

beforeEach(() => {
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue(null as never);
  vi.mocked(useViewer).mockReturnValue(JANE);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

/** The reason the control is held, however the kit tells it. */
function heldBecause(button: HTMLElement): string {
  const described = button.getAttribute("aria-describedby");
  const text = described ? document.getElementById(described)?.textContent : "";
  return `${text ?? ""} ${button.getAttribute("title") ?? ""}`;
}

describe("SteerTurnButton", () => {
  test("is pressable on a turn running in the engine's own loop", () => {
    render(<SteerTurnButton turnId="t-1" seat="CEO" running parked={false} />);
    const steer = screen.getByRole("button", { name: /Steer/ });
    expect(steer.hasAttribute("disabled") || steer.getAttribute("aria-disabled") === "true").toBe(
      false,
    );
    fireEvent.click(steer);
    expect(screen.getByRole("dialog", { name: "Steer CEO's turn" })).toBeTruthy();
    expect(screen.getByText(new RegExp(`0 of ${STEER_NOTE_MAX_RUNES} characters`))).toBeTruthy();
  });

  test("is held on a turn inside a coding agent's own loop, saying why", () => {
    render(<SteerTurnButton turnId="t-1" seat="CEO" running parked />);
    const steer = screen.getByRole("button", { name: /Steer/ });
    expect(steer.hasAttribute("disabled") || steer.getAttribute("aria-disabled") === "true").toBe(
      true,
    );
    expect(heldBecause(steer)).toMatch(/coding agent's own loop/);
    fireEvent.click(steer);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("is held on a turn that has ended", () => {
    render(<SteerTurnButton turnId="t-1" seat="CEO" running={false} parked={false} />);
    const steer = screen.getByRole("button", { name: /Steer/ });
    expect(heldBecause(steer)).toMatch(/a note reaches only a turn that is running/);
  });
});
