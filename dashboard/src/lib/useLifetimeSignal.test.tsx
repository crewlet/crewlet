/**
 * The lifetime signal is live while its component is mounted and aborted the
 * moment it unmounts, and a mount after an unmount is a live one again.
 */

import { render } from "@testing-library/react";
import { StrictMode } from "react";
import { expect, test } from "vitest";
import { useLifetimeSignal } from "./useLifetimeSignal.ts";

let read: () => AbortSignal = () => AbortSignal.abort();

function Probe() {
  read = useLifetimeSignal();
  return null;
}

test("a request started while mounted is aborted when the component leaves", () => {
  const view = render(<Probe />);
  const signal = read();
  expect(signal.aborted).toBe(false);
  view.unmount();
  expect(signal.aborted).toBe(true);
  // ASKED FOR AFTER IT LEFT, nothing may start.
  expect(read().aborted).toBe(true);
});

test("Strict Mode's second mount hands out a live signal, not the first mount's", () => {
  const view = render(
    <StrictMode>
      <Probe />
    </StrictMode>,
  );
  expect(read().aborted).toBe(false);
  view.unmount();
});
