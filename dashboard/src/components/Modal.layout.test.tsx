/**
 * A DIALOG'S FOOT KEEPS ITS ROW, measured. A write control in the foot drew its
 * refusal in the foot's own wrapping row, so the primary action and the
 * refusal fell onto left-aligned lines of their own under the foot and Cancel
 * stood alone at its end. With the refusal above the foot, the foot is the
 * height it is without one and its two buttons share a line, at its end.
 */
import "~/test/layout.tsx";
import { afterEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { Button } from "@crewlethq/ui";
import { Modal } from "./Modal.tsx";
import { WriteButton } from "./WriteButton.tsx";
import type { Act } from "~/lib/useAct.ts";

let root: Root | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  root = null;
  document.body.querySelectorAll("[data-host]").forEach((el) => el.remove());
});

function approve(refused: boolean): Act<"code_review_change"> {
  return {
    access: { can: true, as: "jane", acts: ["code_review_change"] },
    busy: false,
    refusal: refused
      ? {
          sentence:
            "That change is not yours to make here, and nothing was written. Ask whoever owns it.",
          hint: "",
          hostRefusal: "",
          retryable: false,
          grants: [],
          reason: "",
          remedy: null,
        }
      : null,
    run: vi.fn(async () => null),
    retry: vi.fn(async () => null),
    dismiss: vi.fn(),
  };
}

/** The foot's box and its two buttons', with the press refused or not. */
function measure(refused: boolean) {
  const host = document.createElement("div");
  host.dataset.host = "";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <Modal
        open
        size="md"
        title="Approve web!1"
        onClose={() => {}}
        footer={
          <>
            <Button variant="secondary">Cancel</Button>
            <WriteButton write={approve(refused)} variant="primary" onPress={() => {}}>
              Approve
            </WriteButton>
          </>
        }
      >
        <p>Looks good.</p>
      </Modal>,
    ),
  );
  const foot = document.querySelector(".crewlet-modal__footer")!.getBoundingClientRect();
  const box = (name: string) =>
    [...document.querySelectorAll(".crewlet-modal__footer button")]
      .find((b) => b.textContent === name)!
      .getBoundingClientRect();
  const out = { foot, cancel: box("Cancel"), approve: box("Approve") };
  flushSync(() => root!.unmount());
  root = null;
  host.remove();
  return out;
}

test("a refused primary action keeps the dialog's foot on one row, at its end", async () => {
  await page.viewport(1440, 900);
  const calm = measure(false);
  const refused = measure(true);
  expect(refused.foot.height).toBe(calm.foot.height);
  expect(refused.approve.top).toBe(refused.cancel.top);
  expect(refused.approve.left).toBeGreaterThan(refused.cancel.right);
  expect(refused.approve.right).toBe(calm.approve.right);
});
