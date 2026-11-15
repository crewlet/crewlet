import { afterEach, describe, expect, test } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { FirstDialChip, StateBar, degradationOf } from "./StateBar.tsx";

afterEach(cleanup);

const noop = () => {};
const state = (connected: boolean, everConnected: boolean, configured = true) =>
  degradationOf({
    authRejected: false,
    connected,
    everConnected,
    configured,
    onSignIn: noop,
    onConfig: noop,
  });

// THE FIRST DIAL IS THE PAGE BAR'S. In the flow it pushed every page down by
// its height on a cold load, and floated under the bar it lay over the first
// lines of the screen (Home's greeting, struck through). Mutation: draw the
// first dial as a strip again and the PageHeader suite's case goes red.
describe("the state bar's connection notices", () => {
  test("the first dial is said once, as a status, by the chip the page bar draws", () => {
    const first = state(false, false)!;
    expect(first.firstDial).toBe(true);
    render(<FirstDialChip degraded={first} />);
    const status = screen.getByRole("status", { name: "Connecting to the engine." });
    expect(status.textContent).toBe("Connecting to the engine.");
  });

  test("with no page bar to say it in, the first dial stands in the flow", () => {
    const { container } = render(<StateBar degraded={state(false, false)} />);
    expect(container.querySelector(".state-bar")?.textContent).toContain(
      "Connecting to the engine.",
    );
  });

  test("a reconnect and a missing company stay strips under the bar, where a reader is told", () => {
    const reconnect = state(false, true)!;
    expect(reconnect.firstDial).toBeUndefined();
    const drawn = render(<StateBar degraded={reconnect} />);
    expect(drawn.container.textContent).toContain("Reconnecting.");
    drawn.unmount();
    const unconfigured = render(<StateBar degraded={state(true, true, false)} />);
    expect(unconfigured.container.textContent).toContain("No company is active");
  });
});
