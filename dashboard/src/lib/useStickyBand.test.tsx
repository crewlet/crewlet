import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { useStickyBand } from "./useStickyBand.ts";

afterEach(cleanup);

function Bar() {
  return (
    <main className="crewlet-app-shell__main">
      <div className="toolbar" ref={useStickyBand()} />
    </main>
  );
}

// A WRAPPED BAR IS TALLER THAN ONE BAND, so what sticks under it is offset by
// the height it is drawn at, published on its scroller, and taken off with it.
// Mutation: publish nothing, or leave the property behind, and this goes red.
test("a toolbar publishes its drawn height to its scroller, and takes it back", () => {
  const height = vi
    .spyOn(HTMLElement.prototype, "getBoundingClientRect")
    .mockReturnValue({ height: 84 } as DOMRect);
  try {
    const { container, unmount } = render(<Bar />);
    const scroller = container.querySelector<HTMLElement>(".crewlet-app-shell__main")!;
    expect(scroller.style.getPropertyValue("--toolbar-band")).toBe("84px");
    unmount();
    expect(scroller.style.getPropertyValue("--toolbar-band")).toBe("");
  } finally {
    height.mockRestore();
  }
});
