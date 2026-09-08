/**
 * The Config screen renders what the ENGINE sends, not what a type says.
 *
 * Both fixtures below are copied from the emitters: `configapi.meta` for a
 * revision (`revision_id`, `created_by`, `is_active`) and `configapi.Change`
 * for a diff line (`kind`, one of added / removed / changed). The screen read
 * `id`, `author`, `active` and `op`, none of which any answer has ever
 * carried, so every revision row rendered `undefined` and threw on the first
 * `.slice(10)`, and every diff line rendered unstyled.
 *
 * The absence of this file is why that shipped. A shape declared from memory
 * and never rendered against a real answer is a screen nobody has seen.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { ConfigScreen } from "./Config.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

// EXACTLY what internal/api/configapi/answers.go returns for these two
// questions. Change the server and this fixture has to change with it, which
// is the whole point of writing it out rather than building it from the type.
const revisions = [
  {
    revision_id: "01JCFGAAAA0000000000000001",
    created_at: "2026-08-23T15:00:00Z",
    created_by: "founder",
    source: "api",
    summary: "connect datadog",
    is_active: true,
  },
  {
    revision_id: "01JCFGBBBB0000000000000002",
    created_at: "2026-08-22T15:00:00Z",
    created_by: "ops",
    source: "cli",
    summary: "first import",
    is_active: false,
  },
];

const diff = {
  from: "01JCFGBBBB0000000000000002",
  to: "01JCFGAAAA0000000000000001",
  changes: [
    { path: "integrations.datadog.route_to", kind: "added", to: "sre-lead" },
    { path: "integrations.gitlab.url", kind: "changed", from: "a", to: "b" },
    { path: "integrations.slack", kind: "removed", from: {} },
  ],
};

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function mount(hash: string) {
  location.hash = hash;
  const store = new Store();
  const socket = new LiveSocket(store);
  // The screen's only data path. Stubbed rather than driven through a fake
  // server, because what is under test is the rendering of an answer whose
  // shape is already pinned by the Go side.
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    Promise.resolve(what === "config_audit" ? revisions : what === "config_diff" ? diff : {});
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ConfigScreen />
      </Router>
    </ClientContext.Provider>,
  );
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

test("a revision row renders the server's own field names", async () => {
  mount("#/config?lens=audit");

  // The id: truncated to ten characters, which is what threw when the field
  // was read under a name the server does not send.
  expect(await screen.findByText("01JCFGAAAA")).toBeDefined();
  expect(screen.getByText("01JCFGBBBB")).toBeDefined();
  expect(screen.getByText("connect datadog")).toBeDefined();
  // The author, and the active marker.
  expect(screen.getByText("founder")).toBeDefined();
  expect(screen.getByText("ops")).toBeDefined();
  expect(screen.getAllByText("active").length).toBe(1);
});

test("a diff line carries the kind the server sends", async () => {
  const { container } = mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001");

  expect(await screen.findByText("integrations.datadog.route_to")).toBeDefined();
  // data-kind is what the stylesheet selects on. Under the old `data-op` with
  // add / remove / replace, every line rendered with no tone at all.
  const kinds = [...container.querySelectorAll(".diff-line")].map((el) =>
    el.getAttribute("data-kind"),
  );
  expect(kinds).toEqual(["added", "changed", "removed"]);
  // And the value column reads the side that exists for that kind.
  expect(screen.getByText('"sre-lead"')).toBeDefined();
  expect(screen.getByText('"a" → "b"')).toBeDefined();
});
