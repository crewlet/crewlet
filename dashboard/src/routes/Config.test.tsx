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

/** Every question the screen asked, with its parameters. */
let asked: { what: string; params: Record<string, unknown> | undefined }[] = [];

/**
 * What the socket answers, where a case needs something the fixtures are not.
 *
 * A PROPERTY THAT IS PRESENT AND NULL IS AN ANSWER, which is why the defaults
 * turn on absence rather than on nullishness: `config` answers null before a
 * company's first revision, and that is the case the empty state is for.
 */
type Answers = { active?: unknown; diff?: unknown };

function mount(hash: string, answers: Answers = {}) {
  location.hash = hash;
  const active = "active" in answers ? answers.active : {};
  const comparison = "diff" in answers ? answers.diff : diff;
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
          ? comparison
          : what === "config"
            ? active
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
  mount("#/config?lens=diff&revision=01JCFGAAAA0000000000000001", { diff: cutDiff });

  // The count is the COMPARISON's, not the listing's: a screen that showed
  // three would be reporting the response budget as the answer.
  expect(await screen.findByText(/3 of 512 shown/)).toBeDefined();
  // And where the whole thing is: the CLI writes to a terminal, which has no
  // response budget, so it prints every change.
  expect(screen.getByText("crewlet config diff")).toBeDefined();
});

// NULL IS "NOTHING IS ACTIVE", which is what `queries.configDocument` answers
// before a company's first revision. The empty state used to name only the
// command line; the one screen that can create a company is the org chart's
// builder, and this is the first place an operator looks for one.
test("with nothing active, the empty state leads to creating the company", async () => {
  mount("#/config", { active: null });
  expect(await screen.findByText("No company configuration is active")).toBeDefined();
  const links = screen.getAllByRole("link", { name: "Create the company" });
  expect(links[0]?.getAttribute("href")).toBe("#/org?lens=builder");
  expect(screen.getByText(/crewlet config import or PUT \/config/)).toBeDefined();
});

/** The side each diff the screen asked for was compared against. */
const againstAsked = () =>
  asked.filter((q) => q.what === "config_diff").map((q) => q.params?.against);

// WHAT ONE SAVE CHANGED is its revision against its parent. Against the
// active revision, a save that is active now is byte-identical to itself, and
// the builder's "View changes" opened an empty diff.
test("a link naming the side to compare against reads the diff against it", async () => {
  mount(
    "#/config?lens=diff&revision=01JCFGAAAA0000000000000001&against=01JCFGBBBB0000000000000002",
  );
  expect(await screen.findByText("integrations.datadog.route_to")).toBeDefined();
  expect(againstAsked()).toEqual(["01JCFGBBBB0000000000000002"]);
  expect(screen.getByText("against revision 01JCFGBBBB")).toBeDefined();
});

test("a revision picked from the history is compared with the active one again", async () => {
  mount(
    "#/config?lens=diff&revision=01JCFGAAAA0000000000000001&against=01JCFGBBBB0000000000000002",
  );
  fireEvent.click(await screen.findByText("first import"));
  await waitFor(() => expect(location.hash).not.toContain("against="));
  expect(location.hash).toContain("revision=01JCFGBBBB0000000000000002");
  await waitFor(() => expect(againstAsked().at(-1)).toBe("active"));
  expect(screen.getByText("against the active revision")).toBeDefined();
});
