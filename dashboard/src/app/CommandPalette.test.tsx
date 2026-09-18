/**
 * What the palette PROMISES, held against what it does.
 *
 * Every case here is a claim the file makes in prose about itself: the footer
 * chip says the `@` scope lists "seats and units only", the "Open by id" hint
 * says a trace id opens "every event that carries it", and the comment over
 * the work scope says a search that answers nothing because the index is still
 * building is not a company with no such work. Each of those was false in a
 * different way, and none of them could fail a type check — a hit that
 * navigates to the wrong screen, a loop that skips every row, and a read that
 * failed rendered as an empty result are all perfectly well-typed.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { CommandPalette } from "./CommandPalette.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The palette, mounted over a store and a socket the case has arranged. */
function open(arrange?: (store: Store, socket: LiveSocket) => void) {
  const store = new Store();
  const socket = new LiveSocket(store);
  arrange?.(store, socket);
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <CommandPalette onClose={() => {}} />
      </Router>
    </ClientContext.Provider>,
  );
  // BY ROLE, not by its label: the modal shell carries `aria-label="Search"`
  // too, so a label query finds the dialog as well as the box inside it.
  const input = screen.getByRole("textbox");
  return {
    store,
    socket,
    view,
    type: (value: string) => fireEvent.change(input, { target: { value } }),
    /** The row carrying this hint — what a reader would click. */
    row: (hint: string) => screen.getByText(hint).closest("button"),
  };
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  // jsdom implements no scrolling at all, and the palette keeps the cursor row
  // in view — same stub as `frame/DataGrid.test.tsx`, for the same reason.
  Element.prototype.scrollIntoView = () => {};
  localStorage.clear();
  location.hash = "#/";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

describe("an id pasted out of a log", () => {
  // THE TRACE PAGE IS THE ONLY SCREEN THAT ASSEMBLES A TRACE. This hit pointed
  // at `#/activity/events?trace=<id>` instead, and the event log reads no
  // `trace` parameter — so the reader landed on the WHOLE log, unfiltered,
  // which reads as a trace that touched everything.
  test("the trace hit opens the trace, not the whole event log", () => {
    const id = "22222222-2222-4222-8222-222222222222";
    const { type, row } = open();
    type(id);
    fireEvent.click(row("as a trace — every event that carries it")!);
    expect(location.hash).toBe(`#/activity/traces/${id}`);
  });

  test("and the event and turn hits still open their own screens", () => {
    const id = "44444444-4444-4444-8444-444444444444";
    const { type, row } = open();
    type(id);
    fireEvent.click(row("as an event")!);
    expect(location.hash).toBe(`#/activity/events/${id}`);
  });
});

describe("the people scope", () => {
  const org = {
    name: "Acme",
    roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
    units: [
      {
        name: "Platform",
        type: "team",
        roles: [{ name: "SRE", handle: "sre", goal: "Keep it up" }],
      },
    ],
  };

  // THE FOOTER SAYS "seats and units only" — so both, and on an empty term.
  // The unit loop took `Infinity` as its empty-query score (the seat loop's
  // per-field no-match sentinel, which `Math.min` folds there and nothing
  // folds here), so the finiteness guard below it dropped every unit: typing
  // `@` listed the entire roster and not one unit.
  test("`@` lists units beside seats before anything is typed", () => {
    const { type } = open((store) => store.applyOrg(org));
    type("@");
    expect(screen.getByText("Units")).toBeDefined();
    expect(screen.getByText("Platform")).toBeDefined();
    expect(screen.getByText("Seats")).toBeDefined();
  });

  test("and a typed term still narrows them", () => {
    const { type } = open((store) => store.applyOrg(org));
    type("@plat");
    expect(screen.getByText("Platform")).toBeDefined();
    expect(screen.queryByText("CEO")).toBeNull();
  });
});

describe("the work scope is a server query, so it has four answers", () => {
  const hit = {
    key: "ENG-1",
    title: "Authentication rework",
    // A TYPE, because the palette drew a hardcoded tick for every hit — in a
    // list that also holds the DESTINATION rows from `nav.ts`, where a tick
    // legitimately names the Work workspace.
    type: "bug",
    status: "in_progress",
    assignee: "ceo",
  };

  /** A socket whose `work_search` answers per term. */
  function answering(reply: (q: string) => Promise<unknown>) {
    return (_store: Store, socket: LiveSocket) => {
      socket.query = ((_what: string, params?: Record<string, unknown>) =>
        reply(String(params?.q ?? ""))) as typeof socket.query;
    };
  }

  test("a refused read says so rather than claiming nothing matched", async () => {
    // `work_search` is UNREGISTERED on a node with no lexical index — the
    // engine answers `unknown_query`, which is a fact about the node and not
    // about the company's work. Rendered as "Nothing matches" it sends the
    // reader off to file a duplicate for work that already exists.
    const { type } = open(answering(() => Promise.reject(new Error("unknown_query"))));
    type("#auth");
    expect(await screen.findByText(/does not serve this answer/)).toBeDefined();
    expect(screen.queryByText(/Nothing matches/)).toBeNull();
  });

  test("a read the socket dropped is not a refusal either", async () => {
    const { type } = open(answering(() => Promise.reject(new Error("closed"))));
    type("#auth");
    expect(await screen.findByText(/connection went away/)).toBeDefined();
    expect(screen.queryByText(/Nothing matches/)).toBeNull();
  });

  // A HIT SAYS WHAT KIND OF THING IT IS, and says its status in the reader's own
  // vocabulary. The row's label is a key and a title, so the type appears
  // nowhere else on it; and this line printed `item.status` raw, so the palette
  // said `in_progress` where every other surface in the product says "In
  // progress".
  test("a work hit names its type, and its status in words", async () => {
    const { type } = open(answering(() => Promise.resolve({ hits: [hit], available: true })));
    type("#auth");
    expect(await screen.findByText(/Bug · In progress/)).toBeDefined();
    expect(screen.queryByText(/in_progress/)).toBeNull();
  });

  test("an answered term with no items does say nothing matched", async () => {
    const { type } = open(answering(() => Promise.resolve({ hits: [], available: true })));
    type("#zzz");
    expect(await screen.findByText(/Nothing matches “zzz”/)).toBeDefined();
  });

  test("the hits for one term are never shown under the next", async () => {
    // `useQuery` keeps the previous answer across a change of parameters, so
    // the palette listed `auth`'s items as hits for `authz` with nothing
    // saying a query was in flight. A term whose answer has not arrived has no
    // hits and says it is searching.
    const { type } = open(
      answering((q) =>
        q === "auth" ? Promise.resolve({ hits: [hit], available: true }) : new Promise(() => {}),
      ),
    );
    type("#auth");
    expect(await screen.findByText(/ENG-1/)).toBeDefined();

    type("#authz");
    expect(screen.queryByText(/ENG-1/)).toBeNull();
    expect(screen.getByText("Searching…")).toBeDefined();
    expect(screen.queryByText(/Nothing matches/)).toBeNull();
  });
});
