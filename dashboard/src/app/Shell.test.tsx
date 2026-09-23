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
  // jsdom implements no scrolling at all, and the palette keeps its cursor row
  // in view — same stub as `CommandPalette.test.tsx`, for the same reason.
  Element.prototype.scrollIntoView = () => {};
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
    expect(screen.getByText(/applied through 41 of 88/)).toBeDefined();
    expect(screen.getByText("This answer is incomplete")).toBeDefined();
    // AND THE LEVEL IS NOT A WORD ON THE SCREEN. `linearizable` is stronger
    // than this surface's own default; a chip naming it is a label nobody
    // reads, and the one level that matters then arrives as a changed word
    // inside it. See [CoverageTags] in components/work.tsx.
    expect(screen.queryByText("linearizable")).toBeNull();
  });

  // AND A LEVEL THIS SURFACE DID NOT EXPECT IS SAID IN WORDS.
  //
  // `stale`, `session` and `linearizable` are what a dashboard answer is
  // served at. Anything else means the node could not measure its own distance
  // from the log — including a level a later engine invents, which is why the
  // list is the ordinary ones rather than the odd ones: an unknown value is
  // SHOWN.
  test("a level this surface did not expect says what it means", () => {
    frame(
      <Covered
        coverage={{
          read_level: "consistent_prefix",
          complete: true,
          log_seq: 9,
          applied_through: 9,
        }}
      />,
    );
    expect(screen.getByText("age unknown")).toBeDefined();
    expect(screen.queryByText("consistent_prefix")).toBeNull();
  });

  // THE ONE THAT SHIPPED. Land on the Inbox, which publishes; click through to
  // a screen that does not — Admin > Credentials, say — and the inbox's
  // freshness badge and its "this answer is incomplete" banner stayed in the
  // bar as claims about data the new screen never read.
  test("it goes when the screen that published it does", () => {
    const { show } = frame(<Covered coverage={INCOMPLETE} />);
    show(<Bare />);
    expect(screen.getByText("the bare screen")).toBeDefined();
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
        coverage={{ read_level: "stale", complete: true, log_seq: 9, applied_through: 4 }}
      />,
    );
    // FLUSHED, because a reset that lands LATE is the failure being excluded:
    // a deferred one — a microtask, a timeout, a parent effect — runs after
    // the incoming screen has already published and blanks it, and a
    // synchronous assertion cannot see that happen.
    await act(async () => {});
    expect(screen.getByText(/applied through 4 of 9/)).toBeDefined();
    expect(screen.queryByText(/applied through 41 of 88/)).toBeNull();
    expect(screen.queryByText("This answer is incomplete")).toBeNull();
  });
});

describe("the Inbox rail badge", () => {
  /** A frame over a socket answering a bound viewer and one page of notices. */
  function railOver(notices: unknown[], primary_reasons: string[]) {
    const store = new Store();
    const socket = new LiveSocket(store);
    (
      socket as unknown as {
        query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
      }
    ).query = (what: string) => {
      if (what === "viewer") {
        return Promise.resolve({
          login: "U0FOUNDER",
          grants: ["state:read", "work:write", "knowledge:write", "people:manage"],
          handle: "ada",
          name: "Ada",
          kind: "human",
        });
      }
      if (what === "work_inbox") {
        return Promise.resolve({
          handle: "ada",
          notices,
          primary_reasons,
          unread: notices.filter((n) => !(n as { read: boolean }).read).length,
          primary: notices.filter((n) => primary_reasons.includes((n as { reason: string }).reason))
            .length,
        });
      }
      return Promise.resolve({});
    };
    render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <Shell>
            <Bare />
          </Shell>
        </Router>
      </ClientContext.Provider>,
    );
  }

  function notice(reason: string, read: boolean, n: number) {
    return {
      record_id: `r-${n}`,
      log_seq: n,
      log_stream: "CREWLET_WORK_LOG",
      log_generation: 1,
      at: new Date().toISOString(),
      reason,
      primary: false,
      addressed: false,
      kind: "task_updated",
      subject_id: `s-${n}`,
      read,
    };
  }

  // A BADGE NOBODY CAN DRIVE DOWN IS A BROKEN COUNTER.
  //
  // It counted `answer.unread`, which is every notice on the page — and most of
  // a busy company's notices are things it merely told you: a task you watch
  // moved, a comment you were cc'd on landed. Nobody answers those, so the number
  // never reached zero however diligent the reader was, and a count that only
  // ever grows is the first thing that makes a read-only inbox read as broken.
  // The primary half is small by construction and goes down by answering.
  test("counts only what the reader is on the hook for", async () => {
    railOver(
      [
        notice("assignee", false, 1), // unread AND primary — the one that counts
        notice("watcher", false, 2), // unread, not primary
        notice("collaborator", false, 3), // unread, not primary
        notice("assignee", true, 4), // primary, already read
      ],
      ["assignee", "mention"],
    );
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    const badge = document.querySelector(".rail-badge");
    expect(badge?.textContent).toBe("1");
    // And NOT the three unread, nor the two primary: each of those is one half
    // of the question and neither is it.
    expect(badge?.textContent).not.toBe("3");
    expect(badge?.textContent).not.toBe("2");
  });

  // NOTHING TO ANSWER IS NO BADGE AT ALL. A zero drawn in the caution hue is a
  // mark a reader checks, and it would be there permanently on a quiet company.
  test("a page with nothing primary and unread carries no badge", async () => {
    railOver([notice("watcher", false, 1), notice("assignee", true, 2)], ["assignee"]);
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(document.querySelector(".rail-badge")).toBeNull();
  });
});

// A MODAL DOES NOT OUTLIVE THE SCREEN IT WAS OPENED ON.
//
// `Shell` is mounted once for the life of the tab, so what it holds open stays
// open across every route change unless something takes it back. The drawer
// already did; the palette did not — press `Ctrl-K`, then Back, and it was
// still there over a different screen, still offering the objects it had
// ranked for the one the reader had just left.
//
// Picking a palette row closes it on the way out, so what this covers is every
// OTHER way the route moves while it is open: Back, Forward, a phone's back
// gesture, a restored history entry.
describe("what the frame holds open", () => {
  test("a route change closes the palette, the way it already closed the drawer", async () => {
    frame(<Bare />);
    await act(async () => {
      window.dispatchEvent(new KeyboardEvent("keydown", { key: "k", ctrlKey: true }));
    });
    expect(
      screen.queryByRole("dialog"),
      "ctrl-k did not open the palette, so this test proves nothing",
    ).not.toBeNull();

    await act(async () => {
      location.hash = "#/company";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    expect(
      screen.queryByRole("dialog"),
      "the palette survived a route change and is now over a screen it knows nothing about",
    ).toBeNull();
  });
});
