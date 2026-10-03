/**
 * The guarded half of a seat, read off the ORG CHART by the seat's handle —
 * never off `/config`, which holds no seats — and asked by what the reader
 * holds.
 *
 * Nobody is asked anything for a reader who is nobody: the answer is known
 * before the question, and asking only puts a refusal on the wire and a
 * banner over a page that never had a chance. And the runtime half is asked
 * for only by a reader holding the grant the chart serves it under; anybody
 * else is asked for the rows, which the chart answers without the half and
 * says so.
 */

import { act, cleanup, render } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { useSeatSetup } from "./seats.ts";
import { ClientContext } from "./store-hooks.ts";
import { ViewerProvider } from "./viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** Every request the page made, as `path?query`. */
let asked: string[] = [];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  asked = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      const at = new URL(url, "http://localhost");
      asked.push(`${at.pathname}${at.search}`);
      const runtime = at.searchParams.get("runtime") === "true";
      return new Response(
        JSON.stringify({
          seat: {
            handle: "pm",
            name: "PM",
            ...(runtime ? { runtime: { llm: "fast" } } : {}),
          },
          manages: null,
          answer: { level: "consistent_prefix", position: "CREWLET_CHART_LOG@1:10" },
          // THE CHART SAYS WHETHER IT SERVED THE HALF, as the engine does: a
          // reader without the grant is answered the rows and `false`.
          runtime,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/** Mounts a reader of the `pm` seat's setup for a viewer answering `viewer`. */
function probe(viewer: Record<string, unknown>) {
  const store = new Store();
  store.applyOrg({ name: "Acme", roles: [{ name: "PM", handle: "pm" }], units: [] });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : Promise.resolve({});
  let seen: ReturnType<typeof useSeatSetup> | undefined;
  function Reader() {
    seen = useSeatSetup("pm");
    return null;
  }
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Reader />
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  return { reading: () => seen!.reading, seat: () => seen!.seat };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

const person = (grants: string[]) => ({
  login: "ana.diaz",
  grants,
  handle: "",
  name: "",
  kind: "",
  owner: "ana.diaz",
  acts: [],
  project: "",
});

test("an anonymous reader stays unread, and nothing is asked", async () => {
  const { reading, seat } = probe({ ...person([]), login: "", owner: "" });
  await settle();
  expect(seat()?.handle).toBe("pm");
  expect(asked).toEqual([]);
  expect(reading()).toEqual({ state: "unread" });
});

test("a reader holding config:read asks for the runtime half, and is read", async () => {
  const { reading } = probe(person(["state:read", "config:read"]));
  await settle();
  expect(asked).toEqual(["/chart/seats/pm?runtime=true"]);
  expect(reading()).toMatchObject({
    state: "read",
    seat: { handle: "pm", runtime: { llm: "fast" } },
  });
});

// THE HALF WITHHELD IS AN ANSWER, not a refusal: the rows are read and the
// reading says the runtime was not this reader's to see.
test("a reader without config:read asks for the rows, and reads them stripped", async () => {
  const { reading } = probe(person(["state:read"]));
  await settle();
  expect(asked).toEqual(["/chart/seats/pm"]);
  expect(reading()).toEqual({ state: "stripped", seat: { handle: "pm", name: "PM" } });
});
