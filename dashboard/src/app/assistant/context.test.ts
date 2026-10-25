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
  screen: "work-tasks",
  title: "Work › Tasks",
  path: ["work", "tasks"],
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
      screen: "work-tasks",
      title: "Work › Tasks",
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

describe("the registry", () => {
  test("says when the page moved, on the bus, with the new revision", () => {
    const heard: string[] = [];
    const off = on("assistant.context.invalidate", (e) => heard.push(e.reason));
    setFramePage(PAGE);
    setFramePage({ ...PAGE, query: {} });
    setFramePage({ ...PAGE, path: ["work", "ENG-1"], screen: "item" });
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
    scope: "Work list filters (not saved)",
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
    expect(applyPageAction(request("blocked", "work-tasks@00000000")).outcome).toBe("stale");
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

// THE CHIP NAMES THE TAB THE READER IS ON: Work's scope pages end their trail
// in the scope and the lens is a tab, so the lens takes the scope's place
// ("Work › Tasks", the plan's own example); elsewhere it is the trail's last
// two crumbs.
test("the page chip names the Work lens and its scope, and the last two crumbs elsewhere", () => {
  const tasks = framePageOf(
    ["work", "tasks"],
    new URLSearchParams(),
    crumbsFor(["work", "tasks"], {}),
  );
  expect(tasks?.title).toBe("Work › Tasks");
  const projects = framePageOf(["work"], new URLSearchParams(), crumbsFor(["work"], {}));
  expect(projects?.title).toBe("Work › Projects");
  const spend = framePageOf(
    ["settings", "spend"],
    new URLSearchParams(),
    crumbsFor(["settings", "spend"], {}),
  );
  expect(spend?.title).toBe("Settings › Token spend");
});
