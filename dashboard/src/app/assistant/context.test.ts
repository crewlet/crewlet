// @vitest-environment node

/**
 * What a page tells the assistant, held to the engine's own bounds, and the
 * page's half of applying an action.
 *
 * A CONTEXT THE ENGINE WOULD REFUSE IS NEVER SENT: a key that is not a name,
 * a value that is not plain text, or too much of anything is left out here
 * (`internal/assistant/wire.PageContext.Validate` holds the same rules), so a
 * question is never refused after the person pressed Send because the screen
 * under it registered too much.
 */

import { afterEach, describe, expect, test } from "vitest";
import {
  ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS,
  ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES,
} from "~/contract/assistant.ts";
import { pageContextFits } from "~/protocol/assistantWire.ts";
import { crumbsFor } from "../crumbs.ts";
import { on } from "./bus.ts";
import {
  applyPageAction,
  buildPageContext,
  framePageOf,
  pageSnapshot,
  registerPage,
  resetPageContextForTest,
  setFramePage,
  type AssistantPageRegistration,
  type FramePage,
  type PageActionSpec,
} from "./context.ts";

afterEach(resetPageContextForTest);

const PAGE: FramePage = {
  screen: "worklet-tasks",
  title: "Worklet › Tasks",
  path: ["worklet", "tasks"],
  params: {},
  query: { blocked: "true" },
};

describe("the context a turn sends", () => {
  test("names the screen, its address and what is in view, by id", () => {
    const context = buildPageContext(PAGE, {
      resources: [{ kind: "task", id: "ENG-1", version: "4" }],
      selection: { kind: "task", id: "ENG-1" },
    });
    expect(context).toMatchObject({
      screen: "worklet-tasks",
      title: "Worklet › Tasks",
      query: { blocked: "true" },
      resources: [{ kind: "task", id: "ENG-1", version: "4" }],
      selection: { kind: "task", id: "ENG-1" },
      accepts: [],
    });
    expect(pageContextFits(context)).toBe(true);
  });

  test("leaves out what the engine would refuse rather than sending it", () => {
    const context = buildPageContext(
      { ...PAGE, screen: "Not A Screen", query: { "Bad Key": "x", ok: "a\nb", fine: "1" } },
      {
        resources: [
          { kind: "task", id: "" },
          { kind: "task", id: "x".repeat(201) },
          { kind: "task", id: "ENG-2" },
        ],
        // ONLY THE CONTRACT'S IDS are offered: the engine's reserved one, an
        // id nobody declared and one that is no id at all are each left out.
        actions: {
          "work.filter.apply": {} as PageActionSpec<never>,
          "context.invalidate": {} as PageActionSpec<never>,
          "made.up": {} as PageActionSpec<never>,
          notdotted: {} as PageActionSpec<never>,
        } as AssistantPageRegistration["actions"],
      },
    );
    expect(context.screen).toBe("unknown");
    expect(context.query).toEqual({ fine: "1" });
    expect(context.resources).toEqual([{ kind: "task", id: "ENG-2" }]);
    expect(context.accepts).toEqual(["work.filter.apply"]);
  });

  test("is held to the counts and the byte budget, shedding the query first", () => {
    const resources = Array.from({ length: 80 }, (_, i) => ({
      kind: "task" as const,
      id: `T-${"9".repeat(150)}-${i}`,
    }));
    const actions = Object.fromEntries(
      Array.from({ length: 40 }, (_, i) => [`page.a${i}`, {} as PageActionSpec<never>]),
    );
    const context = buildPageContext(
      { ...PAGE, query: { q: "x".repeat(500) } },
      { resources, actions },
    );
    expect(context.resources.length).toBeLessThanOrEqual(ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES);
    expect(context.accepts.length).toBeLessThanOrEqual(ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS);
    expect(pageContextFits(context)).toBe(true);
  });

  test("a revision is the page's: the same page is the same revision, any change another", () => {
    const one = buildPageContext(PAGE, { dataVersion: "7" }).source_revision;
    expect(buildPageContext(PAGE, { dataVersion: "7" }).source_revision).toBe(one);
    expect(buildPageContext(PAGE, { dataVersion: "8" }).source_revision).not.toBe(one);
    expect(buildPageContext({ ...PAGE, query: {} }, { dataVersion: "7" }).source_revision).not.toBe(
      one,
    );
  });
});

describe("what the person has open", () => {
  const record = (
    screen: string,
    params: Record<string, string>,
    query: Record<string, string> = {},
  ): FramePage => ({
    screen,
    title: "Somewhere",
    path: [screen],
    params,
    query,
  });

  test("a record's own screen has that record open, named by what a person calls it", () => {
    expect(buildPageContext(record("item", { id: "ENG-7" }), null).selection).toEqual({
      kind: "task",
      id: "ENG-7",
      noun: "task",
    });
    expect(buildPageContext(record("seat", { handle: "qa" }), null).selection).toEqual({
      kind: "seat",
      id: "qa",
      noun: "agent",
    });
    expect(buildPageContext(record("artifact", { id: "a1" }), null).selection).toEqual({
      kind: "artifact",
      id: "a1",
      noun: "artifact",
    });
    // A CHATLET ROOM'S PAGE IS THAT ROOM, which a brief may then cite.
    expect(buildPageContext(record("chatlet", { id: "r1" }), null).selection).toEqual({
      kind: "room",
      id: "r1",
      noun: "room",
    });
  });

  test("a list screen has nothing open until a row is peeked", () => {
    expect(buildPageContext(PAGE, null).selection).toBeNull();
    expect(buildPageContext({ ...PAGE, query: { peek: "item:ENG-3" } }, null).selection).toEqual({
      kind: "task",
      id: "ENG-3",
      noun: "task",
    });
  });

  test("a peek over a record's screen is what is open, and the screen's own word wins", () => {
    const seat = record("seat", { handle: "qa" }, { peek: "item:ENG-3" });
    expect(buildPageContext(seat, null).selection).toMatchObject({ kind: "task", id: "ENG-3" });
    expect(
      buildPageContext(PAGE, { selection: { kind: "task", id: "ENG-9", noun: "decision" } })
        .selection,
    ).toEqual({ kind: "task", id: "ENG-9", noun: "decision" });
  });

  test("a screen that says nothing is open is believed over the address", () => {
    expect(
      buildPageContext(record("item", { id: "ENG-7" }), { selection: null }).selection,
    ).toBeNull();
  });

  test("a kind with no record noun is sent without one", () => {
    expect(buildPageContext(PAGE, { selection: { kind: "unit", id: "core" } }).selection).toEqual({
      kind: "unit",
      id: "core",
    });
  });
});

describe("the registry", () => {
  test("says when the page moved, on the bus, with the new revision", () => {
    const heard: string[] = [];
    const off = on("assistant.context.invalidate", (e) => heard.push(e.reason));
    setFramePage(PAGE);
    setFramePage({ ...PAGE, query: {} });
    setFramePage({ ...PAGE, path: ["worklet", "ENG-1"], screen: "item" });
    off();
    expect(heard).toEqual(["route", "filters", "route"]);
  });

  test("a screen's registration is withdrawn when it goes", () => {
    setFramePage(PAGE);
    const withdraw = registerPage({ resources: [{ kind: "task", id: "ENG-1" }] });
    expect(pageSnapshot()?.context.resources).toHaveLength(1);
    withdraw();
    expect(pageSnapshot()?.context.resources).toHaveLength(0);
  });
});

describe("a page of several components", () => {
  const noop: PageActionSpec<unknown> = {
    scope: "x",
    args: { type: "object" },
    parse: (raw) => ({ ok: true, value: raw }),
    preview: () => [],
    blocked: () => null,
    apply: () => {},
  };

  test("offers every mounted component's actions and resources, and loses one only when it goes", () => {
    setFramePage(PAGE);
    const project = registerPage({
      resources: [{ kind: "project", id: "ENG" }],
      actions: { "project.lens.set": noop },
    });
    const list = registerPage({
      resources: [{ kind: "project", id: "ENG" }],
      actions: { "work.filter.apply": noop, "work.arrange.set": noop },
    });
    expect(pageSnapshot()?.context.accepts.sort()).toEqual([
      "project.lens.set",
      "work.arrange.set",
      "work.filter.apply",
    ]);
    expect(pageSnapshot()?.context.resources).toEqual([{ kind: "project", id: "ENG" }]);
    list();
    expect(pageSnapshot()?.context.accepts).toEqual(["project.lens.set"]);
    project();
    expect(pageSnapshot()?.context.accepts).toEqual([]);
  });

  test("a move in any component's data is a move of the page", () => {
    setFramePage(PAGE);
    registerPage({ dataVersion: "a" });
    const before = pageSnapshot()!.context.source_revision;
    registerPage({ dataVersion: "b" });
    expect(pageSnapshot()!.context.source_revision).not.toBe(before);
  });
});

describe("applying a page action", () => {
  const applied: string[] = [];
  const spec: PageActionSpec<string> = {
    scope: "Worklet list filters (not saved)",
    args: { type: "object" },
    parse: (raw) =>
      typeof raw === "string" ? { ok: true, value: raw } : { ok: false, reason: "Not a status." },
    preview: (v) => [{ field: "Status", next: v, current: null }],
    blocked: (v) => (v === "locked" ? "This filter is locked on this page." : null),
    apply: (v) => applied.push(v),
  };

  function request(payload: unknown, revision = pageSnapshot()!.context.source_revision) {
    return {
      correlation_id: "c",
      action: "work.filter.apply",
      payload,
      source_revision: revision,
    };
  }

  test("each way it can go is its own outcome, and only an applied one runs the setters", () => {
    setFramePage(PAGE);
    registerPage({ actions: { "work.filter.apply": spec } });
    expect(applyPageAction({ ...request("blocked"), action: "nope.here" }).outcome).toBe(
      "no_handler",
    );
    expect(applyPageAction(request("blocked", "worklet-tasks@00000000")).outcome).toBe("stale");
    expect(applyPageAction(request(42))).toMatchObject({
      outcome: "invalid",
      reason: "Not a status.",
    });
    expect(applyPageAction(request("locked"))).toMatchObject({ outcome: "blocked" });
    expect(applied).toEqual([]);
    expect(applyPageAction(request("blocked")).outcome).toBe("applied");
    expect(applied).toEqual(["blocked"]);
  });
});

// THE CHIP NAMES THE TAB THE READER IS ON: Worklet's scope pages end their trail
// in the scope and the lens is a tab, so the lens takes the scope's place
// ("Worklet › Tasks", the plan's own example); elsewhere it is the trail's last
// two crumbs.
test("the page chip names the Worklet lens and its scope, and the last two crumbs elsewhere", () => {
  const tasks = framePageOf(
    ["worklet", "tasks"],
    new URLSearchParams(),
    crumbsFor(["worklet", "tasks"], {}),
  );
  expect(tasks?.title).toBe("Worklet › Tasks");
  const projects = framePageOf(["worklet"], new URLSearchParams(), crumbsFor(["worklet"], {}));
  expect(projects?.title).toBe("Worklet › Projects");
  const spend = framePageOf(
    ["settings", "spend"],
    new URLSearchParams(),
    crumbsFor(["settings", "spend"], {}),
  );
  expect(spend?.title).toBe("Settings › Token spend");
});

/**
 * A CHANGE GOES STALE ONLY WHEN WHAT IT DEPENDS ON MOVES.
 *
 * The QA run's first Budgets prefill was refused stale at its claim: the page's
 * revision covers the whole page (its query, the peek, every open draft), so a
 * peek opened beside the table, or any ceiling editor opening, moved it under a
 * change that edits neither. Each action's revision tracks only what it states
 * (`tracks`), and the page context carries it per action, so the engine binds
 * the proposal to that and the claim reports that. Mutation: compute an
 * action's revision from every part and the prefill moves with the peek.
 */
describe("an action's own revision", () => {
  const spec = (tracks?: PageActionSpec<unknown>["tracks"]): PageActionSpec<unknown> => ({
    scope: "x",
    args: { type: "object" },
    ...(tracks === undefined ? {} : { tracks }),
    parse: (raw) => ({ ok: true, value: raw }),
    preview: () => [],
    blocked: () => null,
    apply: () => {},
  });
  const BUDGETS: FramePage = {
    screen: "budgets",
    title: "Company › Budgets",
    path: ["company", "budgets"],
    params: {},
    query: {},
  };

  test("a prefill is not moved by a peek or an editor beside it, and the page is", () => {
    setFramePage(BUDGETS);
    registerPage({
      actions: { "budgets.ceiling.prefill": spec([]), "spend.range.set": spec(["query"]) },
    });
    const at = () => pageSnapshot()!.context;
    const before = at();
    expect(before.action_revisions?.["budgets.ceiling.prefill"]).toBeDefined();
    expect(before.action_args?.["budgets.ceiling.prefill"]).toEqual({ type: "object" });

    // THE PERSON OPENS A SEAT'S PEEK, then a ceiling editor registers its draft.
    setFramePage({ ...BUDGETS, query: { peek: "seat:qa" } });
    registerPage({ dataVersion: "draft:1" });
    const after = at();
    expect(after.source_revision).not.toBe(before.source_revision);
    expect(after.action_revisions?.["budgets.ceiling.prefill"]).toBe(
      before.action_revisions?.["budgets.ceiling.prefill"],
    );
    // AN ADDRESS ACTION IS NOT MOVED BY THE PEEK EITHER, which is the selection's.
    expect(after.action_revisions?.["spend.range.set"]).toBe(
      before.action_revisions?.["spend.range.set"],
    );

    // A CHANGE TO WHAT IT TRACKS MOVES IT.
    setFramePage({ ...BUDGETS, query: { peek: "seat:qa", window: "7d" } });
    expect(at().action_revisions?.["spend.range.set"]).not.toBe(
      before.action_revisions?.["spend.range.set"],
    );
  });

  test("the screen judges a change against its action's revision, and says it moved", () => {
    setFramePage(BUDGETS);
    registerPage({ actions: { "budgets.ceiling.prefill": spec([]) } });
    const prepared = pageSnapshot()!.context.action_revisions!["budgets.ceiling.prefill"]!;
    const heard: Record<string, string>[] = [];
    const off = on("assistant.context.invalidate", ({ revisions }) => heard.push({ ...revisions }));
    setFramePage({ ...BUDGETS, query: { peek: "seat:qa" } });
    off();
    expect(heard.at(-1)?.["budgets.ceiling.prefill"]).toBe(prepared);
    expect(
      applyPageAction({
        correlation_id: "c",
        action: "budgets.ceiling.prefill",
        payload: {},
        source_revision: prepared,
      }).outcome,
    ).toBe("applied");
  });

  test("an action that states nothing tracks the whole page, as before", () => {
    setFramePage(PAGE);
    registerPage({ actions: { "work.filter.apply": spec() } });
    const context = pageSnapshot()!.context;
    expect(context.action_revisions?.["work.filter.apply"]).toBe(context.source_revision);
  });
});
