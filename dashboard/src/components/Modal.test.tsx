/**
 * A dialog's foot keeps its row when a write control in it is refused: the
 * refusal is drawn above the foot, at the end of the body, and the foot holds
 * its buttons and nothing else.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Button } from "@crewlethq/ui";
import { confirmStepUp, setStepUpConfirmer } from "~/protocol/signin.ts";
import { STILL_WORKING } from "~/lib/waiting.ts";
import { ConfirmModal, Modal } from "./Modal.tsx";
import { WriteButton } from "./WriteButton.tsx";
import type { Act } from "~/lib/useAct.ts";

afterEach(cleanup);

/** A write the engine refused, as a change the reader may not make answers. */
function refused(): Act<"code_review_change"> {
  return {
    access: { can: true, as: "jane", acts: ["code_review_change"] },
    busy: false,
    refusal: {
      sentence: "That change is not yours to make here, and nothing was written.",
      hint: "",
      hostRefusal: "",
      retryable: false,
      grants: [],
      reason: "",
      remedy: null,
    },
    run: vi.fn(async () => null),
    retry: vi.fn(async () => null),
    dismiss: vi.fn(),
  };
}

// THE APPROVE DIALOG'S FOOT, refused. The refusal was drawn in the foot's own
// wrapping row beside the primary action, which dropped the action, the
// sentence and Dismiss onto left-aligned lines of their own under the foot and
// left Cancel alone at its end.
//
// Mutation: open the kit's Modal (no place), and the refusal is in the foot.
test("a refusal of a control in the foot is drawn above the foot, which keeps its buttons", () => {
  render(
    <Modal
      open
      title="Approve web!1"
      onClose={() => {}}
      footer={
        <>
          <Button variant="secondary">Cancel</Button>
          <WriteButton write={refused()} variant="primary" onPress={() => {}}>
            Approve
          </WriteButton>
        </>
      }
    >
      <p>Looks good.</p>
    </Modal>,
  );
  const alert = screen.getByRole("alert");
  expect(alert.textContent).toContain("not yours to make here");
  const foot = document.querySelector(".crewlet-modal__footer")!;
  expect(foot.contains(alert)).toBe(false);
  expect(document.querySelector(".crewlet-modal__body")!.contains(alert)).toBe(true);
  // THE FOOT HOLDS ITS TWO BUTTONS, and no Dismiss of the refusal's.
  expect([...foot.querySelectorAll("button")].map((b) => b.textContent)).toEqual([
    "Cancel",
    "Approve",
  ]);
});

// AND THE HEAD'S ACTIONS AND THE FOOT'S START ARE ROWS TOO.
test("a refusal of a control in the head or the foot's start is drawn above the foot", () => {
  render(
    <Modal
      open
      title="Edit"
      onClose={() => {}}
      headerActions={
        <WriteButton write={refused()} onPress={() => {}}>
          Apply
        </WriteButton>
      }
    >
      <p>The form.</p>
    </Modal>,
  );
  const alert = screen.getByRole("alert");
  expect(document.querySelector(".crewlet-modal__header")?.contains(alert)).toBe(false);
  expect(document.querySelector(".crewlet-modal__body")!.contains(alert)).toBe(true);
});

// A DIALOG WITH NO FOOT STILL HAS NONE: the place is in the body, and a slot
// the caller left empty is not handed a wrapper the kit would draw a band for.
test("a dialog with no foot draws no foot band", () => {
  render(
    <Modal open title="About" onClose={() => {}}>
      <p>Nothing to do here.</p>
    </Modal>,
  );
  expect(document.querySelector(".crewlet-modal__footer")).toBeNull();
});

/** Why the dialog's close control is unavailable, as it is read out with it. */
function closeReason(): string {
  const close = screen
    .getAllByRole("button")
    .find((b) => b.getAttribute("aria-disabled") === "true");
  const id = close?.getAttribute("aria-describedby") ?? "";
  return document.getElementById(id)?.textContent ?? "";
}

// ONE SENTENCE FOR A DIALOG WHOSE WRITE IS OUT, from `lib/waiting.ts`. Forty
// dialogs spelled their own, each "Waiting for the engine to answer", which
// names a part of the product nobody sees and is wrong while a step-up is
// asking the person rather than the engine.
//
// Mutation: drop the default in `Modal`, and the kit's own sentence is read.
test("a dialog that cannot close says it is still working, or what it waits for", async () => {
  render(
    <>
      <Modal open title="New room" onClose={() => {}} dismissable={false}>
        <p>Creating.</p>
      </Modal>
    </>,
  );
  expect(closeReason()).toBe(STILL_WORKING);

  let confirm: (ok: boolean) => void = () => {};
  const uninstall = setStepUpConfirmer(() => new Promise<boolean>((r) => (confirm = r)));
  let asked: Promise<boolean> = Promise.resolve(false);
  act(() => {
    asked = confirmStepUp();
  });
  expect(closeReason()).toBe("Waiting for you to confirm who you are.");
  await act(async () => {
    confirm(true);
    await asked;
  });
  uninstall();
  expect(closeReason()).toBe(STILL_WORKING);
});

// AND A CONFIRMATION IN FLIGHT SAYS THE SAME, rather than the kit's own words.
test("a confirmation in flight says it is still working", () => {
  render(
    <ConfirmModal
      open
      title="Delete this branch?"
      message="It cannot be undone."
      onClose={() => {}}
      onConfirm={() => {}}
      submitting
    />,
  );
  expect(closeReason()).toBe(STILL_WORKING);
});
