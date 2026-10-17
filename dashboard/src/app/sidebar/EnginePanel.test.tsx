/**
 * The engine's state at the foot of the sidebar: ONE state, chosen by
 * precedence and never "healthy" on the strength of nothing, on one row that
 * keeps its height, and the panel that row opens.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import {
  EnginePanel,
  engineReading,
  type EngineReading,
  type EngineState,
} from "./EnginePanel.tsx";
import type { EngineHealth } from "~/contract/health.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { RetentionReport } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { engineAlarms } from "~/test/retentionAlarms.ts";

afterEach(cleanup);

const healthy: EngineHealth = healthFrame({ nodes: 3, applied_epoch: 42, alarms: { count: 0 } });

const connected = { connected: true, authRejected: false, lostAt: null, everConnected: true };

const read = (over: Partial<Parameters<typeof engineReading>[0]> = {}) =>
  engineReading({ ...connected, health: healthy, ...over });

/** One reading of every state the row can say. */
const EVERY: Record<EngineState, EngineReading> = {
  "signed-out": read({ authRejected: true }),
  refused: read({ accessRefused: "seat_unavailable" }),
  unreachable: read({ connected: false, lostAt: 1, health: null }),
  connecting: read({ connected: false, lostAt: null, everConnected: false, health: null }),
  draining: read({ health: { ...healthy, shutting_down: true } }),
  behind: read({ health: { ...healthy, stall_lag_seconds: 31 } }),
  starting: read({ health: { ...healthy, configured: false, posture: "wait" } }),
  unconfigured: read({ health: { ...healthy, configured: false, posture: "serve" } }),
  diverged: read({ health: { ...healthy, posture: "isolated" } }),
  serving: read({ health: { ...healthy, alarms: { count: 12, worst: "backup_age" } } }),
  healthy: read(),
};

describe("which state the row says", () => {
  test("healthy is a serving node whose alarm table was evaluated and holds nothing", () => {
    expect(read()).toMatchObject({ state: "healthy", tone: "success", title: "Engine healthy" });
  });

  // THE ORDER IS PRECEDENCE: each state below is true AT THE SAME TIME as
  // every state after it, and the row says the one a reader acts on.
  test("nobody signed in outranks everything, and is the one state a press can fix", () => {
    const r = read({
      authRejected: true,
      accessRefused: "seat_unavailable",
      connected: false,
      lostAt: 1,
      health: { ...healthy, shutting_down: true },
    });
    expect(r).toMatchObject({ state: "signed-out", tone: "danger", title: "Not signed in" });
  });

  // A PERSON THE ENGINE KNOWS AND REFUSES is not asked to sign in again:
  // signing in reaches the same person with the same access.
  test("a refusal of somebody the engine knows names its reason, over a lost engine", () => {
    const r = read({ accessRefused: "seat_unavailable", connected: false, lostAt: 1 });
    expect(r).toMatchObject({ state: "refused", tone: "danger", title: "Blocked" });
    expect(r.lead).toContain("seat_unavailable");
  });

  test("a lost engine outranks what the last push claimed", () => {
    const r = read({ connected: false, lostAt: 1, health: { ...healthy, shutting_down: true } });
    expect(r).toMatchObject({ state: "unreachable", tone: "warning", title: "Engine unreachable" });
  });

  test("draining outranks no configuration, which outranks a diverged posture", () => {
    const all = { ...healthy, shutting_down: true, configured: false, posture: "shed" };
    expect(read({ health: all }).title).toBe("Draining");
    expect(read({ health: all }).lead).toBe("Shutting down");
    expect(read({ health: { ...all, shutting_down: false } }).title).toBe("No configuration");
    expect(read({ health: { ...all, shutting_down: false, configured: true } })).toMatchObject({
      state: "diverged",
      tone: "danger",
      title: "Engine shed",
      lead: "Not on the current configuration",
    });
    expect(read({ health: { ...healthy, posture: "wait" } }).title).toBe("Engine healthy");
  });

  // FALLING BEHIND IS THE PUSH'S OWN WORD: the engine sends
  // `stall_lag_seconds` only once the watched duty has missed its beat, so
  // its presence is the state and its value how far behind. It climbs towards
  // the lease TTL, at which the node restarts itself, so it outranks every
  // state that waits for a person, and only a drain, which is somebody's own
  // stop, outranks it.
  test("a node falling behind says how far, and outranks what waits for a person", () => {
    const lag = { ...healthy, stall_lag_seconds: 31.4 };
    expect(read({ health: lag })).toMatchObject({
      state: "behind",
      tone: "warning",
      title: "Falling behind",
      lead: "31s behind",
    });
    expect(read({ health: { ...lag, configured: false, posture: "wait" } }).state).toBe("behind");
    expect(read({ health: { ...lag, configured: false, posture: "serve" } }).state).toBe("behind");
    expect(read({ health: { ...lag, posture: "shed" } }).state).toBe("behind");
    expect(read({ health: { ...lag, shutting_down: true } }).state).toBe("draining");
    // A minute and more is said as one.
    expect(read({ health: { ...healthy, stall_lag_seconds: 75 } }).lead).toBe("1m 15s behind");
    // Absent is on its beat, which is the push of every healthy node.
    expect(read({ health: { ...healthy, stall_lag_seconds: undefined } }).state).toBe("healthy");
  });

  // A TAB STILL CONNECTING IS NOT A TAB THAT LOST THE ENGINE: it has been
  // answered by nothing yet, which is not "healthy" and is not a fault, so it
  // takes the neutral dot rather than the amber one.
  test("nothing reported yet is connecting, in the neutral tone", () => {
    const opening = read({ connected: false, lostAt: null, everConnected: false, health: null });
    expect(opening).toMatchObject({ state: "connecting", tone: "neutral", title: "Connecting" });
    // Open, with the first health frame still on its way.
    expect(read({ health: null })).toMatchObject({ state: "connecting", tone: "neutral" });
  });

  test("a lost engine shows the last state only where one was received", () => {
    expect(read({ connected: false, lostAt: 1, health: null }).lead).toBe(
      "Showing the last state received",
    );
    // The first dial failed: nothing was ever received, so nothing is shown.
    expect(read({ connected: false, lostAt: 1, everConnected: false, health: null }).lead).toBe(
      undefined,
    );
  });

  // STARTING IS NOT "NO CONFIGURATION": a node applying its first revision
  // reports the posture `wait` for the whole of the apply, and nothing about
  // that needs a person.
  test("a node applying its first configuration is starting, in the neutral tone", () => {
    expect(read({ health: { ...healthy, configured: false, posture: "wait" } })).toMatchObject({
      state: "starting",
      tone: "neutral",
      title: "Engine starting",
    });
    expect(read({ health: { ...healthy, configured: false, posture: "serve" } })).toMatchObject({
      state: "unconfigured",
      tone: "warning",
      title: "No configuration",
    });
  });

  // THE DOT IS THE POSTURE, AND "HEALTHY" IS A CLAIM THE ALARMS REFUTE: over
  // a standing alarm, or a table not evaluated yet, the words say "serving".
  test("a standing alarm keeps the posture's dot, and the row says serving", () => {
    const standing = read({ health: { ...healthy, alarms: { count: 2, worst: "trim_blocked" } } });
    expect(standing).toMatchObject({ state: "serving", tone: "success", title: "Engine serving" });
    const unevaluated = read({ health: { ...healthy, alarms: undefined } });
    expect(unevaluated).toMatchObject({
      state: "serving",
      tone: "success",
      title: "Engine serving",
    });
  });

  test("every state says itself in a few words, and none of them uses an em dash", () => {
    // ONE ROW, ONE HEIGHT: the words share the rail with the dot, a count and
    // the chevron, so every title is short enough to stay one line.
    for (const [state, reading] of Object.entries(EVERY)) {
      expect(reading.state, state).toBe(state);
      expect(reading.title.length, reading.title).toBeLessThanOrEqual(18);
      expect(`${reading.title} ${reading.lead ?? ""}`, state).not.toMatch(/—/);
    }
  });
});

/** A client whose socket answers each query from `answers`, recording what it was asked. */
function client(answers: Record<string, unknown> = {}) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) => {
    asked.push(what);
    return Promise.resolve(answers[what] ?? {});
  };
  store.setConnected(true);
  return { store, socket, asked };
}

function panel(
  reading: EngineReading,
  {
    readsReport = false,
    answers = {},
    onSignIn = () => {},
  }: { readsReport?: boolean; answers?: Record<string, unknown>; onSignIn?: () => void } = {},
) {
  const c = client(answers);
  const view = render(
    <ClientContext.Provider value={{ store: c.store, socket: c.socket }}>
      <EnginePanel reading={reading} readsReport={readsReport} onSignIn={onSignIn} />
    </ClientContext.Provider>,
  );
  const rerender = (next: EngineReading) =>
    view.rerender(
      <ClientContext.Provider value={{ store: c.store, socket: c.socket }}>
        <EnginePanel reading={next} readsReport={readsReport} onSignIn={onSignIn} />
      </ClientContext.Provider>,
    );
  return { ...c, view, rerender };
}

/** Let the queries a render made answer, and the renders they cause land. */
async function settle(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
}

/** The panel's facts, as term and detail pairs. */
function facts(dialog: HTMLElement): Record<string, string> {
  const out: Record<string, string> = {};
  for (const term of within(dialog).getAllByRole("term")) {
    out[term.textContent ?? ""] = term.nextElementSibling?.textContent ?? "";
  }
  return out;
}

const fiveAlarms = {
  ...healthy,
  alarms: { count: 5, worst: "backup_age" },
};

const twoAlarms = {
  ...healthy,
  alarms: { count: 2, worst: "backup_age" },
};

// ONE ALARM PER KIND, with the logs it stands on, as the engine renders them:
// every log the missing backup holds is the backup alarm's, and the one log
// another term holds is the trim alarm's.
const alarms = engineAlarms("held_beside_a_blocked_trim");
const report = {
  domains: alarms.flatMap((a) => a.domains ?? []).map(({ domain }) => ({ domain })),
  alarms,
} as unknown as RetentionReport;

describe("the row", () => {
  test("opens the engine panel: the state, the node, the fleet, the epoch, the uptime and the alarms", () => {
    panel(read());
    const row = screen.getByRole("button", { name: "Engine healthy" });
    expect(row.getAttribute("aria-haspopup")).toBe("dialog");
    expect(row.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(row);
    const dialog = screen.getByRole("dialog", { name: "Engine" });
    expect(row.getAttribute("aria-expanded")).toBe("true");
    expect(within(dialog).getByText("Engine healthy")).toBeDefined();
    expect(within(dialog).getByText("node-1")).toBeDefined();
    expect(facts(dialog)).toMatchObject({
      Nodes: "3 live",
      Config: "Epoch 42",
      Alarms: "None",
    });
    expect(facts(dialog).Up).toMatch(/^\d+ days?$/);
    // ONE WAY OUT where nothing stands: the node.
    expect(within(dialog).getByRole("link", { name: "Open node" }).getAttribute("href")).toBe(
      "#/settings/nodes/node-1",
    );
    expect(within(dialog).queryByRole("link", { name: "Open alarms" })).toBeNull();
  });

  test("counts the standing alarms beside its words, once, in words", () => {
    panel(read({ health: fiveAlarms }));
    // The figure is drawn and read once, as the end of the row's name.
    expect(screen.getByRole("button", { name: "Engine serving, 5 alarms" })).toBeDefined();
    cleanup();
    panel(read({ health: { ...healthy, alarms: { count: 1, worst: "backup_age" } } }));
    expect(screen.getByRole("button", { name: "Engine serving, 1 alarm" })).toBeDefined();
  });

  test("lists the alarms by name, longest-standing first, and goes to where the console lists them", async () => {
    const { asked } = panel(read({ health: twoAlarms }), {
      readsReport: true,
      answers: { retention: report },
    });
    fireEvent.click(screen.getByRole("button", { name: "Engine serving, 2 alarms" }));
    await settle();
    expect(asked).toContain("retention");
    const dialog = screen.getByRole("dialog", { name: "Engine" });
    const alarms = within(dialog).getByRole("list");
    expect(
      within(alarms)
        .getAllByRole("listitem")
        .map((item) => item.textContent),
    ).toEqual(["Backup age", "Trim blocked"]);
    expect(within(dialog).getByRole("link", { name: "Open alarms" }).getAttribute("href")).toBe(
      "#/settings/backups?at=alarms",
    );
  });

  // THE NAMES ARE THE REPORT'S, which takes Admin. The push is
  // public and names only the one that has stood longest, so a reader who may
  // not ask the report is told that one and how many more, and the report is
  // not asked to refuse them.
  test("a reader without the report's grant is told the longest-standing alarm and how many more", async () => {
    const { asked } = panel(read({ health: fiveAlarms }), { answers: { retention: report } });
    fireEvent.click(screen.getByRole("button", { name: "Engine serving, 5 alarms" }));
    await settle();
    expect(asked).not.toContain("retention");
    const dialog = screen.getByRole("dialog", { name: "Engine" });
    expect(
      within(within(dialog).getByRole("list"))
        .getAllByRole("listitem")
        .map((item) => item.textContent),
    ).toEqual(["Backup age", "4 more"]);
  });

  test("asks for the names again when the count beside them moves", async () => {
    const { asked, rerender } = panel(read({ health: fiveAlarms }), {
      readsReport: true,
      answers: { retention: report },
    });
    fireEvent.click(screen.getByRole("button", { name: "Engine serving, 5 alarms" }));
    await settle();
    const before = asked.filter((what) => what === "retention").length;
    rerender(read({ health: { ...fiveAlarms, alarms: { count: 4, worst: "backup_age" } } }));
    await settle();
    expect(asked.filter((what) => what === "retention").length).toBe(before + 1);
  });

  // AN ABSENT TABLE IS NOT AN EMPTY ONE, and an absent count is not a zero.
  test("a table not evaluated and a count not read are said, never drawn as nothing", () => {
    panel(read({ health: { ...healthy, alarms: undefined, nodes: undefined } }));
    fireEvent.click(screen.getByRole("button", { name: "Engine serving" }));
    expect(facts(screen.getByRole("dialog", { name: "Engine" }))).toMatchObject({
      Nodes: "Count unavailable",
      Alarms: "Not evaluated",
    });
  });

  test("a node falling behind says by how much under its head", () => {
    panel(EVERY.behind);
    fireEvent.click(screen.getByRole("button", { name: "Falling behind" }));
    const dialog = screen.getByRole("dialog", { name: "Engine" });
    expect(within(dialog).getByText("31s behind")).toBeDefined();
    expect(within(dialog).getByRole("link", { name: "Open node" })).toBeDefined();
  });

  test("a node starting says it is applying its configuration", () => {
    panel(EVERY.starting);
    fireEvent.click(screen.getByRole("button", { name: "Engine starting" }));
    expect(facts(screen.getByRole("dialog", { name: "Engine" })).Config).toBe("Applying");
  });

  test("a way out closes the panel and hands focus back to the row", () => {
    panel(read());
    const row = screen.getByRole("button", { name: "Engine healthy" });
    row.focus();
    fireEvent.click(row);
    fireEvent.click(screen.getByRole("link", { name: "Open node" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(row);
  });

  test("an unreachable engine offers to reconnect now, and says when it last heard", () => {
    // Two and a half minutes, inside "2m ago" whatever the shared clock's
    // last tick was: it may stand a second behind the wall clock.
    const reading = read({ connected: false, lostAt: Date.now() - 150_000, health: null });
    const { socket } = panel(reading);
    const reconnect = vi.spyOn(socket, "reconnect").mockImplementation(() => {});
    fireEvent.click(screen.getByRole("button", { name: "Engine unreachable" }));
    const dialog = screen.getByRole("dialog", { name: "Engine" });
    expect(within(dialog).getByText("Showing the last state received")).toBeDefined();
    expect(facts(dialog)).toEqual({ "Last update": "2m ago" });
    // Nothing here leads anywhere while the engine cannot answer.
    expect(within(dialog).queryByRole("link")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Reconnect now" }));
    expect(reconnect).toHaveBeenCalledOnce();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("nobody signed in is the button that goes to the sign-in, and opens no panel", () => {
    const onSignIn = vi.fn();
    panel(EVERY["signed-out"], { onSignIn });
    const row = screen.getByRole("button", { name: "Not signed in. Sign in" });
    expect(row.hasAttribute("aria-haspopup")).toBe(false);
    fireEvent.click(row);
    expect(onSignIn).toHaveBeenCalledOnce();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  // A STATEMENT WEARS NOTHING A CONTROL WEARS. A refusal drew a chevron, a
  // pointer and a hover border on a card that did nothing and could not be
  // focused. It is repaired by an admin, so the status says it and is no
  // control: no button, no place in the tab order, and nothing a press
  // opens. Mutation: give it the panel's trigger, or the sign-in's button,
  // and a line here goes red.
  test("a refusal is a statement, with nothing to press", () => {
    for (const statement of [EVERY.refused]) {
      const { view } = panel(statement);
      const mark = view.container.querySelector(".crewlet-app-shell__rail-status-mark")!;
      expect(mark.tagName, statement.state).toBe("SPAN");
      // ITS WORDS ARE ITS TITLE AND ITS TEXT FOR A SCREEN READER, never drawn.
      expect(mark.getAttribute("title")).toBe(statement.title);
      expect(within(mark as HTMLElement).getByText(statement.title)).toBeTruthy();
      expect(screen.queryByRole("button")).toBeNull();
      expect(view.container.querySelector("a, button, [tabindex]")).toBeNull();
      fireEvent.click(mark);
      expect(screen.queryByRole("dialog")).toBeNull();
      cleanup();
    }
    // THE CONTROL SAYS IT OPENS A PANEL, by the attribute a popup trigger
    // carries, and its words are its name and its title.
    panel(read());
    const trigger = screen.getByRole("button", { name: "Engine healthy" });
    expect(trigger.getAttribute("aria-haspopup")).toBe("dialog");
    expect(trigger.getAttribute("title")).toBe("Engine healthy");
  });
});
