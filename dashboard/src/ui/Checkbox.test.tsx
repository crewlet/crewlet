/**
 * A checkbox is named by its label, described by its consequence, and
 * toggled from anywhere on its row.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { Checkbox } from "./Checkbox.tsx";

afterEach(cleanup);

function Choice({ disabled }: { disabled?: boolean }) {
  const [on, setOn] = useState(false);
  return (
    <Checkbox
      framed
      tone="critical"
      checked={on}
      onChange={setOn}
      disabled={disabled}
      label="Also remove the accounts Crewlet created"
      description="Each agent's account at the vendor is deleted."
    />
  );
}

test("the name is the label alone, and the consequence is its description", () => {
  render(<Choice />);
  const box = screen.getByRole("checkbox", { name: "Also remove the accounts Crewlet created" });
  const described = document.getElementById(box.getAttribute("aria-describedby") ?? "");
  expect(described?.textContent).toBe("Each agent's account at the vendor is deleted.");
});

test("a press anywhere on the row toggles it, and the change reports the new state", () => {
  const onChange = vi.fn();
  render(<Checkbox label="Clear lead" checked={false} onChange={onChange} />);
  fireEvent.click(screen.getByText("Clear lead"));
  expect(onChange).toHaveBeenCalledWith(true);
});

test("a disabled checkbox cannot be ticked", () => {
  render(<Choice disabled />);
  const box = screen.getByRole("checkbox") as HTMLInputElement;
  fireEvent.click(screen.getByText("Also remove the accounts Crewlet created"));
  expect(box.checked).toBe(false);
  expect(box.disabled).toBe(true);
});

test("a destructive, framed choice carries both on the row, and a plain one carries neither", () => {
  const { container, rerender } = render(<Choice />);
  const row = container.querySelector("label")!;
  expect(row.classList.contains("critical")).toBe(true);
  expect(row.classList.contains("framed")).toBe(true);
  rerender(<Checkbox label="Enabled" checked onChange={() => {}} />);
  const plain = container.querySelector("label")!;
  expect(plain.classList.contains("critical") || plain.classList.contains("framed")).toBe(false);
  expect(screen.getByRole("checkbox").getAttribute("aria-describedby")).toBeNull();
});
