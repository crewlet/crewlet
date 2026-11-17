/**
 * Typing into an open setting panel as a person does: a number into its spin
 * button, a duration as an amount and a unit picked beside it.
 */

import { act, fireEvent, screen } from "@testing-library/react";

const UNITS: Record<string, string> = { s: "seconds", m: "minutes", h: "hours", d: "days" };

/** Picks `unit` ("days") in the open panel's unit picker. */
export async function pickUnit(unit: string): Promise<void> {
  await act(async () => {
    fireEvent.click(screen.getByRole("combobox", { name: "Unit" }));
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("option", { name: unit }));
  });
}

/**
 * Types `value` into the open panel's field and returns it. A duration is
 * written as it reads, "3d" or "30m": its amount is typed and its unit picked.
 */
export async function typeSetting(value: string): Promise<HTMLElement> {
  const field = screen.queryByRole("spinbutton") ?? screen.getByRole("textbox");
  if (screen.queryByRole("combobox", { name: "Unit" })) {
    const [, amount = "", letter] = /^\s*(.*?)\s*([smhd])?\s*$/.exec(value) ?? [];
    if (letter) await pickUnit(UNITS[letter]!);
    fireEvent.change(field, { target: { value: amount } });
  } else {
    fireEvent.change(field, { target: { value } });
  }
  return field;
}
