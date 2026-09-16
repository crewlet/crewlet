/**
 * The frame outlives every screen in it, so what a screen published has to go
 * when the screen does.
 *
 * `Shell` is mounted once for the life of the tab — `App` renders
 * `<Shell><Screen/></Shell>`, and only `Screen`'s children remount per route —
 * so a value a screen writes into the frame is a value the frame will keep
 * showing over the NEXT screen unless something takes it back. The coverage
 * facts are the ones where that is worst: they are the state bar's answer to
 * "can I trust what I am looking at", and only three screens publish them.
 */

import type { ReactNode } from "react";
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { Shell, usePageCoverage } from "./Shell.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { CoverageFacts } from "~/components/work.tsx";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** A screen that publishes coverage, as the Inbox does. */
function Covered({ coverage }: { coverage: CoverageFacts }) {
  usePageCoverage(coverage);
  return <div>the covered screen</div>;
}

/** A screen that publishes none — Admin, Cost, Company and every detail page. */
function Bare() {
  return <div>the bare screen</div>;
}

/** A DIFFERENT component that also publishes, so React really remounts: two
 *  renders of the same function reconcile, which exercises the deps-changed
 *  path rather than the unmount/mount ordering this has to get right. */
function AlsoCovered({ coverage }: { coverage: CoverageFacts }) {
  usePageCoverage(coverage);
  return <div>the other covered screen</div>;
}

const INCOMPLETE: CoverageFacts = {
  read_level: "linearizable",
  complete: false,
  log_seq: 88,
  applied_through: 41,
  incomplete: {
    records: 3,
    version: 4,
    from: { stream: "CREWLET_WORK_LOG", generation: 1, seq: 42 },
    scope: [],
  },
};

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

function frame(child: ReactNode) {
  const store = new Store();
  const view = render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Router>
        <Shell>{child}</Shell>
      </Router>
    </ClientContext.Provider>,
  );
  return {
    store,
    view,
    show: (next: ReactNode) =>
      view.rerender(
        <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
          <Router>
            <Shell>{next}</Shell>
          </Router>
        </ClientContext.Provider>,
      ),
  };
}

describe("the state bar's coverage", () => {
  test("it is drawn for the screen that published it", () => {
    frame(<Covered coverage={INCOMPLETE} />);
    expect(screen.getByText("linearizable")).toBeDefined();
    expect(screen.getByText(/applied through 41 of 88/)).toBeDefined();
    expect(screen.getByText("This answer is incomplete")).toBeDefined();
  });

  // THE ONE THAT SHIPPED. Land on the Inbox, which publishes; click through to
  // a screen that does not — Admin > Credentials, say — and the inbox's
  // freshness badge and its "this answer is incomplete" banner stayed in the
  // bar as claims about data the new screen never read.
  test("it goes when the screen that published it does", () => {
    const { show } = frame(<Covered coverage={INCOMPLETE} />);
    show(<Bare />);
    expect(screen.getByText("the bare screen")).toBeDefined();
    expect(screen.queryByText("linearizable")).toBeNull();
    expect(screen.queryByText(/applied through/)).toBeNull();
    expect(screen.queryByText("This answer is incomplete")).toBeNull();
  });

  // …and the reset must not eat the INCOMING screen's own facts, which is why
  // it is a cleanup on the hook rather than an effect in the Shell keyed on
  // the route: React flushes a child's effects before its parent's, so a
  // Shell-level reset would run after the new screen had already published.
  test("a screen that publishes its own replaces it rather than losing it", async () => {
    const { show } = frame(<Covered coverage={INCOMPLETE} />);
    show(
      <AlsoCovered
        coverage={{ read_level: "bounded", complete: true, log_seq: 9, applied_through: 9 }}
      />,
    );
    // FLUSHED, because a reset that lands LATE is the failure being excluded:
    // a deferred one — a microtask, a timeout, a parent effect — runs after
    // the incoming screen has already published and blanks it, and a
    // synchronous assertion cannot see that happen.
    await act(async () => {});
    expect(screen.getByText("bounded")).toBeDefined();
    expect(screen.queryByText("linearizable")).toBeNull();
    expect(screen.queryByText("This answer is incomplete")).toBeNull();
  });
});
