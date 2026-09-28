/**
 * The write control presses only when it can, never navigates the row it sits
 * in, and keeps the last refusal beside itself.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { WriteButton } from "./WriteButton.tsx";
import type { Act } from "~/lib/useAct.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";

afterEach(cleanup);

function write(over: Partial<Act<"set_pins">> = {}): Act<"set_pins"> {
  return {
    access: { can: true, as: "jane", acts: ["set_pins"] },
    busy: false,
    refusal: null,
    run: vi.fn(async () => null),
    retry: vi.fn(async () => null),
    dismiss: vi.fn(),
    ...over,
  };
}

test("a press is the button's, not the link's around it", () => {
  const onPress = vi.fn();
  const navigate = vi.fn();
  render(
    <a href="#/work/ENG-1" onClick={navigate}>
      <WriteButton write={write()} onPress={onPress}>
        Pin
      </WriteButton>
    </a>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  expect(onPress).toHaveBeenCalledOnce();
  expect(navigate).not.toHaveBeenCalled();
});

test("a control that cannot act stays focusable, says why, and does nothing", () => {
  const onPress = vi.fn();
  render(
    <WriteButton
      write={write({ access: { can: false, block: "anonymous", reason: WRITE_REASONS.anonymous } })}
      onPress={onPress}
    >
      Pin
    </WriteButton>,
  );
  const button = screen.getByRole("button", { name: "Pin" });
  expect(button.hasAttribute("disabled")).toBe(false);
  expect(button.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(button);
  expect(onPress).not.toHaveBeenCalled();
  expect(document.body.textContent).toContain(WRITE_REASONS.anonymous);
});

test("a held control shows a pointer its reason, and a screen reader hears it once", () => {
  render(
    <WriteButton write={write()} onPress={() => {}} blocked="Choose a view first.">
      Pin
    </WriteButton>,
  );
  const button = screen.getByRole("button", { name: "Pin" });
  // THE KIT HIDES THE REASON FROM THE EYE; the title is what a hover shows.
  expect(button.getAttribute("title")).toBe("Choose a view first.");
  // Its description is the kit's hidden sentence, and only that: a title
  // that became the description too would be the same words read twice.
  const described = (button.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .filter(Boolean)
    .map((id) => document.getElementById(id)?.textContent);
  expect(described).toEqual(["Choose a view first."]);
});

test("a control that can act carries no reason as its title", () => {
  render(
    <WriteButton write={write()} onPress={() => {}}>
      Pin
    </WriteButton>,
  );
  expect(screen.getByRole("button", { name: "Pin" }).hasAttribute("title")).toBe(false);
});

test("a press in flight does not press again", () => {
  const onPress = vi.fn();
  render(
    <WriteButton write={write({ busy: true })} onPress={onPress}>
      Pin
    </WriteButton>,
  );
  fireEvent.click(screen.getByRole("button", { name: "Pin" }));
  expect(onPress).not.toHaveBeenCalled();
});

test("a refusal the node caused offers to try again, under the same request", () => {
  const w = write({
    refusal: { sentence: "This node is shutting down.", hint: "", retryable: true },
  });
  render(
    <WriteButton write={w} onPress={() => {}}>
      Pin
    </WriteButton>,
  );
  expect(screen.getByRole("alert").textContent).toContain("This node is shutting down.");
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(w.retry).toHaveBeenCalledOnce();
  expect(w.run).not.toHaveBeenCalled();
});

test("a refusal the request caused offers no retry: the same request would be refused again", () => {
  render(
    <WriteButton
      write={write({
        refusal: { sentence: "views: v9 is not a view", hint: "", retryable: false },
      })}
      onPress={() => {}}
    >
      Pin
    </WriteButton>,
  );
  expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
  expect(screen.getByRole("button", { name: "Dismiss" })).toBeTruthy();
});

test("an argument the engine names in backticks is drawn as a value, not punctuation", () => {
  render(
    <WriteButton
      write={write({
        refusal: {
          sentence: "`title` is 600 bytes and a task's title holds at most 256.",
          hint: "",
          retryable: false,
        },
      })}
      onPress={() => {}}
    >
      Pin
    </WriteButton>,
  );
  const alert = screen.getByRole("alert");
  expect(alert.textContent).not.toContain("`");
  expect(alert.querySelector("code")?.textContent).toBe("title");
});
