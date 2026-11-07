import { afterEach, describe, expect, test } from "vitest";
import { cleanup, render } from "@testing-library/react";

import { StateBar, degradationOf } from "./StateBar.tsx";

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

// THE FIRST DIAL FLOATS: in the flow it pushed every page down by its height
// and pulled it back up when the engine answered, a shift on every cold load.
// Mutation: draw the first dial in the flow and the first case goes red.
describe("the state bar's connection notices", () => {
  test("the first dial floats over the page rather than taking a line", () => {
    const { container } = render(<StateBar degraded={state(false, false)} />);
    const notice = container.querySelector(".state-bar__float");
    expect(notice?.textContent).toContain("Connecting to the engine.");
  });

  test("a reconnect and a missing company stay in the flow, where a reader is told", () => {
    const reconnect = render(<StateBar degraded={state(false, true)} />);
    expect(reconnect.container.querySelector(".state-bar__float")).toBeNull();
    expect(reconnect.container.textContent).toContain("Reconnecting.");
    reconnect.unmount();
    const unconfigured = render(<StateBar degraded={state(true, true, false)} />);
    expect(unconfigured.container.querySelector(".state-bar__float")).toBeNull();
    expect(unconfigured.container.textContent).toContain("No company is active");
  });
});
