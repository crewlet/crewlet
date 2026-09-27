/**
 * EVERY CONTROL THAT CHANGES THE COMPANY IS DRAWN FOR EVERY READER, AND SAYS
 * WHY IT CANNOT ACT WHERE IT CANNOT.
 *
 * Two halves, because either alone passes over the other's failure:
 *
 *  - THE SOURCE: the one write (`protocol/act.ts`'s `act`) is called from
 *    `lib/useAct.ts` and nowhere else, and every module that makes a change
 *    through `useAct` is a module of write controls listed here with a
 *    fixture — so a new write surface cannot land without being rendered
 *    below, and a screen cannot reach `act` around the gating;
 *  - THE RENDER: every control of every such module, for an anonymous
 *    reader, an unbound token, a bound person the engine makes the change
 *    for, and one it does not. The control is PRESENT in all four — a write
 *    control is never hidden — enabled only for the bound person the engine
 *    serves, and otherwise disabled with the sentence that says what would
 *    change that.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { ComponentType } from "react";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import * as writes from "~/components/writes.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { SRC, type Node, isLocalSource, modules, parse } from "~/test/source.ts";

vi.mock("~/lib/store-hooks.ts", () => ({
  useConnection: vi.fn(),
  useOrg: vi.fn(),
  useClient: vi.fn(),
}));
vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

/**
 * Every module that may make a change, why it is one, and the suite that
 * renders its controls for every reader — `null` for the controls rendered
 * below.
 */
const WRITE_MODULES: Readonly<Record<string, { why: string; suite: string | null }>> = {
  "components/writes.tsx": {
    why: "the write controls every screen draws, each rendered below",
    suite: null,
  },
  "app/palette/Palette.tsx": {
    why:
      "the command palette's three actions — ROWS of the kit's list rather than " +
      "buttons, because focus never leaves its field — each drawn for every reader " +
      "with the reason it cannot act as its hint",
    suite: "app/palette/Palette.test.tsx",
  },
  "app/palette/answer.ts": {
    why:
      "the palette's answer from the company's knowledge, asked only for a reader " +
      "who may ask, and telling an anonymous or unbound one what would let them",
    suite: "app/palette/Palette.test.tsx",
  },
};

/** How each control of `components/writes.tsx` is mounted, and the name it is found by. */
const FIXTURES: Readonly<Record<keyof typeof writes, { props: object; name: RegExp }>> = {
  AssignButton: { props: { item: "ENG-1", version: 3, assignee: "" }, name: /^Assign$/ },
  RestoreButton: { props: { item: "ENG-1" }, name: /^Restore ENG-1$/ },
  PinButton: { props: { view: "v1", name: "Triage", pinned: false }, name: /^Pin$/ },
  MarkReadButton: { props: { recordId: "r-1" }, name: /^Mark read$/ },
  AnswerAskButtons: {
    props: {
      item: "LEAD-12",
      comment: "c-1",
      options: [{ id: "hold", label: "Hold release" }],
      recommended: "hold",
    },
    name: /^Hold release$/,
  },
  AnswerRunButton: {
    props: { turnId: "run-1", seat: "Frontend SWE", question: "Drop the fallback?" },
    name: /^Answer$/,
  },
  ReplyAskButton: {
    props: { item: "LEAD-12", comment: "c-2", asker: "CEO", question: "Which vendor?" },
    name: /^Reply$/,
  },
};

/** The tool each control makes its change through, so a reader the engine does not serve it is refused. */
const TOOLS: Readonly<Record<keyof typeof writes, string>> = {
  AssignButton: "update_work_item",
  RestoreButton: "restore_work_item",
  PinButton: "set_pins",
  MarkReadButton: "mark_inbox",
  AnswerAskButtons: "comment_on_work_item",
  AnswerRunButton: "answer_run",
  ReplyAskButton: "comment_on_work_item",
};

const EVERY_TOOL = Object.values(TOOLS);

const NOBODY: ViewerState = {
  operatorID: "",
  operator: false,
  handle: "",
  name: "",
  acts: [],
  project: "",
  kind: "",
  unbound: false,
  anonymous: true,
  loading: false,
  asking: false,
};

const READERS: readonly {
  who: string;
  viewer: (tool: string) => ViewerState;
  reason: string | null;
}[] = [
  { who: "an anonymous reader", viewer: () => NOBODY, reason: WRITE_REASONS.anonymous },
  {
    who: "an unbound token",
    viewer: () => ({
      ...NOBODY,
      anonymous: false,
      operatorID: "ci",
      operator: true,
      unbound: true,
    }),
    reason: WRITE_REASONS.unbound,
  },
  {
    who: "a bound person the engine makes it for",
    viewer: () => ({
      ...NOBODY,
      anonymous: false,
      operatorID: "founder",
      operator: true,
      handle: "jane",
      name: "Jane Founder",
      kind: "human",
      acts: EVERY_TOOL,
    }),
    reason: null,
  },
  {
    who: "a bound person the engine does not make it for",
    viewer: (tool) => ({
      ...NOBODY,
      anonymous: false,
      operatorID: "founder",
      operator: true,
      handle: "jane",
      name: "Jane Founder",
      kind: "human",
      acts: EVERY_TOOL.filter((t) => t !== tool),
    }),
    reason: WRITE_REASONS.not_served,
  },
];

beforeEach(() => {
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue(null as never);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

/** The local modules `text` imports `name` from, by what they resolve to under `src/`. */
function importsOf(text: string, lang: "ts" | "tsx" | "dts"): Map<string, string> {
  const out = new Map<string, string>();
  for (const statement of parse(text, lang).body as Node[]) {
    if (statement.type !== "ImportDeclaration" || statement.importKind === "type") continue;
    const from = String((statement.source as Node).value);
    if (!isLocalSource(from)) continue;
    for (const spec of statement.specifiers as Node[]) {
      if (spec.type !== "ImportSpecifier" || spec.importKind === "type") continue;
      const imported = spec.imported as Node;
      out.set(String(imported.type === "Identifier" ? imported.name : imported.value), from);
    }
  }
  return out;
}

describe("the source", () => {
  const tree = modules().filter(({ path }) => !/\.test\.tsx?$/.test(path));

  test("reaches the one write from the one hook", () => {
    const callers = tree
      .filter(({ text, lang }) => importsOf(text, lang).get("act")?.endsWith("protocol/act.ts"))
      .map(({ path }) => path);
    expect(callers, "a screen makes a change through useAct, which gates it").toEqual([
      "lib/useAct.ts",
    ]);
  });

  test("makes a change only from a module of write controls this gate renders", () => {
    const writers = tree
      .filter(({ text, lang }) => importsOf(text, lang).has("useAct"))
      .map(({ path }) => path)
      .sort();
    expect(writers).toEqual(Object.keys(WRITE_MODULES).sort());
  });

  test("draws each of those controls with the gated button", () => {
    for (const [path, { suite }] of Object.entries(WRITE_MODULES)) {
      if (suite !== null) continue;
      const module = tree.find((m) => m.path === path)!;
      expect(importsOf(module.text, module.lang).has("WriteButton"), path).toBe(true);
    }
  });

  // A MODULE WHOSE CONTROLS ARE NOT BUTTONS is rendered by its own suite, and
  // that suite has to be one that draws them for the readers who cannot act:
  // naming a suite is not the same as the suite covering them.
  test("and a module drawn elsewhere names a suite that renders it for each reader", () => {
    for (const [path, { suite }] of Object.entries(WRITE_MODULES)) {
      if (suite === null) continue;
      const file = join(SRC, suite);
      expect(existsSync(file), `${path} names ${suite}, which does not exist`).toBe(true);
      const text = readFileSync(file, "utf8");
      for (const block of ["anonymous", "unbound", "not_served"]) {
        expect(text, `${suite} never draws ${path} for a reader who is ${block}`).toContain(
          `WRITE_REASONS.${block}`,
        );
      }
    }
  });

  test("has a fixture for every control it exports, and no other", () => {
    expect(Object.keys(FIXTURES).sort()).toEqual(Object.keys(writes).sort());
  });
});

describe.each(Object.keys(FIXTURES) as (keyof typeof writes)[])("%s", (control) => {
  const Control = writes[control] as ComponentType<object>;
  const { props, name } = FIXTURES[control];

  test.each(READERS)("is drawn for $who, enabled only where it can act", ({ viewer, reason }) => {
    vi.mocked(useViewer).mockReturnValue(viewer(TOOLS[control]));
    render(<Control {...props} />);
    const button = screen.getByRole("button", { name });
    if (reason === null) {
      expect(button.getAttribute("aria-disabled")).not.toBe("true");
      return;
    }
    expect(button.getAttribute("aria-disabled")).toBe("true");
    const described = (button.getAttribute("aria-describedby") ?? "")
      .split(/\s+/)
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" ");
    expect(described).toContain(reason);
  });
});
