/**
 * The guarded half of a seat, read once and never asked for without the
 * grant it is read under.
 *
 * The company document takes `config:read`, so a reader without it is refused
 * on every ask: the answer is known before the question. Asking anyway put a
 * refusal on the wire on every seat page such a reader opened, and a
 * `refused` banner over a page that never had a chance.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { useSeatSetup } from "./seats.ts";
import { ClientContext } from "./store-hooks.ts";
import { useViewer, type ViewerState } from "./viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

vi.mock("./viewer.ts", () => ({ useViewer: vi.fn() }));

const READER: ViewerState = {
  login: "jane.doe",
  grants: ["state:read"],
  operatesFleet: false,
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
  store.applyOrg({ name: "Acme", roles: [{ name: "PM", handle: "pm" }], units: [] });
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
  return { asked, reading: () => seen!.reading };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 4; i++) await Promise.resolve();
  });
}

test("a reader without config:read stays unread, and nothing is asked", async () => {
  const { asked, reading } = probe();
  await settle();
  expect(asked).not.toContain("config");
  expect(reading()).toEqual({ state: "unread" });
});

test("a reader holding config:read is read", async () => {
  vi.mocked(useViewer).mockReturnValue({ ...READER, grants: ["state:read", "config:read"] });
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
  store.applyOrg({
    name: "Acme",
    units: [
      { id: "product", name: "Product", lead: "boss", roles: [{ name: "PM", handle: "pm" }] },
    ],
    derived: {
      seats: [],
      units: [
        {
          id: "product",
          name: "Product",
          type: "unit",
          lead: "boss",
          lead_inherited: false,
          channel: "",
          channel_inherited: false,
          seats: ["pm"],
        },
      ],
    },
  });
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
  await vi.waitFor(() => expect(seen!.reading.state).toBe("read"));
  expect(asked).not.toContain("config");
  expect([...paths].sort()).toEqual(["/config/roles/pm", "/config/units/product"]);
  const reading = seen!.reading;
  if (reading.state !== "read") throw new Error(reading.state);
  expect(reading.role.goal).toBe("Plan");
  expect(reading.unit?.mcp_env).toEqual({ tracker: { TOKEN: "${TRACKER}" } });
});
