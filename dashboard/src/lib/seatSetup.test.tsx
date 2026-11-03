/**
 * The guarded half of a seat, read once and never asked for without the
 * grant it is read under.
 *
 * The company document takes Admin, so a reader without it is refused
 * on every ask: the answer is known before the question. Asking anyway put a
 * refusal on the wire on every seat page such a reader opened, and a
 * `refused` banner over a page that never had a chance. And not asking is
 * said as what it is — `withheld` — never as a document that came back empty.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { useSeatSetup } from "./seats.ts";
import { ClientContext } from "./store-hooks.ts";
import { useViewer, type ViewerState } from "./viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks, eventually } from "~/test/settle.ts";

vi.mock("./viewer.ts", () => ({ useViewer: vi.fn() }));

const READER: ViewerState = {
  login: "jane.doe",
  grants: [],
  admin: false,
  worklet: false,
  doclet: false,
  chatlet: false,
  handle: "",
  owner: "jane.doe",
  name: "",
  acts: [],
  project: "",
  kind: "",
  unbound: true,
  anonymous: false,
  loading: false,
  asking: false,
  configWriter: false,
  configManagedBy: [],
  assistant: "disabled",
  assistantModel: null,
  chatVendors: [],
};

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  localStorage.clear();
  vi.mocked(useViewer).mockReturnValue(READER);
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
});

function probe() {
  const store = new Store();
  store.applyOrg(withDerived({ name: "Acme", roles: [{ name: "PM", handle: "pm" }], units: [] }));
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    return what === "config"
      ? Promise.resolve({ name: "Acme", roles: [{ name: "PM", handle: "pm" }] })
      : Promise.resolve({});
  };
  let seen: ReturnType<typeof useSeatSetup> | undefined;
  function Reader() {
    seen = useSeatSetup("pm");
    return null;
  }
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Reader />
    </ClientContext.Provider>,
  );
  return { asked, reading: () => seen!.reading, setup: () => seen! };
}

/** Lets the stubbed reads answer, and what they cause land. */
async function settle() {
  await afterMicrotasks();
}

// NOT ASKED IS ITS OWN ANSWER, and never a company with no configuration: the
// Settings tab read a read nobody sent — no document, not loading — as one
// that came back empty, and told a person opening their own seat "No company
// configuration is active" beside a company running. Mutation: leave such a
// reader `unread` and the panel has nothing to tell the two apart by.
test("a reader without Admin is withheld, and nothing is asked", async () => {
  const { asked, reading, setup } = probe();
  await settle();
  expect(asked).not.toContain("config");
  expect(reading()).toEqual({ state: "withheld" });
  expect(setup().config).toMatchObject({ doc: null, loading: false, error: null });
});

// A VIEWER NOBODY HAS ANSWERED FOR YET IS NOT WITHHELD — it may hold the grant
// — and its read is WAITING rather than answered: until the viewer says who
// this is nothing can be asked, so the panel draws a read in flight rather
// than a document that came back with nothing.
test("before the viewer answers the read claims nothing, and is waiting", async () => {
  vi.mocked(useViewer).mockReturnValue({ ...READER, grants: [], loading: true, asking: true });
  const { asked, reading, setup } = probe();
  await settle();
  expect(asked).not.toContain("config");
  expect(reading()).toEqual({ state: "unread" });
  expect(setup().config.loading).toBe(true);
});

test("a reader holding Admin is read", async () => {
  vi.mocked(useViewer).mockReturnValue({ ...READER, grants: ["admin"] });
  const { asked, reading } = probe();
  await settle();
  expect(asked).toContain("config");
  expect(reading().state).toBe("read");
});

// A LEAD READS WHAT THEY LEAD through the per-seat and per-unit reads the
// engine admits them, never the whole document they may not read: the seat,
// and its unit's own fields, whose credentials the seat inherits.
test("a lead of the seat's unit reads the seat and its unit, and never the document", async () => {
  vi.mocked(useViewer).mockReturnValue({ ...READER, handle: "boss", unbound: false });
  const paths: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input)).pathname;
      paths.push(path);
      const body =
        path === "/config/roles/pm"
          ? { name: "PM", handle: "pm", goal: "Plan" }
          : {
              id: "product",
              name: "Product",
              mcp_env: { tracker: { TOKEN: "${TRACKER}" } },
              roles: [{ name: "PM", handle: "pm" }],
            };
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { "Content-Type": "application/json", ETag: '"r1"' },
      });
    }),
  );
  const store = new Store();
  store.applyOrg(
    withDerived({
      name: "Acme",
      roles: [{ name: "Boss", handle: "boss", kind: "human" }],
      units: [
        { id: "product", name: "Product", lead: "boss", roles: [{ name: "PM", handle: "pm" }] },
      ],
    }),
  );
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    return Promise.resolve({});
  };
  let seen: ReturnType<typeof useSeatSetup> | undefined;
  function Reader() {
    seen = useSeatSetup("pm");
    return null;
  }
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Reader />
    </ClientContext.Provider>,
  );
  await eventually(() => expect(seen!.reading.state).toBe("read"));
  expect(asked).not.toContain("config");
  expect([...paths].sort()).toEqual(["/config/roles/pm", "/config/units/product"]);
  const reading = seen!.reading;
  if (reading.state !== "read") throw new Error(reading.state);
  expect(reading.role.goal).toBe("Plan");
  expect(reading.unit?.mcp_env).toEqual({ tracker: { TOKEN: "${TRACKER}" } });
});
