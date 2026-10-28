/**
 * Steer is offered on every turn that is still going.
 *
 * A note to a turn in the engine's own loop is read at its next round; one to
 * a turn whose coding run is out waits on the run's record and is read by the
 * run at its next step, after its question is answered, or by the review that
 * follows it (`internal/agent/steer`). Only an ended turn holds the control,
 * with the reason — and what the engine answered about WHEN the note is read is
 * what the toast says.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { SteerTurnButton } from "./writes.tsx";
import { steerReadsSentence } from "~/lib/steer.ts";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { STEER_NOTE_MAX_RUNES, STEER_READS } from "~/contract/steer.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";

vi.mock("~/lib/store-hooks.ts", () => ({
  useConnection: vi.fn(),
  useOrg: vi.fn(),
  useClient: vi.fn(),
}));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

const JANE: ViewerState = {
  login: "founder",
  grants: ["admin", "worklet", "doclet"],
  admin: true,
  worklet: true,
  doclet: true,
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
  configWriter: true,
  configManagedBy: [],
  assistant: "disabled",
  assistantModel: null,
};

beforeEach(() => {
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useOrg).mockReturnValue(null as never);
  vi.mocked(useViewer).mockReturnValue(JANE);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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
    render(<SteerTurnButton turnId="t-1" seat="CEO" running />);
    const steer = screen.getByRole("button", { name: /Steer/ });
    expect(steer.hasAttribute("disabled") || steer.getAttribute("aria-disabled") === "true").toBe(
      false,
    );
    fireEvent.click(steer);
    expect(screen.getByRole("dialog", { name: "Steer CEO's turn" })).toBeTruthy();
    expect(
      screen.getByText(
        new RegExp(`reads it at its next step\\. 0 of ${STEER_NOTE_MAX_RUNES} characters`),
      ),
    ).toBeTruthy();
  });

  test("is pressable on a turn whose coding run is out — the note waits on the run", () => {
    // A turn parked on its run is still running to the screen (its stage is
    // `parked`), and that is all the control asks.
    render(<SteerTurnButton turnId="t-1" seat="CEO" running />);
    const steer = screen.getByRole("button", { name: /Steer/ });
    expect(heldBecause(steer)).not.toMatch(/coding run is working/);
    fireEvent.click(steer);
    expect(screen.getByRole("dialog", { name: "Steer CEO's turn" })).toBeTruthy();
  });

  test("is held on a turn that has ended", () => {
    render(<SteerTurnButton turnId="t-1" seat="CEO" running={false} />);
    const steer = screen.getByRole("button", { name: /Steer/ });
    expect(steer.hasAttribute("disabled") || steer.getAttribute("aria-disabled") === "true").toBe(
      true,
    );
    expect(heldBecause(steer)).toMatch(/The turn has ended\./);
    fireEvent.click(steer);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

// A NOTE THE TURN REFUSES ON ITS BOUNDS SAYS WHICH BOUND, in the words that
// tell a person what to do next: one that holds as many unread notes as it may
// takes this note once it has read them, and one that has taken all it takes
// never will. Neither is a write that collided with another, which "look again,
// then retry" would tell them it was.
describe("a note the turn refuses on its bounds", () => {
  test.each([
    ["notes_full", "Turn t-1 already holds 5 notes it has not read yet."],
    ["notes_spent", "Turn t-1 has been sent the 10 notes a turn takes."],
  ] as const)("%s is said in its own words", async (code, detail) => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ error: code, detail }), {
            status: 409,
            headers: { "Content-Type": "application/json" },
          }),
      ),
    );
    render(
      <LayerHost>
        <ToastProvider>
          <SteerTurnButton turnId="t-1" seat="CEO" running />
        </ToastProvider>
      </LayerHost>,
    );
    fireEvent.click(screen.getByRole("button", { name: /Steer/ }));
    const dialog = await screen.findByRole("dialog", { name: "Steer CEO's turn" });
    fireEvent.change(within(dialog).getByLabelText("Note"), { target: { value: "use staging" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Send note" }));
    await waitFor(() => expect(within(dialog).getByText(ACT_ERRORS[code])).toBeTruthy());
    expect(within(dialog).queryByText(ACT_ERRORS.conflict)).toBeNull();
    expect(dialog.textContent).not.toMatch(/collided/);
  });
});

describe("steerReadsSentence", () => {
  test("says when the turn reads the note, for every value the engine answers", () => {
    expect(steerReadsSentence({ outcome: "pending", reads: "next_step" })).toBe(
      "It reads it at its next step.",
    );
    expect(steerReadsSentence({ outcome: "pending", reads: "after_answer" })).toMatch(
      /waiting on a question; it reads the note once that is answered/,
    );
    for (const reads of STEER_READS) {
      expect(steerReadsSentence({ reads })).toBeTruthy();
    }
  });

  test("says nothing of its own for an answer it cannot read", () => {
    expect(steerReadsSentence({ reads: "some_later_value" })).toBeUndefined();
    expect(steerReadsSentence({})).toBeUndefined();
    expect(steerReadsSentence(null)).toBeUndefined();
  });
});
