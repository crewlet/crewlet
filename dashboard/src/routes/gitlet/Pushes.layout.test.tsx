/** GITLET › PUSHES, LAID OUT: the feed and its rail at every width, nothing sideways. */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient, overflowing, sidewaysScrollers } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { Pushes } from "./Pushes.tsx";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const PAGE = { position: POSITION, level: "session" };
const A = "a".repeat(40);
const B = "b".repeat(40);
const LONG_BRANCH = "frontend/".padEnd(120, "stream-the-shell-");

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Frontend Engineer", handle: "frontend" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
  units: [],
};

const push = (id: string, ref: string, extra: Record<string, unknown> = {}) => ({
  push_id: id,
  repo_id: "id-web",
  position: POSITION,
  at: new Date(Date.now() - 25 * 60_000).toISOString(),
  actor: "frontend",
  actor_kind: "agent",
  actor_seat: "frontend",
  via: "engine",
  run: { turn_id: "turn-1", work_backend: "worklet", work_key: "ENG-415" },
  commands: [{ ref: `refs/heads/${ref}`, old: A, new: B, result: "ok" }],
  introduced_count: 1,
  pack_bytes: 8_400,
  received_at: new Date().toISOString(),
  ...extra,
});

const json = (payload: unknown) =>
  new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

/** The engine, for the history, the repositories and the repository's facts and rules. */
function answer(url: URL): Response {
  switch (url.pathname) {
    case "/query/code_pushes":
      return json({
        pushes: [
          push("p3", LONG_BRANCH),
          push("p2", "main", {
            actor: "pat:jane-laptop",
            actor_kind: "operator",
            actor_seat: "jane",
            via: "git",
            run: undefined,
            commands: [
              { ref: "refs/heads/main", old: A, new: B, result: "ng", reason: "protected_direct" },
            ],
            offered: 1,
            pack_bytes: undefined,
          }),
          push("p1", "sre/drain"),
        ],
        ...PAGE,
      });
    case "/query/code_repos":
      return json({ repos: [{ name: "web" }], ...PAGE });
  }
  return json(PAGE);
}

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => answer(new URL(String(input), location.origin))),
  );
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
  location.hash = "";
});

function mount(width: number) {
  location.hash = "#/gitlet/pushes?repo=web";
  host = document.createElement("div");
  host.className = "app";
  host.innerHTML = `<div class="crewlet-app-shell__content" style="display:flex;flex-direction:column;width:${width}px"></div>`;
  document.body.append(host);
  root = createRoot(host.firstElementChild!);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={ORG}>
        <Pushes />
      </LayoutClient>,
    ),
  );
}

/** The feed once its rows have drawn. */
const drawnFeed = () =>
  eventually(() => {
    const feed = document.querySelector<HTMLElement>(".code-pushes-feed");
    expect(feed?.querySelectorAll(".code-list__row")).toHaveLength(3);
    return feed!;
  });

// Mutation: drop `overflow-wrap: anywhere` from a ref's name, and the long branch runs out.
test("a push's row keeps its time and pack at its end at 1024 px with a long branch name", async () => {
  await page.viewport(1024, 900);
  mount(760);
  const feed = await drawnFeed();
  const row = feed.querySelector<HTMLElement>(".code-list__row")!;
  const card = row.getBoundingClientRect();
  const facts = row.querySelector<HTMLElement>(".code-push-facts")!.getBoundingClientRect();
  expect(facts.right).toBeLessThanOrEqual(card.right + 0.5);
  expect(overflowing(row)).toEqual([]);
});

// Mutation: give the facts `white-space: nowrap` under the fold, and the row scrolls sideways.
test("the pushes feed holds its width at 390 px", async () => {
  await page.viewport(390, 900);
  mount(358);
  const feed = await drawnFeed();
  expect(overflowing(feed)).toEqual([]);
  expect(sidewaysScrollers(feed)).toEqual([]);
});
