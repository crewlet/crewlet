/**
 * What the Nodes screen says when the lease table was refused to it.
 *
 * Its own file because it mocks the screen's query hook, which the other
 * renderings of this screen must not see. The client is real and idle: the
 * screen reads the org (for the seat badges) and the engine's health out of
 * the store, and neither asks the socket anything.
 */

import { cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { Fleet } from "./Fleet.tsx";

const fleet = vi.hoisted(() => ({
  data: null as unknown,
  error: null as string | null,
  refusal: null as unknown,
}));

vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: (what: string) =>
    what === "fleet"
      ? {
          data: fleet.data,
          loading: false,
          error: fleet.error,
          refusal: fleet.refusal,
          refetch: () => {},
        }
      : // Nobody here holds `fleet:operate`, so nothing beside the table asks
        // anything further or draws anything.
        {
          data: {
            login: "ana",
            grants: ["state:read"],
            handle: "",
            owner: "ana",
            name: "",
            kind: "",
          },
          loading: false,
          error: null,
          refusal: null,
          refetch: () => {},
        },
}));

function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Fleet />
      </Router>
    </ClientContext.Provider>,
  );
}

afterEach(() => {
  cleanup();
  fleet.data = null;
  fleet.error = null;
  fleet.refusal = null;
});

// A REFUSAL WITH NOTHING KEPT IS THE ANSWER, and it says what would change it.
// The screen drew the refusal's code in a "this poll failed, what is below is
// the last reading" banner over an empty table titled "No nodes are reporting"
// — a reading nobody took and a fleet that does not exist — and dropped the
// grants the engine named.
test("a refused first read names the grant rather than an empty fleet", () => {
  fleet.error = "unauthorized";
  fleet.refusal = { reason: "operator", grants: ["fleet:operate"] };
  mount();
  expect(screen.getByText("fleet:operate")).toBeDefined();
  expect(screen.queryByText("No nodes are reporting")).toBeNull();
  expect(screen.queryByText(/This poll failed/)).toBeNull();
});

// AND A FAILED POLL OVER A READING IT KEPT still says the reading is not now.
test("a failed poll over a kept reading says the reading is old", () => {
  fleet.data = {
    nodes: [],
    seats: [],
    duties: [],
    unplaceable: [],
    unmanned_roles: [],
    target_epoch: 3,
    this_node: "n1",
  };
  fleet.error = "unavailable";
  mount();
  expect(screen.getByText(/This poll failed \(unavailable\)/)).toBeDefined();
});
