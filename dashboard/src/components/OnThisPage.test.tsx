import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { OnThisPage, pageOutline } from "./OnThisPage.tsx";

afterEach(cleanup);

describe("pressing an outline entry", () => {
  const BODY = "## First\n\nwords\n\n## Second\n\nwords";

  function draw() {
    const scrollTo = vi.fn();
    const pressedInto = vi.spyOn(Element.prototype, "scrollIntoView");
    render(
      <div>
        <div data-testid="scroller" style={{ overflowY: "auto" }}>
          {pageOutline(BODY).map((h) => (
            <h2 key={h.id} id={h.id}>
              {h.text}
            </h2>
          ))}
        </div>
        <OnThisPage body={BODY} />
      </div>,
    );
    screen.getByTestId("scroller").scrollTo = scrollTo;
    return { scrollTo, pressedInto };
  }

  it("moves the heading's own scroller and no other, and carries focus there", () => {
    const { scrollTo, pressedInto } = draw();
    fireEvent.click(screen.getByRole("button", { name: "Second" }));
    expect(scrollTo).toHaveBeenCalledTimes(1);
    // A scrollIntoView scrolls every ancestor, the document included.
    expect(pressedInto).not.toHaveBeenCalled();
    expect(document.activeElement?.textContent).toBe("Second");
    pressedInto.mockRestore();
  });
});
