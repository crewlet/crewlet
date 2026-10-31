/**
 * Every widget a section offers draws on Home's board, whatever the engine
 * answers.
 *
 * A widget is a section's component on a page that section does not own, so
 * its failure mode is a crash nobody opening the section would see: a query
 * that answered nothing, a choice that was never made, a reader whose access
 * no longer opens the page. Each one draws inside the board's own frame
 * (`LeafView`), and that frame's error boundary must never be what the reader
 * sees.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { useEffect, useState, type ReactNode } from "react";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, QueryError, Store } from "~/protocol/index.ts";
import { FrameReadings } from "~/app/Shell.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { healthFrame } from "~/test/health.ts";
import { HomeDataProvider, useHomeData } from "../useHomeData.ts";
import { WIDGETS, canUse } from "./registry.tsx";
import { destinationOf, type WidgetDef } from "./kit.tsx";
import { LeafView } from "./widgets.tsx";
import type { ContribWidget } from "./model.ts";
import { afterMicrotasks } from "~/test/settle.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const ADMIN = {
  login: "U0FOUNDER",
  grants: ["admin", "tracker", "knowledge"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["comment_on_work_item", "answer_run", "update_work_item", "create_work_item"],
};

/** A reader holding the tracker and the knowledge base, and not Admin. */
const MEMBER = { ...ADMIN, grants: ["tracker", "knowledge"] };

const seat = (name: string, handle: string, kind: string, managers: string[] = []) => ({
  handle,
  name,
  kind,
  placed_by_ref: false,
  manager: managers[0] ?? "",
  managers,
  reports: [],
  auto_reports: [],
  onboarding_chain: [],
});

const ORG = {
  name: "Nimbus",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "CTO", handle: "cto", kind: "agent" },
  ],
  units: [],
  derived: {
    seats: [seat("Jane Founder", "jane", "human"), seat("CTO", "cto", "agent", ["jane"])],
    units: [],
  },
};

/**
 * The questions Home's own cards ask, answered quietly. Every other question
 * a widget asks is REFUSED as one this engine does not know, the answer an
 * older node gives a newer dashboard: each widget draws the refusal its
 * section's screen draws, never a crash.
 */
const QUIET: Record<string, unknown> = {
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  work_flow: {
    bucket: "day",
    points: [],
    now: { not_started: 0, active: 0, blocked: 0, overdue: 0 },
    blocked_history: false,
    complete: true,
  },
  token_series: {
    group: "unit",
    bucket: "day",
    series: [],
    by_group: [],
    totals: { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 },
    grouped: { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 },
  },
  company_feed: { rows: [], complete: true },
  work_projects: {
    projects: [
      {
        key: "LEAD",
        name: "Launch",
        archived: false,
        lead: { handle: "cto" },
        task_counts: { todo: 2, active: 1, done: 1 },
      },
    ],
    total: 1,
    census: {},
    complete: true,
  },
  sandbox_runs: { runs: [] },
  inbox: { handle: "jane", notices: [], primary_reasons: [] },
};

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/home";
  // EVERY REST READ IS REFUSED: a widget that reads over REST draws the
  // refusal, never a crash.
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(JSON.stringify({ code: "not_found", message: "Not here." }), {
          status: 404,
          headers: { "Content-Type": "application/json" },
        }),
    ),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  reloadForTest();
  location.hash = "";
});

/** Home's providers, as `Home` mounts them, around `children`. */
function Page({ children }: { children: ReactNode }) {
  return <HomeDataProvider value={useHomeData()}>{children}</HomeDataProvider>;
}

function mount(
  children: ReactNode,
  viewer: Record<string, unknown> = ADMIN,
  answers: Record<string, unknown> = {},
) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(ORG as never);
  store.applySeats([] as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string) => {
    const all: Record<string, unknown> = { ...QUIET, viewer, ...answers };
    return what in all
      ? Promise.resolve(all[what])
      : Promise.reject(new QueryError("unknown_query"));
  }) as typeof socket.query;
  return render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <SessionReading>
            <FrameReadings>
              <Router>
                <Page>{children}</Page>
              </Router>
            </FrameReadings>
          </SessionReading>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
}

async function settle() {
  await afterMicrotasks();
}

const leaf = (def: WidgetDef, params: Record<string, string>): ContribWidget => ({
  id: "w-under-test",
  kind: "widget",
  widget: def.key,
  params,
  w: def.size.w,
  h: null,
});

/**
 * A widget with its choices made from its own offer: each param takes the
 * first option its `useChoices` gives, as a reader picking the first entry of
 * the settings sheet would. A param with nothing to offer stays unchosen.
 */
function Chosen({ def }: { def: WidgetDef }) {
  const params: Record<string, string> = {};
  for (const p of def.params ?? []) {
    // One hook per param, in the def's fixed order.
    // eslint-disable-next-line react-hooks/rules-of-hooks
    const first = p.useChoices().options[0];
    if (first) params[p.key] = first.value;
  }
  // The choice settles before the widget is drawn, as Add to Home does.
  const [shown, setShown] = useState<Record<string, string> | null>(null);
  const wanted = JSON.stringify(params);
  useEffect(() => setShown(JSON.parse(wanted) as Record<string, string>), [wanted]);
  return shown ? <LeafView widget={leaf(def, shown)} /> : null;
}

/** The sentence the board's frame draws in place of a widget that could not draw. */
const BOUNDARY = /could not be drawn/;

const defs = [...WIDGETS.values()];

describe("every widget a section offers", () => {
  test("is offered at all: the registry found Home's and every section's", () => {
    const workspaces = new Set(defs.map((d) => destinationOf(d.from)?.workspace));
    for (const w of [
      "home",
      "inbox",
      "me",
      "org",
      "schedules",
      "integrations",
      "budgets",
      "work",
      "live",
      "knowledge",
      "settings",
    ])
      expect(workspaces.has(w as never), w).toBe(true);
  });

  test.each(defs.map((d) => [d.key, d] as const))(
    "%s draws for an admin with its first choices, whatever the engine answers",
    async (_key, def) => {
      const { container } = mount(<Chosen def={def} />);
      await settle();
      expect(container.textContent ?? "").not.toMatch(BOUNDARY);
      expect(container.textContent ?? "").not.toMatch(/no longer/);
      expect((container.textContent ?? "").trim().length).toBeGreaterThan(0);
    },
  );
});

describe("a widget whose choice is not made", () => {
  // `live.events` has a choice with a meaning when unmade: every event.
  const choosers = defs.filter((d) => (d.params?.length ?? 0) > 0 && d.key !== "live.events");

  test("there are some to hold to it", () => {
    expect(choosers.length).toBeGreaterThan(0);
  });

  // Nothing saved yet to choose from: the lists a choice is made from answer empty.
  const NOTHING_SAVED = { work_saved_views: { views: [] }, containers: { containers: [] } };

  test.each(choosers.map((d) => [d.key, d] as const))(
    "%s says where the choice is made",
    async (_key, def) => {
      const { container } = mount(<LeafView widget={leaf(def, {})} />, ADMIN, NOTHING_SAVED);
      await settle();
      expect(container.textContent ?? "").not.toMatch(BOUNDARY);
      expect(screen.getByText(/^Choose an? .+ in Edit Home\.$/)).toBeTruthy();
    },
  );

  test.each(["work.view", "knowledge.space"])(
    "%s says its list of choices was refused rather than waiting on it",
    async (key) => {
      const { container } = mount(<LeafView widget={leaf(WIDGETS.get(key)!, {})} />);
      await settle();
      expect(container.textContent ?? "").not.toMatch(BOUNDARY);
      expect(screen.queryByText(/in Edit Home\.$/)).toBeNull();
      expect(container.querySelector(".crewlet-skeleton")).toBeNull();
    },
  );

  test("a choice that names nothing the widget offers is treated as unmade", async () => {
    const shortcut = WIDGETS.get("home.shortcut")!;
    mount(<LeafView widget={leaf(shortcut, { page: "no-such-page" })} />);
    await settle();
    expect(screen.getByText("Choose a page in Edit Home.")).toBeTruthy();
  });
});

describe("a reader whose access no longer opens a widget's page", () => {
  // WHAT A MEMBER (Tracker and Knowledge, no Admin) CANNOT OPEN, read off the
  // registry's own rule: Spend's readings are every reader's, so a Settings
  // widget is not by its key alone a closed one.
  const closed = defs.filter((d) => !canUse(d, MEMBER.grants));

  test("there are some to hold to it", () => {
    expect(closed.map((d) => d.key)).toContain("settings.models");
  });

  test.each(closed.map((d) => [d.key, d] as const))(
    "%s says so, and asks nothing of its section",
    async (_key, def) => {
      const { container } = mount(<LeafView widget={leaf(def, {})} />, MEMBER);
      await settle();
      const page = destinationOf(def.from)!.label;
      expect(screen.getByText(`You no longer have access to ${page}.`)).toBeTruthy();
      expect(container.textContent ?? "").not.toMatch(BOUNDARY);
    },
  );

  test("a widget its section no longer offers says so rather than vanishing", async () => {
    mount(
      <LeafView
        widget={{ id: "w-gone", kind: "widget", widget: "gone.away", params: {}, w: 4, h: null }}
      />,
    );
    await settle();
    expect(
      screen.getByText("This widget is no longer available. Remove it in Edit Home."),
    ).toBeTruthy();
  });
});
