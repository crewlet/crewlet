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

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
  changes_total: 3,
};

// The same answer for a comparison too long to send whole: the listing is a
// page of it and `changes_total` is how many there are. The server used to
// report the cut as a pathless CHANGE inside `changes`, which this screen
// drew as a blank path turning undefined into a sentence.
const cutDiff = {
  ...diff,
  changes_total: 512,
};

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

// The engine's answer for a collection when nothing is active: nothing at all.
// `queries/config.go` returns nil for ErrNoActiveRevision and the envelope
// omits an absent `data`, so the client settles on `undefined` — which is why
// it is the default here rather than a case's own argument: a default of
// `undefined` cannot be passed back in, and a helper that quietly substituted
// the empty collection for it would test the opposite of what it said.
/** Every question the screen asked, with its parameters. */
let asked: { what: string; params: Record<string, unknown> | undefined }[] = [];

function mount(hash: string, answer: unknown = diff, entities?: unknown) {
  location.hash = hash;
  const store = new Store();
  const socket = new LiveSocket(store);
  // The screen's only data path. Stubbed rather than driven through a fake
  // server, because what is under test is the rendering of an answer whose
  // shape is already pinned by the Go side.
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params) => {
    asked.push({ what, params });
    return Promise.resolve(
      what === "config_audit"
        ? revisions
        : what === "config_diff"
          ? answer
          : what === "config_entities"
            ? entities
            : {},
    );
  };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ConfigScreen />
      </Router>
    </ClientContext.Provider>,
  );
}

beforeEach(() => {
  asked = [];
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
  // NOTHING SAYS IT WAS CUT, because it was not: changes_total equals the
  // number of lines, and a "3 of 3 shown" note would be noise on every diff.
  expect(screen.queryByText(/shown/)).toBeNull();
});

test("a cut diff says how many changes there are", async () => {
  mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001", cutDiff);

  // The count is the COMPARISON's, not the listing's — a screen that showed
  // three would be reporting the response budget as the answer.
  expect(await screen.findByText(/3 of 512 shown/)).toBeDefined();
  // And where the whole thing is: the CLI writes to a terminal, which has no
  // response budget, so it prints every change.
  expect(screen.getByText("crewlet config diff")).toBeDefined();
});

// NO ACTIVE REVISION IS NOT AN EMPTY COLLECTION, and the engine says so by
// answering nothing at all: `queries/config.go` returns nil for
// ErrNoActiveRevision and the envelope omits the field, so this arrives as
// `undefined` with no error. Read as an empty list it produced the one
// sentence that cannot be true on a deployment before its first import —
// "the active revision declares none of these" — while the Active lens on the
// same screen answered the same state correctly.
test("a collection with no active revision says so, not that the revision declares none", async () => {
  mount("#/config?lens=entities", diff, undefined);

  expect(await screen.findByText("No company configuration is active")).toBeDefined();
  expect(screen.queryByText(/declares none of these/)).toBeNull();
});

// THE CONTROL. An active revision that genuinely declares no seats is a
// different fact and keeps its own wording — without this, collapsing both
// into the no-revision sentence would pass the case above.
test("an empty collection under an active revision still reads as empty", async () => {
  mount("#/config?lens=entities", diff, { kind: "roles", ids: [] });

  expect(await screen.findByText("Nothing in this collection")).toBeDefined();
  expect(screen.queryByText("No company configuration is active")).toBeNull();
});

// `entity` is a URL key, so a shared or bookmarked link lands on the panel in
// that same state — where `JSON.stringify(undefined ?? null)` printed the
// literal word `null` as though it were the entity's own slice of the
// document.
test("a deep link to an entity with no active revision renders no literal null", async () => {
  mount("#/config?lens=entities&entity=founder", diff, undefined);

  expect(await screen.findAllByText("No company configuration is active")).toBeDefined();
  expect(screen.queryByText("null")).toBeNull();
});

// ---------------------------------------------------------------------------
// Which side a diff is read against
// ---------------------------------------------------------------------------

/** The side each diff the screen asked for was compared against. */
const againstAsked = () =>
  asked.filter((q) => q.what === "config_diff").map((q) => q.params?.against);

// WHAT ONE SAVE CHANGED is its revision against its PARENT. Against the active
// revision, a save that is active now is byte-identical to itself, so a "View
// changes" link that named only the revision opened an empty diff under a note
// about credential rotation.
test("a link naming the side to compare against reads the diff against it", async () => {
  mount(
    "#/admin/config?lens=diff&revision=01JCFGAAAA0000000000000001&against=01JCFGBBBB0000000000000002",
  );

  expect(await screen.findByText("integrations.datadog.route_to")).toBeDefined();
  expect(againstAsked()).toEqual(["01JCFGBBBB0000000000000002"]);
  expect(screen.getByText("against revision 01JCFGBBBB")).toBeDefined();
});

// THE CONTROL, and the half that keeps the case above from passing on a screen
// that simply forwards whatever the URL holds: a row picked from the history
// is a question about the ACTIVE document again, so it drops the side a link
// named rather than carrying somebody else's comparison into it.
test("a revision picked from the history is compared with the active one again", async () => {
  mount(
    "#/admin/config?lens=diff&revision=01JCFGAAAA0000000000000001&against=01JCFGBBBB0000000000000002",
  );

  // THE ROW'S OWN LINK, which is what a reader presses: the grid draws a real
  // anchor over each row so a revision can be opened in a tab, and a plain
  // left click on it peeks and points the diff.
  await screen.findByText("first import");
  fireEvent.click(screen.getByRole("link", { name: /first import/ }));
  await waitFor(() => expect(location.hash).not.toContain("against="));
  expect(location.hash).toContain("revision=01JCFGBBBB0000000000000002");
  await waitFor(() => expect(againstAsked().at(-1)).toBe("active"));
  expect(screen.getByText("against the active revision")).toBeDefined();
});

// AND THE ORDINARY CASE NAMES NO SIDE, so every link that wants the plain
// comparison carries no parameter at all.
test("with no side named the diff is read against the active revision", async () => {
  mount("#/admin/config?lens=diff&revision=01JCFGAAAA0000000000000001");

  expect(await screen.findByText("integrations.datadog.route_to")).toBeDefined();
  expect(againstAsked()).toEqual(["active"]);
  expect(screen.getByText("against the active revision")).toBeDefined();
});
