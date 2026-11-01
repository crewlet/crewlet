// @vitest-environment node

/**
 * A page action is carried out exactly once: claimed on the server before
 * the screen applies anything, applied only by the tab it was meant for, and
 * reported on the same claim.
 *
 * WHAT NEVER HAPPENS is the case each test below guards: a screen applying a
 * change another tab claimed, a change applied on a claim nobody confirmed,
 * a change applied twice because its frame was read twice, and a tab on
 * another page claiming a change and answering `stale` for the tab that
 * would have applied it.
 */

import { afterEach, beforeEach, describe, expect, test } from "vitest";
import type { AssistantActionEvent, AssistantActionReport } from "~/contract/assistant.ts";
import { RestError } from "~/protocol/rest.ts";
import {
  pageSnapshot,
  registerPage,
  resetPageContextForTest,
  setFramePage,
  undoPageAction,
  type FramePage,
  type PageActionSpec,
} from "../context.ts";
import {
  resetPageActionsForTest,
  takeAction,
  type ActionDeps,
  type ActionLocal,
} from "./pageActions.ts";

const PAGE: FramePage = {
  screen: "worklet-tasks",
  title: "Worklet › Tasks",
  path: ["worklet", "tasks"],
  params: {},
  query: {},
};

let applied: string[];
let shown: string | null;

/** A screen whose one action sets `blocked`, and puts it back while nothing moved it since. */
const spec: PageActionSpec<string> = {
  scope: "Worklet list filters (not saved)",
  args: { type: "object" },
  parse: (raw) =>
    raw && typeof raw === "object" && typeof (raw as { blocked?: unknown }).blocked === "string"
      ? { ok: true, value: (raw as { blocked: string }).blocked }
      : { ok: false, reason: "Not a set of filters." },
  preview: (v) => [{ field: "Blocked", next: v, current: shown }],
  blocked: () => null,
  apply: (v) => {
    applied.push(v);
    shown = v;
  },
  undo: (v) => {
    const before = shown;
    return () => {
      if (shown !== v) return false;
      shown = before;
      return true;
    };
  },
};

function action(over: Partial<AssistantActionEvent> = {}): AssistantActionEvent {
  return {
    action_id: "5950210a-8478-5ed3-b6ed-a28d04417faa",
    name: "work.filter.apply",
    proposal_id: "9035513c-6cd1-5b25-bbe2-c39df0a5a786",
    source_revision: pageSnapshot()!.context.source_revision,
    payload: { blocked: "true" },
    confirm: "approve",
    ...over,
  };
}

/** The reports a take sends, and what each is answered with. */
function harness(answers: ((body: AssistantActionReport) => unknown)[] = []) {
  const reports: AssistantActionReport[] = [];
  const keys: string[] = [];
  const settled: ActionLocal[] = [];
  const deps: ActionDeps = {
    settle: (local) => settled.push(local),
    report: async (_id, body, opKey) => {
      reports.push(body);
      keys.push(opKey);
      const answer = answers.shift();
      if (answer) return answer(body);
      return { action_id: "x", state: body.state };
    },
  };
  return { reports, keys, settled, deps };
}

const noWait = () => Promise.resolve();

beforeEach(() => {
  applied = [];
  shown = null;
  setFramePage(PAGE);
  registerPage({ actions: { "work.filter.apply": spec } });
});

afterEach(() => {
  resetPageContextForTest();
  resetPageActionsForTest();
});

describe("a page action", () => {
  test("is claimed before the screen applies it, and its outcome reported on the claim", async () => {
    const { reports, settled, deps } = harness([
      (body) => {
        // THE CLAIM GOES FIRST: nothing is applied until the engine took it.
        expect(applied).toEqual([]);
        return { action_id: "x", state: body.state };
      },
    ]);
    await takeAction(action(), true, deps, noWait);
    expect(reports.map((r) => r.state)).toEqual(["applying", "applied"]);
    expect(reports[1]).toMatchObject({ receipt: "Applied to Worklet › Tasks." });
    expect(applied).toEqual(["true"]);
    expect(settled).toEqual([
      { state: "applied", words: "Applied to Worklet › Tasks.", undoable: true },
    ]);
  });

  test("its outcome report carries the screen as the action left it", async () => {
    const { reports, deps } = harness();
    const after = { ...pageSnapshot()!.context, source_revision: "worklet-tasks@2" };
    await takeAction(action(), true, { ...deps, screen: async () => after }, noWait);
    expect(reports[0]).not.toHaveProperty("page_context");
    expect(reports[1]).toMatchObject({ state: "applied", page_context: after });
  });

  test("the screen it reports is the one the page settled on after applying", async () => {
    const { reports, deps } = harness();
    // THE SCREEN REPUBLISHES AFTER ITS SETTERS RAN, as a draft does once the
    // page re-renders: the report waits for it.
    const republishing: PageActionSpec<string> = {
      ...spec,
      apply: (v) => {
        spec.apply(v);
        setTimeout(() => setFramePage({ ...PAGE, title: "Worklet › Blocked" }), 10);
      },
    };
    registerPage({ actions: { "work.filter.apply": republishing } });
    await takeAction(action(), true, deps, noWait);
    expect(reports[1]?.page_context?.title).toBe("Worklet › Blocked");
  });

  test("read twice is taken once", async () => {
    const { reports, deps } = harness();
    await takeAction(action(), true, deps, noWait);
    await takeAction(action(), true, deps, noWait);
    expect(applied).toEqual(["true"]);
    expect(reports).toHaveLength(2);
  });

  test("another tab claimed first: nothing is applied here", async () => {
    const { reports, settled, deps } = harness([
      () => {
        throw new RestError(409, { error: "action_already_claimed" }, null, true);
      },
    ]);
    await takeAction(action(), true, deps, noWait);
    expect(applied).toEqual([]);
    expect(reports).toHaveLength(1);
    expect(settled[0]?.state).toBe("elsewhere");
  });

  test("a claim nobody ever answered is not a claim: nothing is applied and nothing reported", async () => {
    const unanswered = () => {
      throw new RestError(0, { error: "unreachable" });
    };
    const { reports, keys, settled, deps } = harness([unanswered, unanswered, unanswered]);
    await takeAction(action(), true, deps, noWait);
    expect(applied).toEqual([]);
    expect(reports.map((r) => r.state)).toEqual(["applying", "applying", "applying"]);
    expect(new Set(keys).size).toBe(1);
    expect(settled[0]).toMatchObject({ state: "unconfirmed", undoable: false });
  });

  test("a claim whose answer was lost is asked again as the same claim, and then applied", async () => {
    const { reports, keys, deps } = harness([
      () => {
        throw new RestError(0, { error: "unreachable" });
      },
    ]);
    await takeAction(action(), true, deps, noWait);
    expect(reports.map((r) => r.state)).toEqual(["applying", "applying", "applied"]);
    // THE SAME OPERATION for the claim and its retry, another for the report.
    expect(keys[0]).toBe(keys[1]);
    expect(keys[2]).not.toBe(keys[0]);
    expect(applied).toEqual(["true"]);
  });

  test("a claim the engine refused applies nothing, in the engine's words", async () => {
    const { settled, deps } = harness([
      () => {
        throw new RestError(
          410,
          { error: "proposal_expired", message: "This change expired." },
          null,
          true,
        );
      },
    ]);
    await takeAction(action(), true, deps, noWait);
    expect(applied).toEqual([]);
    expect(settled[0]).toMatchObject({ state: "unconfirmed", words: "This change expired." });
  });

  test("a tab on another page leaves it for the tab it was prepared in", async () => {
    const { reports, deps } = harness();
    await takeAction(action({ source_revision: "worklet-tasks@00000000" }), false, deps, noWait);
    expect(reports).toEqual([]);
    expect(applied).toEqual([]);
  });

  test("a tab on the very page it was prepared against takes it, though it did not approve it", async () => {
    const { reports, deps } = harness();
    await takeAction(action(), false, deps, noWait);
    expect(reports.map((r) => r.state)).toEqual(["applying", "applied"]);
  });

  test("the page this tab approved it on moved since: claimed and reported stale, nothing applied", async () => {
    const { reports, settled, deps } = harness();
    const prepared = action();
    setFramePage({ ...PAGE, query: { status: "open" } });
    await takeAction(prepared, true, deps, noWait);
    expect(applied).toEqual([]);
    expect(reports.map((r) => r.state)).toEqual(["applying", "stale"]);
    expect(settled[0]?.state).toBe("stale");
  });

  test("a screen's refusal is reported with the screen's own sentence", async () => {
    const { reports, settled, deps } = harness();
    await takeAction(action({ payload: 7 }), true, deps, noWait);
    expect(reports[1]).toMatchObject({ state: "invalid", reason: "Not a set of filters." });
    expect(settled[0]).toMatchObject({ state: "invalid", words: "Not a set of filters." });
  });

  test("an outcome report nobody answered is sent again, and one the engine refused is not", async () => {
    const unanswered = () => {
      throw new RestError(0, { error: "unreachable" });
    };
    const first = harness([(b) => ({ state: b.state }), unanswered, unanswered]);
    await takeAction(action(), true, first.deps, noWait);
    expect(first.reports.map((r) => r.state)).toEqual([
      "applying",
      "applied",
      "applied",
      "applied",
    ]);

    resetPageActionsForTest();
    const second = harness([
      (b) => ({ state: b.state }),
      () => {
        throw new RestError(404, { error: "not_found" }, null, true);
      },
    ]);
    await takeAction(action(), true, second.deps, noWait);
    expect(second.reports.map((r) => r.state)).toEqual(["applying", "applied"]);
  });

  test("the reserved context.invalidate is never claimed", async () => {
    const { reports, deps } = harness();
    await takeAction(
      {
        action_id: "7bee0329-e6f7-5a5f-b19e-b7038180e148",
        name: "context.invalidate",
        invalidate: { domain: "tracker", position: "CREWLET_TRACKER_LOG:1:4812" },
      },
      true,
      deps,
      noWait,
    );
    expect(reports).toEqual([]);
  });
});

describe("an undo", () => {
  test("puts the page back while nothing moved it since, and only once", async () => {
    const { deps } = harness();
    await takeAction(action(), true, deps, noWait);
    expect(undoPageAction(action().action_id)).toBe("undone");
    expect(shown).toBeNull();
    expect(undoPageAction(action().action_id)).toBe("gone");
  });

  test("changes nothing once the person changed the page since", async () => {
    const { deps } = harness();
    await takeAction(action(), true, deps, noWait);
    shown = "false";
    expect(undoPageAction(action().action_id)).toBe("moved");
    expect(shown).toBe("false");
  });

  test("goes with the screen that applied it", async () => {
    const { deps } = harness();
    await takeAction(action(), true, deps, noWait);
    setFramePage({ ...PAGE, screen: "worklet-projects", path: ["worklet", "projects"] });
    expect(undoPageAction(action().action_id)).toBe("gone");
  });
});
